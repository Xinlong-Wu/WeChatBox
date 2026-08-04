package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"

	feishutools "lingobridge/internal/platform/feishu/tools"
	"lingobridge/internal/store"
)

const (
	defaultToolApprovalTTL              = 10 * time.Minute
	defaultApprovedToolExecutionTimeout = 30 * time.Second
)

type toolApprovalStore interface {
	PlatformID() string
	CreateToolApproval(approval store.ToolApproval) (store.ToolApproval, error)
	CreateWorkflowContinuation(store.WorkflowContinuation) (store.WorkflowContinuation, error)
	CancelWorkflowContinuation(requestID, accountID, reason string, now time.Time) error
	StoreWorkflowResult(store.WorkflowResult) (store.WorkflowResult, store.WorkflowContinuation, bool, error)
	ListTerminalWorkflowResultGaps(accountID, kind string, updatedBefore time.Time, limit int) ([]store.WorkflowRequest, error)
	SetToolApprovalCardMessageID(id, accountID, messageID string, now time.Time) error
	DecideToolApproval(id, accountID, decision string, match store.ToolApprovalMatch, now time.Time) (store.ToolApproval, error)
	CompleteToolApproval(id, accountID, state string, now time.Time) error
	FailToolApproval(id, accountID string, now time.Time) error
	GetToolApproval(id, accountID string) (store.ToolApproval, error)
	ExpireToolApprovals(accountID string, now time.Time) (int64, error)
	ListExecutingToolApprovals(accountID string) ([]store.ToolApproval, error)
	UpsertToolApprovalGrant(grant store.ToolApprovalGrant) (store.ToolApprovalGrant, error)
	FindToolApprovalGrant(scope store.ToolApprovalGrantScope) (store.ToolApprovalGrant, bool, error)
	MarkFeishuCardDeliveryDelivered(accountID, requestID, purpose string, revision int64, now time.Time) error
}

func (m *operationApprovalService) recoverPersistedApprovals(ctx context.Context) error {
	now := m.currentTime()
	expired, err := m.store.ExpireToolApprovals(m.account.ID, now)
	if err != nil {
		return fmt.Errorf("expire persisted feishu tool approvals: %w", err)
	}
	interrupted, err := m.store.ListExecutingToolApprovals(m.account.ID)
	if err != nil {
		return fmt.Errorf("list interrupted feishu tool approvals: %w", err)
	}
	resumed := 0
	failed := 0
	for _, approval := range interrupted {
		executor := m.executor(approval.ToolName, approval.ActionKey)
		policy, policyErr := feishutools.OperationApprovalPolicy{}, error(nil)
		if executor != nil {
			policy, policyErr = normalizeOperationApprovalPolicy(executor.OperationApprovalPolicy())
		}
		if policyErr == nil && policy.RecoverInterrupted && policy.ToolName == approval.ToolName && policy.ActionKey == approval.ActionKey {
			releaseTask, accepted := m.tasks.Reserve()
			if !accepted {
				return fmt.Errorf("resume interrupted feishu tool approval %s: runtime is shutting down", shortRequestID(approval.ID))
			}
			resumed++
			go func(approval store.ToolApproval, executor feishutools.OperationApprovalExecutor) {
				defer releaseTask()
				m.executeApproved(approval, executor, "")
			}(approval, executor)
			continue
		}
		if err := m.store.FailToolApproval(approval.ID, approval.AccountID, now); err != nil {
			if errors.Is(err, store.ErrToolApprovalResolved) {
				continue
			}
			return fmt.Errorf("close interrupted feishu tool approval %s: %w", shortRequestID(approval.ID), err)
		}
		failed++
	}
	if expired > 0 {
		feishuLog.Info(ctx, "expired persisted feishu tool approvals account=%s count=%d", m.account.ID, expired)
	}
	if resumed > 0 {
		feishuLog.Info(ctx, "resumed restart-safe interrupted feishu tool approvals account=%s count=%d", m.account.ID, resumed)
	}
	if failed > 0 {
		feishuLog.Warn(ctx, "closed non-recoverable interrupted feishu tool approvals account=%s count=%d", m.account.ID, failed)
	}
	reconciled, err := m.reconcileTerminalApprovalResults(ctx, now)
	if err != nil {
		return fmt.Errorf("reconcile persisted feishu tool approval results: %w", err)
	}
	if reconciled > 0 {
		feishuLog.Info(ctx, "reconciled persisted feishu tool approval results account=%s count=%d", m.account.ID, reconciled)
	}
	return nil
}

type operationApprovalService struct {
	store             toolApprovalStore
	cards             CardService
	account           store.Account
	runCtx            context.Context
	ttl               time.Duration
	executionTimeout  time.Duration
	cardUpdateTimeout time.Duration
	now               func() time.Time

	mu        sync.RWMutex
	executors map[string]feishutools.OperationApprovalExecutor
	tasks     backgroundTaskGroup
}

func newOperationApprovalService(runCtx context.Context, st *store.Store, account store.Account, cards CardService) (*operationApprovalService, error) {
	if st == nil {
		return nil, fmt.Errorf("feishu tool approval store is required")
	}
	if st.PlatformID() != store.PlatformFeishu {
		return nil, fmt.Errorf("feishu tool approval store platform is %q", st.PlatformID())
	}
	if strings.TrimSpace(account.ID) == "" {
		return nil, fmt.Errorf("feishu tool approval account id is required")
	}
	if cards == nil {
		return nil, fmt.Errorf("feishu tool approval card service is required")
	}
	if runCtx == nil {
		runCtx = context.Background()
	}
	manager := &operationApprovalService{
		store:             st,
		cards:             cards,
		account:           account,
		runCtx:            runCtx,
		ttl:               defaultToolApprovalTTL,
		executionTimeout:  defaultApprovedToolExecutionTimeout,
		cardUpdateTimeout: defaultFeishuCardUpdateTimeout,
		now:               time.Now,
		executors:         map[string]feishutools.OperationApprovalExecutor{},
	}
	if err := cards.RegisterAction(approvalCardActionKind, manager.HandleCardAction); err != nil {
		return nil, fmt.Errorf("register feishu tool approval card action: %w", err)
	}
	return manager, nil
}

func (m *operationApprovalService) registerExecutor(executor feishutools.OperationApprovalExecutor) error {
	if executor == nil {
		return fmt.Errorf("feishu approval executor is required")
	}
	policy, err := normalizeOperationApprovalPolicy(executor.OperationApprovalPolicy())
	if err != nil {
		return err
	}
	key := operationApprovalExecutorKey(policy.ToolName, policy.ActionKey)
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.executors[key]; exists {
		return fmt.Errorf("duplicate feishu approval executor %q/%q", policy.ToolName, policy.ActionKey)
	}
	m.executors[key] = executor
	return nil
}

func (m *operationApprovalService) CheckOrRequest(ctx context.Context, request feishutools.OperationApprovalRequest) (feishutools.OperationApprovalResult, error) {
	request, err := normalizeOperationApprovalRequest(request)
	if err != nil {
		return feishutools.OperationApprovalResult{}, err
	}
	executor := m.executor(request.ToolName, request.ActionKey)
	if executor == nil {
		return feishutools.OperationApprovalResult{}, fmt.Errorf("no approval executor registered for %q/%q", request.ToolName, request.ActionKey)
	}
	policy, err := normalizeOperationApprovalPolicy(executor.OperationApprovalPolicy())
	if err != nil {
		return feishutools.OperationApprovalResult{}, err
	}
	if policy.ToolName != request.ToolName || policy.ActionKey != request.ActionKey {
		return feishutools.OperationApprovalResult{}, fmt.Errorf("feishu operation approval policy does not match request")
	}
	if _, err := trustedWorkflowExecutionContext(ctx, m.account.ID, request.ToolName); err != nil {
		return feishutools.OperationApprovalResult{}, err
	}
	if policy.SupportsAll {
		active, err := m.hasActiveGrant(ctx, request)
		if err != nil {
			return feishutools.OperationApprovalResult{}, err
		}
		if active {
			feishuLog.Debug(ctx, "operation approval granted by reusable scope account=%s tool=%s action=%s resource_type=%s resource_ref=%s",
				m.account.ID, request.ToolName, request.ActionKey, request.ResourceType, shortResourceRef(request.ResourceToken))
			return feishutools.OperationApprovalResult{Status: feishutools.OperationApprovalStatusGranted}, nil
		}
	}
	return m.requestApproval(ctx, request, policy)
}

func (m *operationApprovalService) hasActiveGrant(ctx context.Context, request feishutools.OperationApprovalRequest) (bool, error) {
	actor, ok := feishutools.ActorFromContext(ctx)
	if !ok || (actor.OpenID == "" && actor.UserID == "") {
		return false, fmt.Errorf("feishu tool approval requires the requesting user identity")
	}
	chat, ok := feishutools.ChatContextFromContext(ctx)
	if !ok || chat.ChatID == "" {
		return false, fmt.Errorf("feishu tool approval requires the trusted current chat")
	}
	scope, err := toolApprovalGrantScope(
		m.account.ID,
		request.ToolName,
		request.ActionKey,
		request.ResourceType,
		request.ResourceToken,
		actor.OpenID,
		actor.UserID,
		chat.ChatID,
	)
	if err != nil {
		return false, err
	}
	grant, active, err := m.store.FindToolApprovalGrant(scope)
	if err != nil {
		return false, fmt.Errorf("check active feishu tool approval grant: %w", err)
	}
	if active {
		feishuLog.Debug(ctx, "using permanent feishu operation grant account=%s tool=%s action=%s user=%s chat=%s resource_type=%s resource_ref=%s source_request=%s",
			scope.AccountID,
			scope.ToolName,
			scope.ActionKey,
			scope.ActorID,
			scope.ChatID,
			scope.ResourceType,
			shortResourceRef(scope.ResourceToken),
			shortRequestID(grant.SourceRequestID),
		)
	}
	return active, nil
}

func (m *operationApprovalService) requestApproval(ctx context.Context, request feishutools.OperationApprovalRequest, policy feishutools.OperationApprovalPolicy) (feishutools.OperationApprovalResult, error) {
	actor, ok := feishutools.ActorFromContext(ctx)
	if !ok || (actor.OpenID == "" && actor.UserID == "") {
		return feishutools.OperationApprovalResult{}, fmt.Errorf("feishu tool approval requires the requesting user identity")
	}
	chat, ok := feishutools.ChatContextFromContext(ctx)
	if !ok || chat.ChatID == "" {
		return feishutools.OperationApprovalResult{}, fmt.Errorf("feishu tool approval requires the trusted current chat")
	}
	execution, err := trustedWorkflowExecutionContext(ctx, m.account.ID, request.ToolName)
	if err != nil {
		return feishutools.OperationApprovalResult{}, err
	}

	now := m.currentTime()
	if count, err := m.store.ExpireToolApprovals(m.account.ID, now); err != nil {
		return feishutools.OperationApprovalResult{}, fmt.Errorf("expire stale feishu tool approvals: %w", err)
	} else if count > 0 {
		feishuLog.Debug(ctx, "expired stale feishu tool approvals account=%s count=%d", m.account.ID, count)
	}
	approval, err := m.store.CreateToolApproval(store.ToolApproval{
		AccountID:       m.account.ID,
		ToolName:        request.ToolName,
		ActionKey:       request.ActionKey,
		ResourceType:    request.ResourceType,
		ResourceToken:   request.ResourceToken,
		SupportsAll:     policy.SupportsAll,
		ActorOpenID:     actor.OpenID,
		ActorUserID:     actor.UserID,
		ChatID:          chat.ChatID,
		SourceMessageID: chat.MessageID,
		Payload:         string(request.Payload),
		CreatedAt:       now,
		ExpiresAt:       now.Add(m.approvalTTL()),
	})
	if err != nil {
		return feishutools.OperationApprovalResult{}, fmt.Errorf("persist feishu tool approval: %w", err)
	}
	if _, err := persistWorkflowContinuation(m.store, execution, approval.ID, m.currentTime()); err != nil {
		m.failApprovalBestEffort(ctx, approval.ID)
		return feishutools.OperationApprovalResult{}, err
	}
	cardMessageID, err := m.cards.Send(ctx, approval.ChatID, pendingApprovalCard{policy: policy, request: request, approval: approval})
	if err != nil {
		m.failApprovalBestEffort(ctx, approval.ID)
		cancelWorkflowContinuationBestEffort(ctx, m.store, approval.ID, approval.AccountID, "approval card send failed", m.currentTime())
		return feishutools.OperationApprovalResult{}, fmt.Errorf("send feishu tool approval card: %w", err)
	}
	if err := m.store.SetToolApprovalCardMessageID(approval.ID, approval.AccountID, cardMessageID, m.currentTime()); err != nil {
		m.failApprovalBestEffort(ctx, approval.ID)
		cancelWorkflowContinuationBestEffort(ctx, m.store, approval.ID, approval.AccountID, "approval card binding failed", m.currentTime())
		m.updateCardBestEffort(ctx, cardMessageID, statusCard{title: "授权请求失败", template: "red", message: "授权请求未能保存，请重新发起操作。"})
		return feishutools.OperationApprovalResult{}, fmt.Errorf("bind feishu tool approval card: %w", err)
	}
	approval.CardMessageID = cardMessageID
	feishuLog.Info(ctx, "requested feishu operation approval request=%s account=%s tool=%s action=%s user=%s chat=%s resource_type=%s resource_ref=%s supports_all=%t expires_at=%s",
		shortRequestID(approval.ID),
		approval.AccountID,
		approval.ToolName,
		approval.ActionKey,
		approvalActorID(approval),
		approval.ChatID,
		approval.ResourceType,
		shortResourceRef(approval.ResourceToken),
		approval.SupportsAll,
		approval.ExpiresAt.Format(time.RFC3339),
	)
	return feishutools.OperationApprovalResult{
		Status:    feishutools.OperationApprovalStatusPending,
		RequestID: approval.ID,
		ExpiresAt: approval.ExpiresAt,
	}, nil
}

func (m *operationApprovalService) HandleCardAction(ctx context.Context, event *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
	requestID, action, ok := parseApprovalCardAction(event)
	if !ok {
		return nil, nil
	}
	if event == nil || event.Event == nil || event.Event.Operator == nil || event.Event.Context == nil {
		feishuLog.Warn(ctx, "ignored malformed feishu tool approval callback request=%s", shortRequestID(requestID))
		return cardToast("error", "授权回调信息不完整，请重新发起操作。"), nil
	}
	operator := event.Event.Operator
	match := store.ToolApprovalMatch{
		ActorOpenID:   operator.OpenID,
		ActorUserID:   deref(operator.UserID),
		ChatID:        event.Event.Context.OpenChatID,
		CardMessageID: event.Event.Context.OpenMessageID,
	}
	if m.baseContext().Err() != nil {
		feishuLog.Warn(ctx, "rejected feishu tool approval callback after runtime cancellation request=%s account=%s action=%s",
			shortRequestID(requestID), m.account.ID, action)
		return cardToast("error", "LingoBridge 正在关闭，请服务恢复后重新点击本卡片。"), nil
	}
	releaseTask, accepted := m.tasks.Reserve()
	if !accepted {
		feishuLog.Warn(ctx, "rejected feishu tool approval callback while runtime is shutting down request=%s account=%s action=%s",
			shortRequestID(requestID), m.account.ID, action)
		return cardToast("error", "LingoBridge 正在关闭，请服务恢复后重新点击本卡片。"), nil
	}
	taskHandedOff := false
	defer func() {
		if !taskHandedOff {
			releaseTask()
		}
	}()
	if action == approvalCardActionApproveAll {
		pending, err := m.store.GetToolApproval(requestID, m.account.ID)
		if err != nil {
			return m.handleApprovalDecisionError(ctx, pending, requestID, match, err), nil
		}
		if !pending.SupportsAll {
			feishuLog.Warn(ctx, "rejected unsupported approve-all operation request=%s account=%s tool=%s action=%s",
				shortRequestID(requestID), m.account.ID, pending.ToolName, pending.ActionKey)
			return cardToast("error", "该操作不支持永久授权，只能同意本次操作。"), nil
		}
	}
	reason := approvalCardReason(event)
	feishuLog.Debug(ctx, "received feishu tool approval callback request=%s account=%s action=%s reason_provided=%t reason_chars=%d",
		shortRequestID(requestID), m.account.ID, action, reason != "", utf8.RuneCountInString(reason))
	decidedAt := m.currentTime()
	approval, err := m.store.DecideToolApproval(requestID, m.account.ID, approvalDecision(action), match, decidedAt)
	if err != nil {
		return m.handleApprovalDecisionError(ctx, approval, requestID, match, err), nil
	}

	switch action {
	case approvalCardActionReject:
		feishuLog.Info(ctx, "denied feishu tool approval request=%s account=%s tool=%s user=%s chat=%s",
			shortRequestID(approval.ID), approval.AccountID, approval.ToolName, approvalActorID(approval), approval.ChatID)
		m.persistApprovalWorkflowResult(ctx, approval, store.WorkflowResultStateDenied, "denied", "用户拒绝了本次操作。", false, "", decidedAt)
		return approvalCallbackResponse(
			"success",
			"已拒绝，本次操作不会执行。",
			statusCard{title: "已拒绝授权", template: "grey", message: "请求已取消，未执行任何操作。"},
		), nil
	case approvalCardActionApproveOnce, approvalCardActionApproveAll:
		executor := m.executor(approval.ToolName, approval.ActionKey)
		if executor == nil {
			feishuLog.Error(ctx, "approved feishu tool has no executor request=%s account=%s tool=%s", shortRequestID(approval.ID), approval.AccountID, approval.ToolName)
			m.failApprovalBestEffort(ctx, approval.ID)
			m.persistApprovalWorkflowResult(ctx, approval, store.WorkflowResultStateFailed, "failed", "授权已确认，但当前服务无法执行该操作。", false, "", m.currentTime())
			return approvalCallbackResponse(
				"error",
				"授权已确认，但当前服务无法执行该操作。",
				statusCard{title: "执行失败", template: "red", message: "授权已确认，但当前服务无法执行该操作。"},
			), nil
		}
		toast := "已同意本次操作，正在执行；完成后机器人会发送结果。"
		if action == approvalCardActionApproveAll {
			if saved := m.saveReusableApprovalGrant(ctx, approval, decidedAt); saved {
				toast = "已永久授权该用户、机器人、对话、工具、动作和资源的相同操作；当前操作正在执行。"
			} else {
				toast = "已同意本次操作，正在执行；但永久免审批授权保存失败，后续调用仍需审批。"
			}
		}
		feishuLog.Info(ctx, "approved feishu tool approval request=%s account=%s tool=%s user=%s chat=%s mode=%s",
			shortRequestID(approval.ID), approval.AccountID, approval.ToolName, approvalActorID(approval), approval.ChatID, action)
		// The official delayed-update flow requires the callback to return before
		// using its token, so approval returns only a Toast and updates the final
		// card state after the asynchronous operation completes.
		taskHandedOff = true
		go func() {
			defer releaseTask()
			m.executeApproved(approval, executor, event.Event.Token)
		}()
		return cardToast("success", toast), nil
	default:
		return cardToast("error", "不支持的授权操作。"), nil
	}
}

func (m *operationApprovalService) saveReusableApprovalGrant(ctx context.Context, approval store.ToolApproval, now time.Time) bool {
	scope, err := toolApprovalGrantScope(
		approval.AccountID,
		approval.ToolName,
		approval.ActionKey,
		approval.ResourceType,
		approval.ResourceToken,
		approval.ActorOpenID,
		approval.ActorUserID,
		approval.ChatID,
	)
	if err != nil {
		feishuLog.Warn(ctx, "cannot scope reusable feishu operation approval request=%s account=%s tool=%s action=%s: %v",
			shortRequestID(approval.ID), approval.AccountID, approval.ToolName, approval.ActionKey, err)
		return false
	}
	grant, err := m.store.UpsertToolApprovalGrant(store.ToolApprovalGrant{
		ToolApprovalGrantScope: scope,
		SourceRequestID:        approval.ID,
		CreatedAt:              now,
	})
	if err != nil {
		feishuLog.Warn(ctx, "save reusable feishu operation approval failed request=%s account=%s tool=%s action=%s user=%s chat=%s resource_type=%s resource_ref=%s: %v",
			shortRequestID(approval.ID), approval.AccountID, approval.ToolName, approval.ActionKey, scope.ActorID, scope.ChatID,
			scope.ResourceType, shortResourceRef(scope.ResourceToken), err)
		return false
	}
	feishuLog.Info(ctx, "saved permanent feishu operation grant account=%s tool=%s action=%s user=%s chat=%s resource_type=%s resource_ref=%s source_request=%s",
		grant.AccountID, grant.ToolName, grant.ActionKey, grant.ActorID, grant.ChatID, grant.ResourceType,
		shortResourceRef(grant.ResourceToken), shortRequestID(grant.SourceRequestID))
	return true
}

func (m *operationApprovalService) handleApprovalDecisionError(ctx context.Context, approval store.ToolApproval, requestID string, match store.ToolApprovalMatch, err error) *callback.CardActionTriggerResponse {
	switch {
	case errors.Is(err, store.ErrToolApprovalForbidden):
		feishuLog.Warn(ctx, "rejected feishu tool approval actor mismatch request=%s account=%s callback_user=%s",
			shortRequestID(requestID), m.account.ID, approvalMatchActorID(match))
		return cardToast("error", "只有发起请求的用户可以授权。")
	case errors.Is(err, store.ErrToolApprovalContextMismatch):
		feishuLog.Warn(ctx, "rejected feishu tool approval context mismatch request=%s account=%s", shortRequestID(requestID), m.account.ID)
		return cardToast("error", "授权卡片与原请求不匹配。")
	case errors.Is(err, store.ErrToolApprovalExpired):
		feishuLog.Info(ctx, "expired feishu tool approval callback request=%s account=%s tool=%s", shortRequestID(requestID), m.account.ID, approval.ToolName)
		m.persistApprovalWorkflowResult(ctx, approval, store.WorkflowResultStateExpired, "expired", "授权请求已过期。", false, "", m.currentTime())
		return approvalCallbackResponse(
			"error",
			"授权已过期，请重新发起操作。",
			statusCard{title: "授权已过期", template: "grey", message: "该请求已超过有效期，请重新发起操作。"},
		)
	case errors.Is(err, store.ErrToolApprovalResolved):
		feishuLog.Debug(ctx, "ignored resolved feishu tool approval callback request=%s account=%s state=%s", shortRequestID(requestID), m.account.ID, approval.State)
		return cardToast("info", "该授权请求已经处理。")
	case errors.Is(err, store.ErrToolApprovalNotFound):
		feishuLog.Warn(ctx, "unknown feishu tool approval callback request=%s account=%s", shortRequestID(requestID), m.account.ID)
		return cardToast("error", "授权请求不存在或已失效。")
	default:
		feishuLog.Error(ctx, "handle feishu tool approval callback failed request=%s account=%s: %v", shortRequestID(requestID), m.account.ID, err)
		return cardToast("error", "处理授权失败，请稍后重试。")
	}
}

func (m *operationApprovalService) executeApproved(approval store.ToolApproval, executor feishutools.OperationApprovalExecutor, callbackToken string) {
	// An admitted approval task is tracked by the runtime task group and the
	// account lease is held until it drains. Do not turn lifecycle cancellation
	// into a false operation failure during orderly shutdown. Lease ownership
	// loss is different and must cancel the side effect immediately.
	drainCtx, cancelDrain := feishuRuntimeDrainContext(m.baseContext())
	ctx, cancel := context.WithTimeout(drainCtx, m.approvedExecutionTimeout())
	result, err := executor.ExecuteApproved(ctx, approval.ID, json.RawMessage(approval.Payload))
	cancel()
	cancelDrain()
	completedAt := m.currentTime()
	if feishuRuntimeOwnershipLost(m.baseContext()) {
		feishuLog.Warn(m.baseContext(), "preserving executing feishu tool approval after account lease ownership loss request=%s account=%s tool=%s user=%s chat=%s error_type=%T",
			shortRequestID(approval.ID), approval.AccountID, approval.ToolName, approvalActorID(approval), approval.ChatID, err)
		return
	}
	if err != nil {
		completeErr := m.store.CompleteToolApproval(approval.ID, approval.AccountID, store.ToolApprovalStateFailed, completedAt)
		if completeErr != nil {
			feishuLog.Error(m.baseContext(), "mark feishu tool approval failed request=%s account=%s: %v", shortRequestID(approval.ID), approval.AccountID, completeErr)
		}
		feishuLog.Error(m.baseContext(), "execute approved feishu tool failed request=%s account=%s tool=%s user=%s chat=%s: %v",
			shortRequestID(approval.ID), approval.AccountID, approval.ToolName, approvalActorID(approval), approval.ChatID, err)
		if completeErr != nil {
			return
		}
		m.updateApprovalResultCard(approval, callbackToken, statusCard{title: "执行失败", template: "red", message: "授权已确认，但操作执行失败。请稍后重新发起。"})
		m.persistApprovalWorkflowResult(m.baseContext(), approval, store.WorkflowResultStateFailed, "failed", "授权已确认，但操作执行失败。", false, "", completedAt)
		return
	}
	completedState := store.ToolApprovalStateSucceeded
	if result.Warning {
		completedState = store.ToolApprovalStatePartial
	}
	message := strings.TrimSpace(result.Message)
	if message == "" {
		message = "✅ 已完成授权操作。"
	}
	completeErr := m.store.CompleteToolApproval(approval.ID, approval.AccountID, completedState, completedAt)
	if completeErr != nil {
		feishuLog.Error(m.baseContext(), "mark feishu tool approval completed request=%s account=%s state=%s: %v", shortRequestID(approval.ID), approval.AccountID, completedState, completeErr)
		return
	}
	if feishuRuntimeOwnershipLost(m.baseContext()) {
		feishuLog.Warn(m.baseContext(), "deferred feishu tool approval terminal publication after account lease ownership loss request=%s account=%s tool=%s state=%s",
			shortRequestID(approval.ID), approval.AccountID, approval.ToolName, completedState)
		return
	}
	if result.Warning {
		feishuLog.Warn(m.baseContext(), "completed approved feishu tool with warning request=%s account=%s tool=%s user=%s chat=%s warning=%s",
			shortRequestID(approval.ID), approval.AccountID, approval.ToolName, approvalActorID(approval), approval.ChatID,
			truncateApprovalRunes(strings.TrimSpace(result.WarningReason), approvalCardMaxValueRunes))
		m.updateApprovalResultCard(approval, callbackToken, statusCard{title: "执行完成（有警告）", template: "orange", message: message})
	} else {
		feishuLog.Info(m.baseContext(), "completed approved feishu tool request=%s account=%s tool=%s user=%s chat=%s",
			shortRequestID(approval.ID), approval.AccountID, approval.ToolName, approvalActorID(approval), approval.ChatID)
		m.updateApprovalResultCard(approval, callbackToken, statusCard{title: "执行完成", template: "green", message: message})
	}
	status := "succeeded"
	if result.Warning {
		status = "succeeded_with_warning"
	}
	m.persistApprovalWorkflowResult(m.baseContext(), approval, store.WorkflowResultStateSucceeded, status, message, result.Warning, result.WarningReason, completedAt)
}

func (m *operationApprovalService) updateApprovalResultCard(approval store.ToolApproval, callbackToken string, card Card) {
	drainCtx, cancel := feishuRuntimeDrainContext(m.baseContext())
	defer cancel()
	m.updateApprovalResultCardWithContext(drainCtx, approval, callbackToken, card)
}

func (m *operationApprovalService) updateApprovalResultCardWithContext(cardBaseCtx context.Context, approval store.ToolApproval, callbackToken string, card Card) {
	if card == nil {
		return
	}
	// The admitted approval task owns a task-group lease, so final card delivery
	// may use its own bounded context even after the platform run context is
	// canceled during shutdown. Recovery passes its shared bounded context here
	// so a large terminal batch cannot consume one timeout per card.
	if cardBaseCtx == nil {
		cardBaseCtx = context.Background()
	}
	callbackToken = strings.TrimSpace(callbackToken)
	if callbackToken != "" {
		ctx, cancel := context.WithTimeout(cardBaseCtx, m.approvalCardUpdateTimeout())
		err := m.cards.UpdateByCallbackToken(ctx, callbackToken, card)
		cancel()
		if err == nil {
			m.markApprovalCardDeliveryDelivered(approval)
			return
		}
		feishuLog.Warn(m.baseContext(), "delay-update feishu tool approval card failed; falling back to message id request=%s account=%s: %v",
			shortRequestID(approval.ID), approval.AccountID, err)
	}
	messageID := strings.TrimSpace(approval.CardMessageID)
	if messageID == "" {
		feishuLog.Warn(m.baseContext(), "skip feishu tool approval card fallback without original message request=%s account=%s callback_token_present=%t",
			shortRequestID(approval.ID), approval.AccountID, callbackToken != "")
		return
	}
	err := updateFeishuCardByMessageIDWithTimeout(cardBaseCtx, m.cards, messageID, card, m.approvalCardUpdateTimeout())
	if err != nil {
		feishuLog.Warn(m.baseContext(), "delay-update feishu tool approval card failed request=%s account=%s: %v", shortRequestID(approval.ID), approval.AccountID, err)
		return
	}
	m.markApprovalCardDeliveryDelivered(approval)
	feishuLog.Debug(m.baseContext(), "updated feishu tool approval card by message id request=%s account=%s callback_token_present=%t",
		shortRequestID(approval.ID), approval.AccountID, callbackToken != "")
}

func (m *operationApprovalService) markApprovalCardDeliveryDelivered(approval store.ToolApproval) {
	err := m.store.MarkFeishuCardDeliveryDelivered(
		approval.AccountID,
		approval.ID,
		store.FeishuCardDeliveryPurposeToolApprovalTerminal,
		store.FeishuCardDeliveryRevisionTerminal,
		m.currentTime(),
	)
	if err == nil || errors.Is(err, store.ErrFeishuCardDeliveryNotFound) || errors.Is(err, store.ErrFeishuCardDeliveryNotReady) || errors.Is(err, store.ErrFeishuCardDeliveryResolved) {
		return
	}
	feishuLog.Warn(m.baseContext(), "mark feishu tool approval card delivery failed request=%s account=%s: %v",
		shortRequestID(approval.ID), approval.AccountID, err)
}

func (m *operationApprovalService) persistApprovalWorkflowResult(ctx context.Context, approval store.ToolApproval, state, status, message string, warning bool, warningReason string, now time.Time) {
	persistWorkflowResultBestEffort(ctx, m.store, approval.ID, approval.AccountID, state, approvalWorkflowResultPayload(approval, status, message, warning, warningReason), now)
}

func approvalWorkflowResultPayload(approval store.ToolApproval, status, message string, warning bool, warningReason string) map[string]any {
	return map[string]any{
		"status":         status,
		"tool_name":      approval.ToolName,
		"action_key":     approval.ActionKey,
		"resource_type":  approval.ResourceType,
		"resource_token": approval.ResourceToken,
		"message":        truncateApprovalRunes(strings.TrimSpace(message), approvalCardMaxValueRunes),
		"warning":        warning,
		"warning_reason": truncateApprovalRunes(strings.TrimSpace(warningReason), approvalCardMaxValueRunes),
	}
}

func (m *operationApprovalService) reconcileTerminalApprovalResults(ctx context.Context, updatedBefore time.Time) (int, error) {
	const batchSize = 100
	total := 0
	recoveryCardCtx, cancelCardUpdates := context.WithTimeout(m.baseContext(), m.approvalCardUpdateTimeout())
	defer cancelCardUpdates()
	for {
		gaps, err := m.store.ListTerminalWorkflowResultGaps(
			m.account.ID,
			store.WorkflowRequestKindToolApproval,
			updatedBefore,
			batchSize,
		)
		if err != nil {
			return total, err
		}
		for _, gap := range gaps {
			approval, err := m.store.GetToolApproval(gap.ID, gap.AccountID)
			if err != nil {
				return total, fmt.Errorf("load terminal tool approval %s: %w", shortRequestID(gap.ID), err)
			}
			resultState, status, message, warning, warningReason, err := recoveredApprovalResult(gap.State)
			if err != nil {
				return total, err
			}
			if recoveryCardCtx.Err() == nil {
				m.updateApprovalResultCardWithContext(recoveryCardCtx, approval, "", recoveredApprovalStatusCard(gap.State, message))
			}
			_, ready, err := persistWorkflowResult(
				m.store,
				approval.ID,
				approval.AccountID,
				resultState,
				approvalWorkflowResultPayload(approval, status, message, warning, warningReason),
				gap.UpdatedAt,
			)
			if err != nil {
				return total, fmt.Errorf("store recovered tool approval result %s: %w", shortRequestID(gap.ID), err)
			}
			total++
			feishuLog.Debug(ctx, "reconciled feishu tool approval result request=%s account=%s workflow_state=%s result_state=%s ready=%t",
				shortRequestID(gap.ID), gap.AccountID, gap.State, resultState, ready)
		}
		if len(gaps) < batchSize {
			return total, nil
		}
	}
}

func recoveredApprovalStatusCard(workflowState, message string) statusCard {
	switch workflowState {
	case store.WorkflowRequestStateDenied:
		return statusCard{title: "已拒绝授权", template: "grey", message: message}
	case store.WorkflowRequestStateExpired:
		return statusCard{title: "授权已过期", template: "grey", message: message}
	case store.WorkflowRequestStateFailed:
		return statusCard{title: "执行未可靠完成", template: "red", message: message}
	case store.WorkflowRequestStateSucceeded:
		return statusCard{title: "执行已完成（结果待确认）", template: "orange", message: message}
	case store.WorkflowRequestStatePartial:
		return statusCard{title: "执行完成（有警告）", template: "orange", message: message}
	default:
		return statusCard{title: "操作结果", template: "orange", message: message}
	}
}

func recoveredApprovalResult(workflowState string) (resultState, status, message string, warning bool, warningReason string, err error) {
	switch workflowState {
	case store.WorkflowRequestStateDenied:
		return store.WorkflowResultStateDenied, "denied", "用户拒绝了本次操作。", false, "", nil
	case store.WorkflowRequestStateExpired:
		return store.WorkflowResultStateExpired, "expired", "授权请求已过期。", false, "", nil
	case store.WorkflowRequestStateFailed:
		return store.WorkflowResultStateFailed, "failed", "操作未能可靠完成，服务不会自动重试可能产生副作用的操作。", false, "", nil
	case store.WorkflowRequestStateSucceeded:
		return store.WorkflowResultStateSucceeded, "succeeded", "操作已经完成，但服务中断前未保存详细结果；请根据当前资源状态继续，避免重复执行有副作用的操作。", true, "详细执行结果未能持久化。", nil
	case store.WorkflowRequestStatePartial:
		return store.WorkflowResultStateSucceeded, "succeeded_with_warning", "操作已完成但存在警告；服务中断前未保存详细结果，请根据当前资源状态继续。", true, "详细警告内容未能持久化。", nil
	default:
		return "", "", "", false, "", fmt.Errorf("unsupported terminal tool approval state %q", workflowState)
	}
}

func (m *operationApprovalService) updateCardBestEffort(ctx context.Context, messageID string, card Card) {
	if strings.TrimSpace(messageID) == "" || card == nil {
		return
	}
	if err := m.cards.UpdateByMessageID(ctx, messageID, card); err != nil {
		feishuLog.Warn(ctx, "update feishu tool approval card failed message=%s: %v", messageID, err)
	}
}

func (m *operationApprovalService) failApprovalBestEffort(ctx context.Context, requestID string) {
	if err := m.store.FailToolApproval(requestID, m.account.ID, m.currentTime()); err != nil && !errors.Is(err, store.ErrToolApprovalResolved) {
		feishuLog.Error(ctx, "close failed feishu tool approval request=%s account=%s: %v", shortRequestID(requestID), m.account.ID, err)
	}
}

func (m *operationApprovalService) executor(toolName, actionKey string) feishutools.OperationApprovalExecutor {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.executors[operationApprovalExecutorKey(toolName, actionKey)]
}

func (m *operationApprovalService) currentTime() time.Time {
	if m.now == nil {
		return time.Now().UTC()
	}
	return m.now().UTC()
}

func (m *operationApprovalService) approvalTTL() time.Duration {
	if m.ttl <= 0 {
		return defaultToolApprovalTTL
	}
	return m.ttl
}

func (m *operationApprovalService) approvedExecutionTimeout() time.Duration {
	if m.executionTimeout <= 0 {
		return defaultApprovedToolExecutionTimeout
	}
	return m.executionTimeout
}

func (m *operationApprovalService) approvalCardUpdateTimeout() time.Duration {
	if m.cardUpdateTimeout <= 0 {
		return defaultFeishuCardUpdateTimeout
	}
	return m.cardUpdateTimeout
}

func (m *operationApprovalService) baseContext() context.Context {
	if m.runCtx == nil {
		return context.Background()
	}
	return m.runCtx
}

func normalizeOperationApprovalPolicy(policy feishutools.OperationApprovalPolicy) (feishutools.OperationApprovalPolicy, error) {
	policy.ToolName = strings.TrimSpace(policy.ToolName)
	policy.ActionKey = strings.TrimSpace(policy.ActionKey)
	policy.Action = strings.TrimSpace(policy.Action)
	if policy.ToolName == "" || policy.ActionKey == "" || policy.Action == "" {
		return feishutools.OperationApprovalPolicy{}, fmt.Errorf("feishu operation approval policy tool_name, action_key, and action are required")
	}
	return policy, nil
}

func normalizeOperationApprovalRequest(request feishutools.OperationApprovalRequest) (feishutools.OperationApprovalRequest, error) {
	request.ToolName = strings.TrimSpace(request.ToolName)
	request.ActionKey = strings.TrimSpace(request.ActionKey)
	request.ResourceType = strings.ToLower(strings.TrimSpace(request.ResourceType))
	request.ResourceToken = strings.TrimSpace(request.ResourceToken)
	request.Payload = json.RawMessage(strings.TrimSpace(string(request.Payload)))
	if request.ToolName == "" || request.ActionKey == "" || request.ResourceType == "" || request.ResourceToken == "" || len(request.Payload) == 0 {
		return feishutools.OperationApprovalRequest{}, fmt.Errorf("feishu operation approval tool_name, action_key, resource, and payload are required")
	}
	if !json.Valid(request.Payload) {
		return feishutools.OperationApprovalRequest{}, fmt.Errorf("feishu operation approval payload must be valid JSON")
	}
	if len(request.Fields) > approvalCardMaxFields {
		request.Fields = request.Fields[:approvalCardMaxFields]
	}
	fields := make([]feishutools.ApprovalField, 0, len(request.Fields))
	for _, field := range request.Fields {
		label := truncateApprovalRunes(strings.TrimSpace(field.Label), approvalCardMaxLabelRunes)
		value := truncateApprovalRunes(strings.TrimSpace(field.Value), approvalCardMaxValueRunes)
		if label == "" || value == "" {
			continue
		}
		fields = append(fields, feishutools.ApprovalField{Label: label, Value: value})
	}
	request.Fields = fields
	return request, nil
}

func operationApprovalExecutorKey(toolName, actionKey string) string {
	return strings.TrimSpace(toolName) + "\x00" + strings.TrimSpace(actionKey)
}

func approvalDecision(action string) string {
	if action == approvalCardActionReject {
		return store.ToolApprovalDecisionDeny
	}
	return store.ToolApprovalDecisionApprove
}

func approvalActorID(approval store.ToolApproval) string {
	if approval.ActorOpenID != "" {
		return approval.ActorOpenID
	}
	return approval.ActorUserID
}

func approvalMatchActorID(match store.ToolApprovalMatch) string {
	if match.ActorOpenID != "" {
		return match.ActorOpenID
	}
	return match.ActorUserID
}

func toolApprovalGrantScope(accountID, toolName, actionKey, resourceType, resourceToken, actorOpenID, actorUserID, chatID string) (store.ToolApprovalGrantScope, error) {
	actorType := store.ToolApprovalActorTypeOpenID
	actorID := strings.TrimSpace(actorOpenID)
	if actorID == "" {
		actorType = store.ToolApprovalActorTypeUserID
		actorID = strings.TrimSpace(actorUserID)
	}
	scope := store.ToolApprovalGrantScope{
		AccountID:     strings.TrimSpace(accountID),
		ToolName:      strings.TrimSpace(toolName),
		ActionKey:     strings.TrimSpace(actionKey),
		ResourceType:  strings.ToLower(strings.TrimSpace(resourceType)),
		ResourceToken: strings.TrimSpace(resourceToken),
		ActorType:     actorType,
		ActorID:       actorID,
		ChatID:        strings.TrimSpace(chatID),
	}
	if scope.AccountID == "" || scope.ToolName == "" || scope.ActionKey == "" || scope.ResourceType == "" ||
		scope.ResourceToken == "" || scope.ActorID == "" || scope.ChatID == "" {
		return store.ToolApprovalGrantScope{}, fmt.Errorf("feishu operation grant account, tool, action, resource, user, and chat are required")
	}
	return scope, nil
}

func shortRequestID(id string) string {
	id = strings.TrimSpace(id)
	if len(id) <= 12 {
		return id
	}
	return id[:12]
}
