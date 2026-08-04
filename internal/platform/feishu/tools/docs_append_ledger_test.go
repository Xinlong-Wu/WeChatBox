package tools

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"

	"lingobridge/internal/store"
	tooltypes "lingobridge/internal/tools"
)

type blockingDocxRecoveryAccess struct {
	entered chan struct{}
	release chan struct{}
	err     error
	once    sync.Once
}

func (g *blockingDocxRecoveryAccess) Require(ctx context.Context, requirement ResourceAccessRequirement) (AuthorizedResource, error) {
	actor, _ := ActorFromContext(ctx)
	chat, _ := ChatContextFromContext(ctx)
	if requirement.ResourceType == "docx" {
		g.once.Do(func() { close(g.entered) })
		select {
		case <-g.release:
			return AuthorizedResource{}, g.err
		case <-ctx.Done():
			return AuthorizedResource{}, ctx.Err()
		}
	}
	return AuthorizedResource{
		AccountID:             "feishu:cli_test",
		ActorOpenID:           actor.OpenID,
		ActorUserID:           actor.UserID,
		ChatID:                chat.ChatID,
		ResourceType:          requirement.ResourceType,
		ResourceToken:         requirement.ResourceToken,
		EffectivePermission:   requirement.Permission,
		GrantMode:             ResourceAccessGrantModeAll,
		CapabilitySubjectType: "bot",
		CapabilitySubjectID:   "ou_bot",
		Source:                ResourceAccessSourceExistingGrant,
	}, nil
}

func TestDocsAppendRestartReusesFrozenRequestAfterLostResponses(t *testing.T) {
	const (
		documentToken = "doxcnrestartfrozen123"
		requestID     = "req_append_restart_frozen"
	)

	type appendCall struct {
		clientToken string
		body        json.RawMessage
	}
	var (
		mu        sync.Mutex
		getCalls  int
		postCalls []appendCall
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/oauth/v3/token" || r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal":
			writeJSON(t, w, map[string]any{
				"code": 0, "msg": "ok", "tenant_access_token": "tenant-token", "expire": 7200,
			})
		case r.URL.Path == "/open-apis/docx/v1/documents/"+documentToken+"/blocks/"+documentToken+"/children" && r.Method == http.MethodGet:
			mu.Lock()
			getCalls++
			call := getCalls
			mu.Unlock()
			items := []any{map[string]any{"block_id": "existing-1"}}
			if call > 1 {
				items = append(items,
					map[string]any{"block_id": "concurrent-2"},
					map[string]any{"block_id": "concurrent-3"},
				)
			}
			writeJSON(t, w, map[string]any{
				"code": 0,
				"msg":  "ok",
				"data": map[string]any{"items": items, "has_more": false},
			})
		case r.URL.Path == "/open-apis/docx/v1/documents/"+documentToken+"/blocks/"+documentToken+"/children" && r.Method == http.MethodPost:
			var body json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode append request: %v", err)
			}
			mu.Lock()
			postCalls = append(postCalls, appendCall{
				clientToken: r.URL.Query().Get("client_token"),
				body:        append(json.RawMessage(nil), body...),
			})
			call := len(postCalls)
			mu.Unlock()
			if call <= 2 {
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
	st := openDocsFolderTestStore(t)
	acquireTestDocxAppendRuntimeLease(t, st, testDocxAppendExecutionOwner, time.Now().UTC())
	if _, err := st.CreateWorkflowRequest(store.WorkflowRequest{
		ID:        requestID,
		AccountID: "feishu:cli_test",
		Kind:      store.WorkflowRequestKindFeishuDocsAppend,
		State:     store.WorkflowRequestStateExecuting,
		CreatedAt: time.Date(2026, time.August, 4, 18, 50, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("seed append workflow: %v", err)
	}
	appendCipher := newTestDocxAppendCipher(t)
	firstRuntime := &docsService{client: client, store: st, accountID: "feishu:cli_test", appendCipher: appendCipher, appendExecutionOwner: testDocxAppendExecutionOwner, now: time.Now}
	err := firstRuntime.appendTextBlocks(context.Background(), requestID, authorizedDocForTest(documentToken), "frozen paragraph")
	if !errors.Is(err, errDocxAppendOutcomeUnknown) {
		t.Fatalf("first append error = %v, want outcome_unknown after two lost responses", err)
	}

	restartedStore, err := store.Open(store.PlatformFeishu)
	if err != nil {
		t.Fatalf("reopen Feishu store: %v", err)
	}
	defer func() {
		if err := restartedStore.Close(); err != nil {
			t.Fatalf("close restarted Feishu store: %v", err)
		}
	}()
	secondRuntime := &docsService{client: client, store: restartedStore, accountID: "feishu:cli_test", appendCipher: appendCipher, appendExecutionOwner: testDocxAppendExecutionOwner, now: time.Now}
	if err := secondRuntime.appendTextBlocks(context.Background(), requestID, authorizedDocForTest(documentToken), "frozen paragraph"); err != nil {
		t.Fatalf("restarted append returned error: %v", err)
	}

	mu.Lock()
	if getCalls != 1 {
		mu.Unlock()
		t.Fatalf("child-count lookups = %d, want the restarted runtime to reuse the frozen request without another lookup", getCalls)
	}
	if len(postCalls) != 3 {
		mu.Unlock()
		t.Fatalf("append calls = %d, want two uncertain calls plus one restart reconciliation", len(postCalls))
	}
	for i, call := range postCalls[1:] {
		if call.clientToken != postCalls[0].clientToken {
			mu.Unlock()
			t.Fatalf("append call %d client_token = %q, want frozen %q", i+2, call.clientToken, postCalls[0].clientToken)
		}
		if string(call.body) != string(postCalls[0].body) {
			mu.Unlock()
			t.Fatalf("append call %d body = %s, want frozen %s", i+2, call.body, postCalls[0].body)
		}
	}
	mu.Unlock()

	err = secondRuntime.appendTextBlocks(context.Background(), requestID, authorizedDocForTest(documentToken), "different paragraph")
	if !errors.Is(err, store.ErrFeishuDocxAppendOperationConflict) {
		t.Fatalf("conflicting replay error = %v, want ErrFeishuDocxAppendOperationConflict", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if getCalls != 1 || len(postCalls) != 3 {
		t.Fatalf("conflicting replay performed remote work: get_calls=%d post_calls=%d", getCalls, len(postCalls))
	}
}

func TestDocsAppendPermanentGrantStartupRecoveryUsesDurablePayload(t *testing.T) {
	const documentToken = "doxcndirectstartup123"
	type appendCall struct {
		body json.RawMessage
	}
	var (
		mu        sync.Mutex
		getCalls  int
		postCalls []appendCall
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/oauth/v3/token" || r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal":
			writeJSON(t, w, map[string]any{
				"code": 0, "msg": "ok", "tenant_access_token": "tenant-token", "expire": 7200,
			})
		case r.URL.Path == "/open-apis/docx/v1/documents/"+documentToken+"/blocks/"+documentToken+"/children" && r.Method == http.MethodGet:
			mu.Lock()
			getCalls++
			mu.Unlock()
			writeJSON(t, w, map[string]any{
				"code": 0, "msg": "ok", "data": map[string]any{"items": []any{}, "has_more": false},
			})
		case r.URL.Path == "/open-apis/docx/v1/documents/"+documentToken+"/blocks/"+documentToken+"/children" && r.Method == http.MethodPost:
			var body json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode append request: %v", err)
			}
			mu.Lock()
			postCalls = append(postCalls, appendCall{body: append(json.RawMessage(nil), body...)})
			call := len(postCalls)
			mu.Unlock()
			if call <= 2 {
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
	approver := &fakeApprovalRequester{result: OperationApprovalResult{Status: OperationApprovalStatusGranted}}
	st, registered, _ := newDocsToolsForTest(t, client, Config{Docs: DocsToolsConfig{Enabled: true, AllowWrite: true}}, approver)
	result := findDocsTool(t, registered, appendToolName).Execute(groupDocsContext(), tooltypes.Call{
		ID:        "append_direct_startup",
		Name:      appendToolName,
		Arguments: json.RawMessage(`{"token":"doxcndirectstartup123","content":"durable private paragraph"}`),
	})
	if result.IsError {
		t.Fatalf("direct append result = %#v", result)
	}
	var output writeOutput
	if err := json.Unmarshal([]byte(result.Content), &output); err != nil {
		t.Fatalf("decode direct append result: %v", err)
	}
	if output.Status != "outcome_unknown" || output.RequestID == "" {
		t.Fatalf("direct append output = %#v, want recoverable outcome_unknown", output)
	}
	operation, err := st.GetFeishuDocxAppendOperation(output.RequestID, "feishu:cli_test")
	if err != nil || operation.State != store.FeishuDocxAppendOperationStateOutcomeUnknown || operation.EnvelopeCiphertext == "" {
		t.Fatalf("durable append operation = %#v err=%v", operation, err)
	}
	if strings.Contains(operation.EnvelopeCiphertext, "durable private paragraph") {
		t.Fatal("durable append ledger stored plaintext document content")
	}

	restartedStore, err := store.Open(store.PlatformFeishu)
	if err != nil {
		t.Fatalf("reopen Feishu store: %v", err)
	}
	defer func() {
		if err := restartedStore.Close(); err != nil {
			t.Fatalf("close restarted Feishu store: %v", err)
		}
	}()
	recoveryAccess := grantedResourceAccessController("req_recovery_access")
	restartedTools := NewDocsTools(client, restartedStore, "feishu:cli_test", Config{Docs: DocsToolsConfig{Enabled: true, AllowWrite: true}}, approver, recoveryAccess, newTestDocxAppendCipher(t), testDocxAppendExecutionOwner)
	if err := RecoverDocxAppendOperations(context.Background(), restartedTools); err != nil {
		t.Fatalf("RecoverDocxAppendOperations returned error: %v", err)
	}

	recovered, err := restartedStore.GetFeishuDocxAppendOperation(output.RequestID, "feishu:cli_test")
	if err != nil || recovered.State != store.FeishuDocxAppendOperationStateSucceeded || recovered.EnvelopeCiphertext != "" {
		t.Fatalf("recovered append operation = %#v err=%v", recovered, err)
	}
	workflow, err := restartedStore.GetWorkflowRequest(output.RequestID, "feishu:cli_test")
	if err != nil || workflow.State != store.WorkflowRequestStateSucceeded {
		t.Fatalf("recovered append workflow = %#v err=%v", workflow, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if getCalls != 1 || len(postCalls) != 3 {
		t.Fatalf("startup recovery calls get=%d post=%d, want 1/3", getCalls, len(postCalls))
	}
	for i := 1; i < len(postCalls); i++ {
		if string(postCalls[i].body) != string(postCalls[0].body) {
			t.Fatalf("startup recovery call %d body = %s, want frozen %s", i+1, postCalls[i].body, postCalls[0].body)
		}
	}
}

func TestDocsCreateInitialContentRecoveryReusesFrozenAppendEnvelope(t *testing.T) {
	const documentToken = "doxcncreateinitialrestart123"
	type appendCall struct {
		body json.RawMessage
	}
	var (
		mu          sync.Mutex
		createCalls int
		getCalls    int
		postCalls   []appendCall
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/oauth/v3/token" || r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal":
			writeJSON(t, w, map[string]any{
				"code": 0, "msg": "ok", "tenant_access_token": "tenant-token", "expire": 7200,
			})
		case r.URL.Path == "/open-apis/docx/v1/documents" && r.Method == http.MethodPost:
			mu.Lock()
			createCalls++
			mu.Unlock()
			writeJSON(t, w, map[string]any{
				"code": 0,
				"msg":  "ok",
				"data": map[string]any{"document": map[string]any{"document_id": documentToken}},
			})
		case r.URL.Path == "/open-apis/docx/v1/documents/"+documentToken+"/blocks/"+documentToken+"/children" && r.Method == http.MethodGet:
			mu.Lock()
			getCalls++
			call := getCalls
			mu.Unlock()
			items := []any{}
			if call > 1 {
				items = []any{map[string]any{"block_id": "concurrent-block"}}
			}
			writeJSON(t, w, map[string]any{
				"code": 0, "msg": "ok", "data": map[string]any{"items": items, "has_more": false},
			})
		case r.URL.Path == "/open-apis/docx/v1/documents/"+documentToken+"/blocks/"+documentToken+"/children" && r.Method == http.MethodPost:
			var body json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode initial append request: %v", err)
			}
			mu.Lock()
			postCalls = append(postCalls, appendCall{body: append(json.RawMessage(nil), body...)})
			call := len(postCalls)
			mu.Unlock()
			if call <= 2 {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Fatalf("hijack initial append response: %v", err)
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
	st, registered, _ := newDocsToolsForTest(t, client, Config{Docs: DocsToolsConfig{Enabled: true, AllowWrite: true}}, &fakeApprovalRequester{})
	seedDocsCreateApprovalWorkflow(t, st, "req_create_initial_restart")
	payload := json.RawMessage(`{"title":"Restart-safe document","content":"private initial body","folder_token":"fld_token","chat_id":"oc_chat","actor_open_id":"ou_requester"}`)
	firstExecutor := findDocsTool(t, registered, createToolName).(OperationApprovalExecutor)
	first, err := firstExecutor.ExecuteApproved(context.Background(), "req_create_initial_restart", payload)
	if err != nil || !first.Warning || !strings.Contains(first.Message, "初始正文") {
		t.Fatalf("first create execution = %#v err=%v, want unknown initial-content warning", first, err)
	}

	restartedStore, err := store.Open(store.PlatformFeishu)
	if err != nil {
		t.Fatalf("reopen Feishu store: %v", err)
	}
	defer func() {
		if err := restartedStore.Close(); err != nil {
			t.Fatalf("close restarted Feishu store: %v", err)
		}
	}()
	restartedTools := NewDocsTools(
		client,
		restartedStore,
		"feishu:cli_test",
		Config{Docs: DocsToolsConfig{Enabled: true, AllowWrite: true}},
		&fakeApprovalRequester{},
		grantedResourceAccessController("req_recovery_access"),
		newTestDocxAppendCipher(t),
		testDocxAppendExecutionOwner,
	)
	restartedExecutor := findDocsTool(t, restartedTools, createToolName).(OperationApprovalExecutor)
	recovered, err := restartedExecutor.ExecuteApproved(context.Background(), "req_create_initial_restart", payload)
	if err != nil || recovered.Warning || !strings.Contains(recovered.Message, "飞书文档已创建") {
		t.Fatalf("recovered create execution = %#v err=%v, want completed recovery", recovered, err)
	}

	operation, err := restartedStore.GetFeishuDocxAppendOperation("req_create_initial_restart", "feishu:cli_test")
	if err != nil || operation.State != store.FeishuDocxAppendOperationStateSucceeded || operation.EnvelopeCiphertext != "" {
		t.Fatalf("recovered initial append operation = %#v err=%v", operation, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if createCalls != 1 || getCalls != 1 || len(postCalls) != 3 {
		t.Fatalf("create recovery calls create=%d get=%d post=%d, want 1/1/3", createCalls, getCalls, len(postCalls))
	}
	for i := 1; i < len(postCalls); i++ {
		if string(postCalls[i].body) != string(postCalls[0].body) {
			t.Fatalf("initial append recovery call %d body = %s, want frozen %s", i+1, postCalls[i].body, postCalls[0].body)
		}
	}
}

func TestDocsAppendFirstUncertainThenRejectedRemainsOutcomeUnknown(t *testing.T) {
	const (
		documentToken = "doxcnuncertainthenrejected123"
		requestID     = "req_append_uncertain_rejected"
	)
	var postCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/oauth/v3/token" || r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal":
			writeJSON(t, w, map[string]any{
				"code": 0, "msg": "ok", "tenant_access_token": "tenant-token", "expire": 7200,
			})
		case r.URL.Path == "/open-apis/docx/v1/documents/"+documentToken+"/blocks/"+documentToken+"/children" && r.Method == http.MethodGet:
			writeJSON(t, w, map[string]any{
				"code": 0, "msg": "ok", "data": map[string]any{"items": []any{}, "has_more": false},
			})
		case r.URL.Path == "/open-apis/docx/v1/documents/"+documentToken+"/blocks/"+documentToken+"/children" && r.Method == http.MethodPost:
			postCalls++
			if postCalls == 1 {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Fatalf("hijack first append response: %v", err)
				}
				_ = conn.Close()
				return
			}
			writeJSON(t, w, map[string]any{"code": 99991672, "msg": "reconciliation rejected"})
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
	st := openDocsFolderTestStore(t)
	acquireTestDocxAppendRuntimeLease(t, st, testDocxAppendExecutionOwner, time.Now().UTC())
	if _, err := st.CreateWorkflowRequest(store.WorkflowRequest{
		ID:        requestID,
		AccountID: "feishu:cli_test",
		Kind:      store.WorkflowRequestKindFeishuDocsAppend,
		State:     store.WorkflowRequestStateExecuting,
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed append workflow: %v", err)
	}
	service := &docsService{
		client:               client,
		store:                st,
		accountID:            "feishu:cli_test",
		appendCipher:         newTestDocxAppendCipher(t),
		appendExecutionOwner: testDocxAppendExecutionOwner,
		now:                  time.Now,
	}
	err := service.appendTextBlocks(context.Background(), requestID, authorizedDocForTest(documentToken), "private paragraph")
	if !errors.Is(err, errDocxAppendOutcomeUnknown) {
		t.Fatalf("append error = %v, want outcome_unknown after uncertain first response", err)
	}
	operation, err := st.GetFeishuDocxAppendOperation(requestID, "feishu:cli_test")
	if err != nil || operation.State != store.FeishuDocxAppendOperationStateOutcomeUnknown || operation.EnvelopeCiphertext == "" {
		t.Fatalf("append operation = %#v err=%v, want recoverable outcome_unknown", operation, err)
	}
	if postCalls != 2 {
		t.Fatalf("append calls = %d, want first uncertain call plus one reconciliation", postCalls)
	}
}

func TestDocsAppendLaterRecoveryRejectionRemainsOutcomeUnknown(t *testing.T) {
	const (
		documentToken = "doxcnlaterrecoveryrejected123"
		requestID     = "req_append_later_recovery_rejected"
	)
	var postCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/oauth/v3/token" || r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal":
			writeJSON(t, w, map[string]any{
				"code": 0, "msg": "ok", "tenant_access_token": "tenant-token", "expire": 7200,
			})
		case r.URL.Path == "/open-apis/docx/v1/documents/"+documentToken+"/blocks/"+documentToken+"/children" && r.Method == http.MethodGet:
			writeJSON(t, w, map[string]any{
				"code": 0, "msg": "ok", "data": map[string]any{"items": []any{}, "has_more": false},
			})
		case r.URL.Path == "/open-apis/docx/v1/documents/"+documentToken+"/blocks/"+documentToken+"/children" && r.Method == http.MethodPost:
			postCalls++
			if postCalls <= 2 {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Fatalf("hijack append response: %v", err)
				}
				_ = conn.Close()
				return
			}
			writeJSON(t, w, map[string]any{"code": 99991672, "msg": "later recovery rejected"})
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
	st := openDocsFolderTestStore(t)
	acquireTestDocxAppendRuntimeLease(t, st, testDocxAppendExecutionOwner, time.Now().UTC())
	if _, err := st.CreateWorkflowRequest(store.WorkflowRequest{
		ID:        requestID,
		AccountID: "feishu:cli_test",
		Kind:      store.WorkflowRequestKindFeishuDocsAppend,
		State:     store.WorkflowRequestStateExecuting,
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed append workflow: %v", err)
	}
	service := &docsService{
		client:               client,
		store:                st,
		accountID:            "feishu:cli_test",
		appendCipher:         newTestDocxAppendCipher(t),
		appendExecutionOwner: testDocxAppendExecutionOwner,
		now:                  time.Now,
	}
	if err := service.appendTextBlocks(context.Background(), requestID, authorizedDocForTest(documentToken), "private paragraph"); !errors.Is(err, errDocxAppendOutcomeUnknown) {
		t.Fatalf("initial append error = %v, want outcome_unknown", err)
	}
	if err := service.appendTextBlocks(context.Background(), requestID, authorizedDocForTest(documentToken), "private paragraph"); !errors.Is(err, errDocxAppendOutcomeUnknown) {
		t.Fatalf("later recovery error = %v, want outcome_unknown after rejection", err)
	}
	operation, err := st.GetFeishuDocxAppendOperation(requestID, "feishu:cli_test")
	if err != nil || operation.State != store.FeishuDocxAppendOperationStateOutcomeUnknown || operation.EnvelopeCiphertext == "" {
		t.Fatalf("append operation after later rejection = %#v err=%v, want recoverable outcome_unknown", operation, err)
	}
	if postCalls != 3 {
		t.Fatalf("append calls = %d, want two initial uncertain calls plus one later recovery", postCalls)
	}
}

func TestDocsAppendConcurrentRecoveryHasSingleRemoteCaller(t *testing.T) {
	const (
		documentToken = "doxcnconcurrentrecovery123"
		requestID     = "req_append_concurrent_recovery"
	)
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/oauth/v3/token" || r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal":
			writeJSON(t, w, map[string]any{
				"code": 0, "msg": "ok", "tenant_access_token": "tenant-token", "expire": 7200,
			})
		case r.URL.Path == "/open-apis/docx/v1/documents/"+documentToken+"/blocks/"+documentToken+"/children" && r.Method == http.MethodPost:
			entered <- struct{}{}
			<-release
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
	firstStore := openDocsFolderTestStore(t)
	acquireTestDocxAppendRuntimeLease(t, firstStore, testDocxAppendExecutionOwner, time.Now().UTC())
	secondStore, err := store.Open(store.PlatformFeishu)
	if err != nil {
		t.Fatalf("open second Feishu store: %v", err)
	}
	defer func() {
		if err := secondStore.Close(); err != nil {
			t.Fatalf("close second Feishu store: %v", err)
		}
	}()
	now := time.Date(2026, time.August, 4, 20, 0, 0, 0, time.UTC)
	if _, err := firstStore.CreateWorkflowRequest(store.WorkflowRequest{
		ID: requestID, AccountID: "feishu:cli_test", Kind: store.WorkflowRequestKindFeishuDocsAppend,
		State: store.WorkflowRequestStateExecuting, CreatedAt: now,
	}); err != nil {
		t.Fatalf("seed append workflow: %v", err)
	}
	cipher := newTestDocxAppendCipher(t)
	seedService := &docsService{store: firstStore, accountID: "feishu:cli_test", appendCipher: cipher, appendExecutionOwner: testDocxAppendExecutionOwner, now: func() time.Time { return now }}
	blocks := textBlocks("private body")
	operation, _, err := seedService.docxAppendOperationIdentity(requestID, authorizedDocForTest(documentToken), blocks)
	if err != nil {
		t.Fatalf("docxAppendOperationIdentity returned error: %v", err)
	}
	envelope := docxAppendEnvelope{DocumentToken: documentToken, ClientToken: operation.ClientToken, Index: 0, Children: blocks}
	raw, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal append envelope: %v", err)
	}
	operation.InsertionIndex = 0
	operation.EnvelopeHash, err = remoteOperationPayloadHash(envelope)
	if err != nil {
		t.Fatalf("hash append envelope: %v", err)
	}
	operation.EnvelopeCiphertext, err = cipher.encrypt(operation, raw)
	if err != nil {
		t.Fatalf("encrypt append envelope: %v", err)
	}
	if _, _, err := firstStore.PrepareFeishuDocxAppendOperation(operation); err != nil {
		t.Fatalf("prepare append operation: %v", err)
	}
	const seedExecutionToken = "execution_seed"
	if _, claimed, err := firstStore.StartFeishuDocxAppendOperation(requestID, "feishu:cli_test", testDocxAppendExecutionOwner, seedExecutionToken, now.Add(time.Second), time.Minute); err != nil || !claimed {
		t.Fatalf("start append operation claimed=%t err=%v", claimed, err)
	}
	if _, err := firstStore.MarkFeishuDocxAppendOperationOutcomeUnknown(requestID, "feishu:cli_test", seedExecutionToken, "lost_response", now.Add(2*time.Second)); err != nil {
		t.Fatalf("mark append outcome unknown: %v", err)
	}

	firstService := &docsService{client: client, store: firstStore, accountID: "feishu:cli_test", appendCipher: cipher, appendExecutionOwner: testDocxAppendExecutionOwner, now: time.Now}
	secondService := &docsService{client: client, store: secondStore, accountID: "feishu:cli_test", appendCipher: cipher, appendExecutionOwner: testDocxAppendExecutionOwner, now: time.Now}
	firstResult := make(chan error, 1)
	go func() {
		firstResult <- firstService.appendTextBlocks(context.Background(), requestID, authorizedDocForTest(documentToken), "private body")
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("first recovery did not reach the remote append")
	}

	secondResult := make(chan error, 1)
	go func() {
		secondResult <- secondService.appendTextBlocks(context.Background(), requestID, authorizedDocForTest(documentToken), "private body")
	}()
	select {
	case <-entered:
		close(release)
		<-firstResult
		<-secondResult
		t.Fatal("a concurrent recovery issued a second remote append while the first recovery was active")
	case err := <-secondResult:
		if !errors.Is(err, errDocxAppendOutcomeUnknown) {
			close(release)
			<-firstResult
			t.Fatalf("concurrent recovery error = %v, want outcome_unknown without a remote call", err)
		}
	case <-time.After(5 * time.Second):
		close(release)
		<-firstResult
		t.Fatal("concurrent recovery neither returned nor reached the remote append")
	}
	close(release)
	if err := <-firstResult; err != nil {
		t.Fatalf("first recovery returned error: %v", err)
	}
	final, err := firstStore.GetFeishuDocxAppendOperation(requestID, "feishu:cli_test")
	if err != nil || final.State != store.FeishuDocxAppendOperationStateSucceeded || final.EnvelopeCiphertext != "" {
		t.Fatalf("final append operation = %#v err=%v", final, err)
	}
}

func TestDocsAppendLateRejectionAfterTakeoverSuccessUsesSucceededLedger(t *testing.T) {
	const requestID = "req_append_late_rejection_after_success"
	st := openDocsFolderTestStore(t)
	now := time.Date(2026, time.August, 4, 20, 10, 0, 0, time.UTC)
	if _, err := st.CreateWorkflowRequest(store.WorkflowRequest{
		ID: requestID, AccountID: "feishu:cli_test", Kind: store.WorkflowRequestKindFeishuDocsAppend,
		State: store.WorkflowRequestStateExecuting, CreatedAt: now,
	}); err != nil {
		t.Fatalf("seed append workflow: %v", err)
	}
	if _, err := st.AcquireFeishuAccountRuntimeLease("feishu:cli_test", "runtime_old", now, time.Minute); err != nil {
		t.Fatalf("acquire old runtime lease: %v", err)
	}
	operation := store.FeishuDocxAppendOperation{
		RequestID: requestID, AccountID: "feishu:cli_test", ChatID: "oc_chat",
		ActorOpenID: "ou_requester", ActorUserID: "u_requester",
		DocumentToken: "doxcn_late_rejection", ClientToken: "stable-client-token",
		InsertionIndex: 0, PayloadHash: "logical-payload-hash", EnvelopeHash: "envelope-hash",
		EnvelopeCiphertext: "v1.encrypted-envelope", CreatedAt: now,
	}
	if _, _, err := st.PrepareFeishuDocxAppendOperation(operation); err != nil {
		t.Fatalf("prepare append operation: %v", err)
	}
	started, claimed, err := st.StartFeishuDocxAppendOperation(requestID, "feishu:cli_test", "runtime_old", "execution_old", now.Add(time.Second), time.Minute)
	if err != nil || !claimed {
		t.Fatalf("start old append execution = %#v claimed=%t err=%v", started, claimed, err)
	}
	if err := st.ReleaseFeishuAccountRuntimeLease("feishu:cli_test", "runtime_old"); err != nil {
		t.Fatalf("release old runtime lease: %v", err)
	}
	if _, err := st.AcquireFeishuAccountRuntimeLease("feishu:cli_test", "runtime_new", now.Add(2*time.Second), time.Minute); err != nil {
		t.Fatalf("acquire replacement runtime lease: %v", err)
	}
	if _, claimed, err := st.ClaimFeishuDocxAppendOperationRecovery(requestID, "feishu:cli_test", "runtime_new", "execution_new", now.Add(3*time.Second), time.Minute); err != nil || !claimed {
		t.Fatalf("claim replacement append execution claimed=%t err=%v", claimed, err)
	}
	if _, err := st.MarkFeishuDocxAppendOperationSucceeded(requestID, "feishu:cli_test", "execution_new", now.Add(4*time.Second)); err != nil {
		t.Fatalf("complete replacement append execution: %v", err)
	}

	service := &docsService{store: st, accountID: "feishu:cli_test", now: func() time.Time { return now.Add(5 * time.Second) }}
	if err := service.recordDocxAppendFailure(context.Background(), started, "execution_old", "remote_rejected", errors.New("late rejection")); err != nil {
		t.Fatalf("late rejection after authoritative success returned %v, want success", err)
	}
	final, err := st.GetFeishuDocxAppendOperation(requestID, "feishu:cli_test")
	if err != nil || final.State != store.FeishuDocxAppendOperationStateSucceeded {
		t.Fatalf("final append operation = %#v err=%v, want succeeded", final, err)
	}
}

func TestDocsWorkflowBestEffortDoesNotDowngradeSucceededAppendLedger(t *testing.T) {
	const requestID = "req_append_workflow_stale_partial"
	st := openDocsFolderTestStore(t)
	now := time.Date(2026, time.August, 4, 20, 12, 0, 0, time.UTC)
	seedFeishuWorkflow := store.WorkflowRequest{
		ID: requestID, AccountID: "feishu:cli_test", Kind: store.WorkflowRequestKindFeishuDocsAppend,
		State: store.WorkflowRequestStateExecuting, CreatedAt: now,
	}
	if _, err := st.CreateWorkflowRequest(seedFeishuWorkflow); err != nil {
		t.Fatalf("seed append workflow: %v", err)
	}
	acquireTestDocxAppendRuntimeLease(t, st, testDocxAppendExecutionOwner, now)
	operation := store.FeishuDocxAppendOperation{
		RequestID: requestID, AccountID: "feishu:cli_test", ChatID: "oc_chat",
		ActorOpenID: "ou_requester", ActorUserID: "u_requester",
		DocumentToken: "doxcn_workflow_stale_partial", ClientToken: "stable-client-token",
		InsertionIndex: 0, PayloadHash: "logical-payload-hash", EnvelopeHash: "envelope-hash",
		EnvelopeCiphertext: "v1.encrypted-envelope", CreatedAt: now,
	}
	if _, _, err := st.PrepareFeishuDocxAppendOperation(operation); err != nil {
		t.Fatalf("prepare append operation: %v", err)
	}
	if _, claimed, err := st.StartFeishuDocxAppendOperation(requestID, "feishu:cli_test", testDocxAppendExecutionOwner, "execution_success", now.Add(time.Second), time.Minute); err != nil || !claimed {
		t.Fatalf("start append execution claimed=%t err=%v", claimed, err)
	}
	if _, err := st.MarkFeishuDocxAppendOperationSucceeded(requestID, "feishu:cli_test", "execution_success", now.Add(2*time.Second)); err != nil {
		t.Fatalf("complete append execution: %v", err)
	}
	if err := st.UpdateWorkflowRequestState(requestID, "feishu:cli_test", store.WorkflowRequestStateSucceeded, now.Add(3*time.Second)); err != nil {
		t.Fatalf("record workflow success: %v", err)
	}

	service := &docsService{store: st, accountID: "feishu:cli_test", now: func() time.Time { return now.Add(4 * time.Second) }}
	service.updateWorkflowBestEffort(context.Background(), requestID, store.WorkflowRequestStatePartial)
	workflow, err := st.GetWorkflowRequest(requestID, "feishu:cli_test")
	if err != nil || workflow.State != store.WorkflowRequestStateSucceeded {
		t.Fatalf("workflow after stale partial update = %#v err=%v, want succeeded", workflow, err)
	}
}

func TestDocsCreateFailedInitialAppendRecoveryKeepsWorkflowPartial(t *testing.T) {
	const requestID = "req_create_failed_initial_append"
	st := openDocsFolderTestStore(t)
	now := time.Date(2026, time.August, 4, 20, 15, 0, 0, time.UTC)
	acquireTestDocxAppendRuntimeLease(t, st, testDocxAppendExecutionOwner, now)
	if _, err := st.CreateWorkflowRequest(store.WorkflowRequest{
		ID: requestID, AccountID: "feishu:cli_test", Kind: store.WorkflowRequestKindFeishuDocsCreate,
		State: store.WorkflowRequestStateExecuting, CreatedAt: now,
	}); err != nil {
		t.Fatalf("seed create workflow: %v", err)
	}
	operation := store.FeishuDocxAppendOperation{
		RequestID: requestID, AccountID: "feishu:cli_test", ChatID: "oc_chat",
		ActorOpenID: "ou_requester", ActorUserID: "u_requester",
		DocumentToken: "doxcn_failed_initial_append", ClientToken: "stable-client-token",
		InsertionIndex: 0, PayloadHash: "logical-payload-hash", EnvelopeHash: "envelope-hash",
		EnvelopeCiphertext: "v1.encrypted-envelope", CreatedAt: now,
	}
	if _, _, err := st.PrepareFeishuDocxAppendOperation(operation); err != nil {
		t.Fatalf("prepare initial append operation: %v", err)
	}
	const executionToken = "execution_failed_initial"
	if _, claimed, err := st.StartFeishuDocxAppendOperation(requestID, "feishu:cli_test", testDocxAppendExecutionOwner, executionToken, now.Add(time.Second), time.Minute); err != nil || !claimed {
		t.Fatalf("start initial append operation claimed=%t err=%v", claimed, err)
	}
	if _, err := st.MarkFeishuDocxAppendOperationFailed(requestID, "feishu:cli_test", executionToken, "remote_rejected", now.Add(2*time.Second)); err != nil {
		t.Fatalf("mark initial append failed: %v", err)
	}
	registered := NewDocsTools(
		&lark.Client{},
		st,
		"feishu:cli_test",
		Config{Docs: DocsToolsConfig{Enabled: true, AllowWrite: true}},
		&fakeApprovalRequester{},
		grantedResourceAccessController("req_access"),
		newTestDocxAppendCipher(t),
		testDocxAppendExecutionOwner,
	)
	if err := RecoverDocxAppendOperations(context.Background(), registered); err != nil {
		t.Fatalf("RecoverDocxAppendOperations returned error: %v", err)
	}
	workflow, err := st.GetWorkflowRequest(requestID, "feishu:cli_test")
	if err != nil || workflow.State != store.WorkflowRequestStatePartial {
		t.Fatalf("create workflow after failed initial append = %#v err=%v, want partial", workflow, err)
	}
	recoverable, err := st.ListRecoverableFeishuDocxAppendOperations("feishu:cli_test", 10)
	if err != nil || len(recoverable) != 0 {
		t.Fatalf("recovery list after terminal create partial reconciliation = %#v err=%v, want empty", recoverable, err)
	}
}

func TestDocsCreateRequestIDRecoveryReturnsCreatedDocumentWhenInitialAppendRemainsUnknown(t *testing.T) {
	const (
		requestID     = "req_create_recovery_append_unknown"
		documentToken = "doxcn_create_recovery_unknown"
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/oauth/v3/token" || r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal":
			writeJSON(t, w, map[string]any{
				"code": 0, "msg": "ok", "tenant_access_token": "tenant-token", "expire": 7200,
			})
		case r.URL.Path == "/open-apis/docx/v1/documents/"+documentToken+"/blocks/"+documentToken+"/children" && r.Method == http.MethodPost:
			writeJSON(t, w, map[string]any{"code": 99991672, "msg": "recovery rejected"})
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
	st := openDocsFolderTestStore(t)
	now := time.Date(2026, time.August, 4, 20, 20, 0, 0, time.UTC)
	acquireTestDocxAppendRuntimeLease(t, st, testDocxAppendExecutionOwner, time.Now().UTC())
	if _, err := st.CreateWorkflowRequest(store.WorkflowRequest{
		ID: requestID, AccountID: "feishu:cli_test", Kind: store.WorkflowRequestKindFeishuDocsCreate,
		State: store.WorkflowRequestStatePartial, CreatedAt: now,
	}); err != nil {
		t.Fatalf("seed create workflow: %v", err)
	}
	remoteOperation, err := st.PrepareFeishuRemoteOperation(store.FeishuRemoteOperation{
		RequestID: requestID, AccountID: "feishu:cli_test", OperationKind: store.FeishuRemoteOperationKindDocumentCreate,
		ChatID: "oc_chat", ActorOpenID: "ou_requester", ActorUserID: "u_requester",
		ParentResourceType: "folder", ParentResourceToken: "fld_token", BindingParentToken: "fld_token", RequestedName: "Recovered document",
		PayloadHash: "create-payload-hash", InitialContentRequested: true, RemoteResourceType: "docx", CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("prepare document remote operation: %v", err)
	}
	remoteOperation, claimed, err := st.StartFeishuRemoteOperation(requestID, "feishu:cli_test", now.Add(time.Second))
	if err != nil || !claimed {
		t.Fatalf("start document remote operation = %#v claimed=%t err=%v", remoteOperation, claimed, err)
	}
	remoteOperation, err = st.RecordFeishuRemoteOperationSuccess(requestID, "feishu:cli_test", "docx", documentToken, "https://docs.feishu.cn/docx/"+documentToken, now.Add(2*time.Second))
	if err != nil {
		t.Fatalf("record document remote success: %v", err)
	}
	if _, err := st.MarkFeishuRemoteOperationPersisted(requestID, "feishu:cli_test", now.Add(3*time.Second)); err != nil {
		t.Fatalf("mark document remote operation persisted: %v", err)
	}

	cipher := newTestDocxAppendCipher(t)
	seedService := &docsService{store: st, accountID: "feishu:cli_test", appendCipher: cipher, appendExecutionOwner: testDocxAppendExecutionOwner, now: func() time.Time { return now }}
	blocks := textBlocks("private initial body")
	appendOperation, _, err := seedService.docxAppendOperationIdentity(requestID, authorizedDocForTest(documentToken), blocks)
	if err != nil {
		t.Fatalf("docxAppendOperationIdentity returned error: %v", err)
	}
	envelope := docxAppendEnvelope{DocumentToken: documentToken, ClientToken: appendOperation.ClientToken, Index: 0, Children: blocks}
	raw, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal append envelope: %v", err)
	}
	appendOperation.InsertionIndex = 0
	appendOperation.EnvelopeHash, err = remoteOperationPayloadHash(envelope)
	if err != nil {
		t.Fatalf("hash append envelope: %v", err)
	}
	appendOperation.EnvelopeCiphertext, err = cipher.encrypt(appendOperation, raw)
	if err != nil {
		t.Fatalf("encrypt append envelope: %v", err)
	}
	if _, _, err := st.PrepareFeishuDocxAppendOperation(appendOperation); err != nil {
		t.Fatalf("prepare append operation: %v", err)
	}
	if _, claimed, err := st.StartFeishuDocxAppendOperation(requestID, "feishu:cli_test", testDocxAppendExecutionOwner, "execution_old", now.Add(4*time.Second), time.Minute); err != nil || !claimed {
		t.Fatalf("start append operation claimed=%t err=%v", claimed, err)
	}
	if _, err := st.MarkFeishuDocxAppendOperationOutcomeUnknown(requestID, "feishu:cli_test", "execution_old", "lost_response", now.Add(5*time.Second)); err != nil {
		t.Fatalf("mark append outcome unknown: %v", err)
	}

	service := &docsService{
		client: client, store: st, accountID: "feishu:cli_test", resourceAccess: grantedResourceAccessController("req_access"),
		appendCipher: cipher, appendExecutionOwner: testDocxAppendExecutionOwner, now: time.Now,
	}
	out, err := service.recoverDocumentCreate(groupDocsContext(), Actor{OpenID: "ou_requester", UserID: "u_requester"}, ChatContext{ChatID: "oc_chat"}, requestID)
	if err != nil {
		t.Fatalf("recoverDocumentCreate returned error: %v", err)
	}
	if out.DocumentID != documentToken || out.Status != "created" || !strings.Contains(out.Warning, "初始正文") {
		t.Fatalf("recovered document output = %#v, want created document with initial-content warning", out)
	}
	workflow, err := st.GetWorkflowRequest(requestID, "feishu:cli_test")
	if err != nil || workflow.State != store.WorkflowRequestStatePartial {
		t.Fatalf("workflow after request_id recovery = %#v err=%v, want partial", workflow, err)
	}
}

func TestDocsCreateRecoveryDoesNotDowngradeConcurrentAppendSuccess(t *testing.T) {
	const (
		requestID     = "req_create_concurrent_append_success"
		documentToken = "doxcn_create_concurrent_success"
	)
	st := openDocsFolderTestStore(t)
	now := time.Date(2026, time.August, 4, 20, 25, 0, 0, time.UTC)
	acquireTestDocxAppendRuntimeLease(t, st, testDocxAppendExecutionOwner, now)
	if _, err := st.CreateWorkflowRequest(store.WorkflowRequest{
		ID: requestID, AccountID: "feishu:cli_test", Kind: store.WorkflowRequestKindFeishuDocsCreate,
		State: store.WorkflowRequestStateExecuting, CreatedAt: now,
	}); err != nil {
		t.Fatalf("seed create workflow: %v", err)
	}
	if _, err := st.PrepareFeishuRemoteOperation(store.FeishuRemoteOperation{
		RequestID: requestID, AccountID: "feishu:cli_test", OperationKind: store.FeishuRemoteOperationKindDocumentCreate,
		ChatID: "oc_chat", ActorOpenID: "ou_requester", ActorUserID: "u_requester",
		ParentResourceType: "folder", ParentResourceToken: "fld_token", BindingParentToken: "fld_token", RequestedName: "Concurrent recovery document",
		PayloadHash: "create-payload-hash", InitialContentRequested: true, RemoteResourceType: "docx", CreatedAt: now,
	}); err != nil {
		t.Fatalf("prepare document remote operation: %v", err)
	}
	if _, claimed, err := st.StartFeishuRemoteOperation(requestID, "feishu:cli_test", now.Add(time.Second)); err != nil || !claimed {
		t.Fatalf("start document remote operation claimed=%t err=%v", claimed, err)
	}
	if _, err := st.RecordFeishuRemoteOperationSuccess(requestID, "feishu:cli_test", "docx", documentToken, "https://docs.feishu.cn/docx/"+documentToken, now.Add(2*time.Second)); err != nil {
		t.Fatalf("record document remote success: %v", err)
	}
	if _, err := st.MarkFeishuRemoteOperationPersisted(requestID, "feishu:cli_test", now.Add(3*time.Second)); err != nil {
		t.Fatalf("mark document remote operation persisted: %v", err)
	}
	appendOperation := store.FeishuDocxAppendOperation{
		RequestID: requestID, AccountID: "feishu:cli_test", ChatID: "oc_chat",
		ActorOpenID: "ou_requester", ActorUserID: "u_requester",
		DocumentToken: documentToken, ClientToken: "stable-client-token",
		InsertionIndex: 0, PayloadHash: "logical-payload-hash", EnvelopeHash: "envelope-hash",
		EnvelopeCiphertext: "v1.encrypted-envelope", CreatedAt: now,
	}
	if _, _, err := st.PrepareFeishuDocxAppendOperation(appendOperation); err != nil {
		t.Fatalf("prepare initial append operation: %v", err)
	}
	if _, claimed, err := st.StartFeishuDocxAppendOperation(requestID, "feishu:cli_test", testDocxAppendExecutionOwner, "execution_old", now.Add(4*time.Second), time.Minute); err != nil || !claimed {
		t.Fatalf("start initial append operation claimed=%t err=%v", claimed, err)
	}
	if _, err := st.MarkFeishuDocxAppendOperationOutcomeUnknown(requestID, "feishu:cli_test", "execution_old", "lost_response", now.Add(5*time.Second)); err != nil {
		t.Fatalf("mark initial append outcome unknown: %v", err)
	}

	accessErr := errors.New("simulated stale access failure")
	access := &blockingDocxRecoveryAccess{entered: make(chan struct{}), release: make(chan struct{}), err: accessErr}
	service := &docsService{
		store: st, accountID: "feishu:cli_test", resourceAccess: access,
		appendCipher: newTestDocxAppendCipher(t), appendExecutionOwner: testDocxAppendExecutionOwner,
		now: func() time.Time { return now.Add(10 * time.Second) },
	}
	type recoveryResult struct {
		out writeOutput
		err error
	}
	result := make(chan recoveryResult, 1)
	go func() {
		out, err := service.recoverDocumentCreate(
			groupDocsContext(),
			Actor{OpenID: "ou_requester", UserID: "u_requester"},
			ChatContext{ChatID: "oc_chat"},
			requestID,
		)
		result <- recoveryResult{out: out, err: err}
	}()
	select {
	case <-access.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("document recovery did not reach the blocked docx access check")
	}
	if _, claimed, err := st.ClaimFeishuDocxAppendOperationRecovery(requestID, "feishu:cli_test", testDocxAppendExecutionOwner, "execution_winner", now.Add(6*time.Second), time.Minute); err != nil || !claimed {
		close(access.release)
		t.Fatalf("claim concurrent append recovery claimed=%t err=%v", claimed, err)
	}
	if _, err := st.MarkFeishuDocxAppendOperationSucceeded(requestID, "feishu:cli_test", "execution_winner", now.Add(7*time.Second)); err != nil {
		close(access.release)
		t.Fatalf("complete concurrent append recovery: %v", err)
	}
	if err := st.UpdateWorkflowRequestState(requestID, "feishu:cli_test", store.WorkflowRequestStateSucceeded, now.Add(8*time.Second)); err != nil {
		close(access.release)
		t.Fatalf("record concurrent workflow success: %v", err)
	}
	close(access.release)
	var recovered recoveryResult
	select {
	case recovered = <-result:
	case <-time.After(5 * time.Second):
		t.Fatal("document recovery did not finish after the blocked access check was released")
	}
	if recovered.err != nil {
		t.Fatalf("recoverDocumentCreate returned error: %v", recovered.err)
	}
	if recovered.out.DocumentID != documentToken || recovered.out.Status != "created" || recovered.out.Warning != "" {
		t.Fatalf("recovered document output = %#v, want authoritative concurrent success without warning", recovered.out)
	}
	workflow, err := st.GetWorkflowRequest(requestID, "feishu:cli_test")
	if err != nil || workflow.State != store.WorkflowRequestStateSucceeded {
		t.Fatalf("workflow after concurrent append success = %#v err=%v, want succeeded", workflow, err)
	}
}

func TestDocsCreateRecoveryWarnsWhenLegacySuccessHasNoAppendLedger(t *testing.T) {
	const (
		requestID     = "req_create_legacy_success_without_append_ledger"
		documentToken = "doxcn_create_legacy_without_append_ledger"
	)
	st := openDocsFolderTestStore(t)
	now := time.Date(2026, time.August, 4, 20, 28, 0, 0, time.UTC)
	if _, err := st.CreateWorkflowRequest(store.WorkflowRequest{
		ID: requestID, AccountID: "feishu:cli_test", Kind: store.WorkflowRequestKindFeishuDocsCreate,
		State: store.WorkflowRequestStateSucceeded, CreatedAt: now,
	}); err != nil {
		t.Fatalf("seed create workflow: %v", err)
	}
	if _, err := st.PrepareFeishuRemoteOperation(store.FeishuRemoteOperation{
		RequestID: requestID, AccountID: "feishu:cli_test", OperationKind: store.FeishuRemoteOperationKindDocumentCreate,
		ChatID: "oc_chat", ActorOpenID: "ou_requester", ActorUserID: "u_requester",
		ParentResourceType: "folder", ParentResourceToken: "fld_token", BindingParentToken: "fld_token", RequestedName: "Legacy document",
		PayloadHash: "create-payload-hash", InitialContentRequested: true, RemoteResourceType: "docx", CreatedAt: now,
	}); err != nil {
		t.Fatalf("prepare document remote operation: %v", err)
	}
	if _, claimed, err := st.StartFeishuRemoteOperation(requestID, "feishu:cli_test", now.Add(time.Second)); err != nil || !claimed {
		t.Fatalf("start document remote operation claimed=%t err=%v", claimed, err)
	}
	if _, err := st.RecordFeishuRemoteOperationSuccess(requestID, "feishu:cli_test", "docx", documentToken, "https://docs.feishu.cn/docx/"+documentToken, now.Add(2*time.Second)); err != nil {
		t.Fatalf("record document remote success: %v", err)
	}
	if _, err := st.MarkFeishuRemoteOperationPersisted(requestID, "feishu:cli_test", now.Add(3*time.Second)); err != nil {
		t.Fatalf("mark document remote operation persisted: %v", err)
	}

	service := &docsService{
		store: st, accountID: "feishu:cli_test", resourceAccess: grantedResourceAccessController("req_access"),
		now: func() time.Time { return now.Add(4 * time.Second) },
	}
	out, err := service.recoverDocumentCreate(
		groupDocsContext(),
		Actor{OpenID: "ou_requester", UserID: "u_requester"},
		ChatContext{ChatID: "oc_chat"},
		requestID,
	)
	if err != nil {
		t.Fatalf("recoverDocumentCreate returned error: %v", err)
	}
	if out.DocumentID != documentToken || out.Status != "created" || !strings.Contains(out.Warning, "初始正文") {
		t.Fatalf("recovered legacy document output = %#v, want created document with unconfirmed initial-content warning", out)
	}
	workflow, err := st.GetWorkflowRequest(requestID, "feishu:cli_test")
	if err != nil || workflow.State != store.WorkflowRequestStatePartial {
		t.Fatalf("legacy create workflow without append ledger = %#v err=%v, want partial", workflow, err)
	}
}

func TestDocsCreateRecoveryDoesNotHidePersistenceConflictBehindLegacySuccess(t *testing.T) {
	const (
		requestID     = "req_create_legacy_success_conflict"
		documentToken = "doxcn_create_legacy_conflict"
	)
	st := openDocsFolderTestStore(t)
	now := time.Date(2026, time.August, 4, 20, 30, 0, 0, time.UTC)
	if _, err := st.CreateWorkflowRequest(store.WorkflowRequest{
		ID: requestID, AccountID: "feishu:cli_test", Kind: store.WorkflowRequestKindFeishuDocsCreate,
		State: store.WorkflowRequestStateSucceeded, CreatedAt: now,
	}); err != nil {
		t.Fatalf("seed create workflow: %v", err)
	}
	if _, err := st.SaveFeishuBotResource(store.FeishuBotResource{
		AccountID: "feishu:cli_test", ResourceType: "docx", ResourceToken: documentToken,
		ParentToken: "fld_other", Name: "Other request document", SourceRequestID: "req_other",
		CreatedAt: now,
	}); err != nil {
		t.Fatalf("seed conflicting Bot resource: %v", err)
	}
	if _, err := st.PrepareFeishuRemoteOperation(store.FeishuRemoteOperation{
		RequestID: requestID, AccountID: "feishu:cli_test", OperationKind: store.FeishuRemoteOperationKindDocumentCreate,
		ChatID: "oc_chat", ActorOpenID: "ou_requester", ActorUserID: "u_requester",
		ParentResourceType: "folder", ParentResourceToken: "fld_token", BindingParentToken: "fld_token", RequestedName: "Legacy document",
		PayloadHash: "create-payload-hash", InitialContentRequested: true, RemoteResourceType: "docx", CreatedAt: now,
	}); err != nil {
		t.Fatalf("prepare document remote operation: %v", err)
	}
	if _, claimed, err := st.StartFeishuRemoteOperation(requestID, "feishu:cli_test", now.Add(time.Second)); err != nil || !claimed {
		t.Fatalf("start document remote operation claimed=%t err=%v", claimed, err)
	}
	if _, err := st.RecordFeishuRemoteOperationSuccess(requestID, "feishu:cli_test", "docx", documentToken, "https://docs.feishu.cn/docx/"+documentToken, now.Add(2*time.Second)); err != nil {
		t.Fatalf("record document remote success: %v", err)
	}
	service := &docsService{
		store: st, accountID: "feishu:cli_test", resourceAccess: grantedResourceAccessController("req_access"),
		now: func() time.Time { return now.Add(3 * time.Second) },
	}
	out, err := service.recoverDocumentCreate(
		groupDocsContext(),
		Actor{OpenID: "ou_requester", UserID: "u_requester"},
		ChatContext{ChatID: "oc_chat"},
		requestID,
	)
	if err != nil {
		t.Fatalf("recoverDocumentCreate returned error: %v", err)
	}
	if out.DocumentID != documentToken || out.Status != "partial" || !strings.Contains(out.Warning, "后续处理失败") {
		t.Fatalf("recovered document output = %#v, want visible persistence-conflict warning", out)
	}
	workflow, err := st.GetWorkflowRequest(requestID, "feishu:cli_test")
	if err != nil || workflow.State != store.WorkflowRequestStatePartial {
		t.Fatalf("workflow after persistence conflict = %#v err=%v, want partial", workflow, err)
	}
}

func TestDocsAppendRecoveryCipherMismatchKeepsNonterminalLedgerAndPartialWorkflow(t *testing.T) {
	const (
		documentToken = "doxcnciphermismatch123"
		requestID     = "req_append_cipher_mismatch"
	)
	st := openDocsFolderTestStore(t)
	now := time.Date(2026, time.August, 4, 19, 30, 0, 0, time.UTC)
	acquireTestDocxAppendRuntimeLease(t, st, "runtime_old", now)
	if _, err := st.CreateWorkflowRequest(store.WorkflowRequest{
		ID:        requestID,
		AccountID: "feishu:cli_test",
		Kind:      store.WorkflowRequestKindFeishuDocsAppend,
		State:     store.WorkflowRequestStateExecuting,
		CreatedAt: now,
	}); err != nil {
		t.Fatalf("seed append workflow: %v", err)
	}
	goodCipher := newTestDocxAppendCipher(t)
	service := &docsService{store: st, accountID: "feishu:cli_test", appendCipher: goodCipher, appendExecutionOwner: testDocxAppendExecutionOwner, now: func() time.Time { return now }}
	blocks := textBlocks("private body")
	operation, _, err := service.docxAppendOperationIdentity(requestID, authorizedDocForTest(documentToken), blocks)
	if err != nil {
		t.Fatalf("docxAppendOperationIdentity returned error: %v", err)
	}
	envelope := docxAppendEnvelope{
		DocumentToken: documentToken,
		ClientToken:   operation.ClientToken,
		Index:         0,
		Children:      blocks,
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal append envelope: %v", err)
	}
	operation.InsertionIndex = 0
	operation.EnvelopeHash, err = remoteOperationPayloadHash(envelope)
	if err != nil {
		t.Fatalf("hash append envelope: %v", err)
	}
	operation.EnvelopeCiphertext, err = goodCipher.encrypt(operation, raw)
	if err != nil {
		t.Fatalf("encrypt append envelope: %v", err)
	}
	if _, _, err := st.PrepareFeishuDocxAppendOperation(operation); err != nil {
		t.Fatalf("prepare append operation: %v", err)
	}
	if _, claimed, err := st.StartFeishuDocxAppendOperation(requestID, "feishu:cli_test", "runtime_old", "execution_old", now.Add(time.Second), time.Minute); err != nil || !claimed {
		t.Fatalf("start append operation claimed=%t err=%v", claimed, err)
	}

	wrongCipher, err := NewDocxAppendEnvelopeCipher("different-app-secret", "feishu:cli_test")
	if err != nil {
		t.Fatalf("create mismatched cipher: %v", err)
	}
	registered := NewDocsTools(
		&lark.Client{},
		st,
		"feishu:cli_test",
		Config{Docs: DocsToolsConfig{Enabled: true, AllowWrite: true}},
		&fakeApprovalRequester{},
		grantedResourceAccessController("req_access"),
		wrongCipher,
		"runtime_old",
	)
	if err := RecoverDocxAppendOperations(context.Background(), registered); err != nil {
		t.Fatalf("RecoverDocxAppendOperations returned error: %v", err)
	}
	preserved, err := st.GetFeishuDocxAppendOperation(requestID, "feishu:cli_test")
	if err != nil || preserved.State != store.FeishuDocxAppendOperationStateRemoteStarted || preserved.EnvelopeCiphertext == "" {
		t.Fatalf("preserved append operation = %#v err=%v", preserved, err)
	}
	workflow, err := st.GetWorkflowRequest(requestID, "feishu:cli_test")
	if err != nil || workflow.State != store.WorkflowRequestStatePartial {
		t.Fatalf("workflow after cipher mismatch = %#v err=%v, want partial", workflow, err)
	}
}
