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
	request OperationApprovalRequest
	result  OperationApprovalResult
	err     error
	checks  int
}

func (f *fakeApprovalRequester) CheckOrRequest(_ context.Context, request OperationApprovalRequest) (OperationApprovalResult, error) {
	f.request = request
	f.checks++
	return f.result, f.err
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
	if got := NewDocsTools(client, st, "feishu:cli_test", cfg, nil, nil); len(got) != 2 {
		t.Fatalf("write tools without approval or resource access workflow = %d, want search/read", len(got))
	}
	if got := NewDocsTools(client, st, "feishu:cli_test", cfg, &fakeApprovalRequester{}, nil); len(got) != 2 {
		t.Fatalf("write tools without resource access workflow = %d, want search/read", len(got))
	}
	if got := NewDocsTools(client, st, "feishu:cli_test", cfg, &fakeApprovalRequester{result: OperationApprovalResult{Status: OperationApprovalStatusGranted}}, grantedResourceAccessController("req_access")); len(got) != 4 {
		t.Fatalf("write tools with approval workflow = %d, want four tools", len(got))
	} else {
		createPolicy := findDocsTool(t, got, createToolName).(OperationApprovalExecutor).OperationApprovalPolicy()
		appendPolicy := findDocsTool(t, got, appendToolName).(OperationApprovalExecutor).OperationApprovalPolicy()
		if createPolicy.ActionKey != "create" || appendPolicy.ActionKey != "append" || createPolicy.ToolName == appendPolicy.ToolName {
			t.Fatalf("operation policies create=%#v append=%#v, want independent actions", createPolicy, appendPolicy)
		}
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
	approver := &fakeApprovalRequester{result: OperationApprovalResult{Status: OperationApprovalStatusGranted}}
	st, tools, access := newDocsToolsForTest(t, client, cfg, approver)
	readTool := findDocsTool(t, tools, readToolName)
	read := readTool.Execute(groupDocsContext(), tooltypes.Call{
		ID:        "read_external",
		Name:      readToolName,
		Arguments: json.RawMessage(`{"token":"doxcnexternal12345"}`),
	})
	if read.IsError || !strings.Contains(read.Content, "external text") || readCalls != 1 {
		t.Fatalf("external read result=%#v read_calls=%d", read, readCalls)
	}
	if access.requirement.ResourceType != "docx" || access.requirement.ResourceToken != documentToken || access.requirement.Permission != ResourcePermissionRead {
		t.Fatalf("external read requirement = %#v", access.requirement)
	}

	appendTool := findDocsTool(t, tools, appendToolName)
	append := appendTool.Execute(groupDocsContext(), tooltypes.Call{
		ID:        "append_external",
		Name:      appendToolName,
		Arguments: json.RawMessage(`{"token":"doxcnexternal12345","content":"new paragraph"}`),
	})
	if append.IsError || !strings.Contains(append.Content, `"appended":true`) || appendCalls != 1 {
		t.Fatalf("external append result=%#v append_calls=%d", append, appendCalls)
	}
	if len(access.requirements) != 3 || access.requirement.ResourceToken != documentToken || access.requirement.Permission != ResourcePermissionWrite {
		t.Fatalf("external read/append requirements = %#v", access.requirements)
	}
	if approver.checks != 1 || approver.request.ToolName != appendToolName || approver.request.ActionKey != "append" || approver.request.ResourceType != "docx" || approver.request.ResourceToken != documentToken {
		t.Fatalf("external append approval request = %#v checks=%d", approver.request, approver.checks)
	}
	if _, err := st.GetFeishuChatDocument("feishu:cli_test", "oc_chat", documentToken); !errors.Is(err, store.ErrFeishuChatDocumentNotFound) {
		t.Fatalf("external document binding error = %v, want not found", err)
	}
}

func TestDocsAppendToolReturnsPendingOperationApproval(t *testing.T) {
	expiresAt := time.Date(2026, time.August, 1, 12, 10, 0, 0, time.UTC)
	approver := &fakeApprovalRequester{result: OperationApprovalResult{
		Status: OperationApprovalStatusPending, RequestID: "req_append", ExpiresAt: expiresAt,
	}}
	cfg := Config{Docs: DocsToolsConfig{Enabled: true, AllowWrite: true}}
	_, tools, access := newDocsToolsForTest(t, &lark.Client{}, cfg, approver)
	result := findDocsTool(t, tools, appendToolName).Execute(groupDocsContext(), tooltypes.Call{
		ID:        "append_pending",
		Name:      appendToolName,
		Arguments: json.RawMessage(`{"token":"doxcnappend12345","content":"private paragraph"}`),
	})
	if result.IsError || result.PendingWorkflowID != "req_append" {
		t.Fatalf("pending append result = %#v", result)
	}
	var output pendingApprovalOutput
	if err := json.Unmarshal([]byte(result.Content), &output); err != nil {
		t.Fatalf("unmarshal pending append output: %v", err)
	}
	if output.Status != "pending_approval" || output.RequestID != "req_append" || output.ExpiresAt != expiresAt.Format(time.RFC3339) ||
		!strings.Contains(output.Message, "永久允许") || !strings.Contains(output.Message, "请勿重复调用") {
		t.Fatalf("pending append output = %#v", output)
	}
	if approver.checks != 1 || approver.request.ToolName != appendToolName || approver.request.ActionKey != "append" ||
		approver.request.ResourceType != "docx" || approver.request.ResourceToken != "doxcnappend12345" {
		t.Fatalf("append approval request = %#v checks=%d", approver.request, approver.checks)
	}
	var payload approvedAppendPayload
	if err := json.Unmarshal(approver.request.Payload, &payload); err != nil {
		t.Fatalf("unmarshal approved append payload: %v", err)
	}
	if payload.DocumentToken != "doxcnappend12345" || payload.Content != "private paragraph" || payload.ChatID != "oc_chat" || payload.ActorOpenID != "ou_requester" {
		t.Fatalf("approved append payload = %#v", payload)
	}
	for _, field := range approver.request.Fields {
		if strings.Contains(field.Value, "private paragraph") {
			t.Fatalf("append approval field leaked content: %#v", field)
		}
	}
	if len(access.requirements) != 1 || access.requirement.ResourceToken != "doxcnappend12345" || access.requirement.Permission != ResourcePermissionWrite {
		t.Fatalf("pending append access requirements = %#v", access.requirements)
	}
}

func TestDocsAppendApprovedExecutionRevalidatesAndAppends(t *testing.T) {
	const documentToken = "doxcnapproved12345"
	var appendCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/oauth/v3/token" || r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal":
			writeJSON(t, w, map[string]any{
				"code": 0, "msg": "ok", "tenant_access_token": "tenant-token", "expire": 7200,
			})
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
	_, tools, access := newDocsToolsForTest(t, client, cfg, &fakeApprovalRequester{})
	executor := findDocsTool(t, tools, appendToolName).(OperationApprovalExecutor)
	result, err := executor.ExecuteApproved(context.Background(), "req_append_approved", json.RawMessage(
		`{"document_token":"doxcnapproved12345","content":"approved paragraph","chat_id":"oc_chat","actor_open_id":"ou_requester"}`,
	))
	if err != nil {
		t.Fatalf("ExecuteApproved returned error: %v", err)
	}
	if result.Warning || appendCalls != 1 || !strings.Contains(result.Message, "https://docs.feishu.cn/docx/"+documentToken) {
		t.Fatalf("approved append result=%#v append_calls=%d", result, appendCalls)
	}
	if len(access.requirements) != 1 || access.requirement.ResourceToken != documentToken || access.requirement.Permission != ResourcePermissionWrite ||
		access.actor.OpenID != "ou_requester" || access.chat.ChatID != "oc_chat" {
		t.Fatalf("approved append access requirements=%#v actor=%#v chat=%#v", access.requirements, access.actor, access.chat)
	}
}

func TestDocsAppendApprovedExecutionStopsWhenAccessWasRevoked(t *testing.T) {
	cfg := Config{Docs: DocsToolsConfig{Enabled: true, AllowWrite: true}}
	_, tools, access := newDocsToolsForTest(t, &lark.Client{}, cfg, &fakeApprovalRequester{})
	access.err = errors.New("permission revoked")
	executor := findDocsTool(t, tools, appendToolName).(OperationApprovalExecutor)

	_, err := executor.ExecuteApproved(context.Background(), "req_append_approved", json.RawMessage(
		`{"document_token":"doxcnrevoked12345","content":"must not append","chat_id":"oc_chat","actor_open_id":"ou_requester"}`,
	))
	if err == nil || !strings.Contains(err.Error(), "revalidate approved document append access") || !strings.Contains(err.Error(), "permission revoked") {
		t.Fatalf("ExecuteApproved error = %v", err)
	}
}

func TestDocsProtectedToolReturnsStructuredResourceAuthorizationRequired(t *testing.T) {
	cfg := Config{Docs: DocsToolsConfig{Enabled: true}}
	_, tools, access := newDocsToolsForTest(t, &lark.Client{}, cfg, nil)
	requirement := ResourceAccessRequirement{
		ResourceType: "docx", ResourceToken: "doxcnexternal12345", Permission: ResourcePermissionRead,
	}
	access.err = NewResourceAuthorizationRequiredError(requirement, "")
	result := findDocsTool(t, tools, readToolName).Execute(groupDocsContext(), tooltypes.Call{
		ID:        "read_requires_authorization",
		Name:      readToolName,
		Arguments: json.RawMessage(`{"token":"doxcnexternal12345"}`),
	})
	if !result.IsError {
		t.Fatalf("protected read result = %#v, want tool error", result)
	}
	var payload ResourceAuthorizationRequiredError
	if err := json.Unmarshal([]byte(result.Content), &payload); err != nil {
		t.Fatalf("structured authorization error JSON = %q: %v", result.Content, err)
	}
	if payload.Status != ResourceAuthorizationRequiredStatus || payload.RequiredTool != ResourceAccessToolName ||
		payload.ResourceType != requirement.ResourceType || payload.ResourceToken != requirement.ResourceToken || payload.Permission != requirement.Permission {
		t.Fatalf("structured authorization error = %#v", payload)
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
		Arguments: json.RawMessage(`{"token":"doxcnotherchat123"}`),
	})
	if !result.IsError || !strings.Contains(result.Content, "not available to the current Feishu chat") {
		t.Fatalf("cross-chat read result = %#v", result)
	}
	if len(access.requirements) != 1 || access.requirement.ResourceToken != "doxcnotherchat123" || access.requirement.Permission != ResourcePermissionRead {
		t.Fatalf("cross-chat Bot document requirement = %#v", access.requirements)
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
	if len(access.requirements) != 1 || access.requirement.ResourceToken != documentToken || access.requirement.Permission != ResourcePermissionRead {
		t.Fatalf("Bot-owned repair access requirement = %#v", access.requirements)
	}
}

func TestDocsCreateToolReturnsPendingApprovalWithoutCallingFeishuDocs(t *testing.T) {
	expiresAt := time.Date(2026, time.August, 1, 12, 10, 0, 0, time.UTC)
	approver := &fakeApprovalRequester{result: OperationApprovalResult{Status: OperationApprovalStatusPending, RequestID: "req_123", ExpiresAt: expiresAt}}
	cfg := Config{Docs: DocsToolsConfig{Enabled: true, AllowWrite: true}}
	_, tools, _ := newDocsToolsForTest(t, &lark.Client{}, cfg, approver)
	tool := findDocsTool(t, tools, createToolName)
	result := tool.Execute(groupDocsContext(), tooltypes.Call{
		ID:        "call_1",
		Name:      createToolName,
		Arguments: json.RawMessage(`{"title":" Quarterly plan ","content":"private body","folder_token":" fld_token "}`),
	})
	if result.IsError {
		t.Fatalf("Execute result = %#v, want pending approval success", result)
	}
	if result.PendingWorkflowID != "req_123" {
		t.Fatalf("PendingWorkflowID = %q, want req_123", result.PendingWorkflowID)
	}
	var output pendingApprovalOutput
	if err := json.Unmarshal([]byte(result.Content), &output); err != nil {
		t.Fatalf("unmarshal pending output: %v", err)
	}
	if output.Status != "pending_approval" || output.RequestID != "req_123" || output.ExpiresAt != expiresAt.Format(time.RFC3339) || !strings.Contains(output.Message, "永久允许") || !strings.Contains(output.Message, "请勿重复调用") {
		t.Fatalf("pending output = %#v", output)
	}
	if approver.checks != 1 {
		t.Fatalf("operation approval checks = %d, want one", approver.checks)
	}
	if approver.request.ToolName != createToolName || approver.request.ActionKey != "create" || approver.request.ResourceType != "folder" || approver.request.ResourceToken != "fld_token" {
		t.Fatalf("approval request = %#v", approver.request)
	}
	var approvedArgs approvedCreatePayload
	if err := json.Unmarshal(approver.request.Payload, &approvedArgs); err != nil {
		t.Fatalf("unmarshal approval payload: %v", err)
	}
	if approvedArgs.Title != "Quarterly plan" || approvedArgs.FolderToken != "fld_token" || approvedArgs.Content != "private body" || approvedArgs.ChatID != "oc_chat" {
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
	approver := &fakeApprovalRequester{result: OperationApprovalResult{Status: OperationApprovalStatusGranted}}
	cfg := Config{Docs: DocsToolsConfig{Enabled: true, AllowWrite: true}}
	st, tools, access := newDocsToolsForTest(t, client, cfg, approver)
	tool := findDocsTool(t, tools, createToolName)
	result := tool.Execute(groupDocsContext(), tooltypes.Call{
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
	if len(access.requirements) != 2 || access.requirement.ResourceToken != "fld_token" || access.requirement.Permission != ResourcePermissionWrite {
		t.Fatalf("access requirements=%#v", access.requirements)
	}
	if approver.checks != 1 || approver.request.ToolName != createToolName || approver.request.ActionKey != "create" {
		t.Fatalf("operation approval request = %#v checks=%d", approver.request, approver.checks)
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
	st, tools, _ := newDocsToolsForTest(t, client, cfg, &fakeApprovalRequester{result: OperationApprovalResult{Status: OperationApprovalStatusGranted}})
	tool := findDocsTool(t, tools, createToolName)
	result := tool.Execute(groupDocsContext(), tooltypes.Call{
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
	if _, err := st.GetFeishuChatDocument("feishu:cli_test", "oc_chat", output.DocumentID); err != nil {
		t.Fatalf("partially created document was not recoverably bound to chat: %v", err)
	}
}

func TestDocsCreateToolRejectsTitleLongerThanFeishuLimit(t *testing.T) {
	approver := &fakeApprovalRequester{result: OperationApprovalResult{Status: OperationApprovalStatusPending, RequestID: "req_pending", ExpiresAt: time.Now().UTC().Add(10 * time.Minute)}}
	cfg := Config{Docs: DocsToolsConfig{Enabled: true, AllowWrite: true}}
	_, tools, _ := newDocsToolsForTest(t, &lark.Client{}, cfg, approver)
	tool := findDocsTool(t, tools, createToolName)
	args, err := json.Marshal(createArgs{
		Title:       strings.Repeat("文", maxDocxTitle+1),
		FolderToken: "fld_token",
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
	executor, ok := tool.(OperationApprovalExecutor)
	if !ok {
		t.Fatalf("create tool %T does not implement OperationApprovalExecutor", tool)
	}
	result, err := executor.ExecuteApproved(context.Background(), "req_approved", json.RawMessage(`{"title":"Quarterly plan","folder_token":"fld_token","chat_id":"oc_chat","actor_open_id":"ou_requester"}`))
	if err != nil {
		t.Fatalf("ExecuteApproved returned error: %v", err)
	}
	if result.Warning || !strings.Contains(result.Message, "https://docs.feishu.cn/docx/doxcn12345678") {
		t.Fatalf("approved result = %#v, want created document link", result)
	}
	if createRequest.Title != "Quarterly plan" || createRequest.FolderToken != "fld_token" {
		t.Fatalf("create request = %#v", createRequest)
	}
	if len(access.requirements) != 2 || access.requirement.ResourceToken != "fld_token" || access.actor.OpenID != "ou_requester" || access.chat.ChatID != "oc_chat" {
		t.Fatalf("approved access requirements=%#v actor=%#v chat=%#v", access.requirements, access.actor, access.chat)
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
	executor := tool.(OperationApprovalExecutor)
	result, err := executor.ExecuteApproved(context.Background(), "req_approved", json.RawMessage(`{"title":"Quarterly plan","content":"body","folder_token":"fld_token","chat_id":"oc_chat","actor_open_id":"ou_requester"}`))
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
	executor := findDocsTool(t, tools, createToolName).(OperationApprovalExecutor)

	_, err := executor.ExecuteApproved(context.Background(), "req_approved", json.RawMessage(`{"title":"Quarterly plan","folder_token":"fld_token","chat_id":"oc_chat","actor_open_id":"ou_requester"}`))
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
	approver := &fakeApprovalRequester{result: OperationApprovalResult{
		Status: OperationApprovalStatusPending, RequestID: "req_pending", ExpiresAt: time.Now().UTC().Add(10 * time.Minute),
	}}
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

	access.err = NewResourceAuthorizationRequiredError(ResourceAccessRequirement{
		ResourceType: "folder", ResourceToken: "fld_token", Permission: ResourcePermissionWrite,
	}, "")
	missing := tool.Execute(groupDocsContext(), tooltypes.Call{ID: "missing", Name: createToolName, Arguments: json.RawMessage(`{"title":"No access","folder_token":"fld_token"}`)})
	if !missing.IsError || !strings.Contains(missing.Content, `"status":"resource_authorization_required"`) || approver.checks != 0 {
		t.Fatalf("missing access result=%#v approval_checks=%d", missing, approver.checks)
	}
	if len(access.requirements) != 1 || access.requirement.ResourceToken != "fld_token" || access.requirement.Permission != ResourcePermissionWrite {
		t.Fatalf("missing access requirements=%#v", access.requirements)
	}
	access.err = nil
	access.requirements = nil

	result := tool.Execute(groupDocsContext(), tooltypes.Call{ID: "pending", Name: createToolName, Arguments: json.RawMessage(`{"title":"With access","folder_token":"fld_token"}`)})
	if result.IsError {
		t.Fatalf("granted access result = %#v", result)
	}
	if len(access.requirements) != 1 || access.requirement.ResourceToken != "fld_token" || access.requirement.Permission != ResourcePermissionWrite || access.actor.OpenID != "ou_requester" || access.chat.ChatID != "oc_chat" {
		t.Fatalf("access requirements=%#v actor=%#v chat=%#v", access.requirements, access.actor, access.chat)
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

func newDocsToolsForTest(t *testing.T, client *lark.Client, cfg Config, approver OperationApprovalService) (*store.Store, []tooltypes.Tool, *fakeResourceAccessController) {
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
