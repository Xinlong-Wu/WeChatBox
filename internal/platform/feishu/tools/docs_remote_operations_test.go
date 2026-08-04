package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"

	"lingobridge/internal/store"
	tooltypes "lingobridge/internal/tools"
)

func TestFeishuCreateResponseUncertainClassification(t *testing.T) {
	for _, tt := range []struct {
		name        string
		apiResp     *larkcore.ApiResp
		callErr     error
		missingData bool
		want        bool
	}{
		{name: "missing response", want: true},
		{name: "transport error", apiResp: &larkcore.ApiResp{StatusCode: http.StatusOK}, callErr: errors.New("connection reset"), want: true},
		{name: "missing success data", apiResp: &larkcore.ApiResp{StatusCode: http.StatusOK}, missingData: true, want: true},
		{name: "request timeout", apiResp: &larkcore.ApiResp{StatusCode: http.StatusRequestTimeout}, want: true},
		{name: "rate limited", apiResp: &larkcore.ApiResp{StatusCode: http.StatusTooManyRequests}, want: true},
		{name: "server error", apiResp: &larkcore.ApiResp{StatusCode: http.StatusBadGateway}, want: true},
		{name: "definite client error", apiResp: &larkcore.ApiResp{StatusCode: http.StatusBadRequest}, want: false},
		{name: "complete success transport", apiResp: &larkcore.ApiResp{StatusCode: http.StatusOK}, want: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := feishuCreateResponseUncertain(tt.apiResp, tt.callErr, tt.missingData); got != tt.want {
				t.Fatalf("feishuCreateResponseUncertain = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestRemoteCreateResourceClaimableRejectsAnotherRequest(t *testing.T) {
	st := openDocsFolderTestStore(t)
	now := time.Date(2026, time.August, 4, 11, 0, 0, 0, time.UTC)
	if _, err := st.SaveFeishuBotResource(store.FeishuBotResource{
		AccountID:       "feishu:cli_test",
		ResourceType:    "docx",
		ResourceToken:   "doxcn_claimed",
		ParentToken:     "fld_parent",
		SourceRequestID: "req_other",
		CreatedAt:       now,
	}); err != nil {
		t.Fatalf("SaveFeishuBotResource returned error: %v", err)
	}
	operation := store.FeishuRemoteOperation{
		RequestID: "req_current", AccountID: "feishu:cli_test", ChatID: "oc_chat",
	}
	claimable, err := remoteCreateResourceClaimable(st, operation, "docx", "doxcn_claimed")
	if err != nil {
		t.Fatalf("remoteCreateResourceClaimable returned error: %v", err)
	}
	if claimable {
		t.Fatal("resource claimed by another request was accepted")
	}
}

func TestRecordReconciledFeishuRemoteCreateTreatsConcurrentClaimAsUnknown(t *testing.T) {
	st := openDocsFolderTestStore(t)
	now := time.Date(2026, time.August, 4, 11, 30, 0, 0, time.UTC)
	const (
		accountID     = "feishu:cli_test"
		resourceToken = "doxcn_reconciled_concurrent_claim"
	)

	prepareStarted := func(requestID string) store.FeishuRemoteOperation {
		t.Helper()
		request, err := st.CreateWorkflowRequest(store.WorkflowRequest{
			ID:        requestID,
			AccountID: accountID,
			Kind:      store.WorkflowRequestKindFeishuDocsCreate,
			State:     store.WorkflowRequestStateExecuting,
			CreatedAt: now,
		})
		if err != nil {
			t.Fatalf("CreateWorkflowRequest(%q) returned error: %v", requestID, err)
		}
		operation, err := st.PrepareFeishuRemoteOperation(store.FeishuRemoteOperation{
			RequestID:           request.ID,
			AccountID:           request.AccountID,
			OperationKind:       store.FeishuRemoteOperationKindDocumentCreate,
			ChatID:              "oc_chat",
			ActorOpenID:         "ou_requester",
			ParentResourceType:  "folder",
			ParentResourceToken: "fld_parent",
			BindingParentToken:  "fld_parent",
			RequestedName:       requestID,
			PayloadHash:         "payload_" + requestID,
			RemoteResourceType:  "docx",
			CreatedAt:           now,
		})
		if err != nil {
			t.Fatalf("PrepareFeishuRemoteOperation(%q) returned error: %v", requestID, err)
		}
		operation, claimed, err := st.StartFeishuRemoteOperation(operation.RequestID, operation.AccountID, now.Add(time.Second))
		if err != nil || !claimed {
			t.Fatalf("StartFeishuRemoteOperation(%q) operation=%#v claimed=%t err=%v", requestID, operation, claimed, err)
		}
		return operation
	}

	first := prepareStarted("req_reconciled_claim_first")
	second := prepareStarted("req_reconciled_claim_second")
	if _, err := st.RecordFeishuRemoteOperationSuccess(
		first.RequestID,
		first.AccountID,
		"docx",
		resourceToken,
		"https://docs.feishu.cn/docx/"+resourceToken,
		now.Add(2*time.Second),
	); err != nil {
		t.Fatalf("first RecordFeishuRemoteOperationSuccess returned error: %v", err)
	}

	got, status, err := recordReconciledFeishuRemoteCreate(context.Background(), st, second, remoteCreateCandidate{
		ResourceType: "docx",
		Token:        resourceToken,
		URL:          "https://docs.feishu.cn/docx/" + resourceToken,
	}, now.Add(3*time.Second))
	if err != nil {
		t.Fatalf("recordReconciledFeishuRemoteCreate returned error: %v", err)
	}
	if status != "outcome_unknown" || got.State != store.FeishuRemoteOperationStateOutcomeUnknown || got.RemoteResourceToken != "" {
		t.Fatalf("conflicting reconciled candidate result operation=%#v status=%q", got, status)
	}
	if got.LastErrorCategory != "candidate_claimed_by_another_request" {
		t.Fatalf("LastErrorCategory = %q", got.LastErrorCategory)
	}
}

func TestDocumentCreateReconciliationDoesNotClaimCandidateMatchingCompetingOperation(t *testing.T) {
	st := openDocsFolderTestStore(t)
	now := time.Date(2026, time.August, 4, 11, 45, 0, 0, time.UTC)
	const (
		accountID     = "feishu:cli_test"
		parentToken   = "fld_parent"
		documentName  = "Shared name"
		documentToken = "doxcn_competing_request"
	)

	prepareStarted := func(requestID string, startedAt time.Time) store.FeishuRemoteOperation {
		t.Helper()
		request, err := st.CreateWorkflowRequest(store.WorkflowRequest{
			ID:        requestID,
			AccountID: accountID,
			Kind:      store.WorkflowRequestKindFeishuDocsCreate,
			State:     store.WorkflowRequestStateExecuting,
			CreatedAt: now,
		})
		if err != nil {
			t.Fatalf("CreateWorkflowRequest(%q) returned error: %v", requestID, err)
		}
		operation, err := st.PrepareFeishuRemoteOperation(store.FeishuRemoteOperation{
			RequestID:           request.ID,
			AccountID:           request.AccountID,
			OperationKind:       store.FeishuRemoteOperationKindDocumentCreate,
			ChatID:              "oc_chat",
			ActorOpenID:         "ou_requester",
			ParentResourceType:  "folder",
			ParentResourceToken: parentToken,
			BindingParentToken:  parentToken,
			RequestedName:       documentName,
			PayloadHash:         "payload_" + requestID,
			RemoteResourceType:  "docx",
			CreatedAt:           now,
		})
		if err != nil {
			t.Fatalf("PrepareFeishuRemoteOperation(%q) returned error: %v", requestID, err)
		}
		operation, claimed, err := st.StartFeishuRemoteOperation(operation.RequestID, operation.AccountID, startedAt)
		if err != nil || !claimed {
			t.Fatalf("StartFeishuRemoteOperation(%q) operation=%#v claimed=%t err=%v", requestID, operation, claimed, err)
		}
		return operation
	}

	current := prepareStarted("req_reconcile_current", now.Add(time.Second))
	current, err := st.MarkFeishuRemoteOperationReconcileRequired(current.RequestID, current.AccountID, "uncertain_create_response", now.Add(2*time.Second))
	if err != nil {
		t.Fatalf("MarkFeishuRemoteOperationReconcileRequired returned error: %v", err)
	}
	_ = prepareStarted("req_reconcile_competing", now.Add(1500*time.Millisecond))

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/v3/token", "/open-apis/auth/v3/tenant_access_token/internal":
			writeJSON(t, w, map[string]any{"code": 0, "msg": "ok", "tenant_access_token": "tenant-token", "expire": 7200})
		case "/open-apis/drive/explorer/v2/root_folder/meta":
			writeJSON(t, w, map[string]any{"code": 0, "msg": "ok", "data": map[string]any{"token": "fld_root", "user_id": "app_owner"}})
		case "/open-apis/drive/v1/files":
			writeJSON(t, w, map[string]any{
				"code": 0,
				"msg":  "ok",
				"data": map[string]any{
					"files": []any{map[string]any{
						"token": documentToken, "name": documentName, "type": "docx",
						"parent_token": parentToken, "owner_id": "app_owner",
						"created_time": strconv.FormatInt(now.Add(2*time.Second).Unix(), 10),
					}},
					"has_more": false,
				},
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
	service := &docsService{
		client:                client,
		store:                 st,
		accountID:             accountID,
		remoteReconcileDelays: nil,
		now:                   func() time.Time { return now.Add(3 * time.Second) },
	}
	reconciled, status, err := service.reconcileDocumentCreate(context.Background(), current, AuthorizedResource{
		AccountID:           current.AccountID,
		ActorOpenID:         current.ActorOpenID,
		ChatID:              current.ChatID,
		ResourceType:        current.ParentResourceType,
		ResourceToken:       current.ParentResourceToken,
		EffectivePermission: ResourcePermissionWrite,
	})
	if err != nil {
		t.Fatalf("reconcileDocumentCreate returned error: %v", err)
	}
	if status != "outcome_unknown" || reconciled.State != store.FeishuRemoteOperationStateOutcomeUnknown || reconciled.RemoteResourceToken != "" {
		t.Fatalf("competing reconciliation operation=%#v status=%q, want conservative outcome_unknown", reconciled, status)
	}
	if reconciled.LastErrorCategory != "candidate_matches_competing_request" {
		t.Fatalf("LastErrorCategory = %q", reconciled.LastErrorCategory)
	}
}

func TestValidateRemoteOperationParentAccessRejectsCapabilityMismatch(t *testing.T) {
	operation := store.FeishuRemoteOperation{
		AccountID:           "feishu:cli_test",
		ChatID:              "oc_chat",
		ActorOpenID:         "ou_actor",
		ActorUserID:         "u_actor",
		ParentResourceType:  "folder",
		ParentResourceToken: "fld_parent",
	}
	valid := AuthorizedResource{
		AccountID:           operation.AccountID,
		ActorOpenID:         operation.ActorOpenID,
		ActorUserID:         operation.ActorUserID,
		ChatID:              operation.ChatID,
		ResourceType:        operation.ParentResourceType,
		ResourceToken:       operation.ParentResourceToken,
		EffectivePermission: ResourcePermissionWrite,
	}
	if err := validateRemoteOperationParentAccess(operation, valid); err != nil {
		t.Fatalf("valid parent capability rejected: %v", err)
	}
	tests := map[string]func(*AuthorizedResource){
		"account":       func(got *AuthorizedResource) { got.AccountID = "feishu:other" },
		"chat":          func(got *AuthorizedResource) { got.ChatID = "oc_other" },
		"actor open id": func(got *AuthorizedResource) { got.ActorOpenID = "ou_other" },
		"actor user id": func(got *AuthorizedResource) { got.ActorUserID = "u_other" },
		"resource type": func(got *AuthorizedResource) { got.ResourceType = "docx" },
		"token":         func(got *AuthorizedResource) { got.ResourceToken = "fld_other" },
		"permission":    func(got *AuthorizedResource) { got.EffectivePermission = ResourcePermissionRead },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			authorized := valid
			mutate(&authorized)
			if err := validateRemoteOperationParentAccess(operation, authorized); err == nil {
				t.Fatalf("mismatched capability accepted: %#v", authorized)
			}
		})
	}
}

func TestReconcileFeishuRemoteCreateRequiresUniqueCandidate(t *testing.T) {
	startedAt := time.Now().UTC().Truncate(time.Second)
	for _, tt := range []struct {
		name        string
		candidateN  int
		wantOutcome string
	}{
		{name: "none", candidateN: 0, wantOutcome: "no_candidate"},
		{name: "one", candidateN: 1, wantOutcome: "unique_candidate"},
		{name: "multiple", candidateN: 2, wantOutcome: "multiple_candidates"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/oauth/v3/token", "/open-apis/auth/v3/tenant_access_token/internal":
					writeJSON(t, w, map[string]any{"code": 0, "msg": "ok", "tenant_access_token": "tenant-token", "expire": 7200})
				case "/open-apis/drive/v1/files":
					files := make([]any, 0, tt.candidateN)
					for i := 0; i < tt.candidateN; i++ {
						files = append(files, map[string]any{
							"token": "doxcn_candidate_" + strconv.Itoa(i), "name": "Quarterly plan", "type": "docx",
							"parent_token": "fld_parent", "owner_id": "app_owner",
							"created_time": strconv.FormatInt(startedAt.Unix(), 10),
						})
					}
					writeJSON(t, w, map[string]any{"code": 0, "msg": "ok", "data": map[string]any{"files": files, "has_more": false}})
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
			operation := store.FeishuRemoteOperation{
				AccountID:           "feishu:cli_test",
				ChatID:              "oc_chat",
				ActorOpenID:         "ou_actor",
				ActorUserID:         "u_actor",
				ParentResourceType:  "folder",
				ParentResourceToken: "fld_parent",
				RequestedName:       "Quarterly plan",
				RemoteResourceType:  "docx",
				RemoteCallStartedAt: startedAt,
			}
			candidate, outcome, err := reconcileFeishuRemoteCreate(context.Background(), client, operation, AuthorizedResource{
				AccountID:           operation.AccountID,
				ActorOpenID:         operation.ActorOpenID,
				ActorUserID:         operation.ActorUserID,
				ChatID:              operation.ChatID,
				ResourceType:        operation.ParentResourceType,
				ResourceToken:       operation.ParentResourceToken,
				EffectivePermission: ResourcePermissionWrite,
			}, "app_owner", nil)
			if err != nil {
				t.Fatalf("reconcileFeishuRemoteCreate returned error: %v", err)
			}
			if outcome != tt.wantOutcome {
				t.Fatalf("outcome = %q, want %q", outcome, tt.wantOutcome)
			}
			if tt.candidateN == 1 && candidate.Token != "doxcn_candidate_0" {
				t.Fatalf("candidate = %#v", candidate)
			}
			if tt.candidateN != 1 && candidate.Token != "" {
				t.Fatalf("non-unique candidate = %#v, want empty", candidate)
			}
		})
	}
}

func TestDocsCreateRecoveryRepairsRemoteSucceededOperationWithoutCreateCall(t *testing.T) {
	st, tools, _ := newDocsToolsForTest(t, &lark.Client{}, Config{Docs: DocsToolsConfig{Enabled: true, AllowWrite: true}}, &fakeApprovalRequester{})
	now := time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC)
	request, err := st.CreateWorkflowRequest(store.WorkflowRequest{
		ID:        "req_document_restart",
		AccountID: "feishu:cli_test",
		Kind:      store.WorkflowRequestKindFeishuDocsCreate,
		State:     store.WorkflowRequestStateExecuting,
		CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("CreateWorkflowRequest returned error: %v", err)
	}
	operation, err := st.PrepareFeishuRemoteOperation(store.FeishuRemoteOperation{
		RequestID:           request.ID,
		AccountID:           request.AccountID,
		OperationKind:       store.FeishuRemoteOperationKindDocumentCreate,
		ChatID:              "oc_chat",
		ActorOpenID:         "ou_requester",
		ActorUserID:         "u_requester",
		ParentResourceType:  "folder",
		ParentResourceToken: "fld_token",
		BindingParentToken:  "fld_token",
		RequestedName:       "Recovered document",
		PayloadHash:         "payload_hash",
		RemoteResourceType:  "docx",
		CreatedAt:           now,
	})
	if err != nil {
		t.Fatalf("PrepareFeishuRemoteOperation returned error: %v", err)
	}
	operation, claimed, err := st.StartFeishuRemoteOperation(operation.RequestID, operation.AccountID, now.Add(time.Second))
	if err != nil || !claimed {
		t.Fatalf("StartFeishuRemoteOperation operation=%#v claimed=%t err=%v", operation, claimed, err)
	}
	if _, err := st.RecordFeishuRemoteOperationSuccess(
		operation.RequestID,
		operation.AccountID,
		"docx",
		"doxcn_restart",
		"https://docs.feishu.cn/docx/doxcn_restart",
		now.Add(2*time.Second),
	); err != nil {
		t.Fatalf("RecordFeishuRemoteOperationSuccess returned error: %v", err)
	}

	args, _ := json.Marshal(createArgs{RequestID: request.ID})
	result := findDocsTool(t, tools, createToolName).Execute(groupDocsContext(), tooltypes.Call{
		ID: "recover", Name: createToolName, Arguments: args,
	})
	if result.IsError {
		t.Fatalf("recovery result = %#v", result)
	}
	var output writeOutput
	if err := json.Unmarshal([]byte(result.Content), &output); err != nil {
		t.Fatalf("unmarshal recovery output: %v", err)
	}
	if output.Status != "created" || output.DocumentID != "doxcn_restart" || output.RequestID != request.ID {
		t.Fatalf("recovery output = %#v", output)
	}
	remote, err := st.GetFeishuRemoteOperation(request.ID, request.AccountID)
	if err != nil || remote.State != store.FeishuRemoteOperationStatePersisted {
		t.Fatalf("remote operation = %#v err=%v", remote, err)
	}
	if _, err := st.GetFeishuBotResource(request.AccountID, "docx", output.DocumentID); err != nil {
		t.Fatalf("repaired Bot resource: %v", err)
	}
	if _, err := st.GetFeishuChatDocument(request.AccountID, "oc_chat", output.DocumentID); err != nil {
		t.Fatalf("repaired chat document: %v", err)
	}
	workflow, err := st.GetWorkflowRequest(request.ID, request.AccountID)
	if err != nil || workflow.State != store.WorkflowRequestStateSucceeded {
		t.Fatalf("repaired workflow = %#v err=%v", workflow, err)
	}
}

func TestDocsCreateKnownRemoteSuccessSurvivesLocalLedgerWriteFailure(t *testing.T) {
	const documentToken = "doxcn_known_remote_success"
	var createCalls, listCalls int
	createdTime := strconv.FormatInt(time.Now().UTC().Unix(), 10)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/oauth/v3/token" || r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal":
			writeJSON(t, w, map[string]any{"code": 0, "msg": "ok", "tenant_access_token": "tenant-token", "expire": 7200})
		case r.URL.Path == "/open-apis/docx/v1/documents" && r.Method == http.MethodPost:
			createCalls++
			writeJSON(t, w, map[string]any{
				"code": 0,
				"msg":  "ok",
				"data": map[string]any{"document": map[string]any{"document_id": documentToken}},
			})
		case r.URL.Path == "/open-apis/drive/explorer/v2/root_folder/meta":
			writeJSON(t, w, map[string]any{"code": 0, "msg": "ok", "data": map[string]any{"token": "fld_root", "user_id": "app_owner"}})
		case r.URL.Path == "/open-apis/drive/v1/files" && r.Method == http.MethodGet:
			listCalls++
			writeJSON(t, w, map[string]any{
				"code": 0,
				"msg":  "ok",
				"data": map[string]any{
					"files": []any{map[string]any{
						"token": documentToken, "name": "Ledger failure document", "type": "docx",
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
	st, tools, _ := newDocsToolsForTest(t, client, Config{Docs: DocsToolsConfig{Enabled: true, AllowWrite: true}}, &fakeApprovalRequester{
		result: OperationApprovalResult{Status: OperationApprovalStatusGranted},
	})
	dropFailure := rejectRemoteOperationSuccessUpdates(t, st)
	tool := findDocsTool(t, tools, createToolName).(docsTool)
	tool.service.remoteReconcileDelays = []time.Duration{}

	first := tool.Execute(groupDocsContext(), tooltypes.Call{
		ID: "create", Name: createToolName,
		Arguments: json.RawMessage(`{"title":"Ledger failure document","folder_token":"fld_token"}`),
	})
	if first.IsError {
		t.Fatalf("first Execute result = %#v, want recoverable partial success", first)
	}
	var partial writeOutput
	if err := json.Unmarshal([]byte(first.Content), &partial); err != nil {
		t.Fatalf("unmarshal partial output: %v", err)
	}
	if partial.Status != "partial" || partial.RequestID == "" || partial.DocumentID != documentToken ||
		!strings.Contains(partial.Warning, "请勿重新创建") || !strings.Contains(partial.Retry, "request_id") {
		t.Fatalf("partial output = %#v", partial)
	}
	if createCalls != 1 {
		t.Fatalf("create calls = %d, want 1", createCalls)
	}
	workflow, err := st.GetWorkflowRequest(partial.RequestID, "feishu:cli_test")
	if err != nil || workflow.State != store.WorkflowRequestStatePartial {
		t.Fatalf("partial workflow = %#v err=%v", workflow, err)
	}

	dropFailure()
	retryArgs, _ := json.Marshal(createArgs{RequestID: partial.RequestID})
	second := tool.Execute(groupDocsContext(), tooltypes.Call{ID: "recover", Name: createToolName, Arguments: retryArgs})
	if second.IsError {
		t.Fatalf("recovery result = %#v", second)
	}
	var recovered writeOutput
	if err := json.Unmarshal([]byte(second.Content), &recovered); err != nil {
		t.Fatalf("unmarshal recovered output: %v", err)
	}
	if recovered.Status != "created" || recovered.DocumentID != documentToken || recovered.RequestID != partial.RequestID ||
		createCalls != 1 || listCalls != 1 {
		t.Fatalf("recovered output/calls = %#v create=%d list=%d", recovered, createCalls, listCalls)
	}
}

func TestDocsFolderCreateKnownRemoteSuccessSurvivesLocalLedgerWriteFailure(t *testing.T) {
	const folderToken = "fld_known_remote_success"
	var createCalls, listCalls, shareCalls int
	createdTime := strconv.FormatInt(time.Now().UTC().Unix(), 10)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/oauth/v3/token" || r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal":
			writeDocsFolderJSON(t, w, tenantTokenResponse())
		case r.URL.Path == "/open-apis/drive/explorer/v2/root_folder/meta":
			writeDocsFolderJSON(t, w, map[string]any{"code": 0, "msg": "ok", "data": map[string]any{"token": "fld_root", "user_id": "app_owner"}})
		case r.URL.Path == "/open-apis/drive/v1/files/create_folder" && r.Method == http.MethodPost:
			createCalls++
			writeDocsFolderJSON(t, w, map[string]any{
				"code": 0, "msg": "ok",
				"data": map[string]any{"token": folderToken, "url": "https://docs.feishu.cn/drive/folder/" + folderToken},
			})
		case r.URL.Path == "/open-apis/drive/v1/files" && r.Method == http.MethodGet:
			listCalls++
			writeDocsFolderJSON(t, w, map[string]any{
				"code": 0,
				"msg":  "ok",
				"data": map[string]any{
					"files": []any{map[string]any{
						"token": folderToken, "name": "Ledger failure folder", "type": "folder",
						"parent_token": "fld_root", "url": "https://docs.feishu.cn/drive/folder/" + folderToken,
						"created_time": createdTime, "owner_id": "app_owner",
					}},
					"has_more": false,
				},
			})
		case r.URL.Path == "/open-apis/drive/v1/permissions/"+folderToken+"/members" && r.Method == http.MethodPost:
			shareCalls++
			writeDocsFolderJSON(t, w, map[string]any{"code": 0, "msg": "ok", "data": map[string]any{"member": map[string]any{}}})
		default:
			t.Fatalf("unexpected path: %s method=%s", r.URL.Path, r.Method)
		}
	}))
	defer server.Close()

	st := openDocsFolderTestStore(t)
	tool := findDocsTool(t, NewDocsFolderTools(
		newDocsFolderTestClient(server),
		st,
		"feishu:cli_test",
		Config{Docs: DocsToolsConfig{Enabled: true, AllowWrite: true}},
		grantedResourceAccessController("req_access"),
	), folderCreateToolName).(docsFolderTool)
	tool.service.remoteReconcileDelays = []time.Duration{}
	dropFailure := rejectRemoteOperationSuccessUpdates(t, st)

	first := tool.Execute(groupDocsContext(), tooltypes.Call{
		ID: "create", Name: folderCreateToolName,
		Arguments: json.RawMessage(`{"name":"Ledger failure folder"}`),
	})
	if first.IsError {
		t.Fatalf("first Execute result = %#v, want recoverable partial success", first)
	}
	var partial folderCreateOutput
	if err := json.Unmarshal([]byte(first.Content), &partial); err != nil {
		t.Fatalf("unmarshal partial output: %v", err)
	}
	if partial.Status != "partial" || partial.RequestID == "" || partial.FolderToken != folderToken || partial.Shared ||
		!strings.Contains(partial.Warning, "请勿重新创建") || !strings.Contains(partial.Retry, "request_id") {
		t.Fatalf("partial output = %#v", partial)
	}
	if createCalls != 1 || shareCalls != 0 {
		t.Fatalf("create/share calls = %d/%d, want 1/0", createCalls, shareCalls)
	}
	workflow, err := st.GetWorkflowRequest(partial.RequestID, "feishu:cli_test")
	if err != nil || workflow.State != store.WorkflowRequestStatePartial {
		t.Fatalf("partial workflow = %#v err=%v", workflow, err)
	}

	dropFailure()
	retryArgs, _ := json.Marshal(folderCreateArgs{RequestID: partial.RequestID})
	second := tool.Execute(groupDocsContext(), tooltypes.Call{ID: "recover", Name: folderCreateToolName, Arguments: retryArgs})
	if second.IsError {
		t.Fatalf("recovery result = %#v", second)
	}
	var recovered folderCreateOutput
	if err := json.Unmarshal([]byte(second.Content), &recovered); err != nil {
		t.Fatalf("unmarshal recovered output: %v", err)
	}
	if recovered.Status != "created" || recovered.FolderToken != folderToken || recovered.RequestID != partial.RequestID ||
		!recovered.Shared || createCalls != 1 || listCalls != 1 || shareCalls != 1 {
		t.Fatalf("recovered output/calls = %#v create=%d list=%d share=%d", recovered, createCalls, listCalls, shareCalls)
	}
}

func rejectRemoteOperationSuccessUpdates(t *testing.T, st *store.Store) func() {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(st.DataDir(), "lingobridge.db"))
	if err != nil {
		t.Fatalf("open test sqlite connection: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TRIGGER reject_remote_operation_success
		BEFORE UPDATE OF state ON feishu_remote_operations
		WHEN NEW.state='remote_succeeded'
		BEGIN
			SELECT RAISE(ABORT, 'injected remote result persistence failure');
		END`); err != nil {
		t.Fatalf("create remote operation failure trigger: %v", err)
	}
	dropped := false
	return func() {
		t.Helper()
		if dropped {
			return
		}
		dropped = true
		if _, err := db.Exec(`DROP TRIGGER reject_remote_operation_success`); err != nil {
			t.Fatalf("drop remote operation failure trigger: %v", err)
		}
	}
}
