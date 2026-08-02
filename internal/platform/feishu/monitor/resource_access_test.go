package monitor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"

	feishutools "lingobridge/internal/platform/feishu/tools"
	"lingobridge/internal/store"
)

func TestResourceAccessManagerGrantsBotRootWithoutCard(t *testing.T) {
	var rootCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			writeResourceAccessJSON(t, w, tenantTokenResponseForResourceAccess())
		case "/open-apis/drive/explorer/v2/root_folder/meta":
			rootCalls++
			writeResourceAccessJSON(t, w, map[string]any{
				"code": 0,
				"msg":  "Success",
				"data": map[string]any{"token": "fld_bot_root"},
			})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	manager, st, sender := newTestResourceAccessManager(t, server, resourceAccessOAuthConfig{})
	result, err := manager.RequestAccess(approvalRequestContext(), feishutools.ResourceAccessRequest{
		ResourceType:  "folder",
		ResourceToken: feishutools.BotRootResourceAlias,
		Permission:    feishutools.ResourcePermissionWrite,
		Reason:        "create a folder",
	})
	if err != nil {
		t.Fatalf("RequestAccess returned error: %v", err)
	}
	if result.Status != feishutools.ResourceAccessStatusGranted || result.Source != feishutools.ResourceAccessSourceBotOwner || result.ResourceToken != "fld_bot_root" || rootCalls != 1 {
		t.Fatalf("Bot root result = %#v rootCalls=%d", result, rootCalls)
	}
	if cards, _, _ := sender.snapshot(); len(cards) != 0 {
		t.Fatalf("Bot root cards = %#v, want none", cards)
	}
	request, err := st.GetFeishuResourceAccessRequest(result.RequestID, "feishu:cli_test")
	if err != nil || request.State != store.FeishuResourceAccessStateSucceeded || request.GrantSource != store.FeishuResourceGrantSourceBotOwner {
		t.Fatalf("stored Bot root request = %#v err=%v", request, err)
	}
	if _, err := st.GetFeishuBotResource("feishu:cli_test", "folder", "fld_bot_root"); err != nil {
		t.Fatalf("Bot root ownership was not stored: %v", err)
	}
	validated, err := manager.ValidateAccess(approvalRequestContext(), feishutools.ResourceAccessValidation{
		RequestID:     result.RequestID,
		ResourceType:  "folder",
		ResourceToken: "fld_bot_root",
		Permission:    feishutools.ResourcePermissionWrite,
	})
	if err != nil || validated.Status != feishutools.ResourceAccessStatusGranted {
		t.Fatalf("ValidateAccess = %#v err=%v", validated, err)
	}
}

func TestResourceAccessManagerConsumesOnceAndRejectsExpiredGrant(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			writeResourceAccessJSON(t, w, tenantTokenResponseForResourceAccess())
		case "/open-apis/drive/explorer/v2/root_folder/meta":
			writeResourceAccessJSON(t, w, map[string]any{
				"code": 0,
				"msg":  "Success",
				"data": map[string]any{"token": "fld_bot_root"},
			})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	manager, st, _ := newTestResourceAccessManager(t, server, resourceAccessOAuthConfig{})
	ctx := approvalRequestContext()
	request := feishutools.ResourceAccessRequest{
		ResourceType:  "folder",
		ResourceToken: feishutools.BotRootResourceAlias,
		Permission:    feishutools.ResourcePermissionWrite,
	}
	first, err := manager.RequestAccess(ctx, request)
	if err != nil {
		t.Fatalf("first RequestAccess returned error: %v", err)
	}
	second, err := manager.RequestAccess(ctx, request)
	if err != nil {
		t.Fatalf("second RequestAccess returned error: %v", err)
	}
	validation := feishutools.ResourceAccessValidation{
		RequestID:     first.RequestID,
		ResourceType:  "folder",
		ResourceToken: "fld_bot_root",
		Permission:    feishutools.ResourcePermissionWrite,
	}
	if _, err := manager.ConsumeAccess(ctx, validation, "req_create_workflow"); err != nil {
		t.Fatalf("ConsumeAccess returned error: %v", err)
	}
	stored, err := st.GetFeishuResourceAccessRequest(first.RequestID, "feishu:cli_test")
	if err != nil || stored.ConsumedByRequestID != "req_create_workflow" || stored.ConsumedAt.IsZero() {
		t.Fatalf("consumed request = %#v err=%v", stored, err)
	}
	if _, err := manager.ValidateAccess(ctx, validation); err == nil || !strings.Contains(err.Error(), "already been used") {
		t.Fatalf("reused ValidateAccess error = %v", err)
	}

	baseNow := manager.currentTime()
	manager.now = func() time.Time { return baseNow.Add(defaultResourceAccessTTL) }
	validation.RequestID = second.RequestID
	if _, err := manager.ValidateAccess(ctx, validation); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired ValidateAccess error = %v", err)
	}
}

func TestResourceAccessManagerReusesOnlyLiveExactChatGrant(t *testing.T) {
	var authCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			writeResourceAccessJSON(t, w, tenantTokenResponseForResourceAccess())
		case "/open-apis/drive/v1/permissions/doxcn_external/members/auth":
			authCalls++
			if r.URL.Query().Get("type") != "docx" || r.URL.Query().Get("action") != "view" {
				t.Fatalf("auth query = %s", r.URL.RawQuery)
			}
			writeResourceAccessJSON(t, w, map[string]any{"code": 0, "msg": "Success", "data": map[string]any{"auth_result": true}})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	manager, st, sender := newTestResourceAccessManager(t, server, resourceAccessOAuthConfig{})
	now := manager.currentTime()
	if _, err := st.UpsertFeishuResourceGrant(store.FeishuResourceGrant{
		AccountID:       "feishu:cli_test",
		ChatID:          "oc_chat",
		ResourceType:    "docx",
		ResourceToken:   "doxcn_external",
		Permission:      store.FeishuResourcePermissionWrite,
		SubjectType:     "openid",
		SubjectID:       "ou_bot",
		SourceRequestID: "req_original",
		State:           store.FeishuResourceGrantStateActive,
		CreatedAt:       now,
		VerifiedAt:      now,
	}); err != nil {
		t.Fatalf("UpsertFeishuResourceGrant returned error: %v", err)
	}
	result, err := manager.RequestAccess(approvalRequestContext(), feishutools.ResourceAccessRequest{
		ResourceType:  "docx",
		ResourceToken: "doxcn_external",
		Permission:    feishutools.ResourcePermissionRead,
	})
	if err != nil {
		t.Fatalf("RequestAccess returned error: %v", err)
	}
	if result.Status != feishutools.ResourceAccessStatusGranted || result.Source != feishutools.ResourceAccessSourceExistingGrant || authCalls != 1 {
		t.Fatalf("existing grant result = %#v authCalls=%d", result, authCalls)
	}
	if cards, _, _ := sender.snapshot(); len(cards) != 0 {
		t.Fatalf("existing grant cards = %#v, want none", cards)
	}
	request, err := st.GetFeishuResourceAccessRequest(result.RequestID, "feishu:cli_test")
	if err != nil || request.SubjectType != "openid" || request.SubjectID != "ou_bot" || request.VerifiedPermission != store.FeishuResourcePermissionWrite {
		t.Fatalf("existing grant request = %#v err=%v", request, err)
	}
}

func TestResourceAccessOAuthCardCallbackGrantsAndRedirects(t *testing.T) {
	var mu sync.Mutex
	var tokenBody map[string]any
	var permissionBody map[string]any
	var userInfoCalls, permissionCalls, verifyCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/v3/token":
			if err := json.NewDecoder(r.Body).Decode(&tokenBody); err != nil {
				t.Fatalf("decode OAuth token body: %v", err)
			}
			writeResourceAccessJSON(t, w, map[string]any{
				"access_token": "user-access-token",
				"token_type":   "Bearer",
				"expires_in":   7200,
				"scope":        resourceAccessOAuthScope,
			})
		case "/open-apis/authen/v1/user_info":
			mu.Lock()
			userInfoCalls++
			mu.Unlock()
			if got := r.Header.Get("Authorization"); got != "Bearer user-access-token" {
				t.Fatalf("user info Authorization = %q", got)
			}
			writeResourceAccessJSON(t, w, map[string]any{
				"code": 0,
				"msg":  "Success",
				"data": map[string]any{"open_id": "ou_requester", "user_id": "u_requester"},
			})
		case "/open-apis/drive/v1/permissions/doxcn_external/members":
			if r.Method != http.MethodPost {
				t.Fatalf("permission method = %s, want POST", r.Method)
			}
			mu.Lock()
			permissionCalls++
			mu.Unlock()
			if got := r.Header.Get("Authorization"); got != "Bearer user-access-token" {
				t.Fatalf("permission Authorization = %q", got)
			}
			if err := json.NewDecoder(r.Body).Decode(&permissionBody); err != nil {
				t.Fatalf("decode permission body: %v", err)
			}
			writeResourceAccessJSON(t, w, map[string]any{"code": 0, "msg": "Success", "data": map[string]any{"member": permissionBody}})
		case "/open-apis/auth/v3/tenant_access_token/internal":
			writeResourceAccessJSON(t, w, tenantTokenResponseForResourceAccess())
		case "/open-apis/drive/v1/permissions/doxcn_external/members/auth":
			mu.Lock()
			verifyCalls++
			mu.Unlock()
			if r.URL.Query().Get("action") != "edit" {
				t.Fatalf("verify action = %q, want edit", r.URL.Query().Get("action"))
			}
			writeResourceAccessJSON(t, w, map[string]any{"code": 0, "msg": "Success", "data": map[string]any{"auth_result": true}})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	oauth := resourceAccessOAuthConfig{
		ClientID:              "cli_xxx",
		BaseURL:               server.URL,
		CallbackURL:           "https://bridge.example.com/feishu/oauth/callback",
		CallbackListenAddress: "127.0.0.1:0",
	}
	manager, st, sender := newTestResourceAccessManager(t, server, oauth)
	result, err := manager.RequestAccess(approvalRequestContext(), feishutools.ResourceAccessRequest{
		ResourceType:  "docx",
		ResourceToken: "doxcn_external",
		ResourceURL:   "https://docs.feishu.cn/docx/doxcn_external",
		Permission:    feishutools.ResourcePermissionWrite,
		Reason:        "create a reviewed copy",
	})
	if err != nil {
		t.Fatalf("RequestAccess returned error: %v", err)
	}
	if result.Status != feishutools.ResourceAccessStatusPending || result.ExpiresAt.IsZero() {
		t.Fatalf("pending result = %#v", result)
	}
	cards, _, _ := sender.snapshot()
	if len(cards) != 1 || cards[0].chatID != "oc_chat" {
		t.Fatalf("sent cards = %#v", cards)
	}
	authURL := resourceAccessCardURL(t, cards[0].text)
	parsedAuthURL, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse card auth URL: %v", err)
	}
	state := parsedAuthURL.Query().Get("state")
	if parsedAuthURL.Path != "/open-apis/authen/v1/authorize" || parsedAuthURL.Query().Get("client_id") != "cli_xxx" ||
		parsedAuthURL.Query().Get("redirect_uri") != oauth.CallbackURL || parsedAuthURL.Query().Get("scope") != resourceAccessOAuthScope ||
		parsedAuthURL.Query().Get("prompt") != "consent" || parsedAuthURL.Query().Get("code_challenge_method") != "S256" ||
		state == "" || parsedAuthURL.Query().Get("code_challenge") == "" {
		t.Fatalf("authorization URL = %s", authURL)
	}
	storedPending, err := st.GetFeishuResourceAccessRequest(result.RequestID, "feishu:cli_test")
	if err != nil || storedPending.OAuthStateHash == "" || storedPending.OAuthStateHash == state || storedPending.PKCEVerifier == "" {
		t.Fatalf("stored pending request = %#v err=%v", storedPending, err)
	}

	recorder := httptest.NewRecorder()
	callbackRequest := httptest.NewRequest(http.MethodGet, "/feishu/oauth/callback?code=auth-code&state="+url.QueryEscape(state), nil)
	manager.HandleOAuthCallback(recorder, callbackRequest)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != result.ResourceURL {
		t.Fatalf("callback response status/location = %d/%q", recorder.Code, recorder.Header().Get("Location"))
	}
	if tokenBody["grant_type"] != "authorization_code" || tokenBody["client_id"] != "cli_xxx" || tokenBody["client_secret"] != "secret" ||
		tokenBody["code"] != "auth-code" || tokenBody["redirect_uri"] != oauth.CallbackURL || strings.TrimSpace(stringValue(tokenBody["code_verifier"])) == "" {
		t.Fatalf("OAuth token body = %#v", tokenBody)
	}
	if permissionBody["member_type"] != "openid" || permissionBody["member_id"] != "ou_bot" || permissionBody["perm"] != "edit" || permissionBody["type"] != "user" {
		t.Fatalf("permission body = %#v", permissionBody)
	}
	mu.Lock()
	calls := []int{userInfoCalls, permissionCalls, verifyCalls}
	mu.Unlock()
	if calls[0] != 1 || calls[1] != 1 || calls[2] != 1 {
		t.Fatalf("user/permission/verify calls = %#v", calls)
	}
	completed, err := st.GetFeishuResourceAccessRequest(result.RequestID, "feishu:cli_test")
	if err != nil || completed.State != store.FeishuResourceAccessStateSucceeded || completed.GrantSource != store.FeishuResourceGrantSourceNewlyGranted || completed.PKCEVerifier != "" || completed.OAuthStateHash != "" {
		t.Fatalf("completed request = %#v err=%v", completed, err)
	}
	grant, active, err := st.ActiveFeishuResourceGrant("feishu:cli_test", "oc_chat", "docx", "doxcn_external", store.FeishuResourcePermissionWrite)
	if err != nil || !active || grant.SubjectID != "ou_bot" {
		t.Fatalf("saved grant = %#v active=%t err=%v", grant, active, err)
	}
	_, updates, messages := sender.snapshot()
	if len(updates) != 1 || updates[0].messageID != "om_card" || !strings.Contains(updates[0].text, "权限已授予") || len(messages) != 1 || !strings.Contains(messages[0].text, result.RequestID) {
		t.Fatalf("updates/messages = %#v/%#v", updates, messages)
	}
}

func TestResourceAccessManagerRejectsPrivateExternalFolderWithoutOAuthCard(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected request: %s", r.URL.Path)
	}))
	defer server.Close()
	manager, st, sender := newTestResourceAccessManager(t, server, resourceAccessOAuthConfig{
		ClientID:              "cli_xxx",
		BaseURL:               server.URL,
		CallbackURL:           "https://bridge.example.com/feishu/oauth/callback",
		CallbackListenAddress: "127.0.0.1:0",
	})
	ctx := feishutools.WithActor(context.Background(), feishutools.Actor{OpenID: "ou_requester", UserID: "u_requester"})
	ctx = feishutools.WithChatContext(ctx, feishutools.ChatContext{ChatID: "oc_private", MessageID: "om_source", IsGroup: false})
	result, err := manager.RequestAccess(ctx, feishutools.ResourceAccessRequest{
		ResourceType:  "folder",
		ResourceToken: "fld_external",
		Permission:    feishutools.ResourcePermissionWrite,
	})
	if err != nil {
		t.Fatalf("RequestAccess returned error: %v", err)
	}
	if result.Status != feishutools.ResourceAccessStatusUnsupported || !strings.Contains(result.Message, "私聊") {
		t.Fatalf("private folder result = %#v", result)
	}
	request, err := st.GetFeishuResourceAccessRequest(result.RequestID, "feishu:cli_test")
	if err != nil || request.State != store.FeishuResourceAccessStateFailed {
		t.Fatalf("unsupported stored request = %#v err=%v", request, err)
	}
	if cards, _, _ := sender.snapshot(); len(cards) != 0 {
		t.Fatalf("private folder cards = %#v, want none", cards)
	}
}

func TestResourceAccessManagerRejectsNonFeishuRedirectURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected request: %s", r.URL.Path)
	}))
	defer server.Close()
	manager, _, _ := newTestResourceAccessManager(t, server, resourceAccessOAuthConfig{})
	_, err := manager.RequestAccess(approvalRequestContext(), feishutools.ResourceAccessRequest{
		ResourceType:  "docx",
		ResourceToken: "doxcn_external",
		ResourceURL:   "https://example.com/doxcn_external",
		Permission:    feishutools.ResourcePermissionRead,
	})
	if err == nil || !strings.Contains(err.Error(), "Feishu/Lark") {
		t.Fatalf("RequestAccess error = %v, want unsafe resource URL rejection", err)
	}
}

func TestResourceAccessCardRejectIsBoundToRequester(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected request: %s", r.URL.Path)
	}))
	defer server.Close()
	manager, st, _ := newTestResourceAccessManager(t, server, resourceAccessOAuthConfig{
		ClientID:              "cli_xxx",
		BaseURL:               server.URL,
		CallbackURL:           "https://bridge.example.com/feishu/oauth/callback",
		CallbackListenAddress: "127.0.0.1:0",
	})
	result, err := manager.RequestAccess(approvalRequestContext(), feishutools.ResourceAccessRequest{
		ResourceType:  "docx",
		ResourceToken: "doxcn_external",
		Permission:    feishutools.ResourcePermissionRead,
	})
	if err != nil {
		t.Fatalf("RequestAccess returned error: %v", err)
	}
	wrong := resourceAccessCardEvent(result.RequestID, "ou_other", "oc_chat", "om_card")
	response, err := manager.HandleCardAction(context.Background(), wrong)
	if err != nil || response == nil || response.Toast == nil || !strings.Contains(response.Toast.Content, "只有") {
		t.Fatalf("wrong actor response = %#v err=%v", response, err)
	}
	pending, err := st.GetFeishuResourceAccessRequest(result.RequestID, "feishu:cli_test")
	if err != nil || pending.State != store.FeishuResourceAccessStatePending {
		t.Fatalf("pending after wrong actor = %#v err=%v", pending, err)
	}
	response, err = manager.HandleCardAction(context.Background(), resourceAccessCardEvent(result.RequestID, "ou_requester", "oc_chat", "om_card"))
	if err != nil || response == nil || response.Card == nil {
		t.Fatalf("reject response = %#v err=%v", response, err)
	}
	denied, err := st.GetFeishuResourceAccessRequest(result.RequestID, "feishu:cli_test")
	if err != nil || denied.State != store.FeishuResourceAccessStateDenied || denied.PKCEVerifier != "" {
		t.Fatalf("denied request = %#v err=%v", denied, err)
	}
}

func TestResourceAccessOAuthServerUsesConfiguredCallbackPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected Feishu API request: %s", r.URL.Path)
	}))
	defer server.Close()
	manager, _, _ := newTestResourceAccessManager(t, server, resourceAccessOAuthConfig{
		ClientID:              "cli_xxx",
		BaseURL:               server.URL,
		CallbackURL:           "https://bridge.example.com/custom/feishu/callback",
		CallbackListenAddress: "127.0.0.1:0",
	})
	ctx, cancel := context.WithCancel(context.Background())
	callbackServer, err := startResourceAccessOAuthServer(ctx, manager)
	if err != nil {
		t.Fatalf("startResourceAccessOAuthServer returned error: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		if err := callbackServer.Close(); err != nil {
			t.Fatalf("callback server Close returned error: %v", err)
		}
	})
	response, err := http.Get("http://" + callbackServer.listener.Addr().String() + "/custom/feishu/callback")
	if err != nil {
		t.Fatalf("GET OAuth callback returned error: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("callback status = %d, want 400", response.StatusCode)
	}
}

func newTestResourceAccessManager(t *testing.T, server *httptest.Server, oauth resourceAccessOAuthConfig) (*resourceAccessManager, *store.Store, *fakeApprovalSender) {
	t.Helper()
	st := openFeishuApprovalTestStore(t)
	sender := &fakeApprovalSender{}
	cards, err := newCardService(sender)
	if err != nil {
		t.Fatalf("newCardService returned error: %v", err)
	}
	client := lark.NewClient("cli_xxx", "secret",
		lark.WithOpenBaseUrl(server.URL),
		lark.WithOAuthBaseUrl(server.URL),
		lark.WithHttpClient(server.Client()),
	)
	manager, err := newResourceAccessManager(context.Background(), st, client, store.Account{
		ID:       "feishu:cli_test",
		Name:     "fsbot",
		Platform: store.PlatformFeishu,
	}, "ou_bot", cards, sender, oauth)
	if err != nil {
		t.Fatalf("newResourceAccessManager returned error: %v", err)
	}
	now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	manager.now = func() time.Time { return now }
	return manager, st, sender
}

func resourceAccessCardURL(t *testing.T, raw string) string {
	t.Helper()
	var card map[string]any
	if err := json.Unmarshal([]byte(raw), &card); err != nil {
		t.Fatalf("unmarshal resource access card: %v", err)
	}
	body, _ := card["body"].(map[string]any)
	elements, _ := body["elements"].([]any)
	if len(elements) < 2 {
		t.Fatalf("resource access card elements = %#v", elements)
	}
	button, _ := elements[1].(map[string]any)
	behaviors, _ := button["behaviors"].([]any)
	if len(behaviors) != 1 {
		t.Fatalf("resource access button behaviors = %#v", behaviors)
	}
	behavior, _ := behaviors[0].(map[string]any)
	value, _ := behavior["default_url"].(string)
	if behavior["type"] != "open_url" || value == "" {
		t.Fatalf("resource access open URL behavior = %#v", behavior)
	}
	return value
}

func resourceAccessCardEvent(requestID, openID, chatID, messageID string) *callback.CardActionTriggerEvent {
	userID := "u_requester"
	if openID != "ou_requester" {
		userID = "u_other"
	}
	return &callback.CardActionTriggerEvent{
		Event: &callback.CardActionTriggerRequest{
			Operator: &callback.Operator{OpenID: openID, UserID: &userID},
			Context:  &callback.Context{OpenChatID: chatID, OpenMessageID: messageID},
			Action: &callback.CallBackAction{Value: map[string]any{
				"kind":       resourceAccessCardActionKind,
				"request_id": requestID,
				"action":     resourceAccessCardActionReject,
			}},
		},
	}
}

func tenantTokenResponseForResourceAccess() map[string]any {
	return map[string]any{
		"code":                0,
		"msg":                 "ok",
		"tenant_access_token": "tenant-token",
		"expire":              7200,
	}
}

func writeResourceAccessJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}

func stringValue(value any) string {
	result, _ := value.(string)
	return result
}
