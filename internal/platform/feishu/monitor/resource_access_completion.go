package monitor

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"

	feishutools "lingobridge/internal/platform/feishu/tools"
	"lingobridge/internal/store"
)

func (m *resourceAccessManager) preserveResourceAccessAfterNonMutatingInterruption(
	ctx context.Context,
	request store.FeishuResourceAccessRequest,
	cause error,
	phase string,
) bool {
	contextEnded := false
	if ctx != nil && ctx.Err() != nil && errors.Is(cause, ctx.Err()) {
		contextEnded = true
	}
	if baseCtx := m.baseContext(); baseCtx.Err() != nil && errors.Is(cause, baseCtx.Err()) {
		contextEnded = true
	}
	if !contextEnded {
		return false
	}
	current, err := m.store.GetFeishuResourceAccessRequest(request.ID, request.AccountID)
	if err != nil {
		feishuLog.Warn(m.baseContext(), "could not reload feishu resource authorization after context interruption; leaving durable state untouched request=%s account=%s phase=%s: %v",
			shortRequestID(request.ID), request.AccountID, phase, err)
		return true
	}
	if current.State != store.FeishuResourceAccessStatePending {
		return false
	}
	feishuLog.Debug(m.baseContext(), "left pending feishu resource authorization for recovery after context ended before mutation claim request=%s account=%s phase=%s",
		shortRequestID(request.ID), request.AccountID, phase)
	return true
}

func (m *resourceAccessManager) resourceAccessResult(request store.FeishuResourceAccessRequest, status, source string) feishutools.ResourceAccessResult {
	return feishutools.ResourceAccessResult{
		RequestID:     request.ID,
		Status:        status,
		Permission:    request.Permission,
		Source:        source,
		ResourceType:  request.ResourceType,
		ResourceToken: request.ResourceToken,
		ResourceURL:   request.ResourceURL,
	}
}

func (m *resourceAccessManager) resourceAccessDecisionError(ctx context.Context, request store.FeishuResourceAccessRequest, requestID string, err error) *callback.CardActionTriggerResponse {
	switch {
	case errors.Is(err, store.ErrFeishuResourceAccessForbidden):
		return cardToast("error", "只有发起请求的用户可以处理该授权。")
	case errors.Is(err, store.ErrFeishuResourceAccessContextMismatch):
		return cardToast("error", "授权卡片与原请求不匹配。")
	case errors.Is(err, store.ErrFeishuResourceAccessOAuthStateMismatch):
		return cardToast("error", "授权链接的 state 与原请求不匹配，请重新复制或重新发起授权。")
	case errors.Is(err, store.ErrFeishuResourceAccessExpired):
		m.persistResourceWorkflowResult(ctx, request, store.WorkflowResultStateExpired, "expired", "", "资源授权请求已过期。", m.currentTime())
		return approvalCallbackResponse("error", "授权请求已过期。", statusCard{title: "授权已过期", template: "grey", message: "请重新调用资源授权工具。"})
	case errors.Is(err, store.ErrFeishuResourceAccessResolved):
		return cardToast("info", "该授权请求已经处理。")
	case errors.Is(err, store.ErrFeishuResourceAccessNotFound):
		return cardToast("error", "授权请求不存在或已失效。")
	default:
		feishuLog.Error(ctx, "handle feishu resource access card failed request=%s account=%s: %v", shortRequestID(requestID), m.account.ID, err)
		return cardToast("error", "处理授权失败，请稍后重试。")
	}
}

func (m *resourceAccessManager) finishResourceAccessFailure(ctx context.Context, request store.FeishuResourceAccessRequest, cause error, title, message string) {
	if m.preserveResourceAccessAfterOwnershipLoss(ctx, request, cause, "operation") {
		return
	}
	if m.preserveVerifiedResourceAccessCompletion(ctx, request, cause, "operation") {
		return
	}
	if errors.Is(cause, store.ErrFeishuResourceAccessResolved) {
		return
	}
	if errors.Is(cause, store.ErrFeishuResourceAccessExpired) {
		expiredAt := m.currentTime()
		request.UpdatedAt = expiredAt
		expiredMessage := "资源授权请求已过期，请重新调用资源授权工具。"
		feishuLog.Info(ctx, "expired feishu resource authorization during execution claim request=%s account=%s user=%s chat=%s type=%s resource_ref=%s",
			shortRequestID(request.ID), request.AccountID, resourceAccessActorID(request), request.ChatID,
			request.ResourceType, shortResourceRef(request.ResourceToken))
		m.updateResourceAccessResultCard(ctx, request, statusCard{title: "授权已过期", template: "grey", message: expiredMessage})
		m.persistResourceWorkflowResult(ctx, request, store.WorkflowResultStateExpired, "expired", "", expiredMessage, expiredAt)
		return
	}
	failedAt := m.currentTime()
	stored := false
	if err := m.store.FailFeishuResourceAccessRequest(request.ID, request.AccountID, failedAt); err != nil {
		if errors.Is(err, store.ErrFeishuResourceAccessResolved) {
			return
		}
		feishuLog.Error(ctx, "mark feishu resource authorization failed request=%s account=%s: %v", shortRequestID(request.ID), request.AccountID, err)
	} else {
		stored = true
	}
	feishuLog.Warn(ctx, "feishu resource authorization failed request=%s account=%s user=%s chat=%s type=%s resource_ref=%s grant_mode=%s: %v",
		shortRequestID(request.ID), request.AccountID, resourceAccessActorID(request), request.ChatID,
		request.ResourceType, shortResourceRef(request.ResourceToken), request.GrantMode, cause)
	m.updateResourceAccessResultCard(ctx, request, statusCard{title: title, template: "red", message: message})
	if stored {
		m.persistResourceWorkflowResult(ctx, request, store.WorkflowResultStateFailed, "failed", "", message, failedAt)
	}
}

func (m *resourceAccessManager) finishOAuthFailure(ctx context.Context, request store.FeishuResourceAccessRequest, cause error, title, message string) {
	if m.preserveResourceAccessAfterOwnershipLoss(ctx, request, cause, "oauth") {
		return
	}
	if m.preserveVerifiedResourceAccessCompletion(ctx, request, cause, "oauth") {
		return
	}
	failedAt := m.currentTime()
	stored := false
	if err := m.store.FailFeishuResourceAccessRequest(request.ID, request.AccountID, failedAt); err != nil && !errors.Is(err, store.ErrFeishuResourceAccessResolved) {
		feishuLog.Error(ctx, "mark feishu resource OAuth failed request=%s account=%s: %v", shortRequestID(request.ID), request.AccountID, err)
	} else if err == nil {
		stored = true
	}
	feishuLog.Warn(ctx, "feishu resource OAuth failed request=%s account=%s user=%s chat=%s type=%s resource_ref=%s: %v",
		shortRequestID(request.ID), request.AccountID, resourceAccessActorID(request), request.ChatID,
		request.ResourceType, shortResourceRef(request.ResourceToken), cause)
	m.updateResourceAccessResultCard(ctx, request, statusCard{title: title, template: "red", message: message})
	if stored {
		m.persistResourceWorkflowResult(ctx, request, store.WorkflowResultStateFailed, "failed", "", message, failedAt)
	}
}

func (m *resourceAccessManager) preserveVerifiedResourceAccessCompletion(
	ctx context.Context,
	request store.FeishuResourceAccessRequest,
	cause error,
	phase string,
) bool {
	if !errors.Is(cause, errFeishuResourceAccessCompletionDeferred) {
		return false
	}
	feishuLog.Error(ctx, "preserving verified feishu resource authorization for startup recovery after local completion failed request=%s account=%s user=%s chat=%s type=%s resource_ref=%s phase=%s: %v",
		shortRequestID(request.ID), request.AccountID, resourceAccessActorID(request), request.ChatID,
		request.ResourceType, shortResourceRef(request.ResourceToken), phase, cause)
	return true
}

func (m *resourceAccessManager) preserveResourceAccessAfterOwnershipLoss(
	ctx context.Context,
	request store.FeishuResourceAccessRequest,
	cause error,
	phase string,
) bool {
	if !feishuRuntimeOwnershipLost(m.baseContext()) && !errors.Is(cause, errFeishuResourceAccessOwnershipLost) {
		return false
	}
	feishuLog.Warn(ctx, "preserving feishu resource authorization after account lease ownership loss request=%s account=%s user=%s chat=%s type=%s resource_ref=%s phase=%s error_type=%T",
		shortRequestID(request.ID), request.AccountID, resourceAccessActorID(request), request.ChatID,
		request.ResourceType, shortResourceRef(request.ResourceToken), phase, cause)
	return true
}

func (m *resourceAccessManager) persistResourceWorkflowResult(ctx context.Context, request store.FeishuResourceAccessRequest, state, status, source, message string, now time.Time) {
	if strings.TrimSpace(request.ID) == "" {
		return
	}
	persistWorkflowResultBestEffort(ctx, m.store, request.ID, request.AccountID, state, resourceWorkflowResultPayload(request, status, source, message), now)
}

func resourceWorkflowResultPayload(request store.FeishuResourceAccessRequest, status, source, message string) map[string]any {
	payload := map[string]any{
		"status":         status,
		"permission":     request.Permission,
		"source":         source,
		"resource_type":  request.ResourceType,
		"resource_token": request.ResourceToken,
		"resource_url":   request.ResourceURL,
		"message":        strings.TrimSpace(message),
	}
	if request.GrantMode != "" {
		payload["grant_mode"] = request.GrantMode
		payload["once_duration_minutes"] = request.OnceDurationMinutes
		if request.GrantMode == store.FeishuResourceGrantModeOnce && !request.UpdatedAt.IsZero() && status == "granted" {
			payload["access_expires_at"] = request.UpdatedAt.Add(time.Duration(request.OnceDurationMinutes) * time.Minute).UTC().Format(time.RFC3339)
		}
	}
	return payload
}

func (m *resourceAccessManager) updateResourceAccessResultCard(_ context.Context, request store.FeishuResourceAccessRequest, card Card) {
	// The Feishu API operation may have consumed its entire deadline. Final
	// card delivery gets an independent runtime-owned timeout after terminal
	// state is durable.
	if err := m.updateResourceCardWithFreshTimeout(request.CardMessageID, card); err != nil {
		feishuLog.Warn(m.baseContext(), "update feishu resource access card failed message=%s: %v", request.CardMessageID, err)
		return
	}
	m.markResourceTerminalCardDeliveryDelivered(request)
}

func (m *resourceAccessManager) markResourceTerminalCardDeliveryDelivered(request store.FeishuResourceAccessRequest) {
	err := m.store.MarkFeishuCardDeliveryDelivered(
		request.AccountID,
		request.ID,
		store.FeishuCardDeliveryPurposeResourceTerminal,
		store.FeishuCardDeliveryRevisionTerminal,
		m.currentTime(),
	)
	if err == nil || errors.Is(err, store.ErrFeishuCardDeliveryNotFound) || errors.Is(err, store.ErrFeishuCardDeliveryNotReady) || errors.Is(err, store.ErrFeishuCardDeliveryResolved) {
		return
	}
	feishuLog.Warn(m.baseContext(), "mark feishu resource access card delivery failed request=%s account=%s: %v",
		shortRequestID(request.ID), request.AccountID, err)
}

func (m *resourceAccessManager) updateResourceCardWithFreshTimeout(messageID string, card Card) error {
	drainCtx, cancel := feishuRuntimeDrainContext(m.baseContext())
	defer cancel()
	return updateFeishuCardByMessageIDWithTimeout(drainCtx, m.cards, messageID, card, m.resourceCardUpdateTimeout())
}

func (m *resourceAccessManager) updateResourceCardBestEffort(ctx context.Context, messageID string, card Card) {
	if strings.TrimSpace(messageID) == "" || card == nil {
		return
	}
	if err := m.cards.UpdateByMessageID(ctx, messageID, card); err != nil {
		feishuLog.Warn(ctx, "update feishu resource access card failed message=%s: %v", messageID, err)
	}
}

func (m *resourceAccessManager) failResourceAccessBestEffort(ctx context.Context, requestID string) {
	if err := m.store.FailFeishuResourceAccessRequest(requestID, m.account.ID, m.currentTime()); err != nil && !errors.Is(err, store.ErrFeishuResourceAccessResolved) {
		feishuLog.Error(ctx, "close failed feishu resource access request=%s account=%s: %v", shortRequestID(requestID), m.account.ID, err)
	}
}
