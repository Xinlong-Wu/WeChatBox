package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	tooltypes "lingobridge/internal/tools"
)

func TestChatContextRoundTrip(t *testing.T) {
	ctx := WithChatContext(context.Background(), ChatContext{
		ChatID:    " oc_current ",
		MessageID: " om_current ",
		IsGroup:   true,
	})
	chat, ok := ChatContextFromContext(ctx)
	if !ok {
		t.Fatal("ChatContextFromContext returned ok=false")
	}
	if chat.ChatID != "oc_current" || chat.MessageID != "om_current" || !chat.IsGroup {
		t.Fatalf("chat context = %#v", chat)
	}
	if _, ok := ChatContextFromContext(WithChatContext(ctx, ChatContext{})); !ok {
		t.Fatal("empty chat context should preserve the existing context")
	}
}

func TestChatHistoryToolRegistration(t *testing.T) {
	client := &lark.Client{}
	cfg := NormalizeConfig(Config{})
	if cfg.ChatHistory.Enabled {
		t.Fatal("chat history enabled by default, want disabled")
	}
	if got := NewChatHistoryTools(client, cfg); len(got) != 0 {
		t.Fatalf("disabled chat history tools = %d, want 0", len(got))
	}
	cfg.ChatHistory.Enabled = true
	registered := NewChatHistoryTools(client, cfg)
	if len(registered) != 1 {
		t.Fatalf("enabled chat history tools = %d, want 1", len(registered))
	}
	spec := registered[0].Spec()
	if spec.Name != chatHistoryToolName || !strings.Contains(spec.Description, "current Feishu chat") {
		t.Fatalf("spec = %#v", spec)
	}
	if strings.Contains(string(spec.Parameters), "chat_id") {
		t.Fatalf("parameters expose chat_id: %s", spec.Parameters)
	}
}

func TestChatHistoryToolReadsTrustedChatInChronologicalOrder(t *testing.T) {
	api := &fakeChatHistoryAPI{pages: []chatHistoryPage{{
		Messages: []*larkim.Message{
			historyMessage("om_new", "text", `{"text":"new @_user_1"}`, "3000", &larkim.Mention{Key: historyString("@_user_1"), Name: historyString("Alice")}),
			{
				MessageId:  historyString("om_old"),
				RootId:     historyString("om_root"),
				ParentId:   historyString("om_parent"),
				ThreadId:   historyString("omt_thread"),
				MsgType:    historyString("post"),
				CreateTime: historyString("1000"),
				Updated:    historyBool(true),
				Sender: &larkim.Sender{
					Id:         historyString("ou_alice"),
					IdType:     historyString("open_id"),
					SenderType: historyString("user"),
					SenderName: historyString("Alice"),
				},
				Body: &larkim.MessageBody{Content: historyString(`{"zh_cn":{"title":"Status","content":[[{"tag":"text","text":"old"}]]}}`)},
			},
		},
	}}}
	tool := chatHistoryTool{spec: chatHistorySpec(), api: api, maxChars: chatHistoryResultLimit}
	ctx := WithChatContext(context.Background(), ChatContext{ChatID: "oc_current", MessageID: "om_new"})
	result := tool.Execute(ctx, tooltypes.Call{ID: "call_1", Name: chatHistoryToolName, Arguments: json.RawMessage(`{"limit":2}`)})
	if result.IsError {
		t.Fatalf("Execute returned error: %s", result.Content)
	}
	if result.CallID != "call_1" || result.Name != chatHistoryToolName {
		t.Fatalf("result identity = %#v", result)
	}
	if len(api.requests) != 1 || api.requests[0].ChatID != "oc_current" || api.requests[0].PageSize != 2 {
		t.Fatalf("requests = %#v", api.requests)
	}

	var output chatHistoryOutput
	if err := json.Unmarshal([]byte(result.Content), &output); err != nil {
		t.Fatalf("unmarshal output: %v\n%s", err, result.Content)
	}
	if output.ChatID != "oc_current" || output.RequestedLimit != 2 || output.Returned != 2 || output.HasMore {
		t.Fatalf("output metadata = %#v", output)
	}
	if output.Messages[0].MessageID != "om_old" || output.Messages[1].MessageID != "om_new" {
		t.Fatalf("message order = %#v", output.Messages)
	}
	old := output.Messages[0]
	if old.Content != "# Status\n\nold" || old.RootID != "om_root" || old.ParentID != "om_parent" || old.ThreadID != "omt_thread" || !old.Updated {
		t.Fatalf("old message = %#v", old)
	}
	if old.Sender == nil || old.Sender.ID != "ou_alice" || old.Sender.IDType != "open_id" || old.Sender.Name != "Alice" || old.Sender.Type != "user" {
		t.Fatalf("old sender = %#v", old.Sender)
	}
	if output.Messages[1].Content != "new @Alice" {
		t.Fatalf("new content = %q", output.Messages[1].Content)
	}
}

func TestChatHistoryToolRequiresTrustedChatContext(t *testing.T) {
	api := &fakeChatHistoryAPI{}
	tool := chatHistoryTool{spec: chatHistorySpec(), api: api, maxChars: chatHistoryResultLimit}
	result := tool.Execute(context.Background(), tooltypes.Call{ID: "call_1", Name: chatHistoryToolName, Arguments: json.RawMessage(`{}`)})
	if !result.IsError || !strings.Contains(result.Content, "trusted Feishu chat context") {
		t.Fatalf("result = %#v", result)
	}
	if len(api.requests) != 0 {
		t.Fatalf("requests = %#v, want none", api.requests)
	}
}

func TestChatHistoryToolAddsGroupPermissionHint(t *testing.T) {
	api := &fakeChatHistoryAPI{err: errors.New("code=99991672 permission denied")}
	tool := chatHistoryTool{spec: chatHistorySpec(), api: api, maxChars: chatHistoryResultLimit}
	ctx := WithChatContext(context.Background(), ChatContext{ChatID: "oc_group", IsGroup: true})
	result := tool.Execute(ctx, tooltypes.Call{Name: chatHistoryToolName, Arguments: json.RawMessage(`{}`)})
	if !result.IsError || !strings.Contains(result.Content, "获取群组中所有消息") || !strings.Contains(result.Content, "bot must be in the group") {
		t.Fatalf("result = %#v", result)
	}
}

func TestChatHistoryFetchPaginatesAndCapsAtOneHundred(t *testing.T) {
	first := make([]*larkim.Message, 0, 50)
	second := make([]*larkim.Message, 0, 50)
	for i := 100; i >= 51; i-- {
		first = append(first, historyMessage("om_"+strconv.Itoa(i), "text", `{"text":"x"}`, strconv.Itoa(i), nil))
	}
	for i := 50; i >= 1; i-- {
		second = append(second, historyMessage("om_"+strconv.Itoa(i), "text", `{"text":"x"}`, strconv.Itoa(i), nil))
	}
	api := &fakeChatHistoryAPI{pages: []chatHistoryPage{
		{Messages: first, HasMore: true, PageToken: "next"},
		{Messages: second, HasMore: true, PageToken: "ignored"},
	}}
	tool := chatHistoryTool{api: api}
	limit := normalizeChatHistoryLimit(1000)
	messages, hasMore, err := tool.fetch(context.Background(), "oc_current", limit)
	if err != nil {
		t.Fatalf("fetch returned error: %v", err)
	}
	if limit != 100 || len(messages) != 100 || !hasMore {
		t.Fatalf("limit=%d messages=%d hasMore=%v", limit, len(messages), hasMore)
	}
	if len(api.requests) != 2 || api.requests[0].PageSize != 50 || api.requests[0].PageToken != "" || api.requests[1].PageSize != 50 || api.requests[1].PageToken != "next" {
		t.Fatalf("requests = %#v", api.requests)
	}
}

func TestChatHistoryContentSanitizesNonTextMessages(t *testing.T) {
	file := historyMessage("om_file", "file", `{"file_key":"file_secret","file_name":"report.pdf"}`, "1", nil)
	if got := renderChatHistoryContent(file); got != "[file: report.pdf]" || strings.Contains(got, "file_secret") {
		t.Fatalf("file content = %q", got)
	}
	image := historyMessage("om_image", "image", `{"image_key":"img_secret"}`, "2", nil)
	if got := renderChatHistoryContent(image); got != "[image]" || strings.Contains(got, "img_secret") {
		t.Fatalf("image content = %q", got)
	}
	deleted := historyMessage("om_deleted", "text", `{"text":"secret"}`, "3", nil)
	deleted.Deleted = historyBool(true)
	if got := renderChatHistoryContent(deleted); got != "[message deleted]" {
		t.Fatalf("deleted content = %q", got)
	}
}

func TestMarshalBoundedChatHistoryKeepsValidJSON(t *testing.T) {
	output := chatHistoryOutput{
		ChatID:         "oc_current",
		RequestedLimit: 2,
		Messages: []chatHistoryMessage{
			{MessageID: "om_old", Type: "text", Content: strings.Repeat("旧", 1000)},
			{MessageID: "om_new", Type: "text", Content: strings.Repeat("新", 1000)},
		},
	}
	content, err := marshalBoundedChatHistory(&output, 500)
	if err != nil {
		t.Fatalf("marshalBoundedChatHistory returned error: %v", err)
	}
	if utf8.RuneCountInString(content) > 500 {
		t.Fatalf("content length = %d, want <= 500", utf8.RuneCountInString(content))
	}
	var bounded chatHistoryOutput
	if err := json.Unmarshal([]byte(content), &bounded); err != nil {
		t.Fatalf("bounded output is invalid JSON: %v\n%s", err, content)
	}
	if !bounded.OutputTruncated || !bounded.HasMore || bounded.Returned != len(bounded.Messages) || len(bounded.Messages) != 1 || bounded.Messages[0].MessageID != "om_new" || !bounded.Messages[0].ContentTruncated {
		t.Fatalf("bounded output = %#v", bounded)
	}
}

func TestMarshalBoundedChatHistoryContentTruncationDoesNotClaimMoreMessages(t *testing.T) {
	output := chatHistoryOutput{
		ChatID:         "oc_current",
		RequestedLimit: 1,
		Messages: []chatHistoryMessage{
			{MessageID: "om_only", Type: "text", Content: strings.Repeat("长", 1000)},
		},
	}
	content, err := marshalBoundedChatHistory(&output, 500)
	if err != nil {
		t.Fatalf("marshalBoundedChatHistory returned error: %v", err)
	}
	var bounded chatHistoryOutput
	if err := json.Unmarshal([]byte(content), &bounded); err != nil {
		t.Fatalf("bounded output is invalid JSON: %v", err)
	}
	if bounded.HasMore || !bounded.OutputTruncated || len(bounded.Messages) != 1 || !bounded.Messages[0].ContentTruncated {
		t.Fatalf("bounded output = %#v", bounded)
	}
}

func TestLarkChatHistoryAPIUsesChatContainerQuery(t *testing.T) {
	var sawMessages bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal", "/oauth/v3/token":
			writeChatHistoryJSON(t, w, map[string]any{
				"code":                0,
				"msg":                 "ok",
				"tenant_access_token": "tenant-token",
				"expire":              7200,
			})
		case "/open-apis/im/v1/messages":
			sawMessages = true
			query := r.URL.Query()
			if query.Get("container_id_type") != "chat" || query.Get("container_id") != "oc_current" || query.Get("sort_type") != larkim.ReadHistoryMessageV1SortTypeByCreateTimeDesc || query.Get("page_size") != "5" || query.Get("page_token") != "next" {
				t.Fatalf("query = %v", query)
			}
			writeChatHistoryJSON(t, w, map[string]any{
				"code": 0,
				"msg":  "ok",
				"data": map[string]any{
					"has_more":   false,
					"page_token": "",
					"items": []map[string]any{{
						"message_id": "om_1",
						"msg_type":   "text",
						"body":       map[string]any{"content": `{"text":"hello"}`},
					}},
				},
			})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client := lark.NewClient("cli_test", "secret",
		lark.WithOpenBaseUrl(server.URL),
		lark.WithOAuthBaseUrl(server.URL),
	)
	page, err := (larkChatHistoryAPI{client: client}).ListMessages(context.Background(), chatHistoryRequest{
		ChatID: "oc_current", PageSize: 5, PageToken: "next",
	})
	if err != nil {
		t.Fatalf("ListMessages returned error: %v", err)
	}
	if !sawMessages || len(page.Messages) != 1 || deref(page.Messages[0].MessageId) != "om_1" || page.HasMore {
		t.Fatalf("page = %#v sawMessages=%v", page, sawMessages)
	}
}

type fakeChatHistoryAPI struct {
	pages    []chatHistoryPage
	requests []chatHistoryRequest
	err      error
}

func (f *fakeChatHistoryAPI) ListMessages(ctx context.Context, req chatHistoryRequest) (chatHistoryPage, error) {
	f.requests = append(f.requests, req)
	if f.err != nil {
		return chatHistoryPage{}, f.err
	}
	index := len(f.requests) - 1
	if index >= len(f.pages) {
		return chatHistoryPage{}, nil
	}
	return f.pages[index], nil
}

func historyMessage(id, msgType, content, createTime string, mentions ...*larkim.Mention) *larkim.Message {
	return &larkim.Message{
		MessageId:  historyString(id),
		MsgType:    historyString(msgType),
		CreateTime: historyString(createTime),
		Body:       &larkim.MessageBody{Content: historyString(content)},
		Mentions:   mentions,
	}
}

func historyString(value string) *string { return &value }

func historyBool(value bool) *bool { return &value }

func writeChatHistoryJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}

func TestNormalizeChatHistoryLimit(t *testing.T) {
	tests := []struct {
		in   int
		want int
	}{
		{in: 0, want: defaultChatHistoryLimit},
		{in: -1, want: defaultChatHistoryLimit},
		{in: 1, want: 1},
		{in: 100, want: 100},
		{in: 101, want: 100},
	}
	for _, tc := range tests {
		t.Run(fmt.Sprintf("%d", tc.in), func(t *testing.T) {
			if got := normalizeChatHistoryLimit(tc.in); got != tc.want {
				t.Fatalf("normalizeChatHistoryLimit(%d) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}
