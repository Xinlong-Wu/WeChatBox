package tools

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"

	feishuidempotency "lingobridge/internal/platform/feishu/idempotency"
	"lingobridge/internal/store"
	tooltypes "lingobridge/internal/tools"
)

const testDocxAppendExecutionOwner = "runtime_test"

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
	if got := NewDocsRuntime(client, st, "feishu:cli_test", cfg, nil, nil, nil, "").Tools(); len(got) != 0 {
		t.Fatalf("disabled tools = %d, want 0", len(got))
	}
	cfg.Docs.Enabled = true
	if got := NewDocsRuntime(client, st, "feishu:cli_test", cfg, nil, nil, nil, "").Tools(); len(got) != 0 {
		t.Fatalf("docs tools without resource access guard = %d, want 0", len(got))
	}
	cfg.Docs.AllowWrite = true
	if got := NewDocsRuntime(client, st, "feishu:cli_test", cfg, nil, nil, nil, "").Tools(); len(got) != 0 {
		t.Fatalf("write tools without approval or resource access workflow = %d, want 0", len(got))
	}
	if got := NewDocsRuntime(client, st, "feishu:cli_test", cfg, &fakeApprovalRequester{}, nil, nil, "").Tools(); len(got) != 0 {
		t.Fatalf("write tools without resource access workflow = %d, want 0", len(got))
	}
	runtime := NewDocsRuntime(client, st, "feishu:cli_test", cfg, &fakeApprovalRequester{result: OperationApprovalResult{Status: OperationApprovalStatusGranted}}, grantedResourceAccessController("req_access"), newTestDocxAppendCipher(t), testDocxAppendExecutionOwner)
	if got := runtime.Tools(); len(got) != 4 {
		t.Fatalf("write tools with approval workflow = %d, want four tools", len(got))
	} else {
		sharedService := got[0].(docsTool).service
		for _, registered := range got[1:] {
			if service := registered.(docsTool).service; sharedService == nil || service != sharedService {
				t.Fatalf("docs tools do not share one service: first=%p current=%p", sharedService, service)
			}
		}
		createPolicy := findDocsTool(t, got, createToolName).(OperationApprovalExecutor).OperationApprovalPolicy()
		appendPolicy := findDocsTool(t, got, appendToolName).(OperationApprovalExecutor).OperationApprovalPolicy()
		if createPolicy.ActionKey != "create" || appendPolicy.ActionKey != "append" || createPolicy.ToolName == appendPolicy.ToolName ||
			!createPolicy.RecoverInterrupted || !appendPolicy.RecoverInterrupted {
			t.Fatalf("operation policies create=%#v append=%#v, want independent restart-safe actions", createPolicy, appendPolicy)
		}
	}
	mutableView := runtime.Tools()
	mutableView[0] = nil
	if freshView := runtime.Tools(); freshView[0] == nil {
		t.Fatal("DocsRuntime.Tools returned mutable runtime-owned slice")
	}
}

func TestDocsToolWithoutServiceFailsClosed(t *testing.T) {
	result := (docsTool{name: readToolName, spec: docsReadSpec()}).Execute(
		t.Context(),
		tooltypes.Call{ID: "call_nil_service", Name: readToolName, Arguments: json.RawMessage(`{"token":"doc_token"}`)},
	)
	if !result.IsError || !strings.Contains(result.Content, "service is unavailable") {
		t.Fatalf("nil-service result = %#v, want explicit error", result)
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

func TestDocsAppendTextBlocksUsesSinglePageChildCount(t *testing.T) {
	const documentToken = "doxcnsinglepage123"
	const requestID = "req_single_page"
	var getCalls, postCalls int
	var appendIndex int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/oauth/v3/token" || r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal":
			writeJSON(t, w, map[string]any{
				"code": 0, "msg": "ok", "tenant_access_token": "tenant-token", "expire": 7200,
			})
		case r.URL.Path == "/open-apis/docx/v1/documents/"+documentToken+"/blocks/"+documentToken+"/children" && r.Method == http.MethodGet:
			getCalls++
			if got := r.URL.Query().Get("page_size"); got != "500" {
				t.Fatalf("page_size = %q, want 500", got)
			}
			if got := r.URL.Query().Get("page_token"); got != "" {
				t.Fatalf("first page_token = %q, want empty", got)
			}
			writeJSON(t, w, map[string]any{
				"code": 0,
				"msg":  "ok",
				"data": map[string]any{
					"items":    []any{map[string]any{"block_id": "block_1"}, map[string]any{"block_id": "block_2"}},
					"has_more": false,
				},
			})
		case r.URL.Path == "/open-apis/docx/v1/documents/"+documentToken+"/blocks/"+documentToken+"/children" && r.Method == http.MethodPost:
			postCalls++
			if got, want := r.URL.Query().Get("client_token"), docxAppendClientToken(requestID); got != want {
				t.Fatalf("client_token = %q, want %q", got, want)
			}
			var body struct {
				Index *int `json:"index"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode append request: %v", err)
			}
			if body.Index == nil {
				t.Fatal("append request index is nil")
			}
			appendIndex = *body.Index
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
	if err := (&docsService{client: client}).appendTextBlocks(context.Background(), requestID, authorizedDocForTest(documentToken), "new paragraph"); err != nil {
		t.Fatalf("appendTextBlocks returned error: %v", err)
	}
	if getCalls != 1 || postCalls != 1 || appendIndex != 2 {
		t.Fatalf("get_calls=%d post_calls=%d append_index=%d, want 1/1/2", getCalls, postCalls, appendIndex)
	}
}

func TestDocsAppendTextBlocksRetriesLostMutationResponseWithSameClientToken(t *testing.T) {
	const documentToken = "doxcnlostresponse123"
	const requestID = "req_lost_append_response"
	var postCalls int
	var clientTokens []string
	var appendIndexes []int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/oauth/v3/token" || r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal":
			writeJSON(t, w, map[string]any{
				"code": 0, "msg": "ok", "tenant_access_token": "tenant-token", "expire": 7200,
			})
		case r.URL.Path == "/open-apis/docx/v1/documents/"+documentToken+"/blocks/"+documentToken+"/children" && r.Method == http.MethodGet:
			writeJSON(t, w, map[string]any{
				"code": 0,
				"msg":  "ok",
				"data": map[string]any{
					"items":    []any{map[string]any{"block_id": "block_1"}},
					"has_more": false,
				},
			})
		case r.URL.Path == "/open-apis/docx/v1/documents/"+documentToken+"/blocks/"+documentToken+"/children" && r.Method == http.MethodPost:
			postCalls++
			clientTokens = append(clientTokens, r.URL.Query().Get("client_token"))
			var body struct {
				Index int `json:"index"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode append body: %v", err)
			}
			appendIndexes = append(appendIndexes, body.Index)
			if postCalls == 1 {
				hijacker, ok := w.(http.Hijacker)
				if !ok {
					t.Fatal("test server does not support response hijacking")
				}
				conn, _, err := hijacker.Hijack()
				if err != nil {
					t.Fatalf("hijack append response: %v", err)
				}
				_ = conn.Close()
				return
			}
			writeJSON(t, w, map[string]any{"code": 0, "msg": "ok"})
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
	if err := (&docsService{client: client}).appendTextBlocks(context.Background(), requestID, authorizedDocForTest(documentToken), "new paragraph"); err != nil {
		t.Fatalf("appendTextBlocks returned error after idempotent retry: %v", err)
	}
	wantToken := docxAppendClientToken(requestID)
	if postCalls != 2 || len(clientTokens) != 2 || clientTokens[0] != wantToken || clientTokens[1] != wantToken {
		t.Fatalf("append retries/tokens = %d/%#v, want two calls with %q", postCalls, clientTokens, wantToken)
	}
	if len(appendIndexes) != 2 || appendIndexes[0] != 1 || appendIndexes[1] != 1 {
		t.Fatalf("append retry indexes = %#v, want identical request bodies", appendIndexes)
	}
}

func TestDocsAppendTextBlocksDoesNotRetryAfterRuntimeOwnershipLoss(t *testing.T) {
	const documentToken = "doxcnownershiplost123"
	const requestID = "req_append_ownership_lost"
	ownershipCtx, cancelOwnership := context.WithCancel(context.Background())
	defer cancelOwnership()
	var postCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/oauth/v3/token" || r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal":
			writeJSON(t, w, map[string]any{
				"code": 0, "msg": "ok", "tenant_access_token": "tenant-token", "expire": 7200,
			})
		case r.URL.Path == "/open-apis/docx/v1/documents/"+documentToken+"/blocks/"+documentToken+"/children" && r.Method == http.MethodGet:
			writeJSON(t, w, map[string]any{
				"code": 0,
				"msg":  "ok",
				"data": map[string]any{"items": []any{}, "has_more": false},
			})
		case r.URL.Path == "/open-apis/docx/v1/documents/"+documentToken+"/blocks/"+documentToken+"/children" && r.Method == http.MethodPost:
			call := postCalls.Add(1)
			if call == 1 {
				cancelOwnership()
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Fatalf("hijack append response: %v", err)
				}
				_ = conn.Close()
				return
			}
			writeJSON(t, w, map[string]any{"code": 0, "msg": "ok"})
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
	operationCtx := feishuidempotency.WithRetryContext(context.Background(), ownershipCtx)
	err := (&docsService{client: client}).appendTextBlocks(operationCtx, requestID, authorizedDocForTest(documentToken), "new paragraph")
	if !errors.Is(err, errDocxAppendOutcomeUnknown) {
		t.Fatalf("appendTextBlocks error = %v, want conservative outcome_unknown after ownership loss", err)
	}
	if got := postCalls.Load(); got != 1 {
		t.Fatalf("append calls after runtime ownership loss = %d, want no reconciliation request from the old owner", got)
	}
}

func TestDocsAppendApprovedExecutionTreatsTwoLostMutationResponsesAsOutcomeUnknown(t *testing.T) {
	const documentToken = "doxcnlostbothresponses123"
	server, postCalls := newLostDocxAppendResponsesServer(t, documentToken)
	defer server.Close()

	client := lark.NewClient("cli_xxx", "secret",
		lark.WithOpenBaseUrl(server.URL),
		lark.WithOAuthBaseUrl(server.URL),
		lark.WithHttpClient(server.Client()),
	)
	cfg := Config{Docs: DocsToolsConfig{Enabled: true, AllowWrite: true}}
	st, tools, _ := newDocsToolsForTest(t, client, cfg, &fakeApprovalRequester{})
	seedDocsCreateApprovalWorkflow(t, st, "req_append_lost_both")
	executor := findDocsTool(t, tools, appendToolName).(OperationApprovalExecutor)

	result, err := executor.ExecuteApproved(context.Background(), "req_append_lost_both", json.RawMessage(
		`{"document_token":"doxcnlostbothresponses123","content":"approved paragraph","chat_id":"oc_chat","actor_open_id":"ou_requester"}`,
	))
	if err != nil {
		t.Fatalf("ExecuteApproved returned error after two uncertain responses: %v", err)
	}
	if !result.Warning || !strings.Contains(result.WarningReason, "outcome is unknown") ||
		!strings.Contains(result.Message, "请勿重复追加") || !strings.Contains(result.Message, "检查文档") {
		t.Fatalf("approved append result = %#v, want manual-check warning without retry advice", result)
	}
	if got := postCalls(); got != 2 {
		t.Fatalf("append calls = %d, want one request plus one idempotent reconciliation request", got)
	}
}

func TestDocsAppendPermanentGrantReturnsOutcomeUnknownAfterTwoLostMutationResponses(t *testing.T) {
	const documentToken = "doxcnlostdirectresponses123"
	server, postCalls := newLostDocxAppendResponsesServer(t, documentToken)
	defer server.Close()

	client := lark.NewClient("cli_xxx", "secret",
		lark.WithOpenBaseUrl(server.URL),
		lark.WithOAuthBaseUrl(server.URL),
		lark.WithHttpClient(server.Client()),
	)
	cfg := Config{Docs: DocsToolsConfig{Enabled: true, AllowWrite: true}}
	approver := &fakeApprovalRequester{result: OperationApprovalResult{Status: OperationApprovalStatusGranted}}
	st, tools, _ := newDocsToolsForTest(t, client, cfg, approver)

	result := findDocsTool(t, tools, appendToolName).Execute(groupDocsContext(), tooltypes.Call{
		ID:        "append_lost_both_direct",
		Name:      appendToolName,
		Arguments: json.RawMessage(`{"token":"doxcnlostdirectresponses123","content":"new paragraph"}`),
	})
	if result.IsError {
		t.Fatalf("direct append result = %#v, want structured outcome_unknown", result)
	}
	var output writeOutput
	if err := json.Unmarshal([]byte(result.Content), &output); err != nil {
		t.Fatalf("decode direct append output: %v", err)
	}
	if output.Status != "outcome_unknown" || output.RequestID == "" || output.Appended ||
		!strings.Contains(output.Warning, "请勿重复追加") || !strings.Contains(output.Warning, "检查文档") {
		t.Fatalf("direct append output = %#v, want manual-check outcome_unknown", output)
	}
	workflow, err := st.GetWorkflowRequest(output.RequestID, "feishu:cli_test")
	if err != nil || workflow.State != store.WorkflowRequestStatePartial {
		t.Fatalf("direct append workflow = %#v err=%v, want partial", workflow, err)
	}
	if got := postCalls(); got != 2 {
		t.Fatalf("append calls = %d, want one request plus one idempotent reconciliation request", got)
	}
}

func newLostDocxAppendResponsesServer(t *testing.T, documentToken string) (*httptest.Server, func() int) {
	t.Helper()
	var mu sync.Mutex
	postCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/oauth/v3/token" || r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal":
			writeJSON(t, w, map[string]any{
				"code": 0, "msg": "ok", "tenant_access_token": "tenant-token", "expire": 7200,
			})
		case r.URL.Path == "/open-apis/docx/v1/documents/"+documentToken+"/blocks/"+documentToken+"/children" && r.Method == http.MethodGet:
			writeJSON(t, w, map[string]any{
				"code": 0,
				"msg":  "ok",
				"data": map[string]any{"items": []any{}, "has_more": false},
			})
		case r.URL.Path == "/open-apis/docx/v1/documents/"+documentToken+"/blocks/"+documentToken+"/children" && r.Method == http.MethodPost:
			mu.Lock()
			postCalls++
			mu.Unlock()
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("test server does not support response hijacking")
			}
			conn, _, err := hijacker.Hijack()
			if err != nil {
				t.Fatalf("hijack append response: %v", err)
			}
			_ = conn.Close()
		default:
			t.Fatalf("unexpected path: %s method=%s", r.URL.Path, r.Method)
		}
	}))
	return server, func() int {
		mu.Lock()
		defer mu.Unlock()
		return postCalls
	}
}

func TestDocsAppendTextBlocksRetriesAfterOperationDeadlineWithSameClientToken(t *testing.T) {
	const documentToken = "doxcndeadline123"
	const requestID = "req_deadline_append_response"
	var mu sync.Mutex
	var postCalls int
	var clientTokens []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/oauth/v3/token" || r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal":
			writeJSON(t, w, map[string]any{
				"code": 0, "msg": "ok", "tenant_access_token": "tenant-token", "expire": 7200,
			})
		case r.URL.Path == "/open-apis/docx/v1/documents/"+documentToken+"/blocks/"+documentToken+"/children" && r.Method == http.MethodGet:
			writeJSON(t, w, map[string]any{
				"code": 0,
				"msg":  "ok",
				"data": map[string]any{
					"items":    []any{},
					"has_more": false,
				},
			})
		case r.URL.Path == "/open-apis/docx/v1/documents/"+documentToken+"/blocks/"+documentToken+"/children" && r.Method == http.MethodPost:
			mu.Lock()
			postCalls++
			call := postCalls
			clientTokens = append(clientTokens, r.URL.Query().Get("client_token"))
			mu.Unlock()
			if call == 1 {
				time.Sleep(75 * time.Millisecond)
				return
			}
			writeJSON(t, w, map[string]any{"code": 0, "msg": "ok"})
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
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if err := (&docsService{client: client}).appendTextBlocks(ctx, requestID, authorizedDocForTest(documentToken), "new paragraph"); err != nil {
		t.Fatalf("appendTextBlocks returned error after deadline reconciliation: %v", err)
	}
	wantToken := docxAppendClientToken(requestID)
	mu.Lock()
	defer mu.Unlock()
	if postCalls != 2 || len(clientTokens) != 2 || clientTokens[0] != wantToken || clientTokens[1] != wantToken {
		t.Fatalf("append deadline retries/tokens = %d/%#v, want two calls with %q", postCalls, clientTokens, wantToken)
	}
}

func TestDocsAppendTextBlocksUsesAllChildPages(t *testing.T) {
	const documentToken = "doxcnmultipage123"
	const requestID = "req_multi_page"
	var getCalls, postCalls int
	var appendIndex int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/oauth/v3/token" || r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal":
			writeJSON(t, w, map[string]any{
				"code": 0, "msg": "ok", "tenant_access_token": "tenant-token", "expire": 7200,
			})
		case r.URL.Path == "/open-apis/docx/v1/documents/"+documentToken+"/blocks/"+documentToken+"/children" && r.Method == http.MethodGet:
			getCalls++
			switch r.URL.Query().Get("page_token") {
			case "":
				writeJSON(t, w, map[string]any{
					"code": 0,
					"msg":  "ok",
					"data": map[string]any{
						"items":      []any{map[string]any{"block_id": "block_1"}, map[string]any{"block_id": "block_2"}},
						"has_more":   true,
						"page_token": "page-2",
					},
				})
			case "page-2":
				writeJSON(t, w, map[string]any{
					"code": 0,
					"msg":  "ok",
					"data": map[string]any{
						"items": []any{
							map[string]any{"block_id": "block_3"},
							map[string]any{"block_id": "block_4"},
							map[string]any{"block_id": "block_5"},
						},
						"has_more": false,
					},
				})
			default:
				t.Fatalf("unexpected page_token: %q", r.URL.Query().Get("page_token"))
			}
		case r.URL.Path == "/open-apis/docx/v1/documents/"+documentToken+"/blocks/"+documentToken+"/children" && r.Method == http.MethodPost:
			postCalls++
			if got, want := r.URL.Query().Get("client_token"), docxAppendClientToken(requestID); got != want {
				t.Fatalf("client_token = %q, want %q", got, want)
			}
			var body struct {
				Index *int `json:"index"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode append request: %v", err)
			}
			if body.Index == nil {
				t.Fatal("append request index is nil")
			}
			appendIndex = *body.Index
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
	if err := (&docsService{client: client}).appendTextBlocks(context.Background(), requestID, authorizedDocForTest(documentToken), "new paragraph"); err != nil {
		t.Fatalf("appendTextBlocks returned error: %v", err)
	}
	if getCalls != 2 || postCalls != 1 || appendIndex != 5 {
		t.Fatalf("get_calls=%d post_calls=%d append_index=%d, want 2/1/5", getCalls, postCalls, appendIndex)
	}
}

func TestDocsAppendTextBlocksRequiresStableWorkflowRequestID(t *testing.T) {
	err := (&docsService{}).appendTextBlocks(context.Background(), "", authorizedDocForTest("doxcnstableid123"), "must not append")
	if err == nil || !strings.Contains(err.Error(), "stable workflow request id") {
		t.Fatalf("appendTextBlocks error = %v, want stable request id rejection", err)
	}
}

func TestDocsAppendTextBlocksDoesNotWriteWhenChildLookupFails(t *testing.T) {
	const documentToken = "doxcnlookupfailure123"
	var postCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/oauth/v3/token" || r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal":
			writeJSON(t, w, map[string]any{
				"code": 0, "msg": "ok", "tenant_access_token": "tenant-token", "expire": 7200,
			})
		case r.URL.Path == "/open-apis/docx/v1/documents/"+documentToken+"/blocks/"+documentToken+"/children" && r.Method == http.MethodGet:
			writeJSON(t, w, map[string]any{"code": 99991672, "msg": "lookup denied"})
		case r.URL.Path == "/open-apis/docx/v1/documents/"+documentToken+"/blocks/"+documentToken+"/children" && r.Method == http.MethodPost:
			postCalls++
			writeJSON(t, w, map[string]any{"code": 0, "msg": "ok"})
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
	err := (&docsService{client: client}).appendTextBlocks(context.Background(), "req_lookup_failure", authorizedDocForTest(documentToken), "must not append")
	if err == nil || !strings.Contains(err.Error(), "resolve feishu document append position") || !strings.Contains(err.Error(), "lookup denied") {
		t.Fatalf("appendTextBlocks error = %v", err)
	}
	if postCalls != 0 {
		t.Fatalf("append POST calls = %d, want 0 after failed child lookup", postCalls)
	}
}

func TestDocsAppendTextBlocksFailsClosedWhenPaginationStateIsMissing(t *testing.T) {
	const documentToken = "doxcnmissingpagination123"
	var postCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/oauth/v3/token" || r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal":
			writeJSON(t, w, map[string]any{
				"code": 0, "msg": "ok", "tenant_access_token": "tenant-token", "expire": 7200,
			})
		case r.URL.Path == "/open-apis/docx/v1/documents/"+documentToken+"/blocks/"+documentToken+"/children" && r.Method == http.MethodGet:
			writeJSON(t, w, map[string]any{
				"code": 0,
				"msg":  "ok",
				"data": map[string]any{
					"items": []any{map[string]any{"block_id": "block_1"}},
				},
			})
		case r.URL.Path == "/open-apis/docx/v1/documents/"+documentToken+"/blocks/"+documentToken+"/children" && r.Method == http.MethodPost:
			postCalls++
			writeJSON(t, w, map[string]any{"code": 0, "msg": "ok"})
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
	err := (&docsService{client: client}).appendTextBlocks(context.Background(), "req_missing_pagination", authorizedDocForTest(documentToken), "must not append")
	if err == nil || !strings.Contains(err.Error(), "missing has_more") {
		t.Fatalf("appendTextBlocks error = %v, want missing has_more", err)
	}
	if postCalls != 0 {
		t.Fatalf("append POST calls = %d, want 0 when pagination state is incomplete", postCalls)
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

func TestDocsAppendMissingResourceGrantDoesNotRequestOperationApproval(t *testing.T) {
	approver := &fakeApprovalRequester{result: OperationApprovalResult{
		Status: OperationApprovalStatusPending, RequestID: "req_must_not_exist", ExpiresAt: time.Now().UTC().Add(10 * time.Minute),
	}}
	cfg := Config{Docs: DocsToolsConfig{Enabled: true, AllowWrite: true}}
	_, tools, access := newDocsToolsForTest(t, &lark.Client{}, cfg, approver)
	access.err = NewResourceAuthorizationRequiredError(ResourceAccessRequirement{
		ResourceType: "docx", ResourceToken: "doxcnmissing12345", Permission: ResourcePermissionWrite,
	}, "")
	result := findDocsTool(t, tools, appendToolName).Execute(groupDocsContext(), tooltypes.Call{
		ID:        "append_missing_resource",
		Name:      appendToolName,
		Arguments: json.RawMessage(`{"token":"doxcnmissing12345","content":"must not append"}`),
	})
	if !result.IsError || result.PendingWorkflowID != "" || !strings.Contains(result.Content, `"status":"resource_authorization_required"`) {
		t.Fatalf("missing-resource append result = %#v", result)
	}
	if approver.checks != 0 {
		t.Fatalf("operation approval checks = %d, want 0 before resource authorization", approver.checks)
	}
	if len(access.requirements) != 1 || access.requirement.ResourceToken != "doxcnmissing12345" || access.requirement.Permission != ResourcePermissionWrite {
		t.Fatalf("missing-resource append requirements = %#v", access.requirements)
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
	st, tools, access := newDocsToolsForTest(t, client, cfg, &fakeApprovalRequester{})
	seedDocsCreateApprovalWorkflow(t, st, "req_append_approved")
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

func TestDocsCreateLostResponseReconcilesUniqueBotOwnedDocumentWithoutSecondCreate(t *testing.T) {
	const documentToken = "doxcnreconciled123"
	var createCalls, listCalls int
	createdTime := strconv.FormatInt(time.Now().UTC().Unix(), 10)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/oauth/v3/token" || r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal":
			writeJSON(t, w, map[string]any{"code": 0, "msg": "ok", "tenant_access_token": "tenant-token", "expire": 7200})
		case r.URL.Path == "/open-apis/docx/v1/documents" && r.Method == http.MethodPost:
			createCalls++
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("test server does not support response hijacking")
			}
			conn, _, err := hijacker.Hijack()
			if err != nil {
				t.Fatalf("hijack create response: %v", err)
			}
			_ = conn.Close()
		case r.URL.Path == "/open-apis/drive/explorer/v2/root_folder/meta":
			writeJSON(t, w, map[string]any{"code": 0, "msg": "ok", "data": map[string]any{"token": "fld_root", "user_id": "app_owner"}})
		case r.URL.Path == "/open-apis/drive/v1/files" && r.Method == http.MethodGet:
			listCalls++
			if got := r.URL.Query().Get("folder_token"); got != "fld_token" {
				t.Fatalf("reconciliation folder_token = %q, want fld_token", got)
			}
			writeJSON(t, w, map[string]any{
				"code": 0,
				"msg":  "ok",
				"data": map[string]any{
					"files": []any{map[string]any{
						"token": documentToken, "name": "Quarterly plan", "type": "docx",
						"parent_token": "fld_token", "url": "https://docs.feishu.cn/docx/" + documentToken,
						"created_time": createdTime, "owner_id": "app_owner",
					}},
					"has_more": false,
				},
			})
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
	approver := &fakeApprovalRequester{result: OperationApprovalResult{Status: OperationApprovalStatusGranted}}
	st, tools, _ := newDocsToolsForTest(t, client, Config{Docs: DocsToolsConfig{Enabled: true, AllowWrite: true}}, approver)
	tool := findDocsTool(t, tools, createToolName).(docsTool)
	tool.service.remoteReconcileDelays = []time.Duration{}
	result := tool.Execute(groupDocsContext(), tooltypes.Call{
		ID: "create", Name: createToolName,
		Arguments: json.RawMessage(`{"title":"Quarterly plan","folder_token":"fld_token"}`),
	})
	if result.IsError {
		t.Fatalf("Execute result = %#v", result)
	}
	var output writeOutput
	if err := json.Unmarshal([]byte(result.Content), &output); err != nil {
		t.Fatalf("unmarshal reconciled output: %v", err)
	}
	if output.Status != "created" || output.DocumentID != documentToken || createCalls != 1 || listCalls != 1 {
		t.Fatalf("reconciled output/calls = %#v create=%d list=%d", output, createCalls, listCalls)
	}
	operation, err := st.GetFeishuRemoteOperation(output.RequestID, "feishu:cli_test")
	if err != nil || operation.State != store.FeishuRemoteOperationStatePersisted || operation.RemoteResourceToken != documentToken {
		t.Fatalf("persisted remote operation = %#v err=%v", operation, err)
	}
}

func TestDocsCreateUnknownOutcomeRecoveryNeverRepeatsCreate(t *testing.T) {
	const documentToken = "doxcnlatecandidate123"
	var createCalls, listCalls int
	showCandidate := false
	createdTime := strconv.FormatInt(time.Now().UTC().Unix(), 10)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/oauth/v3/token" || r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal":
			writeJSON(t, w, map[string]any{"code": 0, "msg": "ok", "tenant_access_token": "tenant-token", "expire": 7200})
		case r.URL.Path == "/open-apis/docx/v1/documents" && r.Method == http.MethodPost:
			createCalls++
			hijacker := w.(http.Hijacker)
			conn, _, err := hijacker.Hijack()
			if err != nil {
				t.Fatalf("hijack create response: %v", err)
			}
			_ = conn.Close()
		case r.URL.Path == "/open-apis/drive/explorer/v2/root_folder/meta":
			writeJSON(t, w, map[string]any{"code": 0, "msg": "ok", "data": map[string]any{"token": "fld_root", "user_id": "app_owner"}})
		case r.URL.Path == "/open-apis/drive/v1/files" && r.Method == http.MethodGet:
			listCalls++
			files := []any{}
			if showCandidate {
				files = append(files, map[string]any{
					"token": documentToken, "name": "Late plan", "type": "docx",
					"parent_token": "fld_token", "url": "https://docs.feishu.cn/docx/" + documentToken,
					"created_time": createdTime, "owner_id": "app_owner",
				})
			}
			writeJSON(t, w, map[string]any{"code": 0, "msg": "ok", "data": map[string]any{"files": files, "has_more": false}})
		default:
			t.Fatalf("unexpected path: %s method=%s", r.URL.Path, r.Method)
		}
	}))
	defer server.Close()

	client := lark.NewClient("cli_xxx", "secret", lark.WithOpenBaseUrl(server.URL), lark.WithOAuthBaseUrl(server.URL), lark.WithHttpClient(server.Client()))
	approver := &fakeApprovalRequester{result: OperationApprovalResult{Status: OperationApprovalStatusGranted}}
	_, tools, _ := newDocsToolsForTest(t, client, Config{Docs: DocsToolsConfig{Enabled: true, AllowWrite: true}}, approver)
	tool := findDocsTool(t, tools, createToolName).(docsTool)
	tool.service.remoteReconcileDelays = []time.Duration{}
	first := tool.Execute(groupDocsContext(), tooltypes.Call{
		ID: "create", Name: createToolName,
		Arguments: json.RawMessage(`{"title":"Late plan","folder_token":"fld_token"}`),
	})
	if first.IsError {
		t.Fatalf("first Execute result = %#v, want structured unknown outcome", first)
	}
	var pending writeOutput
	if err := json.Unmarshal([]byte(first.Content), &pending); err != nil {
		t.Fatalf("unmarshal unknown output: %v", err)
	}
	if pending.Status != "outcome_unknown" || pending.RequestID == "" || createCalls != 1 || !strings.Contains(pending.Retry, "request_id") {
		t.Fatalf("unknown output/calls = %#v create=%d", pending, createCalls)
	}

	showCandidate = true
	retryArgs, _ := json.Marshal(createArgs{RequestID: pending.RequestID})
	second := tool.Execute(groupDocsContext(), tooltypes.Call{ID: "recover", Name: createToolName, Arguments: retryArgs})
	if second.IsError {
		t.Fatalf("recovery Execute result = %#v", second)
	}
	var recovered writeOutput
	if err := json.Unmarshal([]byte(second.Content), &recovered); err != nil {
		t.Fatalf("unmarshal recovered output: %v", err)
	}
	if recovered.Status != "created" || recovered.DocumentID != documentToken || recovered.RequestID != pending.RequestID || createCalls != 1 || listCalls != 2 {
		t.Fatalf("recovered output/calls = %#v create=%d list=%d", recovered, createCalls, listCalls)
	}
	if approver.checks != 1 {
		t.Fatalf("approval checks = %d, want recovery to bypass a new approval", approver.checks)
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
	st, tools, access := newDocsToolsForTest(t, client, cfg, &fakeApprovalRequester{})
	seedDocsCreateApprovalWorkflow(t, st, "req_approved")
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
	if len(access.requirements) != 1 || access.requirement.ResourceToken != "fld_token" || access.actor.OpenID != "ou_requester" || access.chat.ChatID != "oc_chat" {
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
	st, tools, _ := newDocsToolsForTest(t, client, cfg, &fakeApprovalRequester{})
	seedDocsCreateApprovalWorkflow(t, st, "req_approved")
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

func TestDocsCreateApprovedExecutionReportsUnknownInitialContentWithoutRepeatAdvice(t *testing.T) {
	const documentToken = "doxcnunknowninitial123"
	var createCalls, appendCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/oauth/v3/token" || r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal":
			writeJSON(t, w, map[string]any{
				"code": 0, "msg": "ok", "tenant_access_token": "tenant-token", "expire": 7200,
			})
		case r.URL.Path == "/open-apis/docx/v1/documents" && r.Method == http.MethodPost:
			createCalls.Add(1)
			writeJSON(t, w, map[string]any{
				"code": 0,
				"msg":  "ok",
				"data": map[string]any{"document": map[string]any{"document_id": documentToken}},
			})
		case r.URL.Path == "/open-apis/docx/v1/documents/"+documentToken+"/blocks/"+documentToken+"/children" && r.Method == http.MethodGet:
			writeJSON(t, w, map[string]any{"code": 0, "msg": "ok", "data": map[string]any{"items": []any{}, "has_more": false}})
		case r.URL.Path == "/open-apis/docx/v1/documents/"+documentToken+"/blocks/"+documentToken+"/children" && r.Method == http.MethodPost:
			appendCalls.Add(1)
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Fatalf("hijack append response: %v", err)
			}
			_ = conn.Close()
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
	st, tools, _ := newDocsToolsForTest(t, client, cfg, &fakeApprovalRequester{})
	seedDocsCreateApprovalWorkflow(t, st, "req_unknown_initial")
	executor := findDocsTool(t, tools, createToolName).(OperationApprovalExecutor)

	result, err := executor.ExecuteApproved(context.Background(), "req_unknown_initial", json.RawMessage(
		`{"title":"Quarterly plan","content":"body","folder_token":"fld_token","chat_id":"oc_chat","actor_open_id":"ou_requester"}`,
	))
	if err != nil {
		t.Fatalf("ExecuteApproved returned error after unknown initial append: %v", err)
	}
	if !result.Warning || !strings.Contains(result.Message, "初始正文") ||
		!strings.Contains(result.Message, "检查文档") || !strings.Contains(result.Message, "请勿重复追加") {
		t.Fatalf("unknown initial-content result = %#v, want conservative manual-check warning", result)
	}
	if createCalls.Load() != 1 || appendCalls.Load() != 2 {
		t.Fatalf("remote calls create=%d append=%d, want one create and one same-token append reconciliation", createCalls.Load(), appendCalls.Load())
	}
}

func TestDocsCreateApprovedExecutionRecoveryReusesCompletedInitialContentLedger(t *testing.T) {
	const documentToken = "doxcnrestart123"
	var createCalls, appendCalls int
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
			createCalls++
			writeJSON(t, w, map[string]any{
				"code": 0,
				"msg":  "ok",
				"data": map[string]any{"document": map[string]any{"document_id": documentToken}},
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
	st, tools, _ := newDocsToolsForTest(t, client, cfg, &fakeApprovalRequester{})
	seedDocsCreateApprovalWorkflow(t, st, "req_restart")
	executor := findDocsTool(t, tools, createToolName).(OperationApprovalExecutor)
	payload := json.RawMessage(`{"title":"Quarterly plan","content":"body","folder_token":"fld_token","chat_id":"oc_chat","actor_open_id":"ou_requester"}`)
	if _, err := executor.ExecuteApproved(context.Background(), "req_restart", payload); err != nil {
		t.Fatalf("first ExecuteApproved returned error: %v", err)
	}
	recovered, err := executor.ExecuteApproved(context.Background(), "req_restart", payload)
	if err != nil {
		t.Fatalf("recovered ExecuteApproved returned error: %v", err)
	}
	if recovered.Warning || !strings.Contains(recovered.Message, "飞书文档已创建") {
		t.Fatalf("recovered result = %#v, want completed ledger replay without warning", recovered)
	}
	if createCalls != 1 || appendCalls != 1 {
		t.Fatalf("remote calls after recovery create=%d append=%d, want 1/1", createCalls, appendCalls)
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
	tools := NewDocsRuntime(&lark.Client{}, st, "feishu:cli_test", cfg, approver, access, newTestDocxAppendCipher(t), testDocxAppendExecutionOwner).Tools()
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
	acquireTestDocxAppendRuntimeLease(t, st, testDocxAppendExecutionOwner, time.Now().UTC())
	access := grantedResourceAccessController("req_access")
	return st, NewDocsRuntime(client, st, "feishu:cli_test", cfg, approver, access, newTestDocxAppendCipher(t), testDocxAppendExecutionOwner).Tools(), access
}

func seedDocsCreateApprovalWorkflow(t *testing.T, st *store.Store, requestID string) {
	t.Helper()
	if _, err := st.CreateWorkflowRequest(store.WorkflowRequest{
		ID:        requestID,
		AccountID: "feishu:cli_test",
		Kind:      store.WorkflowRequestKindToolApproval,
		State:     store.WorkflowRequestStateExecuting,
		CreatedAt: time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("seed document create approval workflow: %v", err)
	}
}

func authorizedDocForTest(token string) AuthorizedResource {
	return AuthorizedResource{
		AccountID:           "feishu:cli_test",
		ActorOpenID:         "ou_requester",
		ActorUserID:         "u_requester",
		ChatID:              "oc_chat",
		ResourceType:        "docx",
		ResourceToken:       token,
		EffectivePermission: ResourcePermissionWrite,
	}
}

func newTestDocxAppendCipher(t *testing.T) *DocxAppendEnvelopeCipher {
	t.Helper()
	cipher, err := NewDocxAppendEnvelopeCipher("test-app-secret", "feishu:cli_test")
	if err != nil {
		t.Fatalf("NewDocxAppendEnvelopeCipher returned error: %v", err)
	}
	return cipher
}

func acquireTestDocxAppendRuntimeLease(t *testing.T, st *store.Store, ownerID string, now time.Time) {
	t.Helper()
	if _, err := st.AcquireFeishuAccountRuntimeLease("feishu:cli_test", ownerID, now, 24*time.Hour); err != nil {
		t.Fatalf("AcquireFeishuAccountRuntimeLease returned error: %v", err)
	}
}
