package monitor

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	lark "github.com/larksuite/oapi-sdk-go/v3"
)

func TestMarshalRichTextContentBuildsMarkdownPostContent(t *testing.T) {
	body, err := marshalRichTextContent("hello\n\nworld")
	if err != nil {
		t.Fatalf("marshalRichTextContent returned error: %v", err)
	}

	var content richTextContent
	if err := json.Unmarshal([]byte(body), &content); err != nil {
		t.Fatalf("unmarshal rich text content: %v", err)
	}

	assertMarkdownContent(t, content, "hello\n\nworld")
}

func TestBuildRichTextContentNormalizesCRLF(t *testing.T) {
	content := buildRichTextContent("one\r\ntwo\rthree")

	assertMarkdownContent(t, content, "one\ntwo\nthree")
}

func TestBuildRichTextContentUsesSpaceForEmptyText(t *testing.T) {
	content := buildRichTextContent("")

	assertMarkdownContent(t, content, " ")
}

func TestSDKSenderSendsPostRichTextMessage(t *testing.T) {
	var messageRequest struct {
		ReceiveID string `json:"receive_id"`
		MsgType   string `json:"msg_type"`
		Content   string `json:"content"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/v3/token":
			writeJSON(t, w, map[string]any{
				"code":                0,
				"msg":                 "ok",
				"tenant_access_token": "tenant-token",
				"expire":              7200,
			})
		case "/open-apis/auth/v3/tenant_access_token/internal":
			writeJSON(t, w, map[string]any{
				"code":                0,
				"msg":                 "ok",
				"tenant_access_token": "tenant-token",
				"expire":              7200,
			})
		case "/open-apis/im/v1/messages":
			if r.URL.Query().Get("receive_id_type") != "chat_id" {
				t.Fatalf("receive_id_type = %q, want chat_id", r.URL.Query().Get("receive_id_type"))
			}
			if err := json.NewDecoder(r.Body).Decode(&messageRequest); err != nil {
				t.Fatalf("decode message request: %v", err)
			}
			writeJSON(t, w, map[string]any{
				"code": 0,
				"msg":  "ok",
				"data": map[string]any{},
			})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client := lark.NewClient("cli_xxx", "secret",
		lark.WithOpenBaseUrl(server.URL),
		lark.WithOAuthBaseUrl(server.URL),
		lark.WithHttpClient(server.Client()),
	)
	sender := &sdkSender{client: client}
	if err := sender.SendText(t.Context(), "oc_chat", "hello\nworld"); err != nil {
		t.Fatalf("SendText returned error: %v", err)
	}

	if messageRequest.ReceiveID != "oc_chat" {
		t.Fatalf("receive_id = %q, want oc_chat", messageRequest.ReceiveID)
	}
	if messageRequest.MsgType != "post" {
		t.Fatalf("msg_type = %q, want post", messageRequest.MsgType)
	}
	var content richTextContent
	if err := json.Unmarshal([]byte(messageRequest.Content), &content); err != nil {
		t.Fatalf("unmarshal request content: %v", err)
	}
	assertMarkdownContent(t, content, "hello\nworld")
}

func TestSDKSenderCreatesTextAndReturnsMessageID(t *testing.T) {
	var messageRequest struct {
		ReceiveID string `json:"receive_id"`
		MsgType   string `json:"msg_type"`
		Content   string `json:"content"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/v3/token":
			writeJSON(t, w, map[string]any{
				"code":                0,
				"msg":                 "ok",
				"tenant_access_token": "tenant-token",
				"expire":              7200,
			})
		case "/open-apis/auth/v3/tenant_access_token/internal":
			writeJSON(t, w, map[string]any{
				"code":                0,
				"msg":                 "ok",
				"tenant_access_token": "tenant-token",
				"expire":              7200,
			})
		case "/open-apis/im/v1/messages":
			if r.Method != http.MethodPost {
				t.Fatalf("method = %s, want POST", r.Method)
			}
			if r.URL.Query().Get("receive_id_type") != "chat_id" {
				t.Fatalf("receive_id_type = %q, want chat_id", r.URL.Query().Get("receive_id_type"))
			}
			if err := json.NewDecoder(r.Body).Decode(&messageRequest); err != nil {
				t.Fatalf("decode message request: %v", err)
			}
			writeJSON(t, w, map[string]any{
				"code": 0,
				"msg":  "ok",
				"data": map[string]any{"message_id": "om_stream"},
			})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client := lark.NewClient("cli_xxx", "secret",
		lark.WithOpenBaseUrl(server.URL),
		lark.WithOAuthBaseUrl(server.URL),
		lark.WithHttpClient(server.Client()),
	)
	sender := &sdkSender{client: client}
	messageID, err := sender.CreateText(t.Context(), "oc_chat", "hello")
	if err != nil {
		t.Fatalf("CreateText returned error: %v", err)
	}
	if messageID != "om_stream" {
		t.Fatalf("messageID = %q, want om_stream", messageID)
	}
	if messageRequest.ReceiveID != "oc_chat" || messageRequest.MsgType != "post" {
		t.Fatalf("request = %#v, want post to oc_chat", messageRequest)
	}
}

func TestSDKSenderCreatesInteractiveCardAndReturnsMessageID(t *testing.T) {
	var messageRequest struct {
		ReceiveID string `json:"receive_id"`
		MsgType   string `json:"msg_type"`
		Content   string `json:"content"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/v3/token", "/open-apis/auth/v3/tenant_access_token/internal":
			writeJSON(t, w, map[string]any{
				"code":                0,
				"msg":                 "ok",
				"tenant_access_token": "tenant-token",
				"expire":              7200,
			})
		case "/open-apis/im/v1/messages":
			if r.URL.Query().Get("receive_id_type") != "chat_id" {
				t.Fatalf("receive_id_type = %q, want chat_id", r.URL.Query().Get("receive_id_type"))
			}
			if err := json.NewDecoder(r.Body).Decode(&messageRequest); err != nil {
				t.Fatalf("decode message request: %v", err)
			}
			writeJSON(t, w, map[string]any{
				"code": 0,
				"msg":  "ok",
				"data": map[string]any{"message_id": "om_card"},
			})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client := lark.NewClient("cli_xxx", "secret",
		lark.WithOpenBaseUrl(server.URL),
		lark.WithOAuthBaseUrl(server.URL),
		lark.WithHttpClient(server.Client()),
	)
	sender := &sdkSender{client: client}
	card := `{"schema":"2.0","body":{"elements":[]}}`
	messageID, err := sender.CreateCard(t.Context(), "oc_chat", card)
	if err != nil {
		t.Fatalf("CreateCard returned error: %v", err)
	}
	if messageID != "om_card" {
		t.Fatalf("messageID = %q, want om_card", messageID)
	}
	if messageRequest.ReceiveID != "oc_chat" || messageRequest.MsgType != "interactive" || messageRequest.Content != card {
		t.Fatalf("message request = %#v, want interactive card", messageRequest)
	}
}

func TestSDKSenderRejectsInvalidInteractiveCard(t *testing.T) {
	sender := &sdkSender{}
	if _, err := sender.CreateCard(t.Context(), "oc_chat", "not-json"); err == nil {
		t.Fatal("CreateCard returned nil error for invalid JSON")
	}
	if _, err := sender.CreateCard(t.Context(), "oc_chat", "[]"); err == nil {
		t.Fatal("CreateCard returned nil error for a non-object card")
	}
	if err := sender.UpdateCard(t.Context(), "om_card", ""); err == nil {
		t.Fatal("UpdateCard returned nil error for empty JSON")
	}
}

func TestSDKSenderCreatesReplyTextAndReturnsMessageID(t *testing.T) {
	var replyRequest struct {
		MsgType       string `json:"msg_type"`
		Content       string `json:"content"`
		ReplyInThread bool   `json:"reply_in_thread"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/v3/token":
			writeJSON(t, w, map[string]any{
				"code":                0,
				"msg":                 "ok",
				"tenant_access_token": "tenant-token",
				"expire":              7200,
			})
		case "/open-apis/auth/v3/tenant_access_token/internal":
			writeJSON(t, w, map[string]any{
				"code":                0,
				"msg":                 "ok",
				"tenant_access_token": "tenant-token",
				"expire":              7200,
			})
		case "/open-apis/im/v1/messages/om_original/reply":
			if r.Method != http.MethodPost {
				t.Fatalf("method = %s, want POST", r.Method)
			}
			if err := json.NewDecoder(r.Body).Decode(&replyRequest); err != nil {
				t.Fatalf("decode reply request: %v", err)
			}
			writeJSON(t, w, map[string]any{
				"code": 0,
				"msg":  "ok",
				"data": map[string]any{"message_id": "om_reply"},
			})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client := lark.NewClient("cli_xxx", "secret",
		lark.WithOpenBaseUrl(server.URL),
		lark.WithOAuthBaseUrl(server.URL),
		lark.WithHttpClient(server.Client()),
	)
	sender := &sdkSender{client: client}
	messageID, err := sender.CreateReplyText(t.Context(), "om_original", "hello\nworld")
	if err != nil {
		t.Fatalf("CreateReplyText returned error: %v", err)
	}
	if messageID != "om_reply" {
		t.Fatalf("messageID = %q, want om_reply", messageID)
	}
	if replyRequest.MsgType != "post" {
		t.Fatalf("msg_type = %q, want post", replyRequest.MsgType)
	}
	if replyRequest.ReplyInThread {
		t.Fatal("reply_in_thread = true, want false")
	}
	var content richTextContent
	if err := json.Unmarshal([]byte(replyRequest.Content), &content); err != nil {
		t.Fatalf("unmarshal reply content: %v", err)
	}
	assertMarkdownContent(t, content, "hello\nworld")
}

func TestSDKSenderCreateReplyTextRequiresMessageID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/v3/token":
			writeJSON(t, w, map[string]any{
				"code":                0,
				"msg":                 "ok",
				"tenant_access_token": "tenant-token",
				"expire":              7200,
			})
		case "/open-apis/auth/v3/tenant_access_token/internal":
			writeJSON(t, w, map[string]any{
				"code":                0,
				"msg":                 "ok",
				"tenant_access_token": "tenant-token",
				"expire":              7200,
			})
		case "/open-apis/im/v1/messages/om_original/reply":
			writeJSON(t, w, map[string]any{
				"code": 0,
				"msg":  "ok",
				"data": map[string]any{},
			})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client := lark.NewClient("cli_xxx", "secret",
		lark.WithOpenBaseUrl(server.URL),
		lark.WithOAuthBaseUrl(server.URL),
		lark.WithHttpClient(server.Client()),
	)
	sender := &sdkSender{client: client}
	if _, err := sender.CreateReplyText(t.Context(), "om_original", "hello"); err == nil || !strings.Contains(err.Error(), "missing message_id") {
		t.Fatalf("CreateReplyText error = %v, want missing message_id", err)
	}
}

func TestSDKSenderUpdatesPostRichTextMessage(t *testing.T) {
	var updateRequest struct {
		MsgType string `json:"msg_type"`
		Content string `json:"content"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/v3/token":
			writeJSON(t, w, map[string]any{
				"code":                0,
				"msg":                 "ok",
				"tenant_access_token": "tenant-token",
				"expire":              7200,
			})
		case "/open-apis/auth/v3/tenant_access_token/internal":
			writeJSON(t, w, map[string]any{
				"code":                0,
				"msg":                 "ok",
				"tenant_access_token": "tenant-token",
				"expire":              7200,
			})
		case "/open-apis/im/v1/messages/om_stream":
			if r.Method != http.MethodPut {
				t.Fatalf("method = %s, want PUT", r.Method)
			}
			if err := json.NewDecoder(r.Body).Decode(&updateRequest); err != nil {
				t.Fatalf("decode update request: %v", err)
			}
			writeJSON(t, w, map[string]any{
				"code": 0,
				"msg":  "ok",
				"data": map[string]any{"message_id": "om_stream"},
			})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client := lark.NewClient("cli_xxx", "secret",
		lark.WithOpenBaseUrl(server.URL),
		lark.WithOAuthBaseUrl(server.URL),
		lark.WithHttpClient(server.Client()),
	)
	sender := &sdkSender{client: client}
	if err := sender.UpdateText(t.Context(), "om_stream", "hello\nworld"); err != nil {
		t.Fatalf("UpdateText returned error: %v", err)
	}
	if updateRequest.MsgType != "post" {
		t.Fatalf("msg_type = %q, want post", updateRequest.MsgType)
	}
	var content richTextContent
	if err := json.Unmarshal([]byte(updateRequest.Content), &content); err != nil {
		t.Fatalf("unmarshal update content: %v", err)
	}
	assertMarkdownContent(t, content, "hello\nworld")
}

func TestSDKSenderUpdatesInteractiveCard(t *testing.T) {
	var updateRequest struct {
		Content string `json:"content"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/v3/token", "/open-apis/auth/v3/tenant_access_token/internal":
			writeJSON(t, w, map[string]any{
				"code":                0,
				"msg":                 "ok",
				"tenant_access_token": "tenant-token",
				"expire":              7200,
			})
		case "/open-apis/im/v1/messages/om_card":
			if r.Method != http.MethodPatch {
				t.Fatalf("method = %s, want PATCH", r.Method)
			}
			if err := json.NewDecoder(r.Body).Decode(&updateRequest); err != nil {
				t.Fatalf("decode update request: %v", err)
			}
			writeJSON(t, w, map[string]any{
				"code": 0,
				"msg":  "ok",
				"data": map[string]any{"message_id": "om_card"},
			})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client := lark.NewClient("cli_xxx", "secret",
		lark.WithOpenBaseUrl(server.URL),
		lark.WithOAuthBaseUrl(server.URL),
		lark.WithHttpClient(server.Client()),
	)
	sender := &sdkSender{client: client}
	card := `{"schema":"2.0","body":{"elements":[]}}`
	if err := sender.UpdateCard(t.Context(), "om_card", card); err != nil {
		t.Fatalf("UpdateCard returned error: %v", err)
	}
	if updateRequest.Content != card {
		t.Fatalf("update request = %#v, want message-card content", updateRequest)
	}
}

func TestSDKSenderDelayUpdatesInteractiveCardAfterCallback(t *testing.T) {
	var updateRequest struct {
		Token string                 `json:"token"`
		Card  map[string]interface{} `json:"card"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/v3/token", "/open-apis/auth/v3/tenant_access_token/internal":
			writeJSON(t, w, map[string]any{
				"code":                0,
				"msg":                 "ok",
				"tenant_access_token": "tenant-token",
				"expire":              7200,
			})
		case "/open-apis/interactive/v1/card/update":
			if r.Method != http.MethodPost {
				t.Fatalf("method = %s, want POST", r.Method)
			}
			if err := json.NewDecoder(r.Body).Decode(&updateRequest); err != nil {
				t.Fatalf("decode delayed card update request: %v", err)
			}
			writeJSON(t, w, map[string]any{"code": 0, "msg": "ok"})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client := lark.NewClient("cli_xxx", "secret",
		lark.WithOpenBaseUrl(server.URL),
		lark.WithOAuthBaseUrl(server.URL),
		lark.WithHttpClient(server.Client()),
	)
	sender := &sdkSender{client: client}
	card := `{"schema":"2.0","config":{"update_multi":true},"body":{"elements":[]}}`
	if err := sender.UpdateCardAfterInteraction(t.Context(), "c_callback", card); err != nil {
		t.Fatalf("UpdateCardAfterInteraction returned error: %v", err)
	}
	if updateRequest.Token != "c_callback" || updateRequest.Card["schema"] != "2.0" {
		t.Fatalf("delayed update request = %#v, want callback token and raw Card V2 object", updateRequest)
	}
}

func TestSDKSenderUpdateTextRecognizesEditLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/v3/token":
			writeJSON(t, w, map[string]any{
				"code":                0,
				"msg":                 "ok",
				"tenant_access_token": "tenant-token",
				"expire":              7200,
			})
		case "/open-apis/auth/v3/tenant_access_token/internal":
			writeJSON(t, w, map[string]any{
				"code":                0,
				"msg":                 "ok",
				"tenant_access_token": "tenant-token",
				"expire":              7200,
			})
		case "/open-apis/im/v1/messages/om_stream":
			writeJSON(t, w, map[string]any{
				"code": 230072,
				"msg":  "The message has reached the number of times it can be edited.",
			})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client := lark.NewClient("cli_xxx", "secret",
		lark.WithOpenBaseUrl(server.URL),
		lark.WithOAuthBaseUrl(server.URL),
		lark.WithHttpClient(server.Client()),
	)
	sender := &sdkSender{client: client}
	err := sender.UpdateText(t.Context(), "om_stream", "hello")
	if !errors.Is(err, ErrFeishuMessageEditLimit) {
		t.Fatalf("UpdateText error = %v, want ErrFeishuMessageEditLimit", err)
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, body any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}

func assertMarkdownContent(t *testing.T, content richTextContent, want string) {
	t.Helper()
	lines := content.ZhCN.Content
	if len(lines) != 1 {
		t.Fatalf("content lines = %d, want 1", len(lines))
	}
	if len(lines[0]) != 1 {
		t.Fatalf("content elements = %d, want 1", len(lines[0]))
	}
	element := lines[0][0]
	if element.Tag != "md" || element.Text != want {
		t.Fatalf("element = %#v, want md %q", element, want)
	}
}
