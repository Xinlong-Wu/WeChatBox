package tools

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"

	"lingobridge/internal/store"
	tooltypes "lingobridge/internal/tools"
)

type fakeApprovalRequester struct {
	request     ApprovalRequest
	pending     PendingApproval
	err         error
	active      bool
	activeErr   error
	checkedTool string
	checks      int
}

func (f *fakeApprovalRequester) HasActiveGrant(_ context.Context, toolName string) (bool, error) {
	f.checkedTool = toolName
	f.checks++
	return f.active, f.activeErr
}

func (f *fakeApprovalRequester) RequestApproval(_ context.Context, request ApprovalRequest) (PendingApproval, error) {
	f.request = request
	return f.pending, f.err
}

func TestDocsToolConfigDefaultsAndRegistration(t *testing.T) {
	cfg := NormalizeConfig(Config{})
	if cfg.Docs.Enabled {
		t.Fatal("docs tools enabled by default, want disabled")
	}
	if cfg.Docs.AllowWrite {
		t.Fatal("docs tools allow_write = true by default, want false")
	}
	if cfg.MaxResults != DefaultMaxResults || cfg.MaxChars != DefaultMaxChars {
		t.Fatalf("defaults = %#v, want max defaults", cfg)
	}

	client := &lark.Client{}
	st := openDocsFolderTestStore(t)
	if got := NewDocsTools(client, st, "feishu:cli_test", cfg, nil, nil); len(got) != 0 {
		t.Fatalf("disabled tools = %d, want 0", len(got))
	}
	cfg.Docs.Enabled = true
	if got := NewDocsTools(client, st, "feishu:cli_test", cfg, nil, nil); len(got) != 2 {
		t.Fatalf("read-only tools = %d, want search/read", len(got))
	}
	cfg.Docs.AllowWrite = true
	if got := NewDocsTools(client, st, "feishu:cli_test", cfg, nil, nil); len(got) != 3 {
		t.Fatalf("write tools without approval workflow = %d, want search/read/append", len(got))
	}
	if got := NewDocsTools(client, st, "feishu:cli_test", cfg, &fakeApprovalRequester{}, nil); len(got) != 3 {
		t.Fatalf("write tools without resource access workflow = %d, want search/read/append", len(got))
	}
	if got := NewDocsTools(client, st, "feishu:cli_test", cfg, &fakeApprovalRequester{}, grantedResourceAccessController("req_access")); len(got) != 4 {
		t.Fatalf("write tools with approval workflow = %d, want four tools", len(got))
	}
}

func TestDocsReadAndAppendExternalDocumentUseResourceAccessWithoutBinding(t *testing.T) {
	const documentToken = "doxcnexternal12345"
	var readCalls, appendCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/oauth/v3/token" || r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal":
			writeJSON(t, w, map[string]any{
				"code":                0,
				"msg":                 "ok",
				"tenant_access_token": "tenant-token",
				"expire":              7200,
			})
		case r.URL.Path == "/open-apis/docx/v1/documents/"+documentToken+"/raw_content":
			readCalls++
			writeJSON(t, w, map[string]any{"code": 0, "msg": "ok", "data": map[string]any{"content": "external text"}})
		case r.URL.Path == "/open-apis/docx/v1/documents/"+documentToken+"/blocks/"+documentToken+"/children" && r.Method == http.MethodGet:
			writeJSON(t, w, map[string]any{"code": 0, "msg": "ok", "data": map[string]any{"items": []any{}, "has_more": false}})
		case r.URL.Path == "/open-apis/docx/v1/documents/"+documentToken+"/blocks/"+documentToken+"/children" && r.Method == http.MethodPost:
			appendCalls++
			writeJSON(t, w, map[string]any{"code": 0, "msg": "ok", "data": map[string]any{"children": []any{}}})
		default:
			t.Fatalf("unexpected path: %s method=%s", r.URL.Path, r.Method)
		}
	}))
	defer server.Close()

	client := lark.NewClient("cli_xxx", "secret",
		lark.WithOpenBaseUrl(server.URL),
		lark.WithOAuthBaseUrl(server.URL),
		lark.WithHttpClient(server.Client()),
	)
	cfg := Config{Docs: DocsToolsConfig{Enabled: true, AllowWrite: true}}
	st, tools, access := newDocsToolsForTest(t, client, cfg, &fakeApprovalRequester{})
	readTool := findDocsTool(t, tools, readToolName)
	missing := readTool.Execute(groupDocsContext(), tooltypes.Call{
		ID:        "read_missing",
		Name:      readToolName,
		Arguments: json.RawMessage(`{"token":"doxcnexternal12345"}`),
	})
	if !missing.IsError || !strings.Contains(missing.Content, "access_request_id is required") || readCalls != 0 {
		t.Fatalf("missing external read result=%#v read_calls=%d", missing, readCalls)
	}
	read := readTool.Execute(groupDocsContext(), tooltypes.Call{
		ID:        "read_external",
		Name:      readToolName,
		Arguments: json.RawMessage(`{"token":"doxcnexternal12345","access_request_id":"req_access"}`),
	})
	if read.IsError || !strings.Contains(read.Content, "external text") || readCalls != 1 {
		t.Fatalf("external read result=%#v read_calls=%d", read, readCalls)
	}
	if access.validation.RequestID != "req_access" || access.validation.ResourceType != "docx" || access.validation.ResourceToken != documentToken || access.validation.Permission != ResourcePermissionRead {
		t.Fatalf("external read validation = %#v", access.validation)
	}

	appendTool := findDocsTool(t, tools, appendToolName)
	append := appendTool.Execute(groupDocsContext(), tooltypes.Call{
		ID:        "append_external",
		Name:      appendToolName,
		Arguments: json.RawMessage(`{"token":"doxcnexternal12345","content":"new paragraph","access_request_id":"req_access"}`),
	})
	if append.IsError || !strings.Contains(append.Content, `"appended":true`) || appendCalls != 1 {
		t.Fatalf("external append result=%#v append_calls=%d", append, appendCalls)
	}
	if access.validation.RequestID != "req_access" || access.validation.ResourceToken != documentToken || access.validation.Permission != ResourcePermissionWrite {
		t.Fatalf("external append validation = %#v", access.validation)
	}
	if _, err := st.GetFeishuChatDocument("feishu:cli_test", "oc_chat", documentToken); !errors.Is(err, store.ErrFeishuChatDocumentNotFound) {
		t.Fatalf("external document binding error = %v, want not found", err)
	}
}

func TestDocsReadRejectsBotOwnedDocumentFromAnotherChat(t *testing.T) {
	cfg := Config{Docs: DocsToolsConfig{Enabled: true}}
	st, tools, access := newDocsToolsForTest(t, &lark.Client{}, cfg, nil)
	now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	if _, err := st.SaveFeishuBotResource(store.FeishuBotResource{
		AccountID:       "feishu:cli_test",
		ResourceType:    "docx",
		ResourceToken:   "doxcnotherchat123",
		ParentToken:     "fld_other_chat",
		Name:            "Other chat",
		SourceRequestID: "req_other",
		CreatedAt:       now,
	}); err != nil {
		t.Fatalf("SaveFeishuBotResource returned error: %v", err)
	}
	result := findDocsTool(t, tools, readToolName).Execute(groupDocsContext(), tooltypes.Call{
		ID:        "read_cross_chat",
		Name:      readToolName,
		Arguments: json.RawMessage(`{"token":"doxcnotherchat123","access_request_id":"req_access"}`),
	})
	if !result.IsError || !strings.Contains(result.Content, "not available to the current Feishu chat") {
		t.Fatalf("cross-chat read result = %#v", result)
	}
	if access.validation.RequestID != "" {
		t.Fatalf("cross-chat Bot document unexpectedly used external access: %#v", access.validation)
	}
}

func TestDocsReadRepairsBotOwnedDocumentBindingInCurrentChat(t *testing.T) {
	const documentToken = "doxcnrepair12345"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/v3/token", "/open-apis/auth/v3/tenant_access_token/internal":
			writeJSON(t, w, map[string]any{
				"code":                0,
				"msg":                 "ok",
				"tenant_access_token": "tenant-token",
				"expire":              7200,
			})
		case "/open-apis/docx/v1/documents/" + documentToken + "/raw_content":
			writeJSON(t, w, map[string]any{"code": 0, "msg": "ok", "data": map[string]any{"content": "repaired"}})
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
	st, tools, access := newDocsToolsForTest(t, client, Config{Docs: DocsToolsConfig{Enabled: true}}, nil)
	now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	if _, err := st.SaveFeishuBotResource(store.FeishuBotResource{
		AccountID:       "feishu:cli_test",
		ResourceType:    "docx",
		ResourceToken:   documentToken,
		ParentToken:     "fld_token",
		Name:            "Repair me",
		URL:             "https://docs.feishu.cn/docx/" + documentToken,
		SourceRequestID: "req_created",
		CreatedAt:       now,
	}); err != nil {
		t.Fatalf("SaveFeishuBotResource returned error: %v", err)
	}
	result := findDocsTool(t, tools, readToolName).Execute(groupDocsContext(), tooltypes.Call{
		ID:        "read_repair",
		Name:      readToolName,
		Arguments: json.RawMessage(`{"token":"doxcnrepair12345"}`),
	})
	if result.IsError || !strings.Contains(result.Content, "repaired") {
		t.Fatalf("repair read result = %#v", result)
	}
	document, err := st.GetFeishuChatDocument("feishu:cli_test", "oc_chat", documentToken)
	if err != nil || document.FolderToken != "fld_token" || document.SourceRequestID != "req_created" {
		t.Fatalf("repaired document binding = %#v err=%v", document, err)
	}
	if access.validation.RequestID != "" {
		t.Fatalf("Bot-owned repair unexpectedly used external access: %#v", access.validation)
	}
}

func TestDocsCreateToolReturnsPendingApprovalWithoutCallingFeishuDocs(t *testing.T) {
	expiresAt := time.Date(2026, time.August, 1, 12, 10, 0, 0, time.UTC)
	approver := &fakeApprovalRequester{pending: PendingApproval{RequestID: "req_123", ExpiresAt: expiresAt}}
	cfg := Config{Docs: DocsToolsConfig{Enabled: true, AllowWrite: true}}
	_, tools, _ := newDocsToolsForTest(t, &lark.Client{}, cfg, approver)
	tool := findDocsTool(t, tools, createToolName)
	result := tool.Execute(groupDocsContext(), tooltypes.Call{
		ID:        "call_1",
		Name:      createToolName,
		Arguments: json.RawMessage(`{"title":" Quarterly plan ","content":"private body","folder_token":" fld_token ","access_request_id":"req_access"}`),
	})
	if result.IsError {
		t.Fatalf("Execute result = %#v, want pending approval success", result)
	}
	var output pendingApprovalOutput
	if err := json.Unmarshal([]byte(result.Content), &output); err != nil {
		t.Fatalf("unmarshal pending output: %v", err)
	}
	if output.Status != "pending_approval" || output.RequestID != "req_123" || output.ExpiresAt != expiresAt.Format(time.RFC3339) || !strings.Contains(output.Message, "24 小时") || !strings.Contains(output.Message, "请勿重复调用") {
		t.Fatalf("pending output = %#v", output)
	}
	if approver.checks != 1 || approver.checkedTool != createToolName {
		t.Fatalf("approval grant checks = %d tool=%q, want one create check", approver.checks, approver.checkedTool)
	}
	if approver.request.ToolName != createToolName || approver.request.Action != "创建飞书文档" {
		t.Fatalf("approval request = %#v", approver.request)
	}
	var approvedArgs approvedCreatePayload
	if err := json.Unmarshal(approver.request.Payload, &approvedArgs); err != nil {
		t.Fatalf("unmarshal approval payload: %v", err)
	}
	if approvedArgs.Title != "Quarterly plan" || approvedArgs.FolderToken != "fld_token" || approvedArgs.Content != "private body" || approvedArgs.AccessRequestID != "req_access" || approvedArgs.ChatID != "oc_chat" {
		t.Fatalf("approved args = %#v, want normalized exact payload", approvedArgs)
	}
	for _, field := range approver.request.Fields {
		if strings.Contains(field.Value, "private body") {
			t.Fatalf("approval field leaked document content: %#v", field)
		}
	}
}

func TestDocsCreateToolUsesActiveGrantWithoutSendingCard(t *testing.T) {
	var createCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/v3/token", "/open-apis/auth/v3/tenant_access_token/internal":
			writeJSON(t, w, map[string]any{
				"code":                0,
				"msg":                 "ok",
				"tenant_access_token": "tenant-token",
				"expire":              7200,
			})
		case "/open-apis/docx/v1/documents":
			createCalls++
			writeJSON(t, w, map[string]any{
				"code": 0,
				"msg":  "ok",
				"data": map[string]any{"document": map[string]any{"document_id": "doxcnactive123"}},
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
	approver := &fakeApprovalRequester{active: true}
	cfg := Config{Docs: DocsToolsConfig{Enabled: true, AllowWrite: true}}
	st, tools, access := newDocsToolsForTest(t, client, cfg, approver)
	tool := findDocsTool(t, tools, createToolName)
	result := tool.Execute(groupDocsContext(), tooltypes.Call{
		ID:        "call_1",
		Name:      createToolName,
		Arguments: json.RawMessage(`{"title":"Quarterly plan","folder_token":"fld_token","access_request_id":"req_access"}`),
	})
	if result.IsError {
		t.Fatalf("Execute result = %#v, want direct create", result)
	}
	var output writeOutput
	if err := json.Unmarshal([]byte(result.Content), &output); err != nil {
		t.Fatalf("unmarshal create output: %v", err)
	}
	if output.DocumentID != "doxcnactive123" || output.Warning != "" || createCalls != 1 {
		t.Fatalf("create output/calls = %#v/%d", output, createCalls)
	}
	if access.consumption.RequestID != "req_access" || access.consumption.ResourceToken != "fld_token" || access.consumedBy != output.RequestID {
		t.Fatalf("access consumption=%#v consumed_by=%q", access.consumption, access.consumedBy)
	}
	if approver.request.ToolName != "" {
		t.Fatalf("approval request = %#v, want no card for active grant", approver.request)
	}
	if _, err := st.GetFeishuChatDocument("feishu:cli_test", "oc_chat", output.DocumentID); err != nil {
		t.Fatalf("created document was not bound to chat: %v", err)
	}
	resource, err := st.GetFeishuBotResource("feishu:cli_test", "docx", output.DocumentID)
	if err != nil || resource.ParentToken != "fld_token" || resource.SourceRequestID != output.RequestID {
		t.Fatalf("stored Bot document resource = %#v err=%v", resource, err)
	}
}

func TestDocsCreateToolActiveGrantReportsPartialSuccessWithoutRetrySignal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/oauth/v3/token" || r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal":
			writeJSON(t, w, map[string]any{
				"code":                0,
				"msg":                 "ok",
				"tenant_access_token": "tenant-token",
				"expire":              7200,
			})
		case r.URL.Path == "/open-apis/docx/v1/documents":
			writeJSON(t, w, map[string]any{
				"code": 0,
				"msg":  "ok",
				"data": map[string]any{"document": map[string]any{"document_id": "doxcnactive123"}},
			})
		case strings.Contains(r.URL.Path, "/open-apis/docx/v1/documents/doxcnactive123/blocks/"):
			writeJSON(t, w, map[string]any{"code": 99991672, "msg": "append denied"})
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
	cfg := Config{Docs: DocsToolsConfig{Enabled: true, AllowWrite: true}}
	st, tools, _ := newDocsToolsForTest(t, client, cfg, &fakeApprovalRequester{active: true})
	tool := findDocsTool(t, tools, createToolName)
	result := tool.Execute(groupDocsContext(), tooltypes.Call{
		ID:        "call_1",
		Name:      createToolName,
		Arguments: json.RawMessage(`{"title":"Quarterly plan","content":"body","folder_token":"fld_token","access_request_id":"req_access"}`),
	})
	if result.IsError {
		t.Fatalf("Execute result = %#v, want partial success", result)
	}
	var output writeOutput
	if err := json.Unmarshal([]byte(result.Content), &output); err != nil {
		t.Fatalf("unmarshal partial create output: %v", err)
	}
	if output.DocumentID != "doxcnactive123" || !strings.Contains(output.Warning, "请勿重复创建") || !strings.Contains(output.Warning, "append denied") {
		t.Fatalf("partial create output = %#v", output)
	}
	if _, err := st.GetFeishuChatDocument("feishu:cli_test", "oc_chat", output.DocumentID); err != nil {
		t.Fatalf("partially created document was not recoverably bound to chat: %v", err)
	}
}

func TestDocsCreateToolRejectsTitleLongerThanFeishuLimit(t *testing.T) {
	approver := &fakeApprovalRequester{}
	cfg := Config{Docs: DocsToolsConfig{Enabled: true, AllowWrite: true}}
	_, tools, _ := newDocsToolsForTest(t, &lark.Client{}, cfg, approver)
	tool := findDocsTool(t, tools, createToolName)
	args, err := json.Marshal(createArgs{
		Title:           strings.Repeat("文", maxDocxTitle+1),
		FolderToken:     "fld_token",
		AccessRequestID: "req_access",
	})
	if err != nil {
		t.Fatalf("marshal create args: %v", err)
	}
	result := tool.Execute(groupDocsContext(), tooltypes.Call{ID: "call_1", Name: createToolName, Arguments: args})
	if !result.IsError || !strings.Contains(result.Content, "must not exceed 800 characters") {
		t.Fatalf("Execute result = %#v, want official title-length validation error", result)
	}
	if approver.request.ToolName != "" {
		t.Fatalf("approval request = %#v, want no card for invalid title", approver.request)
	}
	if approver.checks != 0 {
		t.Fatalf("approval grant checks = %d, want validation before lookup", approver.checks)
	}
}

func TestDocsCreateApprovedExecutionCreatesDocument(t *testing.T) {
	var createRequest struct {
		Title       string `json:"title"`
		FolderToken string `json:"folder_token"`
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
		case "/open-apis/docx/v1/documents":
			if r.Method != http.MethodPost {
				t.Fatalf("method = %s, want POST", r.Method)
			}
			if err := json.NewDecoder(r.Body).Decode(&createRequest); err != nil {
				t.Fatalf("decode create document request: %v", err)
			}
			writeJSON(t, w, map[string]any{
				"code": 0,
				"msg":  "ok",
				"data": map[string]any{"document": map[string]any{"document_id": "doxcn12345678"}},
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
	cfg := Config{Docs: DocsToolsConfig{Enabled: true, AllowWrite: true}}
	_, tools, access := newDocsToolsForTest(t, client, cfg, &fakeApprovalRequester{})
	tool := findDocsTool(t, tools, createToolName)
	executor, ok := tool.(ApprovalExecutor)
	if !ok {
		t.Fatalf("create tool %T does not implement ApprovalExecutor", tool)
	}
	result, err := executor.ExecuteApproved(context.Background(), "req_approved", json.RawMessage(`{"title":"Quarterly plan","folder_token":"fld_token","access_request_id":"req_access","chat_id":"oc_chat","actor_open_id":"ou_requester"}`))
	if err != nil {
		t.Fatalf("ExecuteApproved returned error: %v", err)
	}
	if result.Warning || !strings.Contains(result.Message, "https://docs.feishu.cn/docx/doxcn12345678") {
		t.Fatalf("approved result = %#v, want created document link", result)
	}
	if createRequest.Title != "Quarterly plan" || createRequest.FolderToken != "fld_token" {
		t.Fatalf("create request = %#v", createRequest)
	}
	if access.validation.RequestID != "req_access" || access.validation.ResourceToken != "fld_token" || access.actor.OpenID != "ou_requester" || access.chat.ChatID != "oc_chat" {
		t.Fatalf("approved access validation=%#v actor=%#v chat=%#v", access.validation, access.actor, access.chat)
	}
	if access.consumption.RequestID != "req_access" || access.consumption.ResourceToken != "fld_token" || access.consumedBy != "req_approved" {
		t.Fatalf("approved access consumption=%#v consumed_by=%q", access.consumption, access.consumedBy)
	}
}

func TestDocsCreateApprovedExecutionReportsPartialSuccessWithoutRetryingCreate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/oauth/v3/token" || r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal":
			writeJSON(t, w, map[string]any{
				"code":                0,
				"msg":                 "ok",
				"tenant_access_token": "tenant-token",
				"expire":              7200,
			})
		case r.URL.Path == "/open-apis/docx/v1/documents":
			writeJSON(t, w, map[string]any{
				"code": 0,
				"msg":  "ok",
				"data": map[string]any{"document": map[string]any{"document_id": "doxcn12345678"}},
			})
		case strings.Contains(r.URL.Path, "/open-apis/docx/v1/documents/doxcn12345678/blocks/"):
			writeJSON(t, w, map[string]any{"code": 99991672, "msg": "append denied"})
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
	cfg := Config{Docs: DocsToolsConfig{Enabled: true, AllowWrite: true}}
	_, tools, _ := newDocsToolsForTest(t, client, cfg, &fakeApprovalRequester{})
	tool := findDocsTool(t, tools, createToolName)
	executor := tool.(ApprovalExecutor)
	result, err := executor.ExecuteApproved(context.Background(), "req_approved", json.RawMessage(`{"title":"Quarterly plan","content":"body","folder_token":"fld_token","access_request_id":"req_access","chat_id":"oc_chat","actor_open_id":"ou_requester"}`))
	if err != nil {
		t.Fatalf("ExecuteApproved returned error after document creation: %v", err)
	}
	if !result.Warning || !strings.Contains(result.WarningReason, "append denied") || !strings.Contains(result.Message, "请勿重复创建") || !strings.Contains(result.Message, "doxcn12345678") {
		t.Fatalf("partial result = %#v, want warning with existing document link", result)
	}
}

func TestDocsCreateApprovedExecutionStopsWhenAccessWasRevoked(t *testing.T) {
	cfg := Config{Docs: DocsToolsConfig{Enabled: true, AllowWrite: true}}
	_, tools, access := newDocsToolsForTest(t, &lark.Client{}, cfg, &fakeApprovalRequester{})
	access.err = errors.New("permission revoked")
	executor := findDocsTool(t, tools, createToolName).(ApprovalExecutor)

	_, err := executor.ExecuteApproved(context.Background(), "req_approved", json.RawMessage(`{"title":"Quarterly plan","folder_token":"fld_token","access_request_id":"req_access","chat_id":"oc_chat","actor_open_id":"ou_requester"}`))
	if err == nil || !strings.Contains(err.Error(), "revalidate approved document target access") || !strings.Contains(err.Error(), "permission revoked") {
		t.Fatalf("ExecuteApproved error = %v", err)
	}
}

func TestParseDocRef(t *testing.T) {
	tests := []struct {
		name      string
		token     string
		rawURL    string
		kind      string
		wantKind  string
		wantToken string
	}{
		{name: "direct token", token: "doxcnabcdef123456", wantKind: "docx", wantToken: "doxcnabcdef123456"},
		{name: "docx url", rawURL: "https://docs.feishu.cn/docx/doxcnabcdef123456", wantKind: "docx", wantToken: "doxcnabcdef123456"},
		{name: "wiki url", rawURL: "https://wiki.feishu.cn/wiki/wikcnabcdef123456", wantKind: "wiki", wantToken: "wikcnabcdef123456"},
		{name: "kind alias", token: "doxcnabcdef123456", kind: "docs", wantKind: "docx", wantToken: "doxcnabcdef123456"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseDocRef(tt.token, tt.rawURL, tt.kind)
			if err != nil {
				t.Fatalf("parseDocRef returned error: %v", err)
			}
			if got.Kind != tt.wantKind || got.Token != tt.wantToken {
				t.Fatalf("parseDocRef = %#v, want kind=%s token=%s", got, tt.wantKind, tt.wantToken)
			}
		})
	}
}

func TestTruncateRunes(t *testing.T) {
	got, truncated := truncateRunes("你好世界", 2)
	if got != "你好" || !truncated {
		t.Fatalf("truncateRunes = %q %v, want 你好 true", got, truncated)
	}
	got, truncated = truncateRunes("hello", 10)
	if got != "hello" || truncated {
		t.Fatalf("truncateRunes = %q %v, want hello false", got, truncated)
	}
}

func TestDocsCreateRequiresAndRevalidatesFolderAccess(t *testing.T) {
	approver := &fakeApprovalRequester{}
	cfg := Config{Docs: DocsToolsConfig{Enabled: true, AllowWrite: true}}
	st := openDocsFolderTestStore(t)
	if _, err := st.SaveFeishuChatFolder(store.FeishuChatFolder{
		AccountID:       "feishu:cli_test",
		ChatID:          "oc_chat",
		FolderToken:     "fld_token",
		Name:            "Docs",
		ShareMemberType: "openchat",
		ShareMemberID:   "oc_chat",
		ShareState:      store.FeishuFolderShareStateSucceeded,
		CreateRequestID: "req_folder",
		CreatedAt:       time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed folder: %v", err)
	}
	access := grantedResourceAccessController("req_access")
	tools := NewDocsTools(&lark.Client{}, st, "feishu:cli_test", cfg, approver, access)
	tool := findDocsTool(t, tools, createToolName)

	missing := tool.Execute(groupDocsContext(), tooltypes.Call{ID: "missing", Name: createToolName, Arguments: json.RawMessage(`{"title":"No access","folder_token":"fld_token"}`)})
	if !missing.IsError || !strings.Contains(missing.Content, "access_request_id is required") || approver.checks != 0 {
		t.Fatalf("missing access result=%#v approval_checks=%d", missing, approver.checks)
	}

	result := tool.Execute(groupDocsContext(), tooltypes.Call{ID: "pending", Name: createToolName, Arguments: json.RawMessage(`{"title":"With access","folder_token":"fld_token","access_request_id":"req_access"}`)})
	if result.IsError {
		t.Fatalf("granted access result = %#v", result)
	}
	if access.validation.RequestID != "req_access" || access.validation.ResourceToken != "fld_token" || access.validation.Permission != ResourcePermissionWrite || access.actor.OpenID != "ou_requester" || access.chat.ChatID != "oc_chat" {
		t.Fatalf("access validation=%#v actor=%#v chat=%#v", access.validation, access.actor, access.chat)
	}
}

func findDocsTool(t *testing.T, tools []tooltypes.Tool, name string) tooltypes.Tool {
	t.Helper()
	for _, tool := range tools {
		if tool != nil && tool.Spec().Name == name {
			return tool
		}
	}
	t.Fatalf("tool %q not found", name)
	return nil
}

func newDocsToolsForTest(t *testing.T, client *lark.Client, cfg Config, approver ApprovalRequester) (*store.Store, []tooltypes.Tool, *fakeResourceAccessController) {
	t.Helper()
	st := openDocsFolderTestStore(t)
	if _, err := st.SaveFeishuChatFolder(store.FeishuChatFolder{
		AccountID:       "feishu:cli_test",
		ChatID:          "oc_chat",
		FolderToken:     "fld_token",
		Name:            "Docs",
		ShareMemberType: "openchat",
		ShareMemberID:   "oc_chat",
		ShareState:      store.FeishuFolderShareStateSucceeded,
		CreateRequestID: "req_folder",
		CreatedAt:       time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("seed Feishu chat folder: %v", err)
	}
	access := grantedResourceAccessController("req_access")
	return st, NewDocsTools(client, st, "feishu:cli_test", cfg, approver, access), access
}
