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
	defaultToolApprovalGrantTTL         = 24 * time.Hour
	defaultApprovedToolExecutionTimeout = 30 * time.Second
	approvalCardUpdateTimeout           = 10 * time.Second
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
	FailExecutingToolApprovals(accountID string, now time.Time) (int64, error)
	UpsertToolApprovalGrant(grant store.ToolApprovalGrant) (store.ToolApprovalGrant, error)
	ActiveToolApprovalGrant(scope store.ToolApprovalGrantScope, now time.Time) (store.ToolApprovalGrant, bool, error)
}

func (m *approvalManager) recoverPersistedApprovals(ctx context.Context) error {
	now := m.currentTime()
	expired, err := m.store.ExpireToolApprovals(m.account.ID, now)
	if err != nil {
		return fmt.Errorf("expire persisted feishu tool approvals: %w", err)
	}
	interrupted, err := m.store.FailExecutingToolApprovals(m.account.ID, now)
	if err != nil {
		return fmt.Errorf("close interrupted feishu tool approvals: %w", err)
	}
	if expired > 0 {
		feishuLog.Info(ctx, "expired persisted feishu tool approvals account=%s count=%d", m.account.ID, expired)
	}
	if interrupted > 0 {
		feishuLog.Warn(ctx, "closed interrupted feishu tool approvals account=%s count=%d", m.account.ID, interrupted)
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

type approvalManager struct {
	store            toolApprovalStore
	cards            CardService
	account          store.Account
	runCtx           context.Context
	ttl              time.Duration
	grantTTL         time.Duration
	executionTimeout time.Duration
	now              func() time.Time

	mu        sync.RWMutex
	executors map[string]feishutools.ApprovalExecutor
}

func newApprovalManager(runCtx context.Context, st *store.Store, account store.Account, cards CardService) (*approvalManager, error) {
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
	manager := &approvalManager{
		store:            st,
		cards:            cards,
		account:          account,
		runCtx:           runCtx,
		ttl:              defaultToolApprovalTTL,
		grantTTL:         defaultToolApprovalGrantTTL,
		executionTimeout: defaultApprovedToolExecutionTimeout,
		now:              time.Now,
		executors:        map[string]feishutools.ApprovalExecutor{},
	}
	if err := cards.RegisterAction(approvalCardActionKind, manager.HandleCardAction); err != nil {
		return nil, fmt.Errorf("register feishu tool approval card action: %w", err)
	}
	return manager, nil
}

func (m *approvalManager) registerExecutor(executor feishutools.ApprovalExecutor) error {
	if executor == nil {
		return fmt.Errorf("feishu approval executor is required")
	}
	name := strings.TrimSpace(executor.ApprovalToolName())
	if name == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.executors[name]; exists {
		return fmt.Errorf("duplicate feishu approval executor %q", name)
	}
	m.executors[name] = executor
	return nil
}

func (m *approvalManager) HasActiveGrant(ctx context.Context, toolName string) (bool, error) {
	actor, ok := feishutools.ActorFromContext(ctx)
	if !ok || (actor.OpenID == "" && actor.UserID == "") {
		return false, fmt.Errorf("feishu tool approval requires the requesting user identity")
	}
	chat, ok := feishutools.ChatContextFromContext(ctx)
	if !ok || chat.ChatID == "" {
		return false, fmt.Errorf("feishu tool approval requires the trusted current chat")
	}
	scope, err := toolApprovalGrantScope(m.account.ID, toolName, actor.OpenID, actor.UserID, chat.ChatID)
	if err != nil {
		return false, err
	}
	grant, active, err := m.store.ActiveToolApprovalGrant(scope, m.currentTime())
	if err != nil {
		return false, fmt.Errorf("check active feishu tool approval grant: %w", err)
	}
	if active {
		feishuLog.Debug(ctx, "using active feishu tool approval grant account=%s tool=%s user=%s chat=%s expires_at=%s source_request=%s",
			scope.AccountID,
			scope.ToolName,
			scope.ActorID,
			scope.ChatID,
			grant.ExpiresAt.Format(time.RFC3339),
			shortRequestID(grant.SourceRequestID),
		)
	}
	return active, nil
}

func (m *approvalManager) RequestApproval(ctx context.Context, request feishutools.ApprovalRequest) (feishutools.PendingApproval, error) {
	request, err := normalizeApprovalRequest(request)
	if err != nil {
		return feishutools.PendingApproval{}, err
	}
	if m.executor(request.ToolName) == nil {
		return feishutools.PendingApproval{}, fmt.Errorf("no approval executor registered for %q", request.ToolName)
	}
	actor, ok := feishutools.ActorFromContext(ctx)
	if !ok || (actor.OpenID == "" && actor.UserID == "") {
		return feishutools.PendingApproval{}, fmt.Errorf("feishu tool approval requires the requesting user identity")
	}
	chat, ok := feishutools.ChatContextFromContext(ctx)
	if !ok || chat.ChatID == "" {
		return feishutools.PendingApproval{}, fmt.Errorf("feishu tool approval requires the trusted current chat")
	}
	execution, err := trustedWorkflowExecutionContext(ctx, m.account.ID, request.ToolName)
	if err != nil {
		return feishutools.PendingApproval{}, err
	}

	now := m.currentTime()
	if count, err := m.store.ExpireToolApprovals(m.account.ID, now); err != nil {
		return feishutools.PendingApproval{}, fmt.Errorf("expire stale feishu tool approvals: %w", err)
	} else if count > 0 {
		feishuLog.Debug(ctx, "expired stale feishu tool approvals account=%s count=%d", m.account.ID, count)
	}
	approval, err := m.store.CreateToolApproval(store.ToolApproval{
		AccountID:       m.account.ID,
		ToolName:        request.ToolName,
		ActorOpenID:     actor.OpenID,
		ActorUserID:     actor.UserID,
		ChatID:          chat.ChatID,
		SourceMessageID: chat.MessageID,
		Payload:         string(request.Payload),
		CreatedAt:       now,
		ExpiresAt:       now.Add(m.approvalTTL()),
	})
	if err != nil {
		return feishutools.PendingApproval{}, fmt.Errorf("persist feishu tool approval: %w", err)
	}
	if _, err := persistWorkflowContinuation(m.store, execution, approval.ID, m.currentTime()); err != nil {
		m.failApprovalBestEffort(ctx, approval.ID)
		return feishutools.PendingApproval{}, err
	}
	cardMessageID, err := m.cards.Send(ctx, approval.ChatID, pendingApprovalCard{request: request, approval: approval})
	if err != nil {
		m.failApprovalBestEffort(ctx, approval.ID)
		cancelWorkflowContinuationBestEffort(ctx, m.store, approval.ID, approval.AccountID, "approval card send failed", m.currentTime())
		return feishutools.PendingApproval{}, fmt.Errorf("send feishu tool approval card: %w", err)
	}
	if err := m.store.SetToolApprovalCardMessageID(approval.ID, approval.AccountID, cardMessageID, m.currentTime()); err != nil {
		m.failApprovalBestEffort(ctx, approval.ID)
		cancelWorkflowContinuationBestEffort(ctx, m.store, approval.ID, approval.AccountID, "approval card binding failed", m.currentTime())
		m.updateCardBestEffort(ctx, cardMessageID, statusCard{title: "授权请求失败", template: "red", message: "授权请求未能保存，请重新发起操作。"})
		return feishutools.PendingApproval{}, fmt.Errorf("bind feishu tool approval card: %w", err)
	}
	approval.CardMessageID = cardMessageID
	feishuLog.Info(ctx, "requested feishu tool approval request=%s account=%s tool=%s user=%s chat=%s expires_at=%s",
		shortRequestID(approval.ID),
		approval.AccountID,
		approval.ToolName,
		approvalActorID(approval),
		approval.ChatID,
		approval.ExpiresAt.Format(time.RFC3339),
	)
	return feishutools.PendingApproval{RequestID: approval.ID, ExpiresAt: approval.ExpiresAt}, nil
}

func (m *approvalManager) HandleCardAction(ctx context.Context, event *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
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
		executor := m.executor(approval.ToolName)
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
			if expiresAt, saved := m.saveReusableApprovalGrant(ctx, approval, decidedAt); saved {
				toast = fmt.Sprintf("已授权，当前操作正在执行；相同用户、机器人、对话和工具在 %s 前免审批。", expiresAt.Format("2006-01-02 15:04 UTC"))
			} else {
				toast = "已同意本次操作，正在执行；但 24 小时免审批授权保存失败，后续调用仍需审批。"
			}
		}
		feishuLog.Info(ctx, "approved feishu tool approval request=%s account=%s tool=%s user=%s chat=%s mode=%s",
			shortRequestID(approval.ID), approval.AccountID, approval.ToolName, approvalActorID(approval), approval.ChatID, action)
		// The official delayed-update flow requires the callback to return before
		// using its token, so approval returns only a Toast and updates the final
		// card state after the asynchronous operation completes.
		go m.executeApproved(approval, executor, event.Event.Token)
		return cardToast("success", toast), nil
	default:
		return cardToast("error", "不支持的授权操作。"), nil
	}
}

func (m *approvalManager) saveReusableApprovalGrant(ctx context.Context, approval store.ToolApproval, now time.Time) (time.Time, bool) {
	scope, err := toolApprovalGrantScope(approval.AccountID, approval.ToolName, approval.ActorOpenID, approval.ActorUserID, approval.ChatID)
	if err != nil {
		feishuLog.Warn(ctx, "cannot scope reusable feishu tool approval request=%s account=%s tool=%s: %v",
			shortRequestID(approval.ID), approval.AccountID, approval.ToolName, err)
		return time.Time{}, false
	}
	expiresAt := now.Add(m.approvalGrantTTL())
	grant, err := m.store.UpsertToolApprovalGrant(store.ToolApprovalGrant{
		ToolApprovalGrantScope: scope,
		SourceRequestID:        approval.ID,
		CreatedAt:              now,
		ExpiresAt:              expiresAt,
	})
	if err != nil {
		feishuLog.Warn(ctx, "save reusable feishu tool approval failed request=%s account=%s tool=%s user=%s chat=%s: %v",
			shortRequestID(approval.ID), approval.AccountID, approval.ToolName, scope.ActorID, scope.ChatID, err)
		return time.Time{}, false
	}
	feishuLog.Info(ctx, "saved reusable feishu tool approval account=%s tool=%s user=%s chat=%s expires_at=%s source_request=%s",
		grant.AccountID, grant.ToolName, grant.ActorID, grant.ChatID, grant.ExpiresAt.Format(time.RFC3339), shortRequestID(grant.SourceRequestID))
	return grant.ExpiresAt, true
}

func (m *approvalManager) handleApprovalDecisionError(ctx context.Context, approval store.ToolApproval, requestID string, match store.ToolApprovalMatch, err error) *callback.CardActionTriggerResponse {
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

func (m *approvalManager) executeApproved(approval store.ToolApproval, executor feishutools.ApprovalExecutor, callbackToken string) {
	ctx, cancel := context.WithTimeout(m.baseContext(), m.approvedExecutionTimeout())
	result, err := executor.ExecuteApproved(ctx, approval.ID, json.RawMessage(approval.Payload))
	cancel()
	completedAt := m.currentTime()
	if err != nil {
		completeErr := m.store.CompleteToolApproval(approval.ID, approval.AccountID, store.ToolApprovalStateFailed, completedAt)
		if completeErr != nil {
			feishuLog.Error(m.baseContext(), "mark feishu tool approval failed request=%s account=%s: %v", shortRequestID(approval.ID), approval.AccountID, completeErr)
		} else {
			m.persistApprovalWorkflowResult(m.baseContext(), approval, store.WorkflowResultStateFailed, "failed", "授权已确认，但操作执行失败。", false, "", completedAt)
		}
		feishuLog.Error(m.baseContext(), "execute approved feishu tool failed request=%s account=%s tool=%s user=%s chat=%s: %v",
			shortRequestID(approval.ID), approval.AccountID, approval.ToolName, approvalActorID(approval), approval.ChatID, err)
		m.updateApprovalResultCard(approval, callbackToken, statusCard{title: "执行失败", template: "red", message: "授权已确认，但操作执行失败。请稍后重新发起。"})
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
	if err := m.store.CompleteToolApproval(approval.ID, approval.AccountID, completedState, completedAt); err != nil {
		feishuLog.Error(m.baseContext(), "mark feishu tool approval completed request=%s account=%s state=%s: %v", shortRequestID(approval.ID), approval.AccountID, completedState, err)
	} else {
		status := "succeeded"
		if result.Warning {
			status = "succeeded_with_warning"
		}
		m.persistApprovalWorkflowResult(m.baseContext(), approval, store.WorkflowResultStateSucceeded, status, message, result.Warning, result.WarningReason, completedAt)
	}
	if result.Warning {
		feishuLog.Warn(m.baseContext(), "completed approved feishu tool with warning request=%s account=%s tool=%s user=%s chat=%s warning=%s",
			shortRequestID(approval.ID), approval.AccountID, approval.ToolName, approvalActorID(approval), approval.ChatID,
			truncateApprovalRunes(strings.TrimSpace(result.WarningReason), approvalCardMaxValueRunes))
		m.updateApprovalResultCard(approval, callbackToken, statusCard{title: "执行完成（有警告）", template: "orange", message: message})
		return
	}
	feishuLog.Info(m.baseContext(), "completed approved feishu tool request=%s account=%s tool=%s user=%s chat=%s",
		shortRequestID(approval.ID), approval.AccountID, approval.ToolName, approvalActorID(approval), approval.ChatID)
	m.updateApprovalResultCard(approval, callbackToken, statusCard{title: "执行完成", template: "green", message: message})
}

func (m *approvalManager) updateApprovalResultCard(approval store.ToolApproval, callbackToken string, card Card) {
	ctx, cancel := context.WithTimeout(m.baseContext(), approvalCardUpdateTimeout)
	defer cancel()
	m.updateCardAfterInteractionBestEffort(ctx, approval, callbackToken, card)
}

func (m *approvalManager) persistApprovalWorkflowResult(ctx context.Context, approval store.ToolApproval, state, status, message string, warning bool, warningReason string, now time.Time) {
	persistWorkflowResultBestEffort(ctx, m.store, approval.ID, approval.AccountID, state, approvalWorkflowResultPayload(approval, status, message, warning, warningReason), now)
}

func approvalWorkflowResultPayload(approval store.ToolApproval, status, message string, warning bool, warningReason string) map[string]any {
	return map[string]any{
		"status":         status,
		"tool_name":      approval.ToolName,
		"message":        truncateApprovalRunes(strings.TrimSpace(message), approvalCardMaxValueRunes),
		"warning":        warning,
		"warning_reason": truncateApprovalRunes(strings.TrimSpace(warningReason), approvalCardMaxValueRunes),
	}
}

func (m *approvalManager) reconcileTerminalApprovalResults(ctx context.Context, updatedBefore time.Time) (int, error) {
	const batchSize = 100
	total := 0
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

func (m *approvalManager) updateCardBestEffort(ctx context.Context, messageID string, card Card) {
	if strings.TrimSpace(messageID) == "" || card == nil {
		return
	}
	if err := m.cards.UpdateByMessageID(ctx, messageID, card); err != nil {
		feishuLog.Warn(ctx, "update feishu tool approval card failed message=%s: %v", messageID, err)
	}
}

func (m *approvalManager) updateCardAfterInteractionBestEffort(ctx context.Context, approval store.ToolApproval, callbackToken string, card Card) {
	if strings.TrimSpace(callbackToken) == "" || card == nil {
		feishuLog.Warn(ctx, "skip delayed feishu tool approval card update request=%s account=%s: callback token unavailable", shortRequestID(approval.ID), approval.AccountID)
		return
	}
	if err := m.cards.UpdateByCallbackToken(ctx, callbackToken, card); err != nil {
		feishuLog.Warn(ctx, "delay-update feishu tool approval card failed request=%s account=%s: %v", shortRequestID(approval.ID), approval.AccountID, err)
	}
}

func (m *approvalManager) failApprovalBestEffort(ctx context.Context, requestID string) {
	if err := m.store.FailToolApproval(requestID, m.account.ID, m.currentTime()); err != nil && !errors.Is(err, store.ErrToolApprovalResolved) {
		feishuLog.Error(ctx, "close failed feishu tool approval request=%s account=%s: %v", shortRequestID(requestID), m.account.ID, err)
	}
}

func (m *approvalManager) executor(name string) feishutools.ApprovalExecutor {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.executors[strings.TrimSpace(name)]
}

func (m *approvalManager) currentTime() time.Time {
	if m.now == nil {
		return time.Now().UTC()
	}
	return m.now().UTC()
}

func (m *approvalManager) approvalTTL() time.Duration {
	if m.ttl <= 0 {
		return defaultToolApprovalTTL
	}
	return m.ttl
}

func (m *approvalManager) approvalGrantTTL() time.Duration {
	if m.grantTTL <= 0 {
		return defaultToolApprovalGrantTTL
	}
	return m.grantTTL
}

func (m *approvalManager) approvedExecutionTimeout() time.Duration {
	if m.executionTimeout <= 0 {
		return defaultApprovedToolExecutionTimeout
	}
	return m.executionTimeout
}

func (m *approvalManager) baseContext() context.Context {
	if m.runCtx == nil {
		return context.Background()
	}
	return m.runCtx
}

func normalizeApprovalRequest(request feishutools.ApprovalRequest) (feishutools.ApprovalRequest, error) {
	request.ToolName = strings.TrimSpace(request.ToolName)
	request.Action = strings.TrimSpace(request.Action)
	request.Payload = json.RawMessage(strings.TrimSpace(string(request.Payload)))
	if request.ToolName == "" || request.Action == "" || len(request.Payload) == 0 {
		return feishutools.ApprovalRequest{}, fmt.Errorf("feishu approval tool_name, action, and payload are required")
	}
	if !json.Valid(request.Payload) {
		return feishutools.ApprovalRequest{}, fmt.Errorf("feishu approval payload must be valid JSON")
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

func toolApprovalGrantScope(accountID, toolName, actorOpenID, actorUserID, chatID string) (store.ToolApprovalGrantScope, error) {
	actorType := store.ToolApprovalActorTypeOpenID
	actorID := strings.TrimSpace(actorOpenID)
	if actorID == "" {
		actorType = store.ToolApprovalActorTypeUserID
		actorID = strings.TrimSpace(actorUserID)
	}
	scope := store.ToolApprovalGrantScope{
		AccountID: strings.TrimSpace(accountID),
		ToolName:  strings.TrimSpace(toolName),
		ActorType: actorType,
		ActorID:   actorID,
		ChatID:    strings.TrimSpace(chatID),
	}
	if scope.AccountID == "" || scope.ToolName == "" || scope.ActorID == "" || scope.ChatID == "" {
		return store.ToolApprovalGrantScope{}, fmt.Errorf("feishu tool approval grant account, tool, user, and chat are required")
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
