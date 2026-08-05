package tools

import (
	"context"
	"strings"
)

type executionContextKey struct{}

// ExecutionContext contains runtime-owned identity and conversation metadata
// for a tool call. These fields are attached outside the model-visible JSON
// arguments and must never be populated from tool arguments.
type ExecutionContext struct {
	Platform             string
	AccountID            string
	UserKey              string
	SessionID            string
	ChatID               string
	SourceMessageID      string
	ActorOpenID          string
	ActorUserID          string
	ActorName            string
	ActorEmail           string
	ChatIsGroup          bool
	ConversationRevision int64
	TurnID               string
	ToolCallID           string
	ToolName             string
}

// WithExecutionContext attaches a normalized, runtime-owned tool execution
// context. Callers must not copy values into it from model-provided arguments.
func WithExecutionContext(ctx context.Context, execution ExecutionContext) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, executionContextKey{}, normalizeExecutionContext(execution))
}

// ExecutionContextFromContext returns the trusted tool execution context.
func ExecutionContextFromContext(ctx context.Context) (ExecutionContext, bool) {
	if ctx == nil {
		return ExecutionContext{}, false
	}
	execution, ok := ctx.Value(executionContextKey{}).(ExecutionContext)
	if !ok {
		return ExecutionContext{}, false
	}
	return normalizeExecutionContext(execution), true
}

// WithToolCall binds the provider call ID and resolved tool name immediately
// before execution while preserving the runtime-owned conversation identity.
func WithToolCall(ctx context.Context, callID, toolName string) context.Context {
	execution, _ := ExecutionContextFromContext(ctx)
	execution.ToolCallID = callID
	execution.ToolName = toolName
	return WithExecutionContext(ctx, execution)
}

func normalizeExecutionContext(execution ExecutionContext) ExecutionContext {
	execution.Platform = strings.TrimSpace(execution.Platform)
	execution.AccountID = strings.TrimSpace(execution.AccountID)
	execution.UserKey = strings.TrimSpace(execution.UserKey)
	execution.SessionID = strings.TrimSpace(execution.SessionID)
	execution.ChatID = strings.TrimSpace(execution.ChatID)
	execution.SourceMessageID = strings.TrimSpace(execution.SourceMessageID)
	execution.ActorOpenID = strings.TrimSpace(execution.ActorOpenID)
	execution.ActorUserID = strings.TrimSpace(execution.ActorUserID)
	execution.ActorName = strings.TrimSpace(execution.ActorName)
	execution.ActorEmail = strings.TrimSpace(execution.ActorEmail)
	execution.TurnID = strings.TrimSpace(execution.TurnID)
	execution.ToolCallID = strings.TrimSpace(execution.ToolCallID)
	execution.ToolName = strings.TrimSpace(execution.ToolName)
	if execution.ConversationRevision < 0 {
		execution.ConversationRevision = 0
	}
	return execution
}
