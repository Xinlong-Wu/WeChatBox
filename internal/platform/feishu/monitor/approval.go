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
	approvalNotificationTimeout         = 10 * time.Second
	approvalCardActionKind              = "lingobridge_tool_approval"
	approvalCardMaxFields               = 8
	approvalCardMaxLabelRunes           = 32
	approvalCardMaxValueRunes           = 256
)

type toolApprovalStore interface {
	PlatformID() string
	CreateToolApproval(approval store.ToolApproval) (store.ToolApproval, error)
	SetToolApprovalCardMessageID(id, accountID, messageID string, now time.Time) error
	DecideToolApproval(id, accountID, decision string, match store.ToolApprovalMatch, now time.Time) (store.ToolApproval, error)
	CompleteToolApproval(id, accountID, state string, now time.Time) error
	FailToolApproval(id, accountID string, now time.Time) error
	ExpireToolApprovals(accountID string, now time.Time) (int64, error)
	FailExecutingToolApprovals(accountID string, now time.Time) (int64, error)
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
	return nil
}

type approvalCardSender interface {
	SendText(ctx context.Context, chatID, text string) error
	CreateCard(ctx context.Context, chatID, cardJSON string) (string, error)
	UpdateCard(ctx context.Context, messageID, cardJSON string) error
	UpdateCardAfterInteraction(ctx context.Context, callbackToken, cardJSON string) error
}

type approvalManager struct {
	store            toolApprovalStore
	sender           approvalCardSender
	account          store.Account
	runCtx           context.Context
	ttl              time.Duration
	executionTimeout time.Duration
	now              func() time.Time

	mu        sync.RWMutex
	executors map[string]feishutools.ApprovalExecutor
}

func newApprovalManager(runCtx context.Context, st *store.Store, account store.Account, sender approvalCardSender) (*approvalManager, error) {
	if st == nil {
		return nil, fmt.Errorf("feishu tool approval store is required")
	}
	if st.PlatformID() != store.PlatformFeishu {
		return nil, fmt.Errorf("feishu tool approval store platform is %q", st.PlatformID())
	}
	if strings.TrimSpace(account.ID) == "" {
		return nil, fmt.Errorf("feishu tool approval account id is required")
	}
	if sender == nil {
		return nil, fmt.Errorf("feishu tool approval card sender is required")
	}
	if runCtx == nil {
		runCtx = context.Background()
	}
	return &approvalManager{
		store:            st,
		sender:           sender,
		account:          account,
		runCtx:           runCtx,
		ttl:              defaultToolApprovalTTL,
		executionTimeout: defaultApprovedToolExecutionTimeout,
		now:              time.Now,
		executors:        map[string]feishutools.ApprovalExecutor{},
	}, nil
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
		feishuLog.Debug(ctx, "using active feishu tool approval grant account=%s tool=%s user=%s chat=%s expires_at=%s source_approval=%s",
			scope.AccountID,
			scope.ToolName,
			scope.ActorID,
			scope.ChatID,
			grant.ExpiresAt.Format(time.RFC3339),
			shortApprovalID(grant.SourceApprovalID),
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
	cardJSON, err := buildPendingApprovalCard(request, approval)
	if err != nil {
		m.failApprovalBestEffort(ctx, approval.ID)
		return feishutools.PendingApproval{}, fmt.Errorf("build feishu tool approval card: %w", err)
	}
	cardMessageID, err := m.sender.CreateCard(ctx, approval.ChatID, cardJSON)
	if err != nil {
		m.failApprovalBestEffort(ctx, approval.ID)
		return feishutools.PendingApproval{}, fmt.Errorf("send feishu tool approval card: %w", err)
	}
	if err := m.store.SetToolApprovalCardMessageID(approval.ID, approval.AccountID, cardMessageID, m.currentTime()); err != nil {
		m.failApprovalBestEffort(ctx, approval.ID)
		m.updateCardBestEffort(ctx, cardMessageID, buildStatusApprovalCard("授权请求失败", "red", "授权请求未能保存，请重新发起操作。"))
		return feishutools.PendingApproval{}, fmt.Errorf("bind feishu tool approval card: %w", err)
	}
	approval.CardMessageID = cardMessageID
	feishuLog.Info(ctx, "requested feishu tool approval id=%s account=%s tool=%s user=%s chat=%s expires_at=%s",
		shortApprovalID(approval.ID),
		approval.AccountID,
		approval.ToolName,
		approvalActorID(approval),
		approval.ChatID,
		approval.ExpiresAt.Format(time.RFC3339),
	)
	return feishutools.PendingApproval{ID: approval.ID, ExpiresAt: approval.ExpiresAt}, nil
}

func (m *approvalManager) HandleCardAction(ctx context.Context, event *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
	approvalID, decision, ok := parseApprovalCardAction(event)
	if !ok {
		return nil, nil
	}
	if event == nil || event.Event == nil || event.Event.Operator == nil || event.Event.Context == nil {
		feishuLog.Warn(ctx, "ignored malformed feishu tool approval callback id=%s", shortApprovalID(approvalID))
		return approvalToast("error", "授权回调信息不完整，请重新发起操作。"), nil
	}
	operator := event.Event.Operator
	match := store.ToolApprovalMatch{
		ActorOpenID:   operator.OpenID,
		ActorUserID:   deref(operator.UserID),
		ChatID:        event.Event.Context.OpenChatID,
		CardMessageID: event.Event.Context.OpenMessageID,
	}
	approval, err := m.store.DecideToolApproval(approvalID, m.account.ID, decision, match, m.currentTime())
	if err != nil {
		return m.handleApprovalDecisionError(ctx, approval, approvalID, match, err), nil
	}

	switch decision {
	case store.ToolApprovalDecisionDeny:
		feishuLog.Info(ctx, "denied feishu tool approval id=%s account=%s tool=%s user=%s chat=%s",
			shortApprovalID(approval.ID), approval.AccountID, approval.ToolName, approvalActorID(approval), approval.ChatID)
		return approvalCallbackResponse(
			"success",
			"已拒绝，本次操作不会执行。",
			buildStatusApprovalCard("已拒绝授权", "grey", "请求已取消，未执行任何操作。"),
		), nil
	case store.ToolApprovalDecisionApprove:
		executor := m.executor(approval.ToolName)
		if executor == nil {
			feishuLog.Error(ctx, "approved feishu tool has no executor id=%s account=%s tool=%s", shortApprovalID(approval.ID), approval.AccountID, approval.ToolName)
			m.failApprovalBestEffort(ctx, approval.ID)
			return approvalCallbackResponse(
				"error",
				"授权已确认，但当前服务无法执行该操作。",
				buildStatusApprovalCard("执行失败", "red", "授权已确认，但当前服务无法执行该操作。"),
			), nil
		}
		feishuLog.Info(ctx, "approved feishu tool approval id=%s account=%s tool=%s user=%s chat=%s",
			shortApprovalID(approval.ID), approval.AccountID, approval.ToolName, approvalActorID(approval), approval.ChatID)
		// The official delayed-update flow requires the callback to return before
		// using its token, so approval returns only a Toast and updates the final
		// card state after the asynchronous operation completes.
		go m.executeApproved(approval, executor, event.Event.Token)
		return approvalToast("success", "已授权，正在执行；完成后机器人会发送结果。"), nil
	default:
		return approvalToast("error", "不支持的授权操作。"), nil
	}
}

func (m *approvalManager) handleApprovalDecisionError(ctx context.Context, approval store.ToolApproval, approvalID string, match store.ToolApprovalMatch, err error) *callback.CardActionTriggerResponse {
	switch {
	case errors.Is(err, store.ErrToolApprovalForbidden):
		feishuLog.Warn(ctx, "rejected feishu tool approval actor mismatch id=%s account=%s callback_user=%s",
			shortApprovalID(approvalID), m.account.ID, approvalMatchActorID(match))
		return approvalToast("error", "只有发起请求的用户可以授权。")
	case errors.Is(err, store.ErrToolApprovalContextMismatch):
		feishuLog.Warn(ctx, "rejected feishu tool approval context mismatch id=%s account=%s", shortApprovalID(approvalID), m.account.ID)
		return approvalToast("error", "授权卡片与原请求不匹配。")
	case errors.Is(err, store.ErrToolApprovalExpired):
		feishuLog.Info(ctx, "expired feishu tool approval callback id=%s account=%s tool=%s", shortApprovalID(approvalID), m.account.ID, approval.ToolName)
		return approvalCallbackResponse(
			"error",
			"授权已过期，请重新发起操作。",
			buildStatusApprovalCard("授权已过期", "grey", "该请求已超过有效期，请重新发起操作。"),
		)
	case errors.Is(err, store.ErrToolApprovalResolved):
		feishuLog.Debug(ctx, "ignored resolved feishu tool approval callback id=%s account=%s state=%s", shortApprovalID(approvalID), m.account.ID, approval.State)
		return approvalToast("info", "该授权请求已经处理。")
	case errors.Is(err, store.ErrToolApprovalNotFound):
		feishuLog.Warn(ctx, "unknown feishu tool approval callback id=%s account=%s", shortApprovalID(approvalID), m.account.ID)
		return approvalToast("error", "授权请求不存在或已失效。")
	default:
		feishuLog.Error(ctx, "handle feishu tool approval callback failed id=%s account=%s: %v", shortApprovalID(approvalID), m.account.ID, err)
		return approvalToast("error", "处理授权失败，请稍后重试。")
	}
}

func (m *approvalManager) executeApproved(approval store.ToolApproval, executor feishutools.ApprovalExecutor, callbackToken string) {
	ctx, cancel := context.WithTimeout(m.baseContext(), m.approvedExecutionTimeout())
	result, err := executor.ExecuteApproved(ctx, json.RawMessage(approval.Payload))
	cancel()
	completedAt := m.currentTime()
	if err != nil {
		if completeErr := m.store.CompleteToolApproval(approval.ID, approval.AccountID, store.ToolApprovalStateFailed, completedAt); completeErr != nil {
			feishuLog.Error(m.baseContext(), "mark feishu tool approval failed id=%s account=%s: %v", shortApprovalID(approval.ID), approval.AccountID, completeErr)
		}
		feishuLog.Error(m.baseContext(), "execute approved feishu tool failed id=%s account=%s tool=%s user=%s chat=%s: %v",
			shortApprovalID(approval.ID), approval.AccountID, approval.ToolName, approvalActorID(approval), approval.ChatID, err)
		m.notifyApprovalResult(approval, callbackToken, buildStatusApprovalCard("执行失败", "red", "授权已确认，但操作执行失败。请稍后重新发起。"), "❌ 已授权，但操作执行失败。请稍后重试。")
		return
	}
	if err := m.store.CompleteToolApproval(approval.ID, approval.AccountID, store.ToolApprovalStateSucceeded, completedAt); err != nil {
		feishuLog.Error(m.baseContext(), "mark feishu tool approval succeeded id=%s account=%s: %v", shortApprovalID(approval.ID), approval.AccountID, err)
	}
	message := strings.TrimSpace(result.Message)
	if message == "" {
		message = "✅ 已完成授权操作。"
	}
	if result.Warning {
		feishuLog.Warn(m.baseContext(), "completed approved feishu tool with warning id=%s account=%s tool=%s user=%s chat=%s warning=%s",
			shortApprovalID(approval.ID), approval.AccountID, approval.ToolName, approvalActorID(approval), approval.ChatID,
			truncateApprovalRunes(strings.TrimSpace(result.WarningReason), approvalCardMaxValueRunes))
		m.notifyApprovalResult(approval, callbackToken, buildStatusApprovalCard("执行完成（有警告）", "orange", message), message)
		return
	}
	feishuLog.Info(m.baseContext(), "completed approved feishu tool id=%s account=%s tool=%s user=%s chat=%s",
		shortApprovalID(approval.ID), approval.AccountID, approval.ToolName, approvalActorID(approval), approval.ChatID)
	m.notifyApprovalResult(approval, callbackToken, buildStatusApprovalCard("执行完成", "green", message), message)
}

func (m *approvalManager) notifyApprovalResult(approval store.ToolApproval, callbackToken, cardJSON, message string) {
	ctx, cancel := context.WithTimeout(m.baseContext(), approvalNotificationTimeout)
	defer cancel()
	m.updateCardAfterInteractionBestEffort(ctx, approval, callbackToken, cardJSON)
	if err := m.sender.SendText(ctx, approval.ChatID, message); err != nil {
		feishuLog.Warn(ctx, "send feishu tool approval result failed id=%s account=%s chat=%s: %v", shortApprovalID(approval.ID), approval.AccountID, approval.ChatID, err)
	}
}

func (m *approvalManager) updateCardBestEffort(ctx context.Context, messageID, cardJSON string) {
	if strings.TrimSpace(messageID) == "" || strings.TrimSpace(cardJSON) == "" {
		return
	}
	if err := m.sender.UpdateCard(ctx, messageID, cardJSON); err != nil {
		feishuLog.Warn(ctx, "update feishu tool approval card failed message=%s: %v", messageID, err)
	}
}

func (m *approvalManager) updateCardAfterInteractionBestEffort(ctx context.Context, approval store.ToolApproval, callbackToken, cardJSON string) {
	if strings.TrimSpace(callbackToken) == "" || strings.TrimSpace(cardJSON) == "" {
		feishuLog.Warn(ctx, "skip delayed feishu tool approval card update id=%s account=%s: callback token unavailable", shortApprovalID(approval.ID), approval.AccountID)
		return
	}
	if err := m.sender.UpdateCardAfterInteraction(ctx, callbackToken, cardJSON); err != nil {
		feishuLog.Warn(ctx, "delay-update feishu tool approval card failed id=%s account=%s: %v", shortApprovalID(approval.ID), approval.AccountID, err)
	}
}

func (m *approvalManager) failApprovalBestEffort(ctx context.Context, approvalID string) {
	if err := m.store.FailToolApproval(approvalID, m.account.ID, m.currentTime()); err != nil && !errors.Is(err, store.ErrToolApprovalResolved) {
		feishuLog.Error(ctx, "close failed feishu tool approval id=%s account=%s: %v", shortApprovalID(approvalID), m.account.ID, err)
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

func parseApprovalCardAction(event *callback.CardActionTriggerEvent) (string, string, bool) {
	if event == nil || event.Event == nil || event.Event.Action == nil {
		return "", "", false
	}
	value := event.Event.Action.Value
	if stringApprovalValue(value, "kind") != approvalCardActionKind {
		return "", "", false
	}
	approvalID := strings.TrimSpace(stringApprovalValue(value, "approval_id"))
	decision := strings.TrimSpace(stringApprovalValue(value, "decision"))
	if approvalID == "" || (decision != store.ToolApprovalDecisionApprove && decision != store.ToolApprovalDecisionDeny) {
		return approvalID, decision, false
	}
	return approvalID, decision, true
}

func stringApprovalValue(values map[string]interface{}, key string) string {
	if values == nil {
		return ""
	}
	value, _ := values[key].(string)
	return value
}

func buildPendingApprovalCard(request feishutools.ApprovalRequest, approval store.ToolApproval) (string, error) {
	lines := []string{
		"机器人请求执行以下操作：",
		"",
		"**操作**：" + escapeApprovalMarkdown(request.Action),
	}
	for _, field := range request.Fields {
		lines = append(lines, "**"+escapeApprovalMarkdown(field.Label)+"**："+escapeApprovalMarkdown(field.Value))
	}
	lines = append(lines,
		"",
		fmt.Sprintf("该授权仅对本次请求有效，将于 %s 过期。", approval.ExpiresAt.UTC().Format("2006-01-02 15:04 UTC")),
	)
	value := func(decision string) map[string]interface{} {
		return map[string]interface{}{
			"kind":        approvalCardActionKind,
			"approval_id": approval.ID,
			"decision":    decision,
		}
	}
	card := map[string]interface{}{
		"schema": "2.0",
		"config": map[string]interface{}{"update_multi": true},
		"header": map[string]interface{}{
			"title":    map[string]interface{}{"tag": "plain_text", "content": "操作授权确认"},
			"template": "orange",
		},
		"body": map[string]interface{}{
			"direction": "vertical",
			"elements": []interface{}{
				map[string]interface{}{"tag": "markdown", "content": strings.Join(lines, "\n")},
				map[string]interface{}{
					"tag":                "column_set",
					"horizontal_spacing": "8px",
					"columns": []interface{}{
						approvalButtonColumn("确认授权", "primary", value(store.ToolApprovalDecisionApprove)),
						approvalButtonColumn("拒绝", "default", value(store.ToolApprovalDecisionDeny)),
					},
				},
			},
		},
	}
	data, err := json.Marshal(card)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func approvalButtonColumn(text, buttonType string, value map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"tag":    "column",
		"width":  "weighted",
		"weight": 1,
		"elements": []interface{}{
			map[string]interface{}{
				"tag":   "button",
				"text":  map[string]interface{}{"tag": "plain_text", "content": text},
				"type":  buttonType,
				"width": "fill",
				"behaviors": []interface{}{
					map[string]interface{}{"type": "callback", "value": value},
				},
			},
		},
	}
}

func buildStatusApprovalCard(title, template, message string) string {
	card := map[string]interface{}{
		"schema": "2.0",
		"config": map[string]interface{}{"update_multi": true},
		"header": map[string]interface{}{
			"title":    map[string]interface{}{"tag": "plain_text", "content": title},
			"template": template,
		},
		"body": map[string]interface{}{
			"direction": "vertical",
			"elements": []interface{}{
				map[string]interface{}{"tag": "markdown", "content": message},
			},
		},
	}
	data, err := json.Marshal(card)
	if err != nil {
		return ""
	}
	return string(data)
}

func approvalToast(toastType, content string) *callback.CardActionTriggerResponse {
	return &callback.CardActionTriggerResponse{
		Toast: &callback.Toast{Type: toastType, Content: content},
	}
}

func approvalCallbackResponse(toastType, content, cardJSON string) *callback.CardActionTriggerResponse {
	response := approvalToast(toastType, content)
	var cardData map[string]interface{}
	if err := json.Unmarshal([]byte(cardJSON), &cardData); err == nil && cardData != nil {
		response.Card = &callback.Card{Type: "raw", Data: cardData}
	}
	return response
}

func escapeApprovalMarkdown(value string) string {
	replacer := strings.NewReplacer(
		"\\", "\\\\",
		"*", "\\*",
		"_", "\\_",
		"~", "\\~",
		"`", "\\`",
		"[", "\\[",
		"]", "\\]",
		"<", "\\<",
		">", "\\>",
	)
	return replacer.Replace(value)
}

func truncateApprovalRunes(value string, limit int) string {
	if limit <= 0 || utf8.RuneCountInString(value) <= limit {
		return value
	}
	runes := []rune(value)
	return string(runes[:limit-1]) + "…"
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

func shortApprovalID(id string) string {
	id = strings.TrimSpace(id)
	if len(id) <= 12 {
		return id
	}
	return id[:12]
}
