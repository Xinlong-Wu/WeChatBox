package monitor

import (
	"context"
	"errors"
	"fmt"
	"time"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkdrive "github.com/larksuite/oapi-sdk-go/v3/service/drive/v1"

	"lingobridge/internal/store"
)

func (m *resourceAccessManager) completeApprovedResourceAccessFromCard(request store.FeishuResourceAccessRequest) {
	// Approval callbacks reserve a background task before recording the durable
	// decision. Orderly shutdown must let that admitted mutation drain while the
	// account lease is still renewed; ownership loss still cancels immediately.
	drainCtx, cancelDrain := feishuRuntimeDrainContext(m.baseContext())
	defer cancelDrain()
	ctx, cancel := context.WithTimeout(drainCtx, resourceAccessCallbackTimeout)
	defer cancel()
	if err := m.completeApprovedResourceAccess(ctx, request); err != nil {
		if m.preserveResourceAccessAfterNonMutatingInterruption(ctx, request, err, "card_callback") {
			return
		}
		m.finishResourceAccessFailure(
			ctx,
			request,
			err,
			"资源授权失败",
			"LingoBridge 未能完成本次资源授权，请重新调用资源授权工具。",
		)
	}
}

func (m *resourceAccessManager) completeApprovedResourceAccess(ctx context.Context, request store.FeishuResourceAccessRequest) error {
	current, err := m.store.GetFeishuResourceAccessRequest(request.ID, request.AccountID)
	if err != nil {
		return fmt.Errorf("reload approved feishu resource access request: %w", err)
	}
	request = current
	if request.State != store.FeishuResourceAccessStatePending {
		return store.ErrFeishuResourceAccessResolved
	}
	if request.GrantMode != store.FeishuResourceGrantModeOnce && request.GrantMode != store.FeishuResourceGrantModeAll {
		return fmt.Errorf("approved feishu resource access request has no valid grant mode")
	}
	if request.SubjectType == "" || request.SubjectID == "" {
		return fmt.Errorf("approved feishu resource access request has no collaborator subject")
	}

	capability, active, err := m.store.ActiveFeishuResourceCapability(
		request.AccountID,
		request.ResourceType,
		request.ResourceToken,
		request.SubjectType,
		request.SubjectID,
		request.Permission,
	)
	if err != nil {
		return fmt.Errorf("load approved feishu resource capability: %w", err)
	}
	if active {
		verified, verifyErr := m.verifyTenantAccess(
			ctx,
			request.ResourceType,
			request.ResourceToken,
			request.Permission,
			capability.SubjectType,
			capability.SubjectID,
		)
		if verifyErr != nil {
			return fmt.Errorf("live-check approved feishu resource capability: %w", verifyErr)
		}
		if verified {
			capability.SourceRequestID = request.ID
			capability.VerifiedAt = m.currentTime()
			capability.UpdatedAt = capability.VerifiedAt
			return m.completeSelectedResourceGrant(ctx, request, capability, store.FeishuResourceGrantSourceExistingGrant)
		}
		m.revokeResourceCapabilityAndGrantBestEffort(ctx, request, capability.SubjectType, capability.SubjectID, "approved capability no longer verifies")
	}

	if !m.oauthEnabled() {
		return fmt.Errorf("feishu OAuth is unavailable and no verified resource capability exists")
	}
	accessToken, err := m.feishuUserAccessToken(ctx, request.ActorOpenID, request.ActorUserID)
	if err != nil {
		if errors.Is(err, ErrFeishuUserOAuthReauthorizationNeeded) {
			return m.prepareResourceAccessOAuthHandoff(ctx, request)
		}
		return fmt.Errorf("load persisted feishu user OAuth credential: %w", err)
	}
	request, err = m.store.ClaimFeishuResourceAccessExecution(request.ID, request.AccountID, m.currentTime())
	if err != nil {
		return fmt.Errorf("claim approved feishu resource access execution: %w", err)
	}
	return m.grantAndCompleteSelectedResourceAccess(ctx, request, accessToken)
}

func (m *resourceAccessManager) grantAndCompleteSelectedResourceAccess(ctx context.Context, request store.FeishuResourceAccessRequest, accessToken string) error {
	createCode, createErr := m.grantResourceAccess(ctx, accessToken, request)
	if feishuRuntimeOwnershipLost(m.baseContext()) {
		return fmt.Errorf("%w after collaborator mutation", errFeishuResourceAccessOwnershipLost)
	}
	verified, verifyErr := m.verifyTenantAccessAfterMutation(ctx, request, createErr == nil || createCode == 0)
	if verifyErr != nil {
		if createErr != nil {
			return errors.Join(createErr, verifyErr)
		}
		return verifyErr
	}
	// Permission creation has no idempotency token. A lost HTTP response is
	// therefore reconciled by the exact live capability check before treating
	// the request as failed or attempting an existing-member upgrade.
	if !verified && createErr != nil && createCode != 1063003 {
		return createErr
	}
	if !verified && createCode == 1063003 {
		updateErr := m.updateResourceAccess(ctx, accessToken, request)
		if feishuRuntimeOwnershipLost(m.baseContext()) {
			return fmt.Errorf("%w after collaborator update", errFeishuResourceAccessOwnershipLost)
		}
		verified, verifyErr = m.verifyTenantAccessAfterMutation(ctx, request, true)
		if verifyErr != nil {
			if updateErr != nil {
				return errors.Join(updateErr, verifyErr)
			}
			return verifyErr
		}
		if !verified && updateErr != nil {
			return updateErr
		}
	}
	if !verified {
		return fmt.Errorf("feishu permission verification returned false")
	}
	completedAt := m.currentTime()
	capability := store.FeishuResourceCapability{
		AccountID:         request.AccountID,
		ResourceType:      request.ResourceType,
		ResourceToken:     request.ResourceToken,
		SubjectType:       request.SubjectType,
		SubjectID:         request.SubjectID,
		Permission:        request.Permission,
		SourceActorOpenID: request.ActorOpenID,
		SourceActorUserID: request.ActorUserID,
		SourceRequestID:   request.ID,
		State:             store.FeishuResourceCapabilityStateActive,
		CreatedAt:         completedAt,
		VerifiedAt:        completedAt,
	}
	return m.completeSelectedResourceGrant(ctx, request, capability, store.FeishuResourceGrantSourceNewlyGranted)
}

func (m *resourceAccessManager) verifyTenantAccessAfterMutation(
	ctx context.Context,
	request store.FeishuResourceAccessRequest,
	recheckNotVisible bool,
) (bool, error) {
	verified, err := m.verifyTenantAccess(
		ctx,
		request.ResourceType,
		request.ResourceToken,
		request.Permission,
		request.SubjectType,
		request.SubjectID,
	)
	if err == nil && (verified || !recheckNotVisible) {
		return verified, err
	}
	if feishuRuntimeOwnershipLost(m.baseContext()) {
		return false, err
	}

	// The collaborator mutation may have succeeded immediately before the
	// operation deadline elapsed or its first verification response may also
	// have been lost. Give this safe read-only reconciliation one independent,
	// bounded runtime retry instead of replaying the collaborator mutation.
	drainCtx, cancelDrain := feishuRuntimeDrainContext(m.baseContext())
	defer cancelDrain()
	verifyCtx, cancel := context.WithTimeout(drainCtx, resourceAccessVerificationRetryTimeout)
	defer cancel()
	operationContextEnded := ctx != nil && ctx.Err() != nil
	if err != nil {
		feishuLog.Warn(m.baseContext(), "retrying feishu resource permission verification after first check failed request=%s account=%s type=%s resource_ref=%s permission=%s operation_context_ended=%t error_type=%T",
			shortRequestID(request.ID), request.AccountID, request.ResourceType, shortResourceRef(request.ResourceToken), request.Permission, operationContextEnded, err)
	} else {
		feishuLog.Debug(m.baseContext(), "rechecking feishu resource permission after collaborator mutation is not yet visible request=%s account=%s type=%s resource_ref=%s permission=%s",
			shortRequestID(request.ID), request.AccountID, request.ResourceType, shortResourceRef(request.ResourceToken), request.Permission)
		timer := time.NewTimer(resourceAccessVerificationRetryDelay)
		defer timer.Stop()
		select {
		case <-verifyCtx.Done():
			return false, verifyCtx.Err()
		case <-timer.C:
		}
	}
	retryVerified, retryErr := m.verifyTenantAccess(
		verifyCtx,
		request.ResourceType,
		request.ResourceToken,
		request.Permission,
		request.SubjectType,
		request.SubjectID,
	)
	if retryErr != nil {
		if err != nil {
			return false, errors.Join(err, retryErr)
		}
		return false, retryErr
	}
	return retryVerified, nil
}

func (m *resourceAccessManager) completeSelectedResourceGrant(
	ctx context.Context,
	request store.FeishuResourceAccessRequest,
	capability store.FeishuResourceCapability,
	source string,
) error {
	if feishuRuntimeOwnershipLost(m.baseContext()) {
		return errFeishuResourceAccessOwnershipLost
	}
	completedAt := m.currentTime()
	grantActorType, grantActorID, err := resourceAccessGrantActor(request.ActorOpenID, request.ActorUserID)
	if err != nil {
		return err
	}
	grant := store.FeishuResourceGrant{
		AccountID:       request.AccountID,
		ActorType:       grantActorType,
		ActorID:         grantActorID,
		ChatID:          request.ChatID,
		ResourceType:    request.ResourceType,
		ResourceToken:   request.ResourceToken,
		Permission:      request.Permission,
		GrantMode:       request.GrantMode,
		SourceRequestID: request.ID,
		State:           store.FeishuResourceGrantStateActive,
		CreatedAt:       completedAt,
		UpdatedAt:       completedAt,
	}
	if request.GrantMode == store.FeishuResourceGrantModeOnce {
		grant.ExpiresAt = completedAt.Add(time.Duration(request.OnceDurationMinutes) * time.Minute)
	}
	capability.SourceRequestID = request.ID
	capability.VerifiedAt = completedAt
	capability.UpdatedAt = completedAt
	if capability.CreatedAt.IsZero() {
		capability.CreatedAt = completedAt
	}
	if err := m.store.CompleteFeishuResourceAccessRequest(
		request.ID,
		request.AccountID,
		source,
		request.Permission,
		&capability,
		&grant,
		completedAt,
	); err != nil {
		// The Feishu collaborator mutation has already been verified. Never turn
		// a local transaction failure into a terminal authorization failure: the
		// executing request is the durable fence that startup recovery can verify
		// and complete without replaying the remote mutation.
		current, loadErr := m.store.GetFeishuResourceAccessRequest(request.ID, request.AccountID)
		if loadErr == nil && current.State == store.FeishuResourceAccessStateSucceeded {
			feishuLog.Debug(ctx, "reconciled feishu resource authorization completion after ambiguous local error request=%s account=%s",
				shortRequestID(request.ID), request.AccountID)
		} else {
			return fmt.Errorf("%w: %v", errFeishuResourceAccessCompletionDeferred, err)
		}
	}
	request.UpdatedAt = completedAt
	message := resourceAccessSuccessMessage(request, completedAt)
	feishuLog.Info(ctx, "granted feishu resource access request=%s account=%s user=%s chat=%s type=%s resource_ref=%s permission=%s grant_mode=%s once_minutes=%d subject_type=%s source=%s",
		shortRequestID(request.ID), request.AccountID, resourceAccessActorID(request), request.ChatID,
		request.ResourceType, shortResourceRef(request.ResourceToken), request.Permission,
		request.GrantMode, request.OnceDurationMinutes, request.SubjectType, source)
	m.updateResourceAccessResultCard(ctx, request, statusCard{
		title:    "权限已授予",
		template: "green",
		message:  message,
	})
	m.persistResourceWorkflowResult(ctx, request, store.WorkflowResultStateSucceeded, "granted", source, message, completedAt)
	return nil
}

func resourceAccessSuccessMessage(request store.FeishuResourceAccessRequest, completedAt time.Time) string {
	if request.GrantMode == store.FeishuResourceGrantModeAll {
		return "飞书已确认所需资源权限；LingoBridge 已为当前用户、机器人账号、当前对话和这一精确资源保存永久授权。"
	}
	expiresAt := completedAt.Add(time.Duration(request.OnceDurationMinutes) * time.Minute)
	return fmt.Sprintf("飞书已确认所需资源权限；LingoBridge 将允许当前范围使用该权限 %d 分钟，至 %s。到期后不会撤销飞书协作者。",
		request.OnceDurationMinutes, expiresAt.UTC().Format("2006-01-02 15:04 UTC"))
}

func (m *resourceAccessManager) grantResourceAccess(ctx context.Context, accessToken string, request store.FeishuResourceAccessRequest) (int, error) {
	perm := larkdrive.PermCreatePermissionMemberView
	if request.Permission == store.FeishuResourcePermissionWrite {
		perm = larkdrive.PermCreatePermissionMemberEdit
		if request.ResourceType == "folder" && request.SubjectType == larkdrive.MemberTypeOpenChat {
			perm = larkdrive.PermCreatePermissionMemberFullAccess
		}
	}
	memberKind := larkdrive.TypeCreatePermissionMemberUser
	if request.SubjectType == larkdrive.MemberTypeOpenChat {
		memberKind = larkdrive.TypeCreatePermissionMemberChat
	}
	req := larkdrive.NewCreatePermissionMemberReqBuilder().
		Token(request.ResourceToken).
		Type(request.ResourceType).
		NeedNotification(false).
		BaseMember(larkdrive.NewBaseMemberBuilder().
			MemberType(request.SubjectType).
			MemberId(request.SubjectID).
			Perm(perm).
			Type(memberKind).
			Build()).
		Build()
	resp, err := m.client.Drive.PermissionMember.Create(ctx, req, larkcore.WithUserAccessToken(accessToken))
	if err != nil {
		return 0, fmt.Errorf("grant feishu resource permission: %w", err)
	}
	if resp == nil {
		return 0, fmt.Errorf("grant feishu resource permission: empty response")
	}
	if !resp.Success() {
		return resp.Code, fmt.Errorf("grant feishu resource permission code=%d msg=%s", resp.Code, resp.Msg)
	}
	return resp.Code, nil
}

func (m *resourceAccessManager) updateResourceAccess(ctx context.Context, accessToken string, request store.FeishuResourceAccessRequest) error {
	perm := larkdrive.PermUpdatePermissionMemberView
	if request.Permission == store.FeishuResourcePermissionWrite {
		perm = larkdrive.PermUpdatePermissionMemberEdit
		if request.ResourceType == "folder" && request.SubjectType == larkdrive.MemberTypeOpenChat {
			perm = larkdrive.PermUpdatePermissionMemberFullAccess
		}
	}
	memberKind := larkdrive.TypeUpdatePermissionMemberUser
	if request.SubjectType == larkdrive.MemberTypeOpenChat {
		memberKind = larkdrive.TypeUpdatePermissionMemberChat
	}
	req := larkdrive.NewUpdatePermissionMemberReqBuilder().
		Token(request.ResourceToken).
		MemberId(request.SubjectID).
		Type(request.ResourceType).
		NeedNotification(false).
		BaseMember(larkdrive.NewBaseMemberBuilder().
			MemberType(request.SubjectType).
			MemberId(request.SubjectID).
			Perm(perm).
			Type(memberKind).
			Build()).
		Build()
	resp, err := m.client.Drive.PermissionMember.Update(ctx, req, larkcore.WithUserAccessToken(accessToken))
	if err != nil {
		return fmt.Errorf("update feishu resource permission: %w", err)
	}
	if resp == nil {
		return fmt.Errorf("update feishu resource permission: empty response")
	}
	if !resp.Success() {
		return fmt.Errorf("update feishu resource permission code=%d msg=%s", resp.Code, resp.Msg)
	}
	return nil
}
