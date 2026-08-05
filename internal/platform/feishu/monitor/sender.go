package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

var ErrFeishuMessageEditLimit = errors.New("feishu message edit limit reached")

const richTextLogPreviewRunes = 512

type textSender interface {
	SendText(ctx context.Context, chatID, text string) error
	CreateText(ctx context.Context, chatID, text string) (string, error)
	CreateReplyText(ctx context.Context, replyToMessageID, text string) (string, error)
	UpdateText(ctx context.Context, messageID, text string) error
	AddReaction(ctx context.Context, messageID, emojiType string) (string, error)
	DeleteReaction(ctx context.Context, messageID, reactionID string) error
}

type sdkSender struct {
	client *lark.Client
}

type richTextContent struct {
	ZhCN richTextLanguage `json:"zh_cn"`
}

type richTextLanguage struct {
	Content [][]richTextTextElement `json:"content"`
}

type richTextTextElement struct {
	Tag  string `json:"tag"`
	Text string `json:"text"`
}

func (s *sdkSender) SendText(ctx context.Context, chatID, text string) error {
	_, err := s.createText(ctx, chatID, text, false)
	return err
}

func (s *sdkSender) CreateText(ctx context.Context, chatID, text string) (string, error) {
	return s.createText(ctx, chatID, text, true)
}

func (s *sdkSender) CreateTextWithUUID(ctx context.Context, chatID, text, uuid string) (string, error) {
	return s.createTextWithUUID(ctx, chatID, text, strings.TrimSpace(uuid), true)
}

func (s *sdkSender) CreateReplyText(ctx context.Context, replyToMessageID, text string) (string, error) {
	body, err := marshalRichTextContent(text)
	if err != nil {
		return "", fmt.Errorf("marshal feishu rich text content: %w", err)
	}
	req := larkim.NewReplyMessageReqBuilder().
		MessageId(replyToMessageID).
		Body(larkim.NewReplyMessageReqBodyBuilder().
			MsgType(larkim.MsgTypePost).
			Content(body).
			ReplyInThread(false).
			Build()).
		Build()
	resp, err := s.client.Im.Message.Reply(ctx, req)
	if err != nil {
		return "", fmt.Errorf("reply feishu message: %w", err)
	}
	if !resp.Success() {
		return "", fmt.Errorf("reply feishu message code=%d msg=%s", resp.Code, resp.Msg)
	}
	if resp.Data != nil && resp.Data.MessageId != nil && *resp.Data.MessageId != "" {
		return *resp.Data.MessageId, nil
	}
	return "", fmt.Errorf("reply feishu message missing message_id")
}

func (s *sdkSender) CreateCard(ctx context.Context, chatID, cardJSON string) (string, error) {
	cardJSON, _, err := parseInteractiveCardJSON(cardJSON)
	if err != nil {
		return "", err
	}
	req := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType("chat_id").
		Body(larkim.NewCreateMessageReqBodyBuilder().
			ReceiveId(chatID).
			MsgType(larkim.MsgTypeInteractive).
			Content(cardJSON).
			Build()).
		Build()
	resp, err := s.client.Im.Message.Create(ctx, req)
	if err != nil {
		return "", fmt.Errorf("send feishu interactive card: %w", err)
	}
	if resp == nil || !resp.Success() {
		if resp == nil {
			return "", fmt.Errorf("send feishu interactive card: empty response")
		}
		return "", fmt.Errorf("send feishu interactive card code=%d msg=%s", resp.Code, resp.Msg)
	}
	if resp.Data == nil || resp.Data.MessageId == nil || *resp.Data.MessageId == "" {
		return "", fmt.Errorf("send feishu interactive card missing message_id")
	}
	return *resp.Data.MessageId, nil
}

func (s *sdkSender) createText(ctx context.Context, chatID, text string, requireMessageID bool) (string, error) {
	return s.createTextWithUUID(ctx, chatID, text, "", requireMessageID)
}

func (s *sdkSender) createTextWithUUID(ctx context.Context, chatID, text, uuid string, requireMessageID bool) (string, error) {
	body, err := marshalRichTextContent(text)
	if err != nil {
		return "", fmt.Errorf("marshal feishu rich text content: %w", err)
	}
	bodyBuilder := larkim.NewCreateMessageReqBodyBuilder().
		ReceiveId(chatID).
		MsgType(larkim.MsgTypePost).
		Content(body)
	if uuid != "" {
		bodyBuilder = bodyBuilder.Uuid(uuid)
	}
	req := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType("chat_id").
		Body(bodyBuilder.Build()).
		Build()
	resp, err := s.client.Im.Message.Create(ctx, req)
	if err != nil {
		return "", fmt.Errorf("send feishu message: %w", err)
	}
	if !resp.Success() {
		return "", fmt.Errorf("send feishu message code=%d msg=%s", resp.Code, resp.Msg)
	}
	if resp.Data != nil && resp.Data.MessageId != nil && *resp.Data.MessageId != "" {
		return *resp.Data.MessageId, nil
	}
	if requireMessageID {
		return "", fmt.Errorf("send feishu message missing message_id")
	}
	return "", nil
}

func (s *sdkSender) UpdateText(ctx context.Context, messageID, text string) error {
	body, err := marshalRichTextContent(text)
	if err != nil {
		return fmt.Errorf("marshal feishu rich text content: %w", err)
	}
	req := larkim.NewUpdateMessageReqBuilder().
		MessageId(messageID).
		Body(larkim.NewUpdateMessageReqBodyBuilder().
			MsgType(larkim.MsgTypePost).
			Content(body).
			Build()).
		Build()
	resp, err := s.client.Im.Message.Update(ctx, req)
	if err != nil {
		return fmt.Errorf("update feishu message: %w", err)
	}
	if !resp.Success() {
		if resp.Code == 230072 {
			return fmt.Errorf("%w: code=%d msg=%s", ErrFeishuMessageEditLimit, resp.Code, resp.Msg)
		}
		return fmt.Errorf("update feishu message code=%d msg=%s", resp.Code, resp.Msg)
	}
	return nil
}

func (s *sdkSender) UpdateCard(ctx context.Context, messageID, cardJSON string) error {
	cardJSON, _, err := parseInteractiveCardJSON(cardJSON)
	if err != nil {
		return err
	}
	req := larkim.NewPatchMessageReqBuilder().
		MessageId(messageID).
		Body(larkim.NewPatchMessageReqBodyBuilder().
			Content(cardJSON).
			Build()).
		Build()
	resp, err := s.client.Im.Message.Patch(ctx, req)
	if err != nil {
		return fmt.Errorf("update feishu interactive card: %w", err)
	}
	if resp == nil || !resp.Success() {
		if resp == nil {
			return fmt.Errorf("update feishu interactive card: empty response")
		}
		if resp.Code == 230072 {
			return fmt.Errorf("%w: code=%d msg=%s", ErrFeishuMessageEditLimit, resp.Code, resp.Msg)
		}
		return fmt.Errorf("update feishu interactive card code=%d msg=%s", resp.Code, resp.Msg)
	}
	return nil
}

func (s *sdkSender) UpdateCardAfterInteraction(ctx context.Context, callbackToken, cardJSON string) error {
	callbackToken = strings.TrimSpace(callbackToken)
	if callbackToken == "" {
		return fmt.Errorf("feishu card callback token is required")
	}
	_, card, err := parseInteractiveCardJSON(cardJSON)
	if err != nil {
		return err
	}
	resp, err := s.client.Post(ctx, "/open-apis/interactive/v1/card/update", map[string]interface{}{
		"token": callbackToken,
		"card":  card,
	}, larkcore.AccessTokenTypeTenant)
	if err != nil {
		return fmt.Errorf("delay-update feishu interactive card: %w", err)
	}
	if resp == nil {
		return fmt.Errorf("delay-update feishu interactive card: empty response")
	}
	var result struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(resp.RawBody, &result); err != nil {
		return fmt.Errorf("parse delay-update feishu interactive card response: %w", err)
	}
	if resp.StatusCode != http.StatusOK || result.Code != 0 {
		return fmt.Errorf("delay-update feishu interactive card status=%d code=%d msg=%s", resp.StatusCode, result.Code, result.Msg)
	}
	return nil
}

func parseInteractiveCardJSON(cardJSON string) (string, map[string]interface{}, error) {
	cardJSON = strings.TrimSpace(cardJSON)
	var card map[string]interface{}
	if cardJSON == "" || json.Unmarshal([]byte(cardJSON), &card) != nil || card == nil {
		return "", nil, fmt.Errorf("feishu interactive card must be a valid JSON object")
	}
	return cardJSON, card, nil
}

func marshalRichTextContent(text string) (string, error) {
	body, err := json.Marshal(buildRichTextContent(text))
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func buildRichTextContent(text string) richTextContent {
	normalized := strings.ReplaceAll(text, "\r\n", "\n")
	normalized = strings.ReplaceAll(normalized, "\r", "\n")
	if normalized == "" {
		normalized = " "
	}
	return richTextContent{
		ZhCN: richTextLanguage{Content: [][]richTextTextElement{{
			{
				Tag:  "md",
				Text: normalized,
			},
		}}},
	}
}

func (s *sdkSender) AddReaction(ctx context.Context, messageID, emojiType string) (string, error) {
	req := larkim.NewCreateMessageReactionReqBuilder().
		MessageId(messageID).
		Body(larkim.NewCreateMessageReactionReqBodyBuilder().
			ReactionType(larkim.NewEmojiBuilder().EmojiType(emojiType).Build()).
			Build()).
		Build()
	resp, err := s.client.Im.MessageReaction.Create(ctx, req)
	if err != nil {
		return "", fmt.Errorf("add feishu reaction: %w", err)
	}
	if !resp.Success() {
		return "", fmt.Errorf("add feishu reaction code=%d msg=%s", resp.Code, resp.Msg)
	}
	if resp.Data == nil || resp.Data.ReactionId == nil || *resp.Data.ReactionId == "" {
		return "", fmt.Errorf("add feishu reaction missing reaction_id")
	}
	return *resp.Data.ReactionId, nil
}

func (s *sdkSender) DeleteReaction(ctx context.Context, messageID, reactionID string) error {
	req := larkim.NewDeleteMessageReactionReqBuilder().
		MessageId(messageID).
		ReactionId(reactionID).
		Build()
	resp, err := s.client.Im.MessageReaction.Delete(ctx, req)
	if err != nil {
		return fmt.Errorf("delete feishu reaction: %w", err)
	}
	if !resp.Success() {
		return fmt.Errorf("delete feishu reaction code=%d msg=%s", resp.Code, resp.Msg)
	}
	return nil
}
