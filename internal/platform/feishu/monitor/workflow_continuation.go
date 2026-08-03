package monitor

import (
	"context"
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
