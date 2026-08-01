package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"

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
	if got := NewDocsTools(client, cfg, nil); len(got) != 0 {
		t.Fatalf("disabled tools = %d, want 0", len(got))
	}
	cfg.Docs.Enabled = true
	if got := NewDocsTools(client, cfg, nil); len(got) != 2 {
		t.Fatalf("read-only tools = %d, want search/read", len(got))
	}
	cfg.Docs.AllowWrite = true
	if got := NewDocsTools(client, cfg, nil); len(got) != 2 {
		t.Fatalf("write tools without folder allowlist = %d, want read-only", len(got))
	}
	cfg.AllowedFolderTokens = []string{"fld_token"}
	if got := NewDocsTools(client, cfg, nil); len(got) != 3 {
		t.Fatalf("write tools without approval workflow = %d, want search/read/append", len(got))
	}
	if got := NewDocsTools(client, cfg, &fakeApprovalRequester{}); len(got) != 4 {
		t.Fatalf("write tools with folder allowlist = %d, want four tools", len(got))
	}
}

func TestDocsCreateToolReturnsPendingApprovalWithoutCallingFeishuDocs(t *testing.T) {
	expiresAt := time.Date(2026, time.August, 1, 12, 10, 0, 0, time.UTC)
	approver := &fakeApprovalRequester{pending: PendingApproval{RequestID: "req_123", ExpiresAt: expiresAt}}
	cfg := Config{
		AllowedFolderTokens: []string{"fld_token"},
		Docs:                DocsToolsConfig{Enabled: true, AllowWrite: true},
	}
	tool := findDocsTool(t, NewDocsTools(&lark.Client{}, cfg, approver), createToolName)
	result := tool.Execute(context.Background(), tooltypes.Call{
		ID:        "call_1",
		Name:      createToolName,
		Arguments: json.RawMessage(`{"title":" Quarterly plan ","content":"private body","folder_token":" fld_token "}`),
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
	var approvedArgs createArgs
	if err := json.Unmarshal(approver.request.Payload, &approvedArgs); err != nil {
		t.Fatalf("unmarshal approval payload: %v", err)
	}
	if approvedArgs.Title != "Quarterly plan" || approvedArgs.FolderToken != "fld_token" || approvedArgs.Content != "private body" {
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
	cfg := Config{
		AllowedFolderTokens: []string{"fld_token"},
		Docs:                DocsToolsConfig{Enabled: true, AllowWrite: true},
	}
	tool := findDocsTool(t, NewDocsTools(client, cfg, approver), createToolName)
	result := tool.Execute(context.Background(), tooltypes.Call{
		ID:        "call_1",
		Name:      createToolName,
		Arguments: json.RawMessage(`{"title":"Quarterly plan","folder_token":"fld_token"}`),
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
	if approver.request.ToolName != "" {
		t.Fatalf("approval request = %#v, want no card for active grant", approver.request)
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
	cfg := Config{
		AllowedFolderTokens: []string{"fld_token"},
		Docs:                DocsToolsConfig{Enabled: true, AllowWrite: true},
	}
	tool := findDocsTool(t, NewDocsTools(client, cfg, &fakeApprovalRequester{active: true}), createToolName)
	result := tool.Execute(context.Background(), tooltypes.Call{
		ID:        "call_1",
		Name:      createToolName,
		Arguments: json.RawMessage(`{"title":"Quarterly plan","content":"body","folder_token":"fld_token"}`),
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
}

func TestDocsCreateToolRejectsTitleLongerThanFeishuLimit(t *testing.T) {
	approver := &fakeApprovalRequester{}
	cfg := Config{
		AllowedFolderTokens: []string{"fld_token"},
		Docs:                DocsToolsConfig{Enabled: true, AllowWrite: true},
	}
	tool := findDocsTool(t, NewDocsTools(&lark.Client{}, cfg, approver), createToolName)
	args, err := json.Marshal(createArgs{
		Title:       strings.Repeat("文", maxDocxTitle+1),
		FolderToken: "fld_token",
	})
	if err != nil {
		t.Fatalf("marshal create args: %v", err)
	}
	result := tool.Execute(context.Background(), tooltypes.Call{ID: "call_1", Name: createToolName, Arguments: args})
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
	cfg := Config{
		AllowedFolderTokens: []string{"fld_token"},
		Docs:                DocsToolsConfig{Enabled: true, AllowWrite: true},
	}
	tool := findDocsTool(t, NewDocsTools(client, cfg, &fakeApprovalRequester{}), createToolName)
	executor, ok := tool.(ApprovalExecutor)
	if !ok {
		t.Fatalf("create tool %T does not implement ApprovalExecutor", tool)
	}
	result, err := executor.ExecuteApproved(context.Background(), json.RawMessage(`{"title":"Quarterly plan","folder_token":"fld_token"}`))
	if err != nil {
		t.Fatalf("ExecuteApproved returned error: %v", err)
	}
	if result.Warning || !strings.Contains(result.Message, "https://docs.feishu.cn/docx/doxcn12345678") {
		t.Fatalf("approved result = %#v, want created document link", result)
	}
	if createRequest.Title != "Quarterly plan" || createRequest.FolderToken != "fld_token" {
		t.Fatalf("create request = %#v", createRequest)
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
	cfg := Config{
		AllowedFolderTokens: []string{"fld_token"},
		Docs:                DocsToolsConfig{Enabled: true, AllowWrite: true},
	}
	tool := findDocsTool(t, NewDocsTools(client, cfg, &fakeApprovalRequester{}), createToolName)
	executor := tool.(ApprovalExecutor)
	result, err := executor.ExecuteApproved(context.Background(), json.RawMessage(`{"title":"Quarterly plan","content":"body","folder_token":"fld_token"}`))
	if err != nil {
		t.Fatalf("ExecuteApproved returned error after document creation: %v", err)
	}
	if !result.Warning || !strings.Contains(result.WarningReason, "append denied") || !strings.Contains(result.Message, "请勿重复创建") || !strings.Contains(result.Message, "doxcn12345678") {
		t.Fatalf("partial result = %#v, want warning with existing document link", result)
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
