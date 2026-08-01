package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	tooltypes "lingobridge/internal/tools"
)

const (
	chatHistoryToolName          = "feishu_chat_history_get"
	defaultChatHistoryLimit      = 20
	maxChatHistoryLimit          = 100
	chatHistoryPageSize          = 50
	chatHistoryResultLimit       = 12000
	chatHistoryContentTrimBuffer = 64
)

type chatHistoryRequest struct {
	ChatID    string
	PageSize  int
	PageToken string
}

type chatHistoryPage struct {
	Messages  []*larkim.Message
	HasMore   bool
	PageToken string
}

type chatHistoryAPI interface {
	ListMessages(ctx context.Context, req chatHistoryRequest) (chatHistoryPage, error)
}

type larkChatHistoryAPI struct {
	client *lark.Client
}

func (a larkChatHistoryAPI) ListMessages(ctx context.Context, query chatHistoryRequest) (chatHistoryPage, error) {
	if a.client == nil {
		return chatHistoryPage{}, fmt.Errorf("feishu client is unavailable")
	}
	builder := larkim.NewListMessageReqBuilder().
		ContainerIdType("chat").
		ContainerId(query.ChatID).
		SortType(larkim.ReadHistoryMessageV1SortTypeByCreateTimeDesc).
		PageSize(query.PageSize)
	if query.PageToken != "" {
		builder.PageToken(query.PageToken)
	}
	resp, err := a.client.Im.Message.List(ctx, builder.Build())
	if err != nil {
		return chatHistoryPage{}, fmt.Errorf("list feishu chat messages: %w", err)
	}
	if resp == nil {
		return chatHistoryPage{}, fmt.Errorf("list feishu chat messages: empty response")
	}
	if !resp.Success() {
		return chatHistoryPage{}, fmt.Errorf("list feishu chat messages: code=%d msg=%s", resp.Code, resp.Msg)
	}
	if resp.Data == nil {
		return chatHistoryPage{}, nil
	}
	return chatHistoryPage{
		Messages:  resp.Data.Items,
		HasMore:   resp.Data.HasMore != nil && *resp.Data.HasMore,
		PageToken: deref(resp.Data.PageToken),
	}, nil
}

type chatHistoryTool struct {
	spec     tooltypes.Spec
	api      chatHistoryAPI
	maxChars int
}

// NewChatHistoryTools returns a Feishu tool that reads the current chat's history.
func NewChatHistoryTools(client *lark.Client, cfg Config) []tooltypes.Tool {
	cfg = NormalizeConfig(cfg)
	if client == nil || !cfg.ChatHistory.Enabled {
		return nil
	}
	maxChars := cfg.MaxChars
	if maxChars <= 0 || maxChars > chatHistoryResultLimit {
		maxChars = chatHistoryResultLimit
	}
	return []tooltypes.Tool{chatHistoryTool{
		spec:     chatHistorySpec(),
		api:      larkChatHistoryAPI{client: client},
		maxChars: maxChars,
	}}
}

func (t chatHistoryTool) Spec() tooltypes.Spec {
	return t.spec
}

func (t chatHistoryTool) Execute(ctx context.Context, call tooltypes.Call) tooltypes.Result {
	content, err := t.read(ctx, call.Arguments)
	return tooltypes.Result{
		CallID:  call.ID,
		Name:    chatHistoryToolName,
		Content: contentOrError(content, err),
		IsError: err != nil,
	}
}

type chatHistoryArgs struct {
	Limit int `json:"limit,omitempty"`
}

type chatHistoryOutput struct {
	ChatID          string               `json:"chat_id"`
	RequestedLimit  int                  `json:"requested_limit"`
	Returned        int                  `json:"returned"`
	HasMore         bool                 `json:"has_more"`
	OutputTruncated bool                 `json:"output_truncated,omitempty"`
	Messages        []chatHistoryMessage `json:"messages"`
}

type chatHistoryMessage struct {
	MessageID        string             `json:"message_id,omitempty"`
	RootID           string             `json:"root_id,omitempty"`
	ParentID         string             `json:"parent_id,omitempty"`
	ThreadID         string             `json:"thread_id,omitempty"`
	CreateTime       string             `json:"create_time,omitempty"`
	UpdateTime       string             `json:"update_time,omitempty"`
	Type             string             `json:"type,omitempty"`
	Sender           *chatHistorySender `json:"sender,omitempty"`
	Deleted          bool               `json:"deleted,omitempty"`
	Updated          bool               `json:"updated,omitempty"`
	Content          string             `json:"content"`
	ContentTruncated bool               `json:"content_truncated,omitempty"`
}

type chatHistorySender struct {
	ID     string `json:"id,omitempty"`
	IDType string `json:"id_type,omitempty"`
	Type   string `json:"type,omitempty"`
	Name   string `json:"name,omitempty"`
}

func (t chatHistoryTool) read(ctx context.Context, raw json.RawMessage) (string, error) {
	started := time.Now()
	chat, ok := ChatContextFromContext(ctx)
	if !ok {
		return "", fmt.Errorf("trusted Feishu chat context is unavailable")
	}
	if t.api == nil {
		return "", fmt.Errorf("Feishu chat history client is unavailable")
	}

	var args chatHistoryArgs
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return "", fmt.Errorf("parse arguments: %w", err)
		}
	}
	limit := normalizeChatHistoryLimit(args.Limit)
	messages, hasMore, err := t.fetch(ctx, chat.ChatID, limit)
	if err != nil {
		if chat.IsGroup {
			return "", fmt.Errorf("%w; group history requires the Feishu permission \"获取群组中所有消息\" and the bot must be in the group", err)
		}
		return "", err
	}

	output := chatHistoryOutput{
		ChatID:         chat.ChatID,
		RequestedLimit: limit,
		HasMore:        hasMore,
		Messages:       normalizeChatHistoryMessages(messages),
	}
	output.Returned = len(output.Messages)
	content, err := marshalBoundedChatHistory(&output, t.maxChars)
	if err != nil {
		return "", err
	}
	feishuToolsLog.Debug(ctx, "read chat history chat=%s requested=%d returned=%d has_more=%t output_truncated=%t duration_ms=%d", chat.ChatID, limit, output.Returned, output.HasMore, output.OutputTruncated, time.Since(started).Milliseconds())
	return content, nil
}

func (t chatHistoryTool) fetch(ctx context.Context, chatID string, limit int) ([]*larkim.Message, bool, error) {
	messages := make([]*larkim.Message, 0, limit)
	pageToken := ""
	hasMore := false
	for len(messages) < limit {
		remaining := limit - len(messages)
		pageSize := remaining
		if pageSize > chatHistoryPageSize {
			pageSize = chatHistoryPageSize
		}
		page, err := t.api.ListMessages(ctx, chatHistoryRequest{ChatID: chatID, PageSize: pageSize, PageToken: pageToken})
		if err != nil {
			return nil, false, err
		}
		for _, message := range page.Messages {
			if message == nil {
				continue
			}
			if len(messages) == limit {
				hasMore = true
				break
			}
			messages = append(messages, message)
		}
		hasMore = hasMore || page.HasMore
		if len(messages) >= limit || !page.HasMore {
			break
		}
		next := strings.TrimSpace(page.PageToken)
		if next == "" || next == pageToken {
			return nil, false, fmt.Errorf("list feishu chat messages: invalid pagination token")
		}
		pageToken = next
	}
	return messages, hasMore, nil
}

func normalizeChatHistoryLimit(limit int) int {
	if limit <= 0 {
		return defaultChatHistoryLimit
	}
	if limit > maxChatHistoryLimit {
		return maxChatHistoryLimit
	}
	return limit
}

func normalizeChatHistoryMessages(messages []*larkim.Message) []chatHistoryMessage {
	out := make([]chatHistoryMessage, 0, len(messages))
	for i := len(messages) - 1; i >= 0; i-- {
		message := messages[i]
		if message == nil {
			continue
		}
		normalized := chatHistoryMessage{
			MessageID:  deref(message.MessageId),
			RootID:     deref(message.RootId),
			ParentID:   deref(message.ParentId),
			ThreadID:   deref(message.ThreadId),
			CreateTime: deref(message.CreateTime),
			UpdateTime: deref(message.UpdateTime),
			Type:       deref(message.MsgType),
			Deleted:    message.Deleted != nil && *message.Deleted,
			Updated:    message.Updated != nil && *message.Updated,
		}
		if message.Sender != nil {
			normalized.Sender = &chatHistorySender{
				ID:     deref(message.Sender.Id),
				IDType: deref(message.Sender.IdType),
				Type:   deref(message.Sender.SenderType),
				Name:   deref(message.Sender.SenderName),
			}
		}
		normalized.Content = renderChatHistoryContent(message)
		out = append(out, normalized)
	}
	return out
}

func renderChatHistoryContent(message *larkim.Message) string {
	if message == nil {
		return ""
	}
	if message.Deleted != nil && *message.Deleted {
		return "[message deleted]"
	}
	msgType := strings.ToLower(strings.TrimSpace(deref(message.MsgType)))
	raw := ""
	if message.Body != nil {
		raw = deref(message.Body.Content)
	}
	switch msgType {
	case "text":
		return renderChatHistoryText(raw, message.Mentions)
	case "post":
		return renderChatHistoryPost(raw, message.Mentions)
	case "image":
		return "[image]"
	case "file":
		return historyFilePlaceholder("file", raw)
	case "audio":
		return "[audio]"
	case "media":
		return historyFilePlaceholder("media", raw)
	case "sticker":
		return "[sticker]"
	case "interactive":
		return "[interactive card]"
	case "share_chat":
		return "[shared chat]"
	case "share_user":
		return "[shared user]"
	case "system":
		return "[system message]"
	case "":
		return "[unknown message]"
	default:
		return "[" + msgType + " message]"
	}
}

type historyTextContent struct {
	Text string `json:"text"`
}

func renderChatHistoryText(raw string, mentions []*larkim.Mention) string {
	var content historyTextContent
	if err := json.Unmarshal([]byte(raw), &content); err != nil {
		return "[unreadable text message]"
	}
	return strings.TrimSpace(replaceChatHistoryMentions(content.Text, mentions))
}

type historyPostContent struct {
	Title   string                 `json:"title"`
	Content [][]historyPostElement `json:"content"`
}

type historyPostElement struct {
	Tag      string `json:"tag"`
	Text     string `json:"text"`
	Href     string `json:"href"`
	UserName string `json:"user_name"`
	Name     string `json:"name"`
	FileName string `json:"file_name"`
}

func renderChatHistoryPost(raw string, mentions []*larkim.Mention) string {
	post, ok := decodeHistoryPost(raw)
	if !ok {
		return "[unreadable post message]"
	}
	lines := []string{}
	if title := strings.TrimSpace(replaceChatHistoryMentions(post.Title, mentions)); title != "" {
		lines = append(lines, "# "+title, "")
	}
	for _, row := range post.Content {
		var line strings.Builder
		for _, element := range row {
			line.WriteString(renderHistoryPostElement(element, mentions))
		}
		lines = append(lines, strings.TrimSpace(line.String()))
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func decodeHistoryPost(raw string) (historyPostContent, bool) {
	var direct historyPostContent
	if err := json.Unmarshal([]byte(raw), &direct); err == nil && (direct.Title != "" || direct.Content != nil) {
		return direct, true
	}
	var localized map[string]historyPostContent
	if err := json.Unmarshal([]byte(raw), &localized); err != nil || len(localized) == 0 {
		return historyPostContent{}, false
	}
	for _, locale := range []string{"zh_cn", "en_us", "ja_jp"} {
		if post, ok := localized[locale]; ok {
			return post, true
		}
	}
	locales := make([]string, 0, len(localized))
	for locale := range localized {
		locales = append(locales, locale)
	}
	sort.Strings(locales)
	return localized[locales[0]], true
}

func renderHistoryPostElement(element historyPostElement, mentions []*larkim.Mention) string {
	tag := strings.ToLower(strings.TrimSpace(element.Tag))
	text := replaceChatHistoryMentions(element.Text, mentions)
	switch tag {
	case "", "text", "code_block":
		return text
	case "a":
		label := strings.TrimSpace(text)
		href := strings.TrimSpace(element.Href)
		if label == "" {
			label = href
		}
		if href == "" {
			return label
		}
		return "[" + label + "](" + href + ")"
	case "at":
		name := strings.TrimSpace(element.UserName)
		if name == "" {
			name = strings.TrimSpace(element.Name)
		}
		if name == "" {
			return "@user"
		}
		return "@" + name
	case "img":
		return "[image]"
	case "media", "file":
		if name := strings.TrimSpace(element.FileName); name != "" {
			return "[" + tag + ": " + name + "]"
		}
		return "[" + tag + "]"
	case "emotion", "sticker":
		return "[emoji]"
	default:
		if strings.TrimSpace(text) != "" {
			return text
		}
		return "[" + tag + "]"
	}
}

func replaceChatHistoryMentions(text string, mentions []*larkim.Mention) string {
	replacements := map[string]string{}
	for _, mention := range mentions {
		if mention == nil {
			continue
		}
		key := strings.TrimSpace(deref(mention.Key))
		if key == "" {
			continue
		}
		name := strings.TrimSpace(deref(mention.Name))
		if name == "" {
			name = "user"
		}
		replacements[key] = "@" + name
	}
	keys := make([]string, 0, len(replacements))
	for key := range replacements {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	for _, key := range keys {
		text = strings.ReplaceAll(text, key, replacements[key])
	}
	return text
}

func historyFilePlaceholder(kind, raw string) string {
	var content struct {
		FileName string `json:"file_name"`
	}
	if err := json.Unmarshal([]byte(raw), &content); err == nil {
		if name := strings.TrimSpace(content.FileName); name != "" {
			return "[" + kind + ": " + name + "]"
		}
	}
	return "[" + kind + "]"
}

func marshalBoundedChatHistory(output *chatHistoryOutput, maxChars int) (string, error) {
	if output == nil {
		return "", fmt.Errorf("Feishu chat history output is unavailable")
	}
	if maxChars <= 0 || maxChars > chatHistoryResultLimit {
		maxChars = chatHistoryResultLimit
	}
	for {
		output.Returned = len(output.Messages)
		data, err := json.Marshal(output)
		if err != nil {
			return "", err
		}
		length := utf8.RuneCount(data)
		if length <= maxChars {
			return string(data), nil
		}
		output.OutputTruncated = true
		if len(output.Messages) > 1 {
			output.HasMore = true
			output.Messages = output.Messages[1:]
			continue
		}
		if len(output.Messages) == 1 {
			message := &output.Messages[0]
			contentRunes := utf8.RuneCountInString(message.Content)
			if contentRunes > 0 {
				overflow := length - maxChars
				keep := contentRunes - overflow - chatHistoryContentTrimBuffer
				if keep < 0 {
					keep = 0
				}
				if keep >= contentRunes {
					keep = contentRunes - 1
				}
				if keep == 0 {
					message.Content = ""
				} else {
					message.Content = string([]rune(message.Content)[:keep])
				}
				message.ContentTruncated = true
				continue
			}
			output.HasMore = true
			output.Messages = []chatHistoryMessage{}
			continue
		}
		return "", fmt.Errorf("Feishu chat history output limit is too small")
	}
}

func chatHistorySpec() tooltypes.Spec {
	return tooltypes.Spec{
		Name:        chatHistoryToolName,
		Description: "Read recent messages from the current Feishu chat through the Feishu message history API. The current chat_id is bound by the runtime and cannot be changed by tool arguments. Use this when the user asks about earlier messages in the current Feishu conversation.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"limit":{"type":"integer","minimum":1,"maximum":100,"description":"Maximum number of recent Feishu messages to return. Defaults to 20 and is always capped at 100."}},"additionalProperties":false}`),
	}
}
