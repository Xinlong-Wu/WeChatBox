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

	"lingobridge/internal/store"
	tooltypes "lingobridge/internal/tools"
)

func TestDocsFolderToolRegistration(t *testing.T) {
	st := openDocsFolderTestStore(t)
	client := &lark.Client{}
	accountID := "feishu:cli_test"
	if got := NewDocsFolderTools(client, st, accountID, Config{}, nil); len(got) != 0 {
		t.Fatalf("disabled folder tools = %d, want 0", len(got))
	}
	cfg := Config{Docs: DocsToolsConfig{Enabled: true}}
	if got := NewDocsFolderTools(client, st, accountID, cfg, nil); len(got) != 1 || got[0].Spec().Name != folderListToolName {
		t.Fatalf("read-only folder tools = %#v, want list", toolNamesForTest(got))
	}
	cfg.Docs.AllowWrite = true
	if got := NewDocsFolderTools(client, st, accountID, cfg, nil); len(got) != 1 || got[0].Spec().Name != folderListToolName {
		t.Fatalf("write folder tools without resource access = %#v, want list only", toolNamesForTest(got))
	}
	if got := NewDocsFolderTools(client, st, accountID, cfg, grantedResourceAccessController("req_access")); len(got) != 2 || got[0].Spec().Name != folderCreateToolName || got[1].Spec().Name != folderListToolName {
		t.Fatalf("write folder tools = %#v, want create/list", toolNamesForTest(got))
	}
}

func TestDocsFolderCreateUsesApplicationRootAndSharesGroup(t *testing.T) {
	st := openDocsFolderTestStore(t)
	var rootCalls, createCalls, shareCalls int
	var createBody struct {
		Name        string `json:"name"`
		FolderToken string `json:"folder_token"`
	}
	var shareBody struct {
		MemberType string `json:"member_type"`
		MemberID   string `json:"member_id"`
		Perm       string `json:"perm"`
		Type       string `json:"type"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/v3/token", "/open-apis/auth/v3/tenant_access_token/internal":
			writeDocsFolderJSON(t, w, tenantTokenResponse())
		case "/open-apis/drive/explorer/v2/root_folder/meta":
			rootCalls++
			writeDocsFolderJSON(t, w, map[string]any{
				"code": 0,
				"msg":  "Success",
				"data": map[string]any{"token": "fld_root", "id": "root_id", "user_id": "app_owner"},
			})
		case "/open-apis/drive/v1/files/create_folder":
			createCalls++
			if err := json.NewDecoder(r.Body).Decode(&createBody); err != nil {
				t.Fatalf("decode create folder body: %v", err)
			}
			writeDocsFolderJSON(t, w, map[string]any{
				"code": 0,
				"msg":  "success",
				"data": map[string]any{"token": "fld_created", "url": "https://feishu.cn/drive/folder/fld_created"},
			})
		case "/open-apis/drive/v1/permissions/fld_created/members":
			shareCalls++
			if r.URL.Query().Get("type") != "folder" {
				t.Fatalf("permission type = %q, want folder", r.URL.Query().Get("type"))
			}
			if err := json.NewDecoder(r.Body).Decode(&shareBody); err != nil {
				t.Fatalf("decode share body: %v", err)
			}
			writeDocsFolderJSON(t, w, map[string]any{
				"code": 0,
				"msg":  "Success",
				"data": map[string]any{"member": shareBody},
			})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client := newDocsFolderTestClient(server)
	access := grantedResourceAccessController("req_access")
	tool := findDocsTool(t, NewDocsFolderTools(client, st, "feishu:cli_test", Config{Docs: DocsToolsConfig{Enabled: true, AllowWrite: true}}, access), folderCreateToolName)
	result := tool.Execute(groupDocsContext(), tooltypes.Call{
		ID:        "call_folder",
		Name:      folderCreateToolName,
		Arguments: json.RawMessage(`{"name":" Team Docs ","access_request_id":"req_access"}`),
	})
	if result.IsError {
		t.Fatalf("Execute result = %#v, want created folder", result)
	}
	var output folderCreateOutput
	if err := json.Unmarshal([]byte(result.Content), &output); err != nil {
		t.Fatalf("unmarshal folder output: %v", err)
	}
	if output.Status != "created" || !strings.HasPrefix(output.RequestID, "req_") || output.FolderToken != "fld_created" || !output.Default || !output.Shared {
		t.Fatalf("folder output = %#v", output)
	}
	if rootCalls != 1 || createCalls != 1 || shareCalls != 1 || createBody.Name != "Team Docs" || createBody.FolderToken != "fld_root" {
		t.Fatalf("root/create/share calls=%d/%d/%d create=%#v", rootCalls, createCalls, shareCalls, createBody)
	}
	if access.validation.RequestID != "req_access" || access.validation.ResourceType != "folder" || access.validation.ResourceToken != "fld_root" || access.validation.Permission != ResourcePermissionWrite {
		t.Fatalf("access validation = %#v", access.validation)
	}
	if shareBody.MemberType != "openchat" || shareBody.MemberID != "oc_chat" || shareBody.Perm != "full_access" || shareBody.Type != "chat" {
		t.Fatalf("share body = %#v", shareBody)
	}
	folder, err := st.GetFeishuChatFolder("feishu:cli_test", "oc_chat", "fld_created")
	if err != nil || folder.ShareState != store.FeishuFolderShareStateSucceeded || folder.CreateRequestID != output.RequestID {
		t.Fatalf("stored folder = %#v err=%v", folder, err)
	}
	workflow, err := st.GetWorkflowRequest(output.RequestID, "feishu:cli_test")
	if err != nil || workflow.State != store.WorkflowRequestStateSucceeded {
		t.Fatalf("folder workflow = %#v err=%v", workflow, err)
	}
	resource, err := st.GetFeishuBotResource("feishu:cli_test", "folder", "fld_created")
	if err != nil || resource.ParentToken != "fld_root" || resource.SourceRequestID != output.RequestID {
		t.Fatalf("stored Bot folder resource = %#v err=%v", resource, err)
	}
}

func TestDocsFolderCreatePartialRetryOnlyRepeatsSharing(t *testing.T) {
	st := openDocsFolderTestStore(t)
	var createCalls, shareCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/v3/token", "/open-apis/auth/v3/tenant_access_token/internal":
			writeDocsFolderJSON(t, w, tenantTokenResponse())
		case "/open-apis/drive/explorer/v2/root_folder/meta":
			writeDocsFolderJSON(t, w, map[string]any{"code": 0, "msg": "Success", "data": map[string]any{"token": "fld_root"}})
		case "/open-apis/drive/v1/files/create_folder":
			createCalls++
			writeDocsFolderJSON(t, w, map[string]any{"code": 0, "msg": "success", "data": map[string]any{"token": "fld_partial", "url": "https://feishu.cn/drive/folder/fld_partial"}})
		case "/open-apis/drive/v1/permissions/fld_partial/members":
			shareCalls++
			if shareCalls == 1 {
				writeDocsFolderJSON(t, w, map[string]any{"code": 1066001, "msg": "temporary failure"})
				return
			}
			writeDocsFolderJSON(t, w, map[string]any{"code": 0, "msg": "Success", "data": map[string]any{"member": map[string]any{}}})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	tools := NewDocsFolderTools(newDocsFolderTestClient(server), st, "feishu:cli_test", Config{Docs: DocsToolsConfig{Enabled: true, AllowWrite: true}}, grantedResourceAccessController("req_access"))
	tool := findDocsTool(t, tools, folderCreateToolName)
	first := tool.Execute(groupDocsContext(), tooltypes.Call{ID: "call_1", Name: folderCreateToolName, Arguments: json.RawMessage(`{"name":"Retry Docs","access_request_id":"req_access"}`)})
	if first.IsError {
		t.Fatalf("first Execute result = %#v, want partial success", first)
	}
	var partial folderCreateOutput
	if err := json.Unmarshal([]byte(first.Content), &partial); err != nil {
		t.Fatalf("unmarshal partial output: %v", err)
	}
	if partial.Status != "partial" || partial.Shared || partial.RequestID == "" || !strings.Contains(partial.Retry, "request_id") {
		t.Fatalf("partial output = %#v", partial)
	}
	retryArgs, _ := json.Marshal(folderCreateArgs{RequestID: partial.RequestID})
	second := tool.Execute(groupDocsContext(), tooltypes.Call{ID: "call_2", Name: folderCreateToolName, Arguments: retryArgs})
	if second.IsError {
		t.Fatalf("retry Execute result = %#v", second)
	}
	var completed folderCreateOutput
	if err := json.Unmarshal([]byte(second.Content), &completed); err != nil {
		t.Fatalf("unmarshal completed output: %v", err)
	}
	if completed.Status != "created" || !completed.Shared || completed.RequestID != partial.RequestID || completed.FolderToken != partial.FolderToken {
		t.Fatalf("completed output = %#v", completed)
	}
	if createCalls != 1 || shareCalls != 2 {
		t.Fatalf("create/share calls = %d/%d, want 1/2", createCalls, shareCalls)
	}
	workflow, err := st.GetWorkflowRequest(partial.RequestID, "feishu:cli_test")
	if err != nil || workflow.State != store.WorkflowRequestStateSucceeded {
		t.Fatalf("retry workflow = %#v err=%v", workflow, err)
	}
}

func TestDocsFolderCreatePrivateUsesBoundParentAndOpenID(t *testing.T) {
	st := openDocsFolderTestStore(t)
	now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	parent, err := st.SaveFeishuChatFolder(store.FeishuChatFolder{
		AccountID:       "feishu:cli_test",
		ChatID:          "oc_private",
		FolderToken:     "fld_parent",
		Name:            "Parent",
		ShareMemberType: "openid",
		ShareMemberID:   "ou_private",
		ShareState:      store.FeishuFolderShareStateSucceeded,
		CreateRequestID: "req_parent",
		CreatedAt:       now,
	})
	if err != nil {
		t.Fatalf("SaveFeishuChatFolder returned error: %v", err)
	}
	var createParent string
	var shareMemberType, shareMemberID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/v3/token", "/open-apis/auth/v3/tenant_access_token/internal":
			writeDocsFolderJSON(t, w, tenantTokenResponse())
		case "/open-apis/drive/v1/files/create_folder":
			var body struct {
				FolderToken string `json:"folder_token"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode create body: %v", err)
			}
			createParent = body.FolderToken
			writeDocsFolderJSON(t, w, map[string]any{"code": 0, "msg": "success", "data": map[string]any{"token": "fld_child"}})
		case "/open-apis/drive/v1/permissions/fld_child/members":
			var body struct {
				MemberType string `json:"member_type"`
				MemberID   string `json:"member_id"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode permission body: %v", err)
			}
			shareMemberType, shareMemberID = body.MemberType, body.MemberID
			writeDocsFolderJSON(t, w, map[string]any{"code": 0, "msg": "Success", "data": map[string]any{"member": body}})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	access := grantedResourceAccessController("req_access")
	tool := findDocsTool(t, NewDocsFolderTools(newDocsFolderTestClient(server), st, parent.AccountID, Config{Docs: DocsToolsConfig{Enabled: true, AllowWrite: true}}, access), folderCreateToolName)
	ctx := WithActor(context.Background(), Actor{OpenID: "ou_private", UserID: "u_private"})
	ctx = WithChatContext(ctx, ChatContext{ChatID: "oc_private", IsGroup: false})
	result := tool.Execute(ctx, tooltypes.Call{ID: "call", Name: folderCreateToolName, Arguments: json.RawMessage(`{"name":"Child","parent_folder_token":"fld_parent","set_default":true,"access_request_id":"req_access"}`)})
	if result.IsError {
		t.Fatalf("Execute result = %#v", result)
	}
	if createParent != "fld_parent" || shareMemberType != "openid" || shareMemberID != "ou_private" {
		t.Fatalf("parent/share = %q/%q/%q", createParent, shareMemberType, shareMemberID)
	}
	if access.validation.ResourceToken != "fld_parent" || access.actor.OpenID != "ou_private" || access.chat.ChatID != "oc_private" {
		t.Fatalf("private access validation=%#v actor=%#v chat=%#v", access.validation, access.actor, access.chat)
	}
	defaultFolder, err := st.DefaultFeishuChatFolder(parent.AccountID, parent.ChatID)
	if err != nil || defaultFolder.FolderToken != "fld_child" {
		t.Fatalf("default child = %#v err=%v", defaultFolder, err)
	}
}

func TestDocsFolderListReturnsOnlyCurrentChat(t *testing.T) {
	st := openDocsFolderTestStore(t)
	now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	for _, chatID := range []string{"oc_chat", "oc_other"} {
		if _, err := st.SaveFeishuChatFolder(store.FeishuChatFolder{
			AccountID:       "feishu:cli_test",
			ChatID:          chatID,
			FolderToken:     "fld_" + chatID,
			Name:            chatID,
			ShareMemberType: "openchat",
			ShareMemberID:   chatID,
			ShareState:      store.FeishuFolderShareStateSucceeded,
			CreateRequestID: "req_" + chatID,
			CreatedAt:       now,
		}); err != nil {
			t.Fatalf("SaveFeishuChatFolder returned error: %v", err)
		}
	}
	tool := findDocsTool(t, NewDocsFolderTools(&lark.Client{}, st, "feishu:cli_test", Config{Docs: DocsToolsConfig{Enabled: true}}, nil), folderListToolName)
	result := tool.Execute(groupDocsContext(), tooltypes.Call{ID: "list", Name: folderListToolName, Arguments: json.RawMessage(`{}`)})
	if result.IsError {
		t.Fatalf("Execute result = %#v", result)
	}
	var output folderListOutput
	if err := json.Unmarshal([]byte(result.Content), &output); err != nil {
		t.Fatalf("unmarshal list output: %v", err)
	}
	if len(output.Folders) != 1 || output.Folders[0].FolderToken != "fld_oc_chat" {
		t.Fatalf("list output = %#v", output)
	}
}

func TestDocsFolderCreateRequiresGrantedParentAccessBeforeFeishuAPI(t *testing.T) {
	st := openDocsFolderTestStore(t)
	access := grantedResourceAccessController("req_access")
	tool := findDocsTool(t, NewDocsFolderTools(&lark.Client{}, st, "feishu:cli_test", Config{Docs: DocsToolsConfig{Enabled: true, AllowWrite: true}}, access), folderCreateToolName)

	missing := tool.Execute(groupDocsContext(), tooltypes.Call{
		ID:        "missing",
		Name:      folderCreateToolName,
		Arguments: json.RawMessage(`{"name":"No access"}`),
	})
	if !missing.IsError || !strings.Contains(missing.Content, "access_request_id is required") {
		t.Fatalf("missing access result = %#v", missing)
	}
	if access.validation.RequestID != "" {
		t.Fatalf("unexpected access validation = %#v", access.validation)
	}
}

func openDocsFolderTestStore(t *testing.T) *store.Store {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	st, err := store.Open(store.PlatformFeishu)
	if err != nil {
		t.Fatalf("store.Open returned error: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Fatalf("store.Close returned error: %v", err)
		}
	})
	return st
}

func newDocsFolderTestClient(server *httptest.Server) *lark.Client {
	return lark.NewClient("cli_xxx", "secret",
		lark.WithOpenBaseUrl(server.URL),
		lark.WithOAuthBaseUrl(server.URL),
		lark.WithHttpClient(server.Client()),
	)
}

func tenantTokenResponse() map[string]any {
	return map[string]any{
		"code":                0,
		"msg":                 "ok",
		"tenant_access_token": "tenant-token",
		"expire":              7200,
	}
}

func groupDocsContext() context.Context {
	ctx := WithActor(context.Background(), Actor{OpenID: "ou_requester", UserID: "u_requester"})
	return WithChatContext(ctx, ChatContext{ChatID: "oc_chat", MessageID: "om_source", IsGroup: true})
}

func toolNamesForTest(tools []tooltypes.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Spec().Name)
	}
	return names
}

func writeDocsFolderJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}
