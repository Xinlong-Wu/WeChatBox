package monitor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkdrive "github.com/larksuite/oapi-sdk-go/v3/service/drive/v1"

	feishutools "lingobridge/internal/platform/feishu/tools"
	"lingobridge/internal/store"
)

// resourcePermissionService owns the side-effect-free protected-resource
// guard and live Feishu capability verification. It has no dependency on
// cards, OAuth handoff state, continuations, or operation approvals.
type resourcePermissionService struct {
	store     resourcePermissionStore
	client    *lark.Client
	account   store.Account
	botOpenID string
	now       func() time.Time
}

func (s *resourcePermissionService) currentTime() time.Time {
	if s == nil || s.now == nil {
		return time.Now().UTC()
	}
	return s.now().UTC()
}

func (m *resourceAccessManager) resourcePermissionService() *resourcePermissionService {
	if m == nil {
		return nil
	}
	return &resourcePermissionService{
		store:     m.store,
		client:    m.client,
		account:   m.account,
		botOpenID: m.botOpenID,
		now:       m.currentTime,
	}
}

func (m *resourceAccessManager) Require(ctx context.Context, requirement feishutools.ResourceAccessRequirement) (feishutools.AuthorizedResource, error) {
	if m == nil {
		return feishutools.AuthorizedResource{}, fmt.Errorf("feishu resource permission service is unavailable")
	}
	return m.resourcePermissionService().Require(ctx, requirement)
}

func (m *resourceAccessManager) verifyTenantAccess(ctx context.Context, resourceType, resourceToken, permission, subjectType, subjectID string) (bool, error) {
	return m.resourcePermissionService().verifyTenantAccess(ctx, resourceType, resourceToken, permission, subjectType, subjectID)
}

func (m *resourceAccessManager) discoverTenantCapability(ctx context.Context, request store.FeishuResourceAccessRequest) (bool, error) {
	if m != nil && m.discoverCapability != nil {
		return m.discoverCapability(ctx, request)
	}
	return m.resourcePermissionService().discoverTenantCapability(ctx, request)
}

func (m *resourceAccessManager) resourceGrantSubject(chat feishutools.ChatContext, resourceType string) (string, string, string) {
	return m.resourcePermissionService().resourceGrantSubject(chat, resourceType)
}

func (m *resourceAccessManager) revokeResourceCapabilityAndGrantBestEffort(
	ctx context.Context,
	request store.FeishuResourceAccessRequest,
	subjectType, subjectID, reason string,
) {
	m.resourcePermissionService().revokeResourceCapabilityAndGrantBestEffort(ctx, request, subjectType, subjectID, reason)
}

func (s *resourcePermissionService) Require(ctx context.Context, requirement feishutools.ResourceAccessRequirement) (feishutools.AuthorizedResource, error) {
	actor, chat, err := trustedResourceAccessScope(ctx)
	if err != nil {
		return feishutools.AuthorizedResource{}, err
	}
	requirement.ResourceType = feishutools.NormalizeResourceType(requirement.ResourceType)
	requirement.ResourceToken = strings.TrimSpace(requirement.ResourceToken)
	requirement.Permission = strings.ToLower(strings.TrimSpace(requirement.Permission))
	if !feishutools.SupportedResourceType(requirement.ResourceType) || requirement.ResourceToken == "" ||
		(requirement.Permission != feishutools.ResourcePermissionRead && requirement.Permission != feishutools.ResourcePermissionWrite) {
		return feishutools.AuthorizedResource{}, fmt.Errorf("valid feishu resource type, token, and read/write permission are required")
	}
	if _, err := s.store.GetFeishuBotResource(s.account.ID, requirement.ResourceType, requirement.ResourceToken); err == nil {
		feishuLog.Debug(ctx, "allowed Bot-owned feishu resource access account=%s chat=%s type=%s resource_ref=%s permission=%s",
			s.account.ID, chat.ChatID, requirement.ResourceType, shortResourceRef(requirement.ResourceToken), requirement.Permission)
		return feishutools.AuthorizedResource{
			AccountID:             s.account.ID,
			ActorOpenID:           actor.OpenID,
			ActorUserID:           actor.UserID,
			ChatID:                chat.ChatID,
			ResourceType:          requirement.ResourceType,
			ResourceToken:         requirement.ResourceToken,
			EffectivePermission:   requirement.Permission,
			GrantMode:             feishutools.ResourceAccessGrantModeBotOwner,
			CapabilitySubjectType: "bot",
			CapabilitySubjectID:   s.botOpenID,
			Source:                feishutools.ResourceAccessSourceBotOwner,
		}, nil
	} else if !errors.Is(err, store.ErrFeishuBotResourceNotFound) {
		return feishutools.AuthorizedResource{}, fmt.Errorf("check Bot-owned feishu resource: %w", err)
	}
	subjectType, subjectID, unsupportedMessage := s.resourceGrantSubject(chat, requirement.ResourceType)
	if subjectType == "" || subjectID == "" {
		return feishutools.AuthorizedResource{}, feishutools.NewResourceAuthorizationRequiredError(requirement, unsupportedMessage)
	}
	grantActorType, grantActorID, actorErr := resourceAccessGrantActor(actor.OpenID, actor.UserID)
	if actorErr != nil {
		return feishutools.AuthorizedResource{}, actorErr
	}
	grant, activeGrant, grantErr := s.store.ActiveFeishuResourceGrant(
		s.account.ID,
		grantActorType,
		grantActorID,
		chat.ChatID,
		requirement.ResourceType,
		requirement.ResourceToken,
		requirement.Permission,
		s.currentTime(),
	)
	if grantErr != nil {
		return feishutools.AuthorizedResource{}, fmt.Errorf("load feishu resource grant: %w", grantErr)
	}
	if !activeGrant {
		return feishutools.AuthorizedResource{}, feishutools.NewResourceAuthorizationRequiredError(requirement, "")
	}
	capability, activeCapability, capabilityErr := s.store.ActiveFeishuResourceCapability(
		s.account.ID,
		requirement.ResourceType,
		requirement.ResourceToken,
		subjectType,
		subjectID,
		requirement.Permission,
	)
	if capabilityErr != nil {
		return feishutools.AuthorizedResource{}, fmt.Errorf("load feishu resource capability: %w", capabilityErr)
	}
	request := store.FeishuResourceAccessRequest{
		AccountID:     s.account.ID,
		ActorOpenID:   actor.OpenID,
		ActorUserID:   actor.UserID,
		ChatID:        chat.ChatID,
		ResourceType:  requirement.ResourceType,
		ResourceToken: requirement.ResourceToken,
		Permission:    requirement.Permission,
		SubjectType:   subjectType,
		SubjectID:     subjectID,
	}
	if !activeCapability {
		s.revokeResourceCapabilityAndGrantBestEffort(ctx, request, subjectType, subjectID, "missing protected-tool capability")
		return feishutools.AuthorizedResource{}, feishutools.NewResourceAuthorizationRequiredError(requirement, "")
	}
	verified, verifyErr := s.verifyTenantAccess(ctx, requirement.ResourceType, requirement.ResourceToken, requirement.Permission, capability.SubjectType, capability.SubjectID)
	if verifyErr != nil {
		return feishutools.AuthorizedResource{}, fmt.Errorf("live-check feishu resource access: %w", verifyErr)
	}
	if !verified {
		s.revokeResourceCapabilityAndGrantBestEffort(ctx, request, capability.SubjectType, capability.SubjectID, "invalid protected-tool access")
		return feishutools.AuthorizedResource{}, feishutools.NewResourceAuthorizationRequiredError(requirement, "")
	}
	verifiedAt := s.currentTime()
	capability.VerifiedAt = verifiedAt
	capability.UpdatedAt = verifiedAt
	if _, err := s.store.UpsertFeishuResourceCapability(capability); err != nil {
		return feishutools.AuthorizedResource{}, fmt.Errorf("refresh feishu resource capability verification: %w", err)
	}
	feishuLog.Debug(ctx, "allowed scoped feishu resource access account=%s user=%s chat=%s type=%s resource_ref=%s permission=%s subject_type=%s",
		s.account.ID, grantActorID, chat.ChatID, requirement.ResourceType,
		shortResourceRef(requirement.ResourceToken), requirement.Permission, capability.SubjectType)
	return feishutools.AuthorizedResource{
		AccountID:             s.account.ID,
		ActorOpenID:           actor.OpenID,
		ActorUserID:           actor.UserID,
		ChatID:                chat.ChatID,
		ResourceType:          requirement.ResourceType,
		ResourceToken:         requirement.ResourceToken,
		EffectivePermission:   grant.Permission,
		GrantMode:             grant.GrantMode,
		ExpiresAt:             grant.ExpiresAt,
		CapabilitySubjectType: capability.SubjectType,
		CapabilitySubjectID:   capability.SubjectID,
		Source:                feishutools.ResourceAccessSourceExistingGrant,
	}, nil
}

func (s *resourcePermissionService) verifyTenantAccess(ctx context.Context, resourceType, resourceToken, permission, subjectType, subjectID string) (bool, error) {
	if resourceType == "folder" {
		resp, err := s.client.Drive.PermissionMember.List(ctx, larkdrive.NewListPermissionMemberReqBuilder().
			Token(resourceToken).
			Type(resourceType).
			Build())
		if err != nil {
			return false, fmt.Errorf("list feishu folder collaborators: %w", err)
		}
		if resp == nil || !resp.Success() {
			if resp == nil {
				return false, fmt.Errorf("list feishu folder collaborators: empty response")
			}
			return false, fmt.Errorf("list feishu folder collaborators code=%d msg=%s", resp.Code, resp.Msg)
		}
		if resp.Data == nil {
			return false, nil
		}
		for _, member := range resp.Data.Items {
			if member == nil || strings.TrimSpace(deref(member.MemberType)) != subjectType || strings.TrimSpace(deref(member.MemberId)) != subjectID {
				continue
			}
			return feishuCollaboratorPermissionSatisfies(strings.TrimSpace(deref(member.Perm)), permission, true), nil
		}
		return false, nil
	}
	action := larkdrive.PermView
	if permission == store.FeishuResourcePermissionWrite {
		action = larkdrive.PermEdit
	}
	resp, err := s.client.Drive.PermissionMember.Auth(ctx, larkdrive.NewAuthPermissionMemberReqBuilder().
		Token(resourceToken).
		Type(resourceType).
		Action(action).
		Build())
	if err != nil {
		return false, fmt.Errorf("verify feishu resource permission: %w", err)
	}
	if resp == nil || !resp.Success() {
		if resp == nil {
			return false, fmt.Errorf("verify feishu resource permission: empty response")
		}
		return false, fmt.Errorf("verify feishu resource permission code=%d msg=%s", resp.Code, resp.Msg)
	}
	return resp.Data != nil && resp.Data.AuthResult != nil && *resp.Data.AuthResult, nil
}

// discoverTenantCapability performs a read-only Feishu permission check when
// LingoBridge has no sufficient local capability row. A positive result records
// the external fact only; it deliberately does not create the user/chat-scoped
// local grant, which still requires an explicit once/all card decision.
func (s *resourcePermissionService) discoverTenantCapability(ctx context.Context, request store.FeishuResourceAccessRequest) (bool, error) {
	verified, err := s.verifyTenantAccess(
		ctx,
		request.ResourceType,
		request.ResourceToken,
		request.Permission,
		request.SubjectType,
		request.SubjectID,
	)
	if err != nil {
		return false, err
	}
	if !verified {
		feishuLog.Debug(ctx, "no existing feishu resource capability discovered request=%s account=%s chat=%s type=%s resource_ref=%s permission=%s subject_type=%s",
			shortRequestID(request.ID), request.AccountID, request.ChatID, request.ResourceType,
			shortResourceRef(request.ResourceToken), request.Permission, request.SubjectType)
		return false, nil
	}
	verifiedAt := s.currentTime()
	capability, err := s.store.UpsertFeishuResourceCapability(store.FeishuResourceCapability{
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
		CreatedAt:         verifiedAt,
		VerifiedAt:        verifiedAt,
		UpdatedAt:         verifiedAt,
	})
	if err != nil {
		return false, fmt.Errorf("persist discovered feishu resource capability: %w", err)
	}
	feishuLog.Info(ctx, "discovered existing feishu resource capability request=%s account=%s chat=%s type=%s resource_ref=%s permission=%s subject_type=%s source_request=%s",
		shortRequestID(request.ID), request.AccountID, request.ChatID, request.ResourceType,
		shortResourceRef(request.ResourceToken), capability.Permission, capability.SubjectType, shortRequestID(capability.SourceRequestID))
	return true, nil
}

func (s *resourcePermissionService) resourceGrantSubject(chat feishutools.ChatContext, resourceType string) (string, string, string) {
	if resourceType == "folder" {
		if !chat.IsGroup {
			return "", "", "飞书不支持把应用机器人直接添加为私聊外部文件夹的协作者。请使用 Bot 自有目录，或在群聊中给当前群组授予该文件夹权限。"
		}
		return larkdrive.MemberTypeOpenChat, chat.ChatID, ""
	}
	return larkdrive.MemberTypeOpenId, s.botOpenID, ""
}

func (s *resourcePermissionService) revokeResourceCapabilityAndGrantBestEffort(
	ctx context.Context,
	request store.FeishuResourceAccessRequest,
	subjectType, subjectID, reason string,
) {
	now := s.currentTime()
	if strings.TrimSpace(subjectType) != "" && strings.TrimSpace(subjectID) != "" {
		if err := s.store.RevokeFeishuResourceCapability(
			request.AccountID,
			request.ResourceType,
			request.ResourceToken,
			subjectType,
			subjectID,
			now,
		); err != nil && !errors.Is(err, store.ErrFeishuResourceCapabilityNotFound) {
			feishuLog.Warn(ctx, "revoke feishu resource capability failed request=%s account=%s chat=%s type=%s resource_ref=%s reason=%q: %v",
				shortRequestID(request.ID), request.AccountID, request.ChatID, request.ResourceType,
				shortResourceRef(request.ResourceToken), reason, err)
		}
	}
	if err := s.store.RevokeFeishuResourceGrant(
		request.AccountID,
		request.ChatID,
		request.ResourceType,
		request.ResourceToken,
		now,
	); err != nil && !errors.Is(err, store.ErrFeishuResourceGrantNotFound) {
		feishuLog.Warn(ctx, "revoke feishu resource grant failed request=%s account=%s chat=%s type=%s resource_ref=%s reason=%q: %v",
			shortRequestID(request.ID), request.AccountID, request.ChatID, request.ResourceType,
			shortResourceRef(request.ResourceToken), reason, err)
	}
}
