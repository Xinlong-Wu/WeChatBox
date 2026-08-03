package core

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"lingobridge/internal/llm"
	"lingobridge/internal/store"
)

func isCompactCommand(text string) bool {
	parts := strings.Fields(strings.TrimSpace(text))
	return len(parts) > 0 && parts[0] == "/compact"
}

func (b *Bot) handleCompactCommand(ctx context.Context, msg InboundMessage, sender Sender) error {
	if !msg.CommandPolicy.Allows("/compact") {
		return sender.Send(ctx, OutboundMessage{Text: "此平台暂不支持 /compact。"})
	}

	sess, err := b.Sessions.GetOrCreateCurrentSession(msg.UserKey)
	if err != nil {
		coreLog.Error(ctx, "get session for compact: %v", err)
		_ = sender.Send(ctx, OutboundMessage{Text: "❌ 会话加载失败，请重试。"})
		return err
	}

	model, llmClient, err := b.llmForSession(msg.UserKey, sess.ID)
	if err != nil {
		coreLog.Error(ctx, "resolve LLM for compact: %v", err)
		_ = sender.Send(ctx, OutboundMessage{Text: "❌ 模型配置不可用，请检查配置。"})
		return err
	}
	compact := llm.CompactConfig{
		Mode:          string(model.Compact.Mode),
		ContextWindow: model.ContextWindow,
		Threshold:     model.Compact.Threshold,
		Instructions:  model.Compact.Instructions,
	}
	if compact.Mode == compactModeFalse {
		return sender.Send(ctx, OutboundMessage{Text: fmt.Sprintf("❌ 当前模型已禁用上下文压缩：%s", model.Name)})
	}
	compactor, ok := llmClient.(llm.ContextCompactor)
	if !ok {
		return sender.Send(ctx, OutboundMessage{Text: fmt.Sprintf("❌ 当前模型不支持上下文压缩：provider=%s endpoint=%s", model.Provider, model.Endpoint)})
	}

	conv, err := b.Sessions.LoadHistory(msg.UserKey, sess.ID)
	if err != nil {
		coreLog.Warn(ctx, "load history for compact: %v", err)
		conv = &store.Conversation{}
	}
	if len(conv.Messages) <= nativeContextKeepRecentMessages {
		return sender.Send(ctx, OutboundMessage{Text: "当前会话没有足够的旧历史可压缩。"})
	}

	cutoff := len(conv.Messages) - nativeContextKeepRecentMessages
	providerContext := providerContextForModel(conv, model.Name)
	notice := CompactNotice{
		ModelName:         model.Name,
		Manual:            true,
		CompactedMessages: cutoff,
		RetainedMessages:  nativeContextKeepRecentMessages,
	}
	noticeHandle := startCompactNotice(ctx, sender, notice)
	compactedContext, err := compactor.CompactContext(b.LLMConfig.SystemPrompt, conv.Messages[:cutoff], providerContext, compact)
	if err != nil {
		if errors.Is(err, llm.ErrCompactionNotTriggered) {
			return sender.Send(ctx, OutboundMessage{Text: "当前会话尚未达到供应商原生压缩触发阈值，未保存压缩上下文。"})
		}
		coreLog.Error(ctx, "manual compact failed provider=%s model=%s: %v", model.Provider, model.Name, err)
		_ = sender.Send(ctx, OutboundMessage{Text: b.errorNotice(msg, err)})
		return err
	}
	if compactedContext.IsEmpty() {
		return sender.Send(ctx, OutboundMessage{Text: "当前会话没有生成可保存的压缩上下文。"})
	}

	if conv.ProviderContexts == nil {
		conv.ProviderContexts = map[string]store.ProviderContext{}
	}
	conv.ProviderContexts[model.Name] = compactedContext
	conv.Messages = retainRecentMessages(conv.Messages, nativeContextKeepRecentMessages)
	notice.RetainedMessages = len(conv.Messages)
	if err := b.Sessions.SaveHistory(msg.UserKey, sess.ID, conv); err != nil {
		coreLog.Warn(ctx, "save compacted history: %v", err)
	}

	return finishCompactNotice(ctx, sender, noticeHandle, notice)
}
