package monitor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"lingobridge/internal/store"
)

// resourceAccessRecoveryService owns startup recovery and terminal-result
// reconciliation for the resource-access workflow. It coordinates the
// workflow facade but is kept separate from live card and OAuth callbacks.
type resourceAccessRecoveryService struct {
	workflow *resourceAccessManager
}

func (m *resourceAccessManager) resourceAccessRecoveryService() *resourceAccessRecoveryService {
	return &resourceAccessRecoveryService{workflow: m}
}

func (m *resourceAccessManager) recoverPersistedRequests(ctx context.Context) error {
	if m == nil {
		return fmt.Errorf("feishu resource access recovery is unavailable")
	}
	return m.resourceAccessRecoveryService().recoverPersistedRequests(ctx)
}

func (m *resourceAccessManager) recoverApprovedPendingResourceAccess(ctx context.Context, now time.Time) (int, error) {
	if m == nil {
		return 0, fmt.Errorf("feishu resource access recovery is unavailable")
	}
	return m.resourceAccessRecoveryService().recoverApprovedPendingResourceAccess(ctx, now)
}

func (m *resourceAccessManager) reconcileTerminalResourceAccessResults(ctx context.Context, updatedBefore time.Time) (int, error) {
	if m == nil {
		return 0, fmt.Errorf("feishu resource access recovery is unavailable")
	}
	return m.resourceAccessRecoveryService().reconcileTerminalResourceAccessResults(ctx, updatedBefore)
}

func (s *resourceAccessRecoveryService) recoverPersistedRequests(ctx context.Context) error {
	m := s.workflow
	if err := m.recoverFeishuOAuthRefreshAttempts(ctx); err != nil {
		return fmt.Errorf("recover persisted feishu OAuth refresh attempts: %w", err)
	}
	if _, err := m.cleanupTerminalFeishuOAuthRefreshAttempts(ctx); err != nil {
		feishuLog.Warn(ctx, "cleanup terminal feishu OAuth refresh attempts during startup failed account=%s error_type=%T: %v",
			m.account.ID, err, err)
	}
	now := m.currentTime()
	expiredGrants, err := m.store.ExpireFeishuResourceGrants(m.account.ID, now)
	if err != nil {
		return fmt.Errorf("expire persisted feishu resource grants: %w", err)
	}
	expired, err := m.store.ExpireFeishuResourceAccessRequests(m.account.ID, now)
	if err != nil {
		return fmt.Errorf("expire persisted feishu resource access requests: %w", err)
	}
	interrupted, err := m.store.ListExecutingFeishuResourceAccessRequests(m.account.ID)
	if err != nil {
		return fmt.Errorf("list interrupted feishu resource access requests: %w", err)
	}
	if expired > 0 {
		feishuLog.Info(ctx, "expired persisted feishu resource access requests account=%s count=%d", m.account.ID, expired)
	}
	if expiredGrants > 0 {
		feishuLog.Info(ctx, "expired persisted feishu resource grants account=%s count=%d", m.account.ID, expiredGrants)
	}
	recoveredInterrupted, failedInterrupted, err := s.recoverExecutingResourceAccess(ctx, interrupted)
	if err != nil {
		return err
	}
	if recoveredInterrupted > 0 {
		feishuLog.Info(ctx, "recovered verified interrupted feishu resource access requests account=%s count=%d", m.account.ID, recoveredInterrupted)
	}
	if failedInterrupted > 0 {
		feishuLog.Warn(ctx, "closed unverified interrupted feishu resource access requests account=%s count=%d", m.account.ID, failedInterrupted)
	}
	resumed, err := s.recoverApprovedPendingResourceAccess(ctx, now)
	if err != nil {
		return err
	}
	if resumed > 0 {
		feishuLog.Info(ctx, "resumed approved feishu resource access requests account=%s count=%d", m.account.ID, resumed)
	}
	reconciled, err := s.reconcileTerminalResourceAccessResults(ctx, now)
	if err != nil {
		return fmt.Errorf("reconcile persisted feishu resource access results: %w", err)
	}
	if reconciled > 0 {
		feishuLog.Info(ctx, "reconciled persisted feishu resource access results account=%s count=%d", m.account.ID, reconciled)
	}
	return nil
}

func (s *resourceAccessRecoveryService) recoverExecutingResourceAccess(
	ctx context.Context,
	interrupted []store.FeishuResourceAccessRequest,
) (recovered, failed int, err error) {
	m := s.workflow
	for _, request := range interrupted {
		verified, verifyErr := m.verifyTenantAccessAfterMutation(ctx, request, true)
		if verifyErr != nil {
			return recovered, failed, fmt.Errorf("verify interrupted feishu resource access %s: %w", shortRequestID(request.ID), verifyErr)
		}
		if verified {
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
			if completeErr := m.completeSelectedResourceGrant(ctx, request, capability, store.FeishuResourceGrantSourceNewlyGranted); completeErr != nil {
				return recovered, failed, fmt.Errorf("complete verified interrupted feishu resource access %s: %w", shortRequestID(request.ID), completeErr)
			}
			recovered++
			continue
		}

		failedAt := m.currentTime()
		if failErr := m.store.FailFeishuResourceAccessRequest(request.ID, request.AccountID, failedAt); failErr != nil {
			if errors.Is(failErr, store.ErrFeishuResourceAccessResolved) {
				continue
			}
			return recovered, failed, fmt.Errorf("close unverified interrupted feishu resource access %s: %w", shortRequestID(request.ID), failErr)
		}
		request.UpdatedAt = failedAt
		message := "LingoBridge 重启后核验到飞书并未授予所需资源权限，请重新调用资源授权工具。"
		m.updateResourceAccessResultCard(ctx, request, statusCard{title: "资源授权未完成", template: "red", message: message})
		m.persistResourceWorkflowResult(ctx, request, store.WorkflowResultStateFailed, "failed", "", message, failedAt)
		failed++
	}
	return recovered, failed, nil
}

func (s *resourceAccessRecoveryService) recoverApprovedPendingResourceAccess(ctx context.Context, now time.Time) (int, error) {
	m := s.workflow
	const batchSize = 100
	resumed := 0
	previousBatch := ""
	for {
		approved, err := m.store.ListApprovedPendingFeishuResourceAccessRequests(m.account.ID, now, batchSize)
		if err != nil {
			return resumed, fmt.Errorf("list approved pending feishu resource access requests: %w", err)
		}
		if len(approved) == 0 {
			return resumed, nil
		}
		ids := make([]string, 0, len(approved))
		for _, request := range approved {
			ids = append(ids, request.ID)
		}
		batchKey := strings.Join(ids, "\x00")
		if batchKey == previousBatch {
			return resumed, fmt.Errorf("recover approved feishu resource access requests made no progress")
		}
		previousBatch = batchKey
		for _, request := range approved {
			if err := m.completeApprovedResourceAccess(ctx, request); err != nil {
				if m.preserveResourceAccessAfterNonMutatingInterruption(ctx, request, err, "startup_recovery") {
					if ctxErr := ctx.Err(); ctxErr != nil {
						return resumed, ctxErr
					}
					return resumed, nil
				}
				m.finishResourceAccessFailure(
					ctx,
					request,
					err,
					"资源授权恢复失败",
					"LingoBridge 重启后未能继续本次资源授权，请重新调用资源授权工具。",
				)
			}
			resumed++
		}
	}
}

func (s *resourceAccessRecoveryService) reconcileTerminalResourceAccessResults(ctx context.Context, updatedBefore time.Time) (int, error) {
	m := s.workflow
	const batchSize = 100
	total := 0
	recoveryCardCtx, cancelCardUpdates := context.WithTimeout(m.baseContext(), m.resourceCardUpdateTimeout())
	defer cancelCardUpdates()
	for {
		gaps, err := m.store.ListTerminalWorkflowResultGaps(
			m.account.ID,
			store.WorkflowRequestKindFeishuResourceAccess,
			updatedBefore,
			batchSize,
		)
		if err != nil {
			return total, err
		}
		for _, gap := range gaps {
			request, err := m.store.GetFeishuResourceAccessRequest(gap.ID, gap.AccountID)
			if err != nil {
				return total, fmt.Errorf("load terminal resource access request %s: %w", shortRequestID(gap.ID), err)
			}
			resultState, status, source, message, err := recoveredResourceAccessResult(request, gap.State)
			if err != nil {
				return total, err
			}
			if recoveryCardCtx.Err() == nil {
				m.updateResourceAccessResultCard(recoveryCardCtx, request, recoveredResourceAccessStatusCard(gap.State, message))
			}
			_, ready, err := persistWorkflowResult(
				m.store,
				request.ID,
				request.AccountID,
				resultState,
				resourceWorkflowResultPayload(request, status, source, message),
				gap.UpdatedAt,
			)
			if err != nil {
				return total, fmt.Errorf("store recovered resource access result %s: %w", shortRequestID(gap.ID), err)
			}
			total++
			feishuLog.Debug(ctx, "reconciled feishu resource access result request=%s account=%s workflow_state=%s result_state=%s ready=%t",
				shortRequestID(gap.ID), gap.AccountID, gap.State, resultState, ready)
		}
		if len(gaps) < batchSize {
			return total, nil
		}
	}
}

func recoveredResourceAccessStatusCard(workflowState, message string) statusCard {
	switch workflowState {
	case store.WorkflowRequestStateDenied:
		return statusCard{title: "已拒绝授权", template: "grey", message: message}
	case store.WorkflowRequestStateExpired:
		return statusCard{title: "授权已过期", template: "grey", message: message}
	case store.WorkflowRequestStateFailed:
		return statusCard{title: "资源授权未完成", template: "red", message: message}
	case store.WorkflowRequestStateSucceeded:
		return statusCard{title: "权限已授予", template: "green", message: message}
	default:
		return statusCard{title: "资源授权结果", template: "orange", message: message}
	}
}

func recoveredResourceAccessResult(request store.FeishuResourceAccessRequest, workflowState string) (resultState, status, source, message string, err error) {
	switch workflowState {
	case store.WorkflowRequestStateDenied:
		return store.WorkflowResultStateDenied, "denied", "", "用户拒绝了本次资源授权。", nil
	case store.WorkflowRequestStateExpired:
		return store.WorkflowResultStateExpired, "expired", "", "资源授权请求已过期。", nil
	case store.WorkflowRequestStateFailed:
		return store.WorkflowResultStateFailed, "failed", "", "资源授权未能完成。", nil
	case store.WorkflowRequestStateSucceeded:
		source := strings.TrimSpace(request.GrantSource)
		return store.WorkflowResultStateSucceeded, "granted", source, resourceAccessSuccessMessage(request, request.UpdatedAt), nil
	default:
		return "", "", "", "", fmt.Errorf("unsupported terminal resource access state %q", workflowState)
	}
}
