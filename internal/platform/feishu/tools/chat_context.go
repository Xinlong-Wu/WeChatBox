package tools

import (
	"context"
	"strings"
)

type chatContextKey struct{}

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
	return context.WithValue(ctx, chatContextKey{}, chat)
}

// ChatContextFromContext returns the trusted Feishu chat attached by WithChatContext.
func ChatContextFromContext(ctx context.Context) (ChatContext, bool) {
	if ctx == nil {
		return ChatContext{}, false
	}
	chat, ok := ctx.Value(chatContextKey{}).(ChatContext)
	if !ok {
		return ChatContext{}, false
	}
	chat = normalizeChatContext(chat)
	return chat, chat.ChatID != ""
}

func normalizeChatContext(chat ChatContext) ChatContext {
	chat.ChatID = strings.TrimSpace(chat.ChatID)
	chat.MessageID = strings.TrimSpace(chat.MessageID)
	return chat
}
