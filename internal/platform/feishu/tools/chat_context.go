package tools

import (
	"context"
	"strings"

	tooltypes "lingobridge/internal/tools"
)

// ChatContext identifies the trusted Feishu chat that triggered a tool call.
type ChatContext struct {
	ChatID    string
	MessageID string
	IsGroup   bool
}

// WithChatContext attaches trusted Feishu chat metadata to tool execution.
func WithChatContext(ctx context.Context, chat ChatContext) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	chat = normalizeChatContext(chat)
	if chat.ChatID == "" {
		return ctx
	}
	execution, _ := tooltypes.ExecutionContextFromContext(ctx)
	execution.ChatID = chat.ChatID
	execution.SourceMessageID = chat.MessageID
	execution.ChatIsGroup = chat.IsGroup
	return tooltypes.WithExecutionContext(ctx, execution)
}

// ChatContextFromContext returns the trusted Feishu chat attached by WithChatContext.
func ChatContextFromContext(ctx context.Context) (ChatContext, bool) {
	if ctx == nil {
		return ChatContext{}, false
	}
	execution, ok := tooltypes.ExecutionContextFromContext(ctx)
	if !ok {
		return ChatContext{}, false
	}
	chat := ChatContext{
		ChatID:    execution.ChatID,
		MessageID: execution.SourceMessageID,
		IsGroup:   execution.ChatIsGroup,
	}
	chat = normalizeChatContext(chat)
	return chat, chat.ChatID != ""
}

func normalizeChatContext(chat ChatContext) ChatContext {
	chat.ChatID = strings.TrimSpace(chat.ChatID)
	chat.MessageID = strings.TrimSpace(chat.MessageID)
	return chat
}
