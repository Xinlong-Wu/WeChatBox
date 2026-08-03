package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"lingobridge/internal/store"
	tooltypes "lingobridge/internal/tools"
)

type workflowContinuationCreator interface {
	CreateWorkflowContinuation(store.WorkflowContinuation) (store.WorkflowContinuation, error)
}

type workflowContinuationCanceler interface {
	CancelWorkflowContinuation(requestID, accountID, reason string, now time.Time) error
}

type workflowResultWriter interface {
	StoreWorkflowResult(store.WorkflowResult) (store.WorkflowResult, store.WorkflowContinuation, bool, error)
}

func trustedWorkflowExecutionContext(ctx context.Context, accountID, toolName string) (tooltypes.ExecutionContext, error) {
	execution, ok := tooltypes.ExecutionContextFromContext(ctx)
	accountID = strings.TrimSpace(accountID)
	toolName = strings.TrimSpace(toolName)
	if !ok {
		return tooltypes.ExecutionContext{}, fmt.Errorf("trusted tool execution context is required for asynchronous workflow continuation")
	}
	if execution.Platform != store.PlatformFeishu || execution.AccountID != accountID {
		return tooltypes.ExecutionContext{}, fmt.Errorf("trusted tool execution platform and account do not match the Feishu workflow")
	}
	if execution.ToolName != toolName {
		return tooltypes.ExecutionContext{}, fmt.Errorf("trusted tool execution name does not match the Feishu workflow")
	}
	if execution.UserKey == "" || execution.SessionID == "" || execution.ChatID == "" || execution.SourceMessageID == "" ||
		execution.TurnID == "" || execution.ToolCallID == "" || (execution.ActorOpenID == "" && execution.ActorUserID == "") {
		return tooltypes.ExecutionContext{}, fmt.Errorf("trusted user, session, chat, message, actor, turn, and tool call are required for asynchronous workflow continuation")
	}
	return execution, nil
}

func persistWorkflowContinuation(creator workflowContinuationCreator, execution tooltypes.ExecutionContext, requestID string, now time.Time) (store.WorkflowContinuation, error) {
	if creator == nil {
		return store.WorkflowContinuation{}, fmt.Errorf("workflow continuation store is required")
	}
	continuation, err := creator.CreateWorkflowContinuation(store.WorkflowContinuation{
		RequestID:       strings.TrimSpace(requestID),
		AccountID:       execution.AccountID,
		Platform:        execution.Platform,
		UserKey:         execution.UserKey,
		SessionID:       execution.SessionID,
		ChatID:          execution.ChatID,
		ChatIsGroup:     execution.ChatIsGroup,
		SourceMessageID: execution.SourceMessageID,
		ActorOpenID:     execution.ActorOpenID,
		ActorUserID:     execution.ActorUserID,
		OriginRevision:  execution.ConversationRevision,
		OriginTurnID:    execution.TurnID,
		ToolCallID:      execution.ToolCallID,
		ToolName:        execution.ToolName,
		CreatedAt:       now,
	})
	if err != nil {
		return store.WorkflowContinuation{}, fmt.Errorf("persist workflow continuation: %w", err)
	}
	return continuation, nil
}

func cancelWorkflowContinuationBestEffort(ctx context.Context, canceler workflowContinuationCanceler, requestID, accountID, reason string, now time.Time) {
	if canceler == nil {
		return
	}
	if err := canceler.CancelWorkflowContinuation(requestID, accountID, reason, now); err != nil && !errors.Is(err, store.ErrWorkflowContinuationResolved) {
		feishuLog.Warn(ctx, "cancel feishu workflow continuation failed request=%s account=%s: %v", shortRequestID(requestID), accountID, err)
	}
}

func persistWorkflowResultBestEffort(ctx context.Context, writer workflowResultWriter, requestID, accountID, state string, payload any, now time.Time) {
	continuation, ready, err := persistWorkflowResult(writer, requestID, accountID, state, payload, now)
	if err == nil {
		feishuLog.Debug(ctx, "persisted feishu workflow result request=%s account=%s state=%s session=%s continuation_state=%s ready=%t",
			shortRequestID(requestID), accountID, state, continuation.SessionID, continuation.State, ready)
		return
	}
	if errors.Is(err, store.ErrWorkflowContinuationNotFound) {
		feishuLog.Debug(ctx, "skip workflow result without continuation request=%s account=%s state=%s", shortRequestID(requestID), accountID, state)
		return
	}
	feishuLog.Error(ctx, "persist feishu workflow result failed request=%s account=%s state=%s: %v", shortRequestID(requestID), accountID, state, err)
}

func persistWorkflowResult(writer workflowResultWriter, requestID, accountID, state string, payload any, now time.Time) (store.WorkflowContinuation, bool, error) {
	if writer == nil {
		return store.WorkflowContinuation{}, false, fmt.Errorf("workflow result store is unavailable")
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return store.WorkflowContinuation{}, false, fmt.Errorf("marshal workflow result: %w", err)
	}
	_, continuation, ready, err := writer.StoreWorkflowResult(store.WorkflowResult{
		RequestID: requestID,
		AccountID: accountID,
		State:     state,
		Payload:   raw,
		CreatedAt: now,
	})
	if err != nil {
		return continuation, false, err
	}
	return continuation, ready, nil
}
