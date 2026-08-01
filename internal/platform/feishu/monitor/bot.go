package monitor

import (
	"context"
	"strings"
	"time"

	"lingobridge/internal/core"
	feishutools "lingobridge/internal/platform/feishu/tools"
	"lingobridge/internal/store"
	tooltypes "lingobridge/internal/tools"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

const (
	unsupportedMessageText         = "暂不支持此类飞书消息，请发送文字。"
	feishuProcessingReactionEmoji  = "Typing"
	feishuCompactDoneReactionEmoji = "DONE"
	feishuReactionClearDelay       = 1500 * time.Millisecond
)

type textProcessor interface {
	Handle(ctx context.Context, msg core.InboundMessage, sender core.Sender) error
}

type bot struct {
	handler       textProcessor
	sender        textSender
	tools         []tooltypes.Tool
	account       store.Account
	botOpenID     string
	eventCommands map[string][]string
	deduper       *eventDeduper
	runCtx        context.Context
	reactionDelay time.Duration
}

type feishuResponder struct {
	sender           textSender
	chatID           string
	messageID        string
	replyToMessageID string
	mentions         []feishuMention
}

func (r feishuResponder) Send(ctx context.Context, msg core.OutboundMessage) error {
	if msg.Text != "" {
		text := r.renderMentions(msg.Text)
		if r.replyToMessageID != "" {
			_, err := r.sender.CreateReplyText(ctx, r.replyToMessageID, text)
			return err
		}
		return r.sender.SendText(ctx, r.chatID, text)
	}
	if len(msg.Image.Data) > 0 || msg.Image.Filename != "" || msg.Image.LocalPath != "" {
		return core.ErrUnsupportedImage
	}
	return nil
}

func (r feishuResponder) StartTyping(ctx context.Context) func() {
	return func() {}
}

func (r feishuResponder) StartTextStream(ctx context.Context) (core.TextStream, error) {
	return &feishuTextStream{
		sender:           r.sender,
		chatID:           r.chatID,
		replyToMessageID: r.replyToMessageID,
		renderFinalText:  r.renderMentions,
		now:              time.Now,
	}, nil
}

func (r feishuResponder) StartCompactNotice(ctx context.Context, notice core.CompactNotice) (core.CompactNoticeHandle, error) {
	messageID, err := r.createText(ctx, core.CompactStartText())
	if err != nil {
		return core.CompactNoticeHandle{}, err
	}
	return core.CompactNoticeHandle{MessageID: messageID}, nil
}

func (r feishuResponder) createText(ctx context.Context, text string) (string, error) {
	if r.replyToMessageID != "" {
		return r.sender.CreateReplyText(ctx, r.replyToMessageID, text)
	}
	return r.sender.CreateText(ctx, r.chatID, text)
}

func (r feishuResponder) renderMentions(text string) string {
	return renderFeishuMentions(text, r.mentions)
}

func (r feishuResponder) FinishCompactNotice(ctx context.Context, handle core.CompactNoticeHandle, notice core.CompactNotice) error {
	if handle.MessageID == "" {
		return nil
	}
	if err := r.sender.UpdateText(ctx, handle.MessageID, core.CompactSuccessText(notice)); err != nil {
		return err
	}
	if r.messageID == "" {
		return nil
	}
	_, err := r.sender.AddReaction(ctx, r.messageID, feishuCompactDoneReactionEmoji)
	return err
}

func (b *bot) handleMessage(ctx context.Context, event *larkim.P2MessageReceiveV1) error {
	logReceivedMessage(ctx, event)
	in, ok := normalizeEvent(ctx, event, b.botOpenID)
	if !ok {
		return nil
	}
	if in.MentionAll {
		feishuLog.Debug(ctx, "feishu message ignored because it mentions all members chat=%s message=%s event=%s", in.ChatID, in.MessageID, feishuEventID(event))
		return nil
	}
	if in.IsGroup && !in.MentionBot {
		feishuLog.Debug(ctx, "feishu group message ignored because it does not mention the bot chat=%s message=%s event=%s", in.ChatID, in.MessageID, feishuEventID(event))
		return nil
	}
	dedupeKey := feishuDedupeKey(event)
	if dedupeKey == "" {
		feishuLog.Warn(ctx, "feishu message missing dedupe key; processing without dedupe")
	} else if b.deduperOrDefault().seenOrMark(dedupeKey) {
		feishuLog.Debug(ctx, "duplicate feishu message ignored key=%s", dedupeKey)
		return nil
	}

	go b.processMessage(in)
	return nil
}

func logReceivedMessage(ctx context.Context, event *larkim.P2MessageReceiveV1) {
	if event == nil || event.Event == nil || event.Event.Message == nil {
		return
	}
	msg := event.Event.Message
	user := ""
	if event.Event.Sender != nil {
		user = userOpenID(event.Event.Sender.SenderId)
		if user == "" {
			user = userID(event.Event.Sender.SenderId)
		}
	}
	feishuLog.Info(ctx, "received feishu message chat=%s user=%s message=%s type=%s chat_type=%s event=%s",
		deref(msg.ChatId),
		user,
		deref(msg.MessageId),
		deref(msg.MessageType),
		deref(msg.ChatType),
		feishuEventID(event),
	)
}

func (b *bot) processMessage(in incomingMessage) {
	ctx := feishutools.WithActor(b.processingContext(), feishutools.Actor{
		OpenID: in.SenderOpenID,
		UserID: in.SenderUserID,
	})
	ctx = feishutools.WithChatContext(ctx, feishutools.ChatContext{
		ChatID:    in.ChatID,
		MessageID: in.MessageID,
		IsGroup:   in.IsGroup,
	})
	resp := feishuResponder{sender: b.sender, chatID: in.ChatID, messageID: in.MessageID, replyToMessageID: in.ReplyToMessageID, mentions: in.Mentions}
	if in.Unsupported {
		if err := resp.Send(ctx, core.OutboundMessage{Text: unsupportedMessageText}); err != nil {
			feishuLog.Warn(ctx, "send unsupported feishu message notice failed chat=%s: %v", in.ChatID, err)
		}
		return
	}
	stopReaction := b.startProcessingReaction(ctx, in)
	defer stopReaction()
	if err := b.handler.Handle(ctx, core.InboundMessage{
		Platform:    store.PlatformFeishu,
		AccountID:   b.account.ID,
		AccountName: b.account.Name,
		UserKey:     in.UserID,
		CommandText: in.Text,
		LLMText:     in.Text,
		Metadata: map[string]string{
			"feishu.chat_id":        in.ChatID,
			"feishu.message_id":     in.MessageID,
			"feishu.sender_open_id": in.SenderOpenID,
			"feishu.sender_user_id": in.SenderUserID,
		},
		Tools: b.tools,
	}, resp); err != nil {
		feishuLog.Warn(ctx, "process feishu message failed user=%s chat=%s: %v", in.UserID, in.ChatID, err)
	}
}

func (b *bot) startProcessingReaction(ctx context.Context, in incomingMessage) func() {
	if in.MessageID == "" || strings.HasPrefix(strings.TrimSpace(in.Text), "/") {
		return func() {}
	}
	reactionID, err := b.sender.AddReaction(ctx, in.MessageID, feishuProcessingReactionEmoji)
	if err != nil {
		feishuLog.Warn(ctx, "add feishu reaction failed message=%s emoji=%s: %v", in.MessageID, feishuProcessingReactionEmoji, err)
		return func() {}
	}
	return func() {
		if !sleepContext(ctx, b.reactionClearDelay()) {
			return
		}
		if err := b.sender.DeleteReaction(ctx, in.MessageID, reactionID); err != nil {
			feishuLog.Warn(ctx, "delete feishu reaction failed message=%s reaction=%s: %v", in.MessageID, reactionID, err)
		}
	}
}

func (b *bot) reactionClearDelay() time.Duration {
	if b.reactionDelay <= 0 {
		return 0
	}
	return b.reactionDelay
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (b *bot) deduperOrDefault() *eventDeduper {
	if b.deduper == nil {
		b.deduper = newEventDeduper(defaultFeishuDedupeTTL)
	}
	return b.deduper
}

func (b *bot) processingContext() context.Context {
	if b.runCtx != nil {
		return b.runCtx
	}
	return context.Background()
}

func feishuDedupeKey(event *larkim.P2MessageReceiveV1) string {
	if event == nil {
		return ""
	}
	if event.Event != nil && event.Event.Message != nil {
		if key := deref(event.Event.Message.MessageId); key != "" {
			return key
		}
	}
	if event.EventV2Base != nil && event.EventV2Base.Header != nil {
		return event.EventV2Base.Header.EventID
	}
	return ""
}

func feishuEventID(event *larkim.P2MessageReceiveV1) string {
	if event == nil || event.EventV2Base == nil || event.EventV2Base.Header == nil {
		return ""
	}
	return event.EventV2Base.Header.EventID
}
