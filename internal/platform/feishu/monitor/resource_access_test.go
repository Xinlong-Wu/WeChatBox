package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"

	"lingobridge/internal/logging"
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
	result, err := manager.RequestAccess(resourceAccessRequestContext(), feishutools.ResourceAccessRequest{
		ResourceType:        "folder",
		ResourceToken:       feishutools.BotRootResourceAlias,
		Permission:          feishutools.ResourcePermissionWrite,
		OnceDurationMinutes: 30,
		Reason:              "create a folder",
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
	if err != nil || request.State != store.FeishuResourceAccessStateSucceeded || request.GrantSource != store.FeishuResourceGrantSourceBotOwner || request.ResourceDisplayName != "Bot Root" {
		t.Fatalf("stored Bot root request = %#v err=%v", request, err)
	}
	if _, err := st.GetFeishuBotResource("feishu:cli_test", "folder", "fld_bot_root"); err != nil {
		t.Fatalf("Bot root ownership was not stored: %v", err)
	}
	if _, err := st.GetWorkflowContinuation(result.RequestID, "feishu:cli_test"); !errors.Is(err, store.ErrWorkflowContinuationNotFound) {
		t.Fatalf("Bot-owned request continuation error = %v, want ErrWorkflowContinuationNotFound", err)
	}
	err = manager.RequireResourceAccess(resourceAccessRequestContext(), feishutools.ResourceAccessRequirement{
		ResourceType:  "folder",
		ResourceToken: "fld_bot_root",
		Permission:    feishutools.ResourcePermissionWrite,
	})
	if err != nil {
		t.Fatalf("RequireResourceAccess returned error: %v", err)
	}
}

func TestResourceAccessDisplayNameResolutionUsesTrustedMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("display-name resolution unexpectedly called Feishu API: %s", r.URL.Path)
	}))
	defer server.Close()
	manager, st, _ := newTestResourceAccessManager(t, server, resourceAccessOAuthConfig{})
	now := manager.currentTime()
	if _, err := st.SaveFeishuBotResource(store.FeishuBotResource{
		AccountID: "feishu:cli_test", ResourceType: "docx", ResourceToken: "doxcn_owned",
		Name: "Bot 项目文档", CreatedAt: now,
	}); err != nil {
		t.Fatalf("SaveFeishuBotResource returned error: %v", err)
	}
	if _, err := st.SaveFeishuChatFolder(store.FeishuChatFolder{
		AccountID: "feishu:cli_test", ChatID: "oc_chat", FolderToken: "fld_bound", Name: "群聊交付目录",
		ShareMemberType: "openchat", ShareMemberID: "oc_chat", ShareState: store.FeishuFolderShareStateSucceeded,
		CreateRequestID: "req_folder_name", CreatedAt: now,
	}); err != nil {
		t.Fatalf("SaveFeishuChatFolder returned error: %v", err)
	}
	if _, err := st.SaveFeishuChatDocument(store.FeishuChatDocument{
		AccountID: "feishu:cli_test", ChatID: "oc_chat", DocumentToken: "doxcn_bound", FolderToken: "fld_bound",
		Title: "群聊计划文档", SourceRequestID: "req_doc_name", CreatedAt: now,
	}); err != nil {
		t.Fatalf("SaveFeishuChatDocument returned error: %v", err)
	}
	chat := feishutools.ChatContext{ChatID: "oc_chat", IsGroup: true}
	tests := []struct {
		name         string
		resourceType string
		token        string
		want         string
	}{
		{name: "Bot ownership", resourceType: "docx", token: "doxcn_owned", want: "Bot 项目文档"},
		{name: "chat folder", resourceType: "folder", token: "fld_bound", want: "群聊交付目录"},
		{name: "chat document", resourceType: "docx", token: "doxcn_bound", want: "群聊计划文档"},
		{name: "safe fallback", resourceType: "sheet", token: "sht_external", want: "飞书电子表格（" + shortResourceRef("sht_external") + "）"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := manager.resolveResourceDisplayName(chat, tt.resourceType, tt.token)
			if err != nil || got != tt.want {
				t.Fatalf("resolveResourceDisplayName = %q err=%v, want %q", got, err, tt.want)
			}
		})
	}
}

func TestResourceAccessOAuthDisplayStatusUsesMetadataWithoutRefreshing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("OAuth display status unexpectedly called Feishu API: %s", r.URL.Path)
	}))
	defer server.Close()
	manager, st, _ := newTestResourceAccessManager(t, server, resourceAccessOAuthConfig{
		ClientID: "cli_xxx", BaseURL: server.URL, CallbackURL: "https://bridge.example.com/feishu/oauth/callback",
	})
	actor := feishutools.Actor{OpenID: "ou_requester", UserID: "u_requester"}
	status, err := manager.resourceAccessOAuthDisplayStatus(actor, false)
	if err != nil || status != resourceAccessOAuthStatusAuthorizationNeeded {
		t.Fatalf("missing credential OAuth status = %q err=%v", status, err)
	}
	now := manager.currentTime()
	credential, err := st.SaveFeishuUserOAuthCredential(store.FeishuUserOAuthCredential{
		AccountID: "feishu:cli_test", ActorOpenID: actor.OpenID, ActorUserID: actor.UserID,
		AccessTokenCiphertext: "v1.encrypted-access", AccessTokenExpiresAt: now.Add(time.Hour),
		RefreshTokenCiphertext: "v1.encrypted-refresh", RefreshTokenExpiresAt: now.Add(30 * 24 * time.Hour),
		Scopes: resourceAccessOAuthScope, AuthorizedAt: now, ReauthorizeAt: now.Add(365 * 24 * time.Hour),
		Status: store.FeishuUserOAuthCredentialStatusActive, CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("SaveFeishuUserOAuthCredential returned error: %v", err)
	}
	status, err = manager.resourceAccessOAuthDisplayStatus(actor, false)
	if err != nil || status != resourceAccessOAuthStatusCredentialReady {
		t.Fatalf("stored credential OAuth status = %q err=%v", status, err)
	}
	unchanged, err := st.GetFeishuUserOAuthCredential("feishu:cli_test", actor.OpenID, actor.UserID)
	if err != nil || unchanged.Version != credential.Version || unchanged.Status != credential.Status {
		t.Fatalf("OAuth display inspection mutated credential = %#v err=%v", unchanged, err)
	}
	status, err = manager.resourceAccessOAuthDisplayStatus(actor, true)
	if err != nil || status != resourceAccessOAuthStatusCapabilityReady {
		t.Fatalf("capability OAuth status = %q err=%v", status, err)
	}

	disabled, _, _ := newTestResourceAccessManager(t, server, resourceAccessOAuthConfig{})
	status, err = disabled.resourceAccessOAuthDisplayStatus(actor, false)
	if err != nil || status != resourceAccessOAuthStatusConfigurationMissing {
		t.Fatalf("disabled OAuth status = %q err=%v", status, err)
	}
}

func TestResourceAccessManagerRequiresBotOwnedAccessWithoutRequestBinding(t *testing.T) {
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
	ctx := resourceAccessRequestContext()
	request := feishutools.ResourceAccessRequest{
		ResourceType:        "folder",
		ResourceToken:       feishutools.BotRootResourceAlias,
		Permission:          feishutools.ResourcePermissionWrite,
		OnceDurationMinutes: 30,
	}
	first, err := manager.RequestAccess(ctx, request)
	if err != nil {
		t.Fatalf("first RequestAccess returned error: %v", err)
	}
	second, err := manager.RequestAccess(ctx, request)
	if err != nil {
		t.Fatalf("second RequestAccess returned error: %v", err)
	}
	requirement := feishutools.ResourceAccessRequirement{
		ResourceType:  "folder",
		ResourceToken: "fld_bot_root",
		Permission:    feishutools.ResourcePermissionWrite,
	}
	if err := manager.RequireResourceAccess(ctx, requirement); err != nil {
		t.Fatalf("RequireResourceAccess returned error: %v", err)
	}
	stored, err := st.GetFeishuResourceAccessRequest(first.RequestID, "feishu:cli_test")
	if err != nil || stored.State != store.FeishuResourceAccessStateSucceeded {
		t.Fatalf("resource request = %#v err=%v", stored, err)
	}

	baseNow := manager.currentTime()
	manager.now = func() time.Time { return baseNow.Add(defaultResourceAccessTTL) }
	if err := manager.RequireResourceAccess(ctx, requirement); err != nil {
		t.Fatalf("Bot-owned access should remain valid independently of request card TTL: %v", err)
	}
	if first.RequestID == second.RequestID {
		t.Fatal("separate authorization workflow IDs unexpectedly matched")
	}
}

func TestResourceAccessManagerReturnsStructuredMissingGrantWithoutStartingWorkflow(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("missing local grant unexpectedly called Feishu API: %s", r.URL.Path)
	}))
	defer server.Close()
	manager, _, sender := newTestResourceAccessManager(t, server, resourceAccessOAuthConfig{
		ClientID:    "cli_xxx",
		BaseURL:     server.URL,
		CallbackURL: "https://bridge.example.com/feishu/oauth/callback",
	})
	requirement := feishutools.ResourceAccessRequirement{
		ResourceType: "docx", ResourceToken: "doxcn_external", Permission: feishutools.ResourcePermissionWrite,
	}
	err := manager.RequireResourceAccess(resourceAccessRequestContext(), requirement)
	var required *feishutools.ResourceAuthorizationRequiredError
	if !errors.As(err, &required) {
		t.Fatalf("RequireResourceAccess error = %v, want structured authorization required", err)
	}
	if required.Status != feishutools.ResourceAuthorizationRequiredStatus || required.RequiredTool != feishutools.ResourceAccessToolName ||
		required.ResourceType != requirement.ResourceType || required.ResourceToken != requirement.ResourceToken || required.Permission != requirement.Permission {
		t.Fatalf("structured authorization error = %#v", required)
	}
	if cards, updates, _ := sender.snapshot(); len(cards) != 0 || len(updates) != 0 {
		t.Fatalf("side-effect-free check sent cards=%#v updates=%#v", cards, updates)
	}
}

func TestResourceAccessManagerRequiresExactActorChatGrantAndInheritsWriteForRead(t *testing.T) {
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
	manager, st, _ := newTestResourceAccessManager(t, server, resourceAccessOAuthConfig{})
	now := manager.currentTime()
	if _, err := st.UpsertFeishuResourceCapability(store.FeishuResourceCapability{
		AccountID: "feishu:cli_test", ResourceType: "docx", ResourceToken: "doxcn_external",
		SubjectType: "openid", SubjectID: "ou_bot", Permission: store.FeishuResourcePermissionWrite,
		SourceActorOpenID: "ou_requester", SourceRequestID: "req_capability", State: store.FeishuResourceCapabilityStateActive,
		CreatedAt: now, VerifiedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed resource capability: %v", err)
	}
	if _, err := st.UpsertFeishuResourceGrant(store.FeishuResourceGrant{
		AccountID: "feishu:cli_test", ActorType: store.FeishuResourceGrantActorTypeOpenID, ActorID: "ou_requester",
		ChatID: "oc_chat", ResourceType: "docx", ResourceToken: "doxcn_external", Permission: store.FeishuResourcePermissionWrite,
		GrantMode: store.FeishuResourceGrantModeAll, SourceRequestID: "req_grant", State: store.FeishuResourceGrantStateActive,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed resource grant: %v", err)
	}
	requirement := feishutools.ResourceAccessRequirement{
		ResourceType: "docx", ResourceToken: "doxcn_external", Permission: feishutools.ResourcePermissionRead,
	}
	if err := manager.RequireResourceAccess(resourceAccessRequestContext(), requirement); err != nil {
		t.Fatalf("RequireResourceAccess returned error: %v", err)
	}
	if authCalls != 1 {
		t.Fatalf("live verification calls = %d, want 1", authCalls)
	}

	wrongActor := feishutools.WithActor(context.Background(), feishutools.Actor{OpenID: "ou_other"})
	wrongActor = feishutools.WithChatContext(wrongActor, feishutools.ChatContext{ChatID: "oc_chat", IsGroup: true})
	var required *feishutools.ResourceAuthorizationRequiredError
	if err := manager.RequireResourceAccess(wrongActor, requirement); !errors.As(err, &required) {
		t.Fatalf("wrong actor error = %v, want authorization required", err)
	}
	wrongChat := feishutools.WithActor(context.Background(), feishutools.Actor{OpenID: "ou_requester"})
	wrongChat = feishutools.WithChatContext(wrongChat, feishutools.ChatContext{ChatID: "oc_other", IsGroup: true})
	if err := manager.RequireResourceAccess(wrongChat, requirement); !errors.As(err, &required) {
		t.Fatalf("wrong chat error = %v, want authorization required", err)
	}
	writeRequirement := requirement
	writeRequirement.ResourceToken = "doxcn_read_only"
	writeRequirement.Permission = feishutools.ResourcePermissionWrite
	if _, err := st.UpsertFeishuResourceGrant(store.FeishuResourceGrant{
		AccountID: "feishu:cli_test", ActorType: store.FeishuResourceGrantActorTypeOpenID, ActorID: "ou_requester",
		ChatID: "oc_chat", ResourceType: "docx", ResourceToken: writeRequirement.ResourceToken, Permission: store.FeishuResourcePermissionRead,
		GrantMode: store.FeishuResourceGrantModeAll, SourceRequestID: "req_read_only", State: store.FeishuResourceGrantStateActive,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed read-only grant: %v", err)
	}
	if err := manager.RequireResourceAccess(resourceAccessRequestContext(), writeRequirement); !errors.As(err, &required) {
		t.Fatalf("read-only grant write error = %v, want authorization required", err)
	}
	if authCalls != 1 {
		t.Fatalf("scope mismatch unexpectedly reached live verification; calls=%d", authCalls)
	}
}

func TestResourceAccessManagerRevokesStaleCapabilityAndGrant(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			writeResourceAccessJSON(t, w, tenantTokenResponseForResourceAccess())
		case "/open-apis/drive/v1/permissions/doxcn_stale/members/auth":
			writeResourceAccessJSON(t, w, map[string]any{"code": 0, "msg": "Success", "data": map[string]any{"auth_result": false}})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	manager, st, _ := newTestResourceAccessManager(t, server, resourceAccessOAuthConfig{})
	now := manager.currentTime()
	capability := store.FeishuResourceCapability{
		AccountID: "feishu:cli_test", ResourceType: "docx", ResourceToken: "doxcn_stale",
		SubjectType: "openid", SubjectID: "ou_bot", Permission: store.FeishuResourcePermissionWrite,
		SourceActorOpenID: "ou_requester", SourceRequestID: "req_stale", State: store.FeishuResourceCapabilityStateActive,
		CreatedAt: now, VerifiedAt: now, UpdatedAt: now,
	}
	if _, err := st.UpsertFeishuResourceCapability(capability); err != nil {
		t.Fatalf("seed stale capability: %v", err)
	}
	if _, err := st.UpsertFeishuResourceGrant(store.FeishuResourceGrant{
		AccountID: capability.AccountID, ActorType: store.FeishuResourceGrantActorTypeOpenID, ActorID: "ou_requester",
		ChatID: "oc_chat", ResourceType: capability.ResourceType, ResourceToken: capability.ResourceToken,
		Permission: store.FeishuResourcePermissionWrite, GrantMode: store.FeishuResourceGrantModeAll,
		SourceRequestID: capability.SourceRequestID, State: store.FeishuResourceGrantStateActive, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed stale grant: %v", err)
	}
	requirement := feishutools.ResourceAccessRequirement{
		ResourceType: capability.ResourceType, ResourceToken: capability.ResourceToken, Permission: feishutools.ResourcePermissionWrite,
	}
	var required *feishutools.ResourceAuthorizationRequiredError
	if err := manager.RequireResourceAccess(resourceAccessRequestContext(), requirement); !errors.As(err, &required) {
		t.Fatalf("stale capability error = %v, want authorization required", err)
	}
	if _, active, err := st.ActiveFeishuResourceCapability(capability.AccountID, capability.ResourceType, capability.ResourceToken, capability.SubjectType, capability.SubjectID, capability.Permission); err != nil || active {
		t.Fatalf("stale capability active=%t err=%v", active, err)
	}
	if _, active, err := st.ActiveFeishuResourceGrant(capability.AccountID, store.FeishuResourceGrantActorTypeOpenID, "ou_requester", "oc_chat", capability.ResourceType, capability.ResourceToken, capability.Permission, now); err != nil || active {
		t.Fatalf("stale grant active=%t err=%v", active, err)
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
	if _, err := st.UpsertFeishuResourceCapability(store.FeishuResourceCapability{
		AccountID:         "feishu:cli_test",
		ResourceType:      "docx",
		ResourceToken:     "doxcn_external",
		Permission:        store.FeishuResourcePermissionWrite,
		SubjectType:       "openid",
		SubjectID:         "ou_bot",
		SourceActorOpenID: "ou_requester",
		SourceRequestID:   "req_original",
		State:             store.FeishuResourceCapabilityStateActive,
		CreatedAt:         now,
		VerifiedAt:        now,
	}); err != nil {
		t.Fatalf("UpsertFeishuResourceCapability returned error: %v", err)
	}
	if _, err := st.UpsertFeishuResourceGrant(store.FeishuResourceGrant{
		AccountID:       "feishu:cli_test",
		ActorType:       store.FeishuResourceGrantActorTypeOpenID,
		ActorID:         "ou_requester",
		ChatID:          "oc_chat",
		ResourceType:    "docx",
		ResourceToken:   "doxcn_external",
		Permission:      store.FeishuResourcePermissionWrite,
		GrantMode:       store.FeishuResourceGrantModeOnce,
		SourceRequestID: "req_original",
		State:           store.FeishuResourceGrantStateActive,
		ExpiresAt:       now.Add(10 * time.Minute),
		CreatedAt:       now,
		UpdatedAt:       now,
	}); err != nil {
		t.Fatalf("UpsertFeishuResourceGrant returned error: %v", err)
	}
	result, err := manager.RequestAccess(resourceAccessRequestContext(), feishutools.ResourceAccessRequest{
		ResourceType:        "docx",
		ResourceToken:       "doxcn_external",
		Permission:          feishutools.ResourcePermissionRead,
		OnceDurationMinutes: 30,
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

func TestResourceAccessManagerApprovesPermanentGrantFromExistingCapabilityWithoutOAuth(t *testing.T) {
	var authCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			writeResourceAccessJSON(t, w, tenantTokenResponseForResourceAccess())
		case "/open-apis/drive/v1/permissions/doxcn_external/members/auth":
			authCalls++
			writeResourceAccessJSON(t, w, map[string]any{"code": 0, "msg": "Success", "data": map[string]any{"auth_result": true}})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	manager, st, sender := newTestResourceAccessManager(t, server, resourceAccessOAuthConfig{})
	now := manager.currentTime()
	if _, err := st.UpsertFeishuResourceCapability(store.FeishuResourceCapability{
		AccountID: "feishu:cli_test", ResourceType: "docx", ResourceToken: "doxcn_external",
		SubjectType: "openid", SubjectID: "ou_bot", Permission: store.FeishuResourcePermissionWrite,
		SourceActorOpenID: "ou_requester", SourceRequestID: "req_capability",
		State: store.FeishuResourceCapabilityStateActive, CreatedAt: now, VerifiedAt: now,
	}); err != nil {
		t.Fatalf("UpsertFeishuResourceCapability returned error: %v", err)
	}
	result, err := manager.RequestAccess(resourceAccessRequestContext(), feishutools.ResourceAccessRequest{
		ResourceType: "docx", ResourceToken: "doxcn_external", Permission: feishutools.ResourcePermissionRead,
		OnceDurationMinutes: 20, Reason: "读取项目计划",
	})
	if err != nil || result.Status != feishutools.ResourceAccessStatusPending {
		t.Fatalf("RequestAccess = %#v err=%v", result, err)
	}
	cards, _, _ := sender.snapshot()
	if len(cards) != 1 || !strings.Contains(cards[0].text, "已有可核验的飞书资源权限") ||
		!strings.Contains(cards[0].text, fallbackResourceDisplayName("docx", "doxcn_external")) {
		t.Fatalf("initial resource authorization card = %#v", cards)
	}
	pending, err := st.GetFeishuResourceAccessRequest(result.RequestID, "feishu:cli_test")
	if err != nil || pending.ResourceDisplayName != fallbackResourceDisplayName("docx", "doxcn_external") {
		t.Fatalf("pending resource display context = %#v err=%v", pending, err)
	}
	event := resourceAccessCardEvent(result.RequestID, "ou_requester", "oc_chat", "om_card")
	event.Event.Action.Value["action"] = resourceAccessCardActionApproveAll
	response, err := manager.HandleCardAction(context.Background(), event)
	if err != nil || response == nil || response.Toast == nil || response.Toast.Type != "success" {
		t.Fatalf("approve-all response = %#v err=%v", response, err)
	}
	completed := waitForResourceAccessCompletion(t, st, sender, result.RequestID)
	if completed.GrantMode != store.FeishuResourceGrantModeAll || authCalls != 1 {
		t.Fatalf("completed permanent request = %#v auth_calls=%d", completed, authCalls)
	}
	grant, active, err := st.ActiveFeishuResourceGrant(
		"feishu:cli_test", store.FeishuResourceGrantActorTypeOpenID, "ou_requester", "oc_chat",
		"docx", "doxcn_external", store.FeishuResourcePermissionRead, manager.currentTime(),
	)
	if err != nil || !active || grant.GrantMode != store.FeishuResourceGrantModeAll || !grant.ExpiresAt.IsZero() {
		t.Fatalf("permanent resource grant = %#v active=%t err=%v", grant, active, err)
	}
	_, updates, _ := sender.snapshot()
	for _, update := range updates {
		if strings.Contains(update.text, "前往飞书官方授权页面") {
			t.Fatalf("existing capability unexpectedly entered OAuth: %s", update.text)
		}
	}
}

func TestResourceAccessManagerUsesPersistedOAuthCredentialAfterApproval(t *testing.T) {
	var permissionCalls, verifyCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/drive/v1/permissions/doxcn_external/members":
			permissionCalls++
			if r.Header.Get("Authorization") != "Bearer stored-user-access-token" {
				t.Fatalf("permission Authorization = %q", r.Header.Get("Authorization"))
			}
			writeResourceAccessJSON(t, w, map[string]any{"code": 0, "msg": "Success", "data": map[string]any{}})
		case "/open-apis/auth/v3/tenant_access_token/internal":
			writeResourceAccessJSON(t, w, tenantTokenResponseForResourceAccess())
		case "/open-apis/drive/v1/permissions/doxcn_external/members/auth":
			verifyCalls++
			writeResourceAccessJSON(t, w, map[string]any{"code": 0, "msg": "Success", "data": map[string]any{"auth_result": true}})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	oauth := resourceAccessOAuthConfig{
		ClientID: "cli_xxx", BaseURL: server.URL,
		CallbackURL: "https://bridge.example.com/feishu/oauth/callback",
	}
	manager, st, sender := newTestResourceAccessManager(t, server, oauth)
	if _, err := manager.persistFeishuOAuthCredential(context.Background(), feishuOAuthIdentity{
		OpenID: "ou_requester", UserID: "u_requester",
	}, feishuOAuthTokenBundle{
		AccessToken: "stored-user-access-token", AccessTokenExpiresIn: 2 * time.Hour,
		RefreshToken: "stored-refresh-token", RefreshTokenExpiresIn: 30 * 24 * time.Hour,
		Scopes: resourceAccessOAuthScope,
	}); err != nil {
		t.Fatalf("persistFeishuOAuthCredential returned error: %v", err)
	}
	result, err := manager.RequestAccess(resourceAccessRequestContext(), feishutools.ResourceAccessRequest{
		ResourceType: "docx", ResourceToken: "doxcn_external", Permission: feishutools.ResourcePermissionWrite,
		OnceDurationMinutes: 40,
	})
	if err != nil || result.Status != feishutools.ResourceAccessStatusPending {
		t.Fatalf("RequestAccess = %#v err=%v", result, err)
	}
	cards, _, _ := sender.snapshot()
	if len(cards) != 1 || !strings.Contains(cards[0].text, "已保存可能可用的加密 OAuth 凭证") {
		t.Fatalf("stored-credential resource card = %#v", cards)
	}
	event := resourceAccessCardEvent(result.RequestID, "ou_requester", "oc_chat", "om_card")
	event.Event.Action.Value["action"] = resourceAccessCardActionApproveOnce
	response, err := manager.HandleCardAction(context.Background(), event)
	if err != nil || response == nil || response.Toast == nil || response.Toast.Type != "success" {
		t.Fatalf("approve-once response = %#v err=%v", response, err)
	}
	completed := waitForResourceAccessCompletion(t, st, sender, result.RequestID)
	if completed.GrantMode != store.FeishuResourceGrantModeOnce || permissionCalls != 1 || verifyCalls != 1 {
		t.Fatalf("completed stored-OAuth request = %#v permission_calls=%d verify_calls=%d", completed, permissionCalls, verifyCalls)
	}
	_, updates, _ := sender.snapshot()
	for _, update := range updates {
		if strings.Contains(update.text, "前往飞书官方授权页面") {
			t.Fatalf("stored OAuth credential unexpectedly produced OAuth handoff card: %s", update.text)
		}
	}
}

func TestResourceAccessManagerUpgradesExistingCollaboratorAfterApproval(t *testing.T) {
	var createCalls, updateCalls, verifyCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/drive/v1/permissions/doxcn_external/members":
			createCalls++
			if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer stored-user-access-token" {
				t.Fatalf("create collaborator method/auth = %s/%q", r.Method, r.Header.Get("Authorization"))
			}
			writeResourceAccessJSON(t, w, map[string]any{"code": 1063003, "msg": "Invalid operation"})
		case "/open-apis/drive/v1/permissions/doxcn_external/members/auth":
			verifyCalls++
			writeResourceAccessJSON(t, w, map[string]any{
				"code": 0,
				"msg":  "Success",
				"data": map[string]any{"auth_result": verifyCalls > 1},
			})
		case "/open-apis/drive/v1/permissions/doxcn_external/members/ou_bot":
			updateCalls++
			if r.Method != http.MethodPut || r.Header.Get("Authorization") != "Bearer stored-user-access-token" {
				t.Fatalf("update collaborator method/auth = %s/%q", r.Method, r.Header.Get("Authorization"))
			}
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode update collaborator request: %v", err)
			}
			if body["member_type"] != "openid" || body["perm"] != "edit" {
				t.Fatalf("update collaborator body = %#v", body)
			}
			writeResourceAccessJSON(t, w, map[string]any{"code": 0, "msg": "Success", "data": map[string]any{}})
		case "/open-apis/auth/v3/tenant_access_token/internal":
			writeResourceAccessJSON(t, w, tenantTokenResponseForResourceAccess())
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	manager, st, sender := newTestResourceAccessManager(t, server, resourceAccessOAuthConfig{
		ClientID:    "cli_xxx",
		BaseURL:     server.URL,
		CallbackURL: "https://bridge.example.com/feishu/oauth/callback",
	})
	if _, err := manager.persistFeishuOAuthCredential(context.Background(), feishuOAuthIdentity{
		OpenID: "ou_requester", UserID: "u_requester",
	}, feishuOAuthTokenBundle{
		AccessToken: "stored-user-access-token", AccessTokenExpiresIn: 2 * time.Hour,
		RefreshToken: "stored-refresh-token", RefreshTokenExpiresIn: 30 * 24 * time.Hour,
		Scopes: resourceAccessOAuthScope,
	}); err != nil {
		t.Fatalf("persistFeishuOAuthCredential returned error: %v", err)
	}
	result, err := manager.RequestAccess(resourceAccessRequestContext(), feishutools.ResourceAccessRequest{
		ResourceType: "docx", ResourceToken: "doxcn_external", Permission: feishutools.ResourcePermissionWrite,
		OnceDurationMinutes: 30,
	})
	if err != nil || result.Status != feishutools.ResourceAccessStatusPending {
		t.Fatalf("RequestAccess = %#v err=%v", result, err)
	}
	event := resourceAccessCardEvent(result.RequestID, "ou_requester", "oc_chat", "om_card")
	event.Event.Action.Value["action"] = resourceAccessCardActionApproveOnce
	if response, err := manager.HandleCardAction(context.Background(), event); err != nil || response == nil || response.Toast == nil || response.Toast.Type != "success" {
		t.Fatalf("approve-once response = %#v err=%v", response, err)
	}
	completed := waitForResourceAccessCompletion(t, st, sender, result.RequestID)
	if completed.GrantMode != store.FeishuResourceGrantModeOnce || createCalls != 1 || updateCalls != 1 || verifyCalls != 2 {
		t.Fatalf("completed upgraded request = %#v create=%d update=%d verify=%d", completed, createCalls, updateCalls, verifyCalls)
	}
}

func TestResourceAccessManagerDoesNotReuseGrantWithoutActiveCapability(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected Feishu API request without active capability: %s", r.URL.Path)
	}))
	defer server.Close()
	manager, st, sender := newTestResourceAccessManager(t, server, resourceAccessOAuthConfig{})
	now := manager.currentTime()
	if _, err := st.UpsertFeishuResourceGrant(store.FeishuResourceGrant{
		AccountID:       "feishu:cli_test",
		ActorType:       store.FeishuResourceGrantActorTypeOpenID,
		ActorID:         "ou_requester",
		ChatID:          "oc_chat",
		ResourceType:    "docx",
		ResourceToken:   "doxcn_external",
		Permission:      store.FeishuResourcePermissionWrite,
		GrantMode:       store.FeishuResourceGrantModeOnce,
		SourceRequestID: "req_original",
		State:           store.FeishuResourceGrantStateActive,
		ExpiresAt:       now.Add(10 * time.Minute),
		CreatedAt:       now,
		UpdatedAt:       now,
	}); err != nil {
		t.Fatalf("UpsertFeishuResourceGrant returned error: %v", err)
	}
	result, err := manager.RequestAccess(resourceAccessRequestContext(), feishutools.ResourceAccessRequest{
		ResourceType:        "docx",
		ResourceToken:       "doxcn_external",
		Permission:          feishutools.ResourcePermissionRead,
		OnceDurationMinutes: 30,
	})
	if err != nil {
		t.Fatalf("RequestAccess returned error: %v", err)
	}
	if result.Status != feishutools.ResourceAccessStatusUnsupported || result.Source != "" {
		t.Fatalf("missing-capability result = %#v", result)
	}
	if _, active, err := st.ActiveFeishuResourceGrant(
		"feishu:cli_test", store.FeishuResourceGrantActorTypeOpenID, "ou_requester", "oc_chat", "docx", "doxcn_external", store.FeishuResourcePermissionRead, now,
	); err != nil || active {
		t.Fatalf("stale local grant active=%t err=%v, want false", active, err)
	}
	if cards, _, _ := sender.snapshot(); len(cards) != 0 {
		t.Fatalf("missing-capability cards = %#v, want none without OAuth", cards)
	}
}

func TestResourceAccessManagerKeepsCapabilityOnTransientLiveCheckError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			writeResourceAccessJSON(t, w, tenantTokenResponseForResourceAccess())
		case "/open-apis/drive/v1/permissions/doxcn_external/members/auth":
			w.WriteHeader(http.StatusInternalServerError)
			writeResourceAccessJSON(t, w, map[string]any{"code": 99999, "msg": "temporary failure"})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	manager, st, sender := newTestResourceAccessManager(t, server, resourceAccessOAuthConfig{})
	now := manager.currentTime()
	if _, err := st.UpsertFeishuResourceCapability(store.FeishuResourceCapability{
		AccountID:         "feishu:cli_test",
		ResourceType:      "docx",
		ResourceToken:     "doxcn_external",
		Permission:        store.FeishuResourcePermissionWrite,
		SubjectType:       "openid",
		SubjectID:         "ou_bot",
		SourceActorOpenID: "ou_requester",
		SourceRequestID:   "req_original",
		State:             store.FeishuResourceCapabilityStateActive,
		CreatedAt:         now,
		VerifiedAt:        now,
	}); err != nil {
		t.Fatalf("UpsertFeishuResourceCapability returned error: %v", err)
	}
	if _, err := st.UpsertFeishuResourceGrant(store.FeishuResourceGrant{
		AccountID:       "feishu:cli_test",
		ActorType:       store.FeishuResourceGrantActorTypeOpenID,
		ActorID:         "ou_requester",
		ChatID:          "oc_chat",
		ResourceType:    "docx",
		ResourceToken:   "doxcn_external",
		Permission:      store.FeishuResourcePermissionWrite,
		GrantMode:       store.FeishuResourceGrantModeOnce,
		SourceRequestID: "req_original",
		State:           store.FeishuResourceGrantStateActive,
		ExpiresAt:       now.Add(10 * time.Minute),
		CreatedAt:       now,
		UpdatedAt:       now,
	}); err != nil {
		t.Fatalf("UpsertFeishuResourceGrant returned error: %v", err)
	}
	if _, err := manager.RequestAccess(resourceAccessRequestContext(), feishutools.ResourceAccessRequest{
		ResourceType:        "docx",
		ResourceToken:       "doxcn_external",
		Permission:          feishutools.ResourcePermissionRead,
		OnceDurationMinutes: 30,
	}); err == nil || !strings.Contains(err.Error(), "live-check existing feishu resource capability") {
		t.Fatalf("transient live-check error = %v", err)
	}
	if _, active, err := st.ActiveFeishuResourceCapability(
		"feishu:cli_test", "docx", "doxcn_external", "openid", "ou_bot", store.FeishuResourcePermissionRead,
	); err != nil || !active {
		t.Fatalf("capability after transient error active=%t err=%v, want true", active, err)
	}
	if _, active, err := st.ActiveFeishuResourceGrant(
		"feishu:cli_test", store.FeishuResourceGrantActorTypeOpenID, "ou_requester", "oc_chat", "docx", "doxcn_external", store.FeishuResourcePermissionRead, now,
	); err != nil || !active {
		t.Fatalf("grant after transient error active=%t err=%v, want true", active, err)
	}
	if cards, _, _ := sender.snapshot(); len(cards) != 0 {
		t.Fatalf("transient live-check cards = %#v, want none", cards)
	}
}

func TestResourceAccessOAuthHTTPCallbackGrantsAndRedirects(t *testing.T) {
	testResourceAccessOAuthCompletion(t, "http")
}

func TestResourceAccessOAuthCardURLHandoffGrantsWithoutListener(t *testing.T) {
	testResourceAccessOAuthCompletion(t, "card_url")
}

func TestResourceAccessOAuthCardCodeHandoffGrantsWithoutListener(t *testing.T) {
	testResourceAccessOAuthCompletion(t, "card_code")
}

func TestResourceAccessOAuthTokenErrorDoesNotExposeSupportInstructions(t *testing.T) {
	logs := captureMonitorLogs(t)
	logging.SetLevel(logging.Debug)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/v3/token" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("X-Tt-Logid", "log_pkce_failure")
		w.WriteHeader(http.StatusBadRequest)
		writeResourceAccessJSON(t, w, map[string]any{
			"code":              20049,
			"error":             "invalid_grant",
			"error_description": "PKCE code challenge failed.",
		})
	}))
	defer server.Close()

	oauth := resourceAccessOAuthConfig{
		ClientID:    "cli_xxx",
		BaseURL:     server.URL,
		CallbackURL: "https://bridge.example.com/feishu/oauth/callback",
	}
	manager, st, sender := newTestResourceAccessManager(t, server, oauth)
	result, err := manager.RequestAccess(resourceAccessRequestContext(), feishutools.ResourceAccessRequest{
		ResourceType:        "docx",
		ResourceToken:       "doxcn_external",
		Permission:          feishutools.ResourcePermissionWrite,
		OnceDurationMinutes: 30,
	})
	if err != nil {
		t.Fatalf("RequestAccess returned error: %v", err)
	}
	if _, _, err := st.CommitWorkflowContinuation(result.RequestID, "feishu:cli_test", 8, manager.currentTime()); err != nil {
		t.Fatalf("CommitWorkflowContinuation returned error: %v", err)
	}
	cards, _, _ := sender.snapshot()
	if len(cards) != 1 {
		t.Fatalf("sent cards = %#v", cards)
	}
	oauthCard := approveResourceAccessAndWaitForOAuthCard(t, manager, st, sender, result.RequestID, resourceAccessCardActionApproveOnce)
	for _, fragment := range []string{
		fallbackResourceDisplayName("docx", "doxcn_external"),
		"允许 30 分钟",
		"授权成功后 30 分钟",
		"批准后需要在飞书官方页面完成 OAuth",
	} {
		if !strings.Contains(oauthCard, fragment) {
			t.Fatalf("OAuth handoff card lost resource context %q: %s", fragment, oauthCard)
		}
	}
	state := resourceAccessCardState(t, oauthCard)
	pending, err := st.GetFeishuResourceAccessRequest(result.RequestID, "feishu:cli_test")
	if err != nil {
		t.Fatalf("load pending request: %v", err)
	}
	if pending.PKCEVerifier != "" {
		t.Fatalf("new OAuth request has legacy PKCE verifier: %#v", pending)
	}
	recorder := httptest.NewRecorder()
	callbackRequest := httptest.NewRequest(http.MethodGet, "/feishu/oauth/callback?code=auth-code&state="+url.QueryEscape(state), nil)
	manager.HandleOAuthCallback(recorder, callbackRequest)
	if recorder.Code != http.StatusBadGateway || !strings.Contains(recorder.Body.String(), "authorization code exchange failed") {
		t.Fatalf("callback response = %d %q", recorder.Code, recorder.Body.String())
	}
	failed, err := st.GetFeishuResourceAccessRequest(result.RequestID, "feishu:cli_test")
	if err != nil || failed.State != store.FeishuResourceAccessStateFailed || failed.PKCEVerifier != "" || failed.OAuthStateHash != "" {
		t.Fatalf("failed request = %#v err=%v", failed, err)
	}
	workflowResult, err := st.GetWorkflowResult(result.RequestID, "feishu:cli_test")
	if err != nil || workflowResult.State != store.WorkflowResultStateFailed || !strings.Contains(string(workflowResult.Payload), `"status":"failed"`) {
		t.Fatalf("failed workflow result = %#v err=%v", workflowResult, err)
	}
	resumer := &fakeWorkflowResumer{text: "oauth failure handled"}
	resumeSender := &fakeWorkflowResumeTextSender{}
	worker, err := newWorkflowContinuationWorker(st, resumer, resumeSender, &fakeWorkflowResumeCardUpdater{}, store.Account{ID: "feishu:cli_test"}, nil)
	if err != nil {
		t.Fatalf("newWorkflowContinuationWorker returned error: %v", err)
	}
	worker.now = func() time.Time { return manager.currentTime().Add(time.Second) }
	worker.processAvailable(t.Context())
	worker.processAvailable(t.Context())
	if len(resumer.requests) != 1 || resumer.requests[0].Result.State != store.WorkflowResultStateFailed || len(resumeSender.calls) != 1 {
		t.Fatalf("OAuth failure resume requests/sends = %#v/%#v", resumer.requests, resumeSender.calls)
	}
	_, updates, messages := sender.snapshot()
	if len(updates) < 2 || updates[len(updates)-1].messageID != "om_card" || !strings.Contains(updates[len(updates)-1].text, "授权失败") || len(messages) != 0 {
		t.Fatalf("updates/messages = %#v/%#v", updates, messages)
	}
	for _, output := range []string{updates[len(updates)-1].text, recorder.Body.String()} {
		for _, forbidden := range []string{"request_log_id", "log_pkce_failure", "联系飞书支持", "联系管理员"} {
			if strings.Contains(output, forbidden) {
				t.Fatalf("user-facing OAuth failure exposed %q: %q", forbidden, output)
			}
		}
	}
	logText := logs.String()
	for _, fragment := range []string{
		"feishu resource OAuth token error response",
		"pkce_mode=disabled",
		"sdk_code_verifier_present=false",
		"oauth_service_inconsistency=true",
		"http_status=400",
		"feishu_code=20049",
		`oauth_error_type="invalid_grant"`,
		`request_log_id="log_pkce_failure"`,
		"code_ref=" + shortResourceRef("auth-code"),
	} {
		if !strings.Contains(logText, fragment) {
			t.Fatalf("OAuth failure diagnostics missing %q:\n%s", fragment, logText)
		}
	}
	for _, secret := range []string{state, "auth-code", oauth.CallbackURL} {
		if strings.Contains(logText, secret) {
			t.Fatalf("OAuth failure diagnostics leaked sensitive value %q:\n%s", secret, logText)
		}
	}
}

func TestResourceAccessOAuthRejectsLegacyPKCERequestBeforeTokenExchange(t *testing.T) {
	var tokenCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenCalls++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	manager, st, sender := newTestResourceAccessManager(t, server, resourceAccessOAuthConfig{
		ClientID:    "cli_xxx",
		BaseURL:     server.URL,
		CallbackURL: "https://bridge.example.com/feishu/oauth/callback",
	})
	now := manager.currentTime()
	request, err := st.CreateFeishuResourceAccessRequest(store.FeishuResourceAccessRequest{
		AccountID:           "feishu:cli_test",
		ActorOpenID:         "ou_requester",
		ActorUserID:         "u_requester",
		ChatID:              "oc_chat",
		SourceMessageID:     "om_source",
		ResourceType:        "docx",
		ResourceToken:       "doxcn_external",
		ResourceURL:         "https://docs.feishu.cn/docx/doxcn_external",
		Permission:          store.FeishuResourcePermissionWrite,
		Reason:              "legacy request",
		OnceDurationMinutes: 30,
		CreatedAt:           now,
		ExpiresAt:           now.Add(10 * time.Minute),
	})
	if err != nil {
		t.Fatalf("CreateFeishuResourceAccessRequest returned error: %v", err)
	}
	state := "legacy-oauth-state"
	legacyVerifier := "legacy-pkce-verifier"
	if err := st.SetFeishuResourceAccessCardMessageID(request.ID, request.AccountID, "om_card", now); err != nil {
		t.Fatalf("SetFeishuResourceAccessCardMessageID returned error: %v", err)
	}
	request, err = st.ApproveFeishuResourceAccessRequest(request.ID, request.AccountID, store.FeishuResourceGrantModeOnce, store.FeishuResourceAccessMatch{
		ActorOpenID: request.ActorOpenID, ActorUserID: request.ActorUserID, ChatID: request.ChatID, CardMessageID: "om_card",
	}, now)
	if err != nil {
		t.Fatalf("ApproveFeishuResourceAccessRequest returned error: %v", err)
	}
	if err := st.PrepareFeishuResourceAccessOAuth(request.ID, request.AccountID, hashResourceAccessState(state), legacyVerifier, "openid", "ou_bot", now); err != nil {
		t.Fatalf("PrepareFeishuResourceAccessOAuth returned error: %v", err)
	}

	recorder := httptest.NewRecorder()
	callbackRequest := httptest.NewRequest(http.MethodGet, "/feishu/oauth/callback?code=auth-code&state="+url.QueryEscape(state), nil)
	manager.HandleOAuthCallback(recorder, callbackRequest)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "legacy PKCE authorization request") {
		t.Fatalf("legacy callback response = %d %q", recorder.Code, recorder.Body.String())
	}
	if tokenCalls != 0 {
		t.Fatalf("legacy request reached token endpoint %d times", tokenCalls)
	}
	failed, err := st.GetFeishuResourceAccessRequest(request.ID, request.AccountID)
	if err != nil || failed.State != store.FeishuResourceAccessStateFailed || failed.PKCEVerifier != "" || failed.OAuthStateHash != "" {
		t.Fatalf("failed legacy request = %#v err=%v", failed, err)
	}
	_, updates, messages := sender.snapshot()
	if len(updates) != 1 || !strings.Contains(updates[0].text, "旧版本") || len(messages) != 0 {
		t.Fatalf("legacy updates/messages = %#v/%#v", updates, messages)
	}
	if strings.Contains(updates[0].text, legacyVerifier) {
		t.Fatalf("legacy verifier leaked to user-facing output: %q", updates[0].text)
	}
}

func testResourceAccessOAuthCompletion(t *testing.T, mode string) {
	t.Helper()
	logs := captureMonitorLogs(t)
	logging.SetLevel(logging.Debug)
	var mu sync.Mutex
	var tokenBody map[string]any
	var permissionBody map[string]any
	var userInfoCalls, permissionCalls, verifyCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/v3/token":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode OAuth token body: %v", err)
			}
			mu.Lock()
			tokenBody = body
			mu.Unlock()
			writeResourceAccessJSON(t, w, map[string]any{
				"access_token":             "user-access-token",
				"token_type":               "Bearer",
				"expires_in":               7200,
				"refresh_token":            "user-refresh-token",
				"refresh_token_expires_in": 2592000,
				"scope":                    resourceAccessOAuthScope,
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
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode permission body: %v", err)
			}
			mu.Lock()
			permissionBody = body
			mu.Unlock()
			writeResourceAccessJSON(t, w, map[string]any{"code": 0, "msg": "Success", "data": map[string]any{"member": body}})
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
		ClientID:    "cli_xxx",
		BaseURL:     server.URL,
		CallbackURL: "https://bridge.example.com/feishu/oauth/callback",
	}
	if mode == "http" {
		oauth.CallbackListenAddress = "127.0.0.1:0"
	}
	manager, st, sender := newTestResourceAccessManager(t, server, oauth)
	result, err := manager.RequestAccess(resourceAccessRequestContext(), feishutools.ResourceAccessRequest{
		ResourceType:        "docx",
		ResourceToken:       "doxcn_external",
		ResourceURL:         "https://docs.feishu.cn/docx/doxcn_external",
		Permission:          feishutools.ResourcePermissionWrite,
		OnceDurationMinutes: 30,
		Reason:              "create a reviewed copy",
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
	if !strings.Contains(cards[0].text, "允许 30 分钟") || !strings.Contains(cards[0].text, "永久允许") || strings.Contains(cards[0].text, "前往飞书官方授权页面") {
		t.Fatalf("initial resource authorization choice card = %s", cards[0].text)
	}
	approvalAction := resourceAccessCardActionApproveOnce
	wantGrantMode := store.FeishuResourceGrantModeOnce
	if mode == "card_code" {
		approvalAction = resourceAccessCardActionApproveAll
		wantGrantMode = store.FeishuResourceGrantModeAll
	}
	oauthCard := approveResourceAccessAndWaitForOAuthCard(t, manager, st, sender, result.RequestID, approvalAction)
	authURL := resourceAccessCardURL(t, oauthCard)
	parsedAuthURL, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse card auth URL: %v", err)
	}
	state := parsedAuthURL.Query().Get("state")
	if parsedAuthURL.Path != "/open-apis/authen/v1/authorize" || parsedAuthURL.Query().Get("client_id") != "cli_xxx" ||
		parsedAuthURL.Query().Get("redirect_uri") != oauth.CallbackURL || parsedAuthURL.Query().Get("scope") != resourceAccessOAuthScope ||
		parsedAuthURL.Query().Get("prompt") != "consent" || state == "" || parsedAuthURL.Query().Has("code_challenge") ||
		parsedAuthURL.Query().Has("code_challenge_method") {
		t.Fatalf("authorization URL = %s", authURL)
	}
	storedPending, err := st.GetFeishuResourceAccessRequest(result.RequestID, "feishu:cli_test")
	if err != nil || storedPending.GrantMode != wantGrantMode || storedPending.DecisionAt.IsZero() || storedPending.OAuthStateHash == "" || storedPending.OAuthStateHash == state || storedPending.PKCEVerifier != "" {
		t.Fatalf("stored pending request = %#v err=%v", storedPending, err)
	}
	continuation, err := st.GetWorkflowContinuation(result.RequestID, "feishu:cli_test")
	if err != nil {
		t.Fatalf("GetWorkflowContinuation returned error: %v", err)
	}
	if continuation.State != store.WorkflowContinuationStateWaiting || continuation.CommittedRevision != -1 ||
		continuation.UserKey != "feishu:ou_requester" || continuation.SessionID != "session-work" ||
		continuation.ChatID != "oc_chat" || continuation.SourceMessageID != "om_source" ||
		continuation.ActorOpenID != "ou_requester" || continuation.OriginRevision != 7 ||
		continuation.OriginTurnID != "turn-test" || continuation.ToolCallID != "call-test" ||
		continuation.ToolName != feishutools.ResourceAccessToolName {
		t.Fatalf("stored continuation = %#v", continuation)
	}

	expectedCode := "auth-code"
	encodedCallbackCode := "auth-code"
	if mode == "http" || mode == "card_url" {
		expectedCode = "auth+code/%value"
		encodedCallbackCode = "auth+code%2F%25value"
	}

	switch mode {
	case "http":
		recorder := httptest.NewRecorder()
		callbackRequest := httptest.NewRequest(http.MethodGet, "/feishu/oauth/callback?code="+encodedCallbackCode+"&state="+url.QueryEscape(state), nil)
		manager.HandleOAuthCallback(recorder, callbackRequest)
		if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != result.ResourceURL {
			t.Fatalf("callback response status/location = %d/%q", recorder.Code, recorder.Header().Get("Location"))
		}
	case "card_url":
		callbackURL := oauth.CallbackURL + "?code=" + encodedCallbackCode + "&state=" + url.QueryEscape(state)
		response, err := manager.HandleCardAction(context.Background(), resourceAccessCardSubmitEvent(result.RequestID, "ou_requester", "oc_chat", "om_card", callbackURL))
		if err != nil || response == nil || response.Toast == nil || response.Toast.Type != "success" {
			t.Fatalf("card URL handoff response = %#v err=%v", response, err)
		}
	case "card_code":
		response, err := manager.HandleCardAction(context.Background(), resourceAccessCardSubmitEvent(result.RequestID, "ou_requester", "oc_chat", "om_card", "auth-code"))
		if err != nil || response == nil || response.Toast == nil || response.Toast.Type != "success" {
			t.Fatalf("card code handoff response = %#v err=%v", response, err)
		}
	default:
		t.Fatalf("unsupported OAuth completion test mode %q", mode)
	}
	completed := waitForResourceAccessCompletion(t, st, sender, result.RequestID)
	mu.Lock()
	gotTokenBody := tokenBody
	gotPermissionBody := permissionBody
	calls := []int{userInfoCalls, permissionCalls, verifyCalls}
	mu.Unlock()
	_, hasCodeVerifier := gotTokenBody["code_verifier"]
	if gotTokenBody["grant_type"] != "authorization_code" || gotTokenBody["client_id"] != "cli_xxx" || gotTokenBody["client_secret"] != "secret" ||
		gotTokenBody["code"] != expectedCode || gotTokenBody["redirect_uri"] != oauth.CallbackURL || hasCodeVerifier {
		t.Fatalf("OAuth token body = %#v", gotTokenBody)
	}
	if gotPermissionBody["member_type"] != "openid" || gotPermissionBody["member_id"] != "ou_bot" || gotPermissionBody["perm"] != "edit" || gotPermissionBody["type"] != "user" {
		t.Fatalf("permission body = %#v", gotPermissionBody)
	}
	if calls[0] != 1 || calls[1] != 1 || calls[2] != 1 {
		t.Fatalf("user/permission/verify calls = %#v", calls)
	}
	if completed.State != store.FeishuResourceAccessStateSucceeded || completed.GrantSource != store.FeishuResourceGrantSourceNewlyGranted || completed.PKCEVerifier != "" || completed.OAuthStateHash != "" {
		t.Fatalf("completed request = %#v", completed)
	}
	workflowResult, err := st.GetWorkflowResult(result.RequestID, "feishu:cli_test")
	if err != nil || workflowResult.State != store.WorkflowResultStateSucceeded ||
		!strings.Contains(string(workflowResult.Payload), `"status":"granted"`) ||
		!strings.Contains(string(workflowResult.Payload), `"resource_token":"doxcn_external"`) {
		t.Fatalf("completed workflow result = %#v err=%v", workflowResult, err)
	}
	grant, active, err := st.ActiveFeishuResourceGrant(
		"feishu:cli_test", store.FeishuResourceGrantActorTypeOpenID, "ou_requester", "oc_chat", "docx", "doxcn_external", store.FeishuResourcePermissionWrite, manager.currentTime(),
	)
	if err != nil || !active || grant.SourceRequestID != result.RequestID || grant.GrantMode != wantGrantMode {
		t.Fatalf("saved grant = %#v active=%t err=%v", grant, active, err)
	}
	if wantGrantMode == store.FeishuResourceGrantModeOnce && !grant.ExpiresAt.Equal(manager.currentTime().Add(30*time.Minute)) {
		t.Fatalf("temporary grant expiry = %s, want %s", grant.ExpiresAt, manager.currentTime().Add(30*time.Minute))
	}
	if wantGrantMode == store.FeishuResourceGrantModeAll && !grant.ExpiresAt.IsZero() {
		t.Fatalf("permanent grant expiry = %s, want zero", grant.ExpiresAt)
	}
	capability, active, err := st.ActiveFeishuResourceCapability("feishu:cli_test", "docx", "doxcn_external", "openid", "ou_bot", store.FeishuResourcePermissionWrite)
	if err != nil || !active || capability.SourceActorOpenID != "ou_requester" || capability.SourceRequestID != result.RequestID {
		t.Fatalf("saved capability = %#v active=%t err=%v", capability, active, err)
	}
	credential, err := st.GetFeishuUserOAuthCredential("feishu:cli_test", "ou_requester", "u_requester")
	if err != nil || credential.Status != store.FeishuUserOAuthCredentialStatusActive || credential.Version != 1 ||
		credential.AccessTokenCiphertext == "" || credential.RefreshTokenCiphertext == "" ||
		strings.Contains(credential.AccessTokenCiphertext, "user-access-token") || strings.Contains(credential.RefreshTokenCiphertext, "user-refresh-token") {
		t.Fatalf("encrypted OAuth credential = %#v err=%v", credential, err)
	}
	storedAccessToken, err := manager.feishuUserAccessToken(context.Background(), "ou_requester", "u_requester")
	if err != nil || storedAccessToken != "user-access-token" {
		t.Fatalf("stored OAuth access token = %q err=%v", storedAccessToken, err)
	}
	_, updates, messages := sender.snapshot()
	if len(updates) < 2 || updates[len(updates)-1].messageID != "om_card" || !strings.Contains(updates[len(updates)-1].text, "权限已授予") || len(messages) != 0 {
		t.Fatalf("updates/messages = %#v/%#v", updates, messages)
	}
	logText := logs.String()
	callbackLog := "parsed feishu resource OAuth card handoff"
	if mode == "http" {
		callbackLog = "parsed feishu resource OAuth HTTP callback"
	}
	requiredLogFragments := []string{
		"prepared feishu resource OAuth request",
		"built feishu resource OAuth authorization URL",
		callbackLog,
		"prepared feishu resource OAuth token request",
		"received feishu resource OAuth token response",
		"pkce_mode=disabled",
		"state_ref=" + shortResourceRef(storedPending.OAuthStateHash),
		"auth_state_ref=" + shortResourceRef(storedPending.OAuthStateHash),
		"code_ref=" + shortResourceRef(expectedCode),
		"sdk_code_ref=" + shortResourceRef(expectedCode),
		"redirect_ref=" + shortResourceRef(oauth.CallbackURL),
		"sdk_redirect_ref=" + shortResourceRef(oauth.CallbackURL),
		"state_matches=true",
		"auth_code_challenge_present=false",
		"auth_code_challenge_method_present=false",
		"redirect_matches=true",
		"sdk_code_matches=true",
		"sdk_code_verifier_present=false",
		"sdk_redirect_matches=true",
		"scope_count=4",
		"access_token_present=true",
		"refresh_token_present=true",
	}
	for _, fragment := range requiredLogFragments {
		if !strings.Contains(logText, fragment) {
			t.Fatalf("OAuth diagnostic logs missing %q:\n%s", fragment, logText)
		}
	}
	for _, secret := range []string{
		state,
		expectedCode,
		authURL,
		oauth.CallbackURL,
		"user-access-token",
		"user-refresh-token",
		"doxcn_external",
	} {
		if strings.Contains(logText, secret) {
			t.Fatalf("OAuth diagnostic logs leaked sensitive value %q:\n%s", secret, logText)
		}
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
		ResourceType:        "folder",
		ResourceToken:       "fld_external",
		Permission:          feishutools.ResourcePermissionWrite,
		OnceDurationMinutes: 30,
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
	_, err := manager.RequestAccess(resourceAccessRequestContext(), feishutools.ResourceAccessRequest{
		ResourceType:        "docx",
		ResourceToken:       "doxcn_external",
		ResourceURL:         "https://example.com/doxcn_external",
		Permission:          feishutools.ResourcePermissionRead,
		OnceDurationMinutes: 30,
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
	result, err := manager.RequestAccess(resourceAccessRequestContext(), feishutools.ResourceAccessRequest{
		ResourceType:        "docx",
		ResourceToken:       "doxcn_external",
		Permission:          feishutools.ResourcePermissionRead,
		OnceDurationMinutes: 30,
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
	workflowResult, err := st.GetWorkflowResult(result.RequestID, "feishu:cli_test")
	if err != nil || workflowResult.State != store.WorkflowResultStateDenied || !strings.Contains(string(workflowResult.Payload), `"status":"denied"`) {
		t.Fatalf("denied workflow result = %#v err=%v", workflowResult, err)
	}
}

func TestResourceAccessRecoveryReconcilesExpiredWorkflowResult(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	manager, st, _ := newTestResourceAccessManager(t, server, resourceAccessOAuthConfig{
		ClientID:    "cli_xxx",
		BaseURL:     server.URL,
		CallbackURL: "https://oauth.wulongxin.com/feishu/oauth/callback",
	})
	result, err := manager.RequestAccess(resourceAccessRequestContext(), feishutools.ResourceAccessRequest{
		ResourceType:        "docx",
		ResourceToken:       "doxcn_external",
		ResourceURL:         "https://example.feishu.cn/docx/doxcn_external",
		Permission:          feishutools.ResourcePermissionWrite,
		OnceDurationMinutes: 30,
		Reason:              "append a summary",
	})
	if err != nil {
		t.Fatalf("RequestAccess returned error: %v", err)
	}
	manager.now = func() time.Time { return result.ExpiresAt }

	if err := manager.recoverPersistedRequests(t.Context()); err != nil {
		t.Fatalf("recoverPersistedRequests returned error: %v", err)
	}
	workflowResult, err := st.GetWorkflowResult(result.RequestID, "feishu:cli_test")
	if err != nil || workflowResult.State != store.WorkflowResultStateExpired || !strings.Contains(string(workflowResult.Payload), `"status":"expired"`) {
		t.Fatalf("reconciled workflow result = %#v err=%v", workflowResult, err)
	}
	continuation, err := st.GetWorkflowContinuation(result.RequestID, "feishu:cli_test")
	if err != nil || continuation.State != store.WorkflowContinuationStateWaiting {
		t.Fatalf("reconciled continuation = %#v err=%v", continuation, err)
	}
}

func TestResourceAccessRecoveryResumesApprovedPendingRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			writeResourceAccessJSON(t, w, tenantTokenResponseForResourceAccess())
		case "/open-apis/drive/v1/permissions/doxcn_external/members/auth":
			writeResourceAccessJSON(t, w, map[string]any{"code": 0, "msg": "Success", "data": map[string]any{"auth_result": true}})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	manager, st, sender := newTestResourceAccessManager(t, server, resourceAccessOAuthConfig{})
	now := manager.currentTime()
	if _, err := st.UpsertFeishuResourceCapability(store.FeishuResourceCapability{
		AccountID: "feishu:cli_test", ResourceType: "docx", ResourceToken: "doxcn_external",
		SubjectType: "openid", SubjectID: "ou_bot", Permission: store.FeishuResourcePermissionRead,
		SourceActorOpenID: "ou_requester", SourceRequestID: "req_capability",
		State: store.FeishuResourceCapabilityStateActive, CreatedAt: now, VerifiedAt: now,
	}); err != nil {
		t.Fatalf("UpsertFeishuResourceCapability returned error: %v", err)
	}
	result, err := manager.RequestAccess(resourceAccessRequestContext(), feishutools.ResourceAccessRequest{
		ResourceType: "docx", ResourceToken: "doxcn_external", Permission: feishutools.ResourcePermissionRead,
		OnceDurationMinutes: 25,
	})
	if err != nil || result.Status != feishutools.ResourceAccessStatusPending {
		t.Fatalf("RequestAccess = %#v err=%v", result, err)
	}
	request, err := st.GetFeishuResourceAccessRequest(result.RequestID, "feishu:cli_test")
	if err != nil {
		t.Fatalf("load pending request: %v", err)
	}
	if _, err := st.ApproveFeishuResourceAccessRequest(request.ID, request.AccountID, store.FeishuResourceGrantModeOnce, store.FeishuResourceAccessMatch{
		ActorOpenID: request.ActorOpenID, ActorUserID: request.ActorUserID, ChatID: request.ChatID, CardMessageID: request.CardMessageID,
	}, now.Add(time.Minute)); err != nil {
		t.Fatalf("persist approval decision: %v", err)
	}
	manager.now = func() time.Time { return now.Add(2 * time.Minute) }
	if err := manager.recoverPersistedRequests(t.Context()); err != nil {
		t.Fatalf("recoverPersistedRequests returned error: %v", err)
	}
	completed, err := st.GetFeishuResourceAccessRequest(result.RequestID, "feishu:cli_test")
	if err != nil || completed.State != store.FeishuResourceAccessStateSucceeded || completed.GrantMode != store.FeishuResourceGrantModeOnce {
		t.Fatalf("recovered request = %#v err=%v", completed, err)
	}
	grant, active, err := st.ActiveFeishuResourceGrant(
		"feishu:cli_test", store.FeishuResourceGrantActorTypeOpenID, "ou_requester", "oc_chat",
		"docx", "doxcn_external", store.FeishuResourcePermissionRead, manager.currentTime(),
	)
	wantExpiry := manager.currentTime().Add(25 * time.Minute)
	if err != nil || !active || !grant.ExpiresAt.Equal(wantExpiry) {
		t.Fatalf("recovered temporary grant = %#v active=%t err=%v want_expiry=%s", grant, active, err, wantExpiry)
	}
	_, updates, _ := sender.snapshot()
	if len(updates) == 0 || !strings.Contains(updates[len(updates)-1].text, "权限已授予") {
		t.Fatalf("recovered card updates = %#v", updates)
	}
}

func TestPendingResourceGrantCardContainsResourceOAuthAndGrantContext(t *testing.T) {
	raw, err := (pendingResourceGrantCard{request: store.FeishuResourceAccessRequest{
		ID:                  "req_test",
		ResourceType:        "folder",
		ResourceToken:       "fld_external",
		ResourceURL:         "https://docs.feishu.cn/drive/folder/fld_external",
		ResourceDisplayName: "项目交付目录",
		Permission:          store.FeishuResourcePermissionWrite,
		Reason:              "写入项目交付物",
		OnceDurationMinutes: 45,
		SubjectType:         "openchat",
		SubjectID:           "oc_chat",
		ExpiresAt:           time.Date(2026, time.August, 1, 12, 10, 0, 0, time.UTC),
	}, oauthStatus: resourceAccessOAuthStatusCredentialReady}).JSON()
	if err != nil {
		t.Fatalf("pendingResourceGrantCard.JSON returned error: %v", err)
	}
	var card map[string]any
	if err := json.Unmarshal([]byte(raw), &card); err != nil {
		t.Fatalf("unmarshal resource grant card: %v", err)
	}
	header, _ := card["header"].(map[string]any)
	title, _ := header["title"].(map[string]any)
	if title["content"] != "飞书资源权限申请" || header["template"] != "blue" {
		t.Fatalf("resource grant card header = %#v", header)
	}
	body, _ := card["body"].(map[string]any)
	elements, _ := body["elements"].([]any)
	form, _ := elements[0].(map[string]any)
	formElements, _ := form["elements"].([]any)
	description, _ := formElements[0].(map[string]any)
	content, _ := description["content"].(string)
	for _, fragment := range []string{"项目交付目录", "在飞书中打开", "写入（包含读取）", "当前群聊（openchat）", "已保存可能可用的加密 OAuth 凭证", "允许 45 分钟", "永久允许", "不会移除或降低飞书中的 Bot/群聊协作者权限", "写入项目交付物"} {
		if !strings.Contains(content, fragment) {
			t.Fatalf("resource grant card description missing %q: %s", fragment, content)
		}
	}
	if strings.Contains(content, "资源 Token") {
		t.Fatalf("resource grant card exposed a visible resource token field: %s", content)
	}
	once := findCardElementByName(card, "Button_resource_once")
	all := findCardElementByName(card, "Button_resource_all")
	reject := findCardElementByName(card, "Button_k7l2449r9dj")
	assertResourceAccessCardButtonBinding(t, once, "req_test", resourceAccessCardActionApproveOnce)
	assertResourceAccessCardButtonBinding(t, all, "req_test", resourceAccessCardActionApproveAll)
	assertResourceAccessCardButtonBinding(t, reject, "req_test", resourceAccessCardActionReject)
}

func TestPendingResourceAccessCardContainsOAuthHandoffForm(t *testing.T) {
	authURL := "https://accounts.feishu.cn/open-apis/authen/v1/authorize?state=secret"
	raw, err := (pendingResourceAccessCard{
		request: store.FeishuResourceAccessRequest{
			ID:                  "req_test",
			ResourceType:        "docx",
			ResourceToken:       "doxcn_external",
			ResourceURL:         "https://docs.feishu.cn/docx/doxcn_external",
			ResourceDisplayName: "项目计划文档",
			Permission:          store.FeishuResourcePermissionWrite,
			Reason:              "创建项目计划文档",
			OnceDurationMinutes: 30,
			GrantMode:           store.FeishuResourceGrantModeOnce,
			SubjectType:         "openid",
			SubjectID:           "ou_bot",
			ExpiresAt:           time.Date(2026, time.August, 1, 12, 10, 0, 0, time.UTC),
		},
		authURL: authURL,
	}).JSON()
	if err != nil {
		t.Fatalf("pendingResourceAccessCard.JSON returned error: %v", err)
	}
	var card map[string]any
	if err := json.Unmarshal([]byte(raw), &card); err != nil {
		t.Fatalf("unmarshal resource access card: %v", err)
	}
	if card["schema"] != "2.0" {
		t.Fatalf("resource card schema = %#v", card["schema"])
	}
	config, _ := card["config"].(map[string]any)
	style, _ := config["style"].(map[string]any)
	textSize, _ := style["text_size"].(map[string]any)
	normalV2, _ := textSize["normal_v2"].(map[string]any)
	if config["update_multi"] != true || normalV2["default"] != "normal" || normalV2["pc"] != "normal" || normalV2["mobile"] != "heading" {
		t.Fatalf("resource card config = %#v", config)
	}
	header, _ := card["header"].(map[string]any)
	title, _ := header["title"].(map[string]any)
	tags, _ := header["text_tag_list"].([]any)
	tag, _ := tags[0].(map[string]any)
	tagText, _ := tag["text"].(map[string]any)
	if title["content"] != "飞书文档权限申请" || tagText["content"] != "安全加密" || tag["color"] != "blue" || header["padding"] != "12px 8px 12px 8px" {
		t.Fatalf("resource card header = %#v", header)
	}
	body, _ := card["body"].(map[string]any)
	elements, _ := body["elements"].([]any)
	if body["direction"] != "vertical" || len(elements) != 4 {
		t.Fatalf("resource card body = %#v", body)
	}
	intro, _ := elements[0].(map[string]any)
	introText, _ := intro["text"].(map[string]any)
	if intro["tag"] != "div" || introText["content"] != "飞书资源授权" || introText["text_size"] != "normal_v2" {
		t.Fatalf("resource card intro = %#v", intro)
	}
	markdown, _ := elements[1].(map[string]any)
	markdownContent, _ := markdown["content"].(string)
	if markdown["tag"] != "markdown" || markdown["element_id"] != "KNJPSduXTksKaRe28qq6" || markdown["text_size"] != "normal_v2" ||
		!strings.Contains(markdownContent, "为了更好地为您提供服务") || !strings.Contains(markdownContent, "项目计划文档") ||
		!strings.Contains(markdownContent, "在飞书中打开") || !strings.Contains(markdownContent, "允许 30 分钟") ||
		!strings.Contains(markdownContent, "授权成功后 30 分钟") || !strings.Contains(markdownContent, "飞书官方页面完成 OAuth") ||
		!strings.Contains(markdownContent, "创建项目计划文档") ||
		!strings.Contains(markdownContent, "点击下方“前往飞书官方授权页面”按钮") ||
		!strings.Contains(markdownContent, "使用应用密钥加密保存 user_access_token 与 refresh_token") ||
		!strings.Contains(markdownContent, "不会发送给大模型或写入日志") || strings.Contains(markdownContent, "资源 Token") || strings.Contains(markdownContent, authURL) {
		t.Fatalf("resource card description = %#v", markdown)
	}
	openButton, _ := elements[2].(map[string]any)
	openButtonText, _ := openButton["text"].(map[string]any)
	openBehaviors, _ := openButton["behaviors"].([]any)
	if openButton["tag"] != "button" || openButtonText["content"] != "前往飞书官方授权页面" || openButton["type"] != "primary" ||
		openButton["width"] != "default" || openButton["size"] != "medium" || openButton["margin"] != "0px 0px 0px 0px" || len(openBehaviors) != 1 {
		t.Fatalf("resource card OAuth button = %#v", openButton)
	}
	openBehavior, _ := openBehaviors[0].(map[string]any)
	if openBehavior["type"] != "open_url" || openBehavior["default_url"] != authURL || openBehavior["android_url"] != "" || openBehavior["ios_url"] != "" || openBehavior["pc_url"] != "" {
		t.Fatalf("resource card OAuth button behavior = %#v", openBehavior)
	}
	form, _ := elements[3].(map[string]any)
	if form["tag"] != "form" || form["name"] != "privacy_form" || form["element_id"] != "STIJ_lgxwvFvn9xFUnT8" || form["padding"] != "12px 12px 12px 12px" {
		t.Fatalf("resource card form = %#v", form)
	}
	input := findCardElementByName(card, resourceAccessOAuthResultField)
	label, _ := input["label"].(map[string]any)
	if input == nil || input["tag"] != "input" || input["required"] != false || input["element_id"] != "e45nAhDEUoVmMTaWcZKP" || label["content"] != "授权回调 URL 或授权码" {
		t.Fatalf("OAuth result input = %#v", input)
	}
	submit := findCardElementByName(card, "submit_btn")
	if submit == nil || submit["form_action_type"] != "submit" || submit["width"] != "fill" || submit["element_id"] != "yJZDKLb72aTt6mKHuVam" || cardButtonAction(submit) != resourceAccessCardActionSubmitOAuth {
		t.Fatalf("OAuth submit button = %#v", submit)
	}
	assertResourceAccessCardButtonBinding(t, submit, "req_test", resourceAccessCardActionSubmitOAuth)
	suggestion := findCardElementByName(card, "Input_9luq5y9ljxa")
	if suggestion == nil || suggestion["tag"] != "input" || suggestion["width"] != "fill" {
		t.Fatalf("resource suggestion input = %#v", suggestion)
	}
	reject := findCardElementByName(card, "Button_ylh56j56ycl")
	if reject == nil || reject["form_action_type"] != "submit" || reject["type"] != "danger" || cardButtonAction(reject) != resourceAccessCardActionReject {
		t.Fatalf("resource reject button = %#v", reject)
	}
	assertResourceAccessCardButtonBinding(t, reject, "req_test", resourceAccessCardActionReject)
}

func TestParseResourceAccessOAuthSubmissionValidatesURLAndRawCode(t *testing.T) {
	manager := &resourceAccessManager{oauth: resourceAccessOAuthConfig{
		CallbackURL: "https://oauth.wulongxin.com/feishu/oauth/callback",
	}}
	submission, err := manager.parseResourceAccessOAuthSubmission("https://oauth.wulongxin.com/feishu/oauth/callback?code=auth-code&state=random-state")
	if err != nil || submission.InputKind != "url" || submission.Response.Code != "auth-code" || submission.StateHash != hashResourceAccessState("random-state") {
		t.Fatalf("URL submission = %#v err=%v", submission, err)
	}
	submission, err = manager.parseResourceAccessOAuthSubmission("raw-authorization-code")
	if err != nil || submission.InputKind != "code" || submission.Response.Code != "raw-authorization-code" || submission.StateHash != "" {
		t.Fatalf("raw-code submission = %#v err=%v", submission, err)
	}

	tests := []struct {
		name  string
		value string
	}{
		{name: "empty", value: ""},
		{name: "wrong scheme", value: "http://oauth.wulongxin.com/feishu/oauth/callback?code=x&state=y"},
		{name: "wrong host", value: "https://attacker.example/feishu/oauth/callback?code=x&state=y"},
		{name: "wrong path", value: "https://oauth.wulongxin.com/other?code=x&state=y"},
		{name: "missing state", value: "https://oauth.wulongxin.com/feishu/oauth/callback?code=x"},
		{name: "code and error", value: "https://oauth.wulongxin.com/feishu/oauth/callback?code=x&error=denied&state=y"},
		{name: "duplicate state", value: "https://oauth.wulongxin.com/feishu/oauth/callback?code=x&state=y&state=z"},
		{name: "raw whitespace", value: "code with spaces"},
		{name: "too long", value: strings.Repeat("a", resourceAccessOAuthResultMaxLength+1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := manager.parseResourceAccessOAuthSubmission(test.value); err == nil {
				t.Fatalf("parseResourceAccessOAuthSubmission(%q) returned no error", test.name)
			}
		})
	}
}

func TestNewResourceAccessOAuthValuesGeneratesHashedState(t *testing.T) {
	state, stateHash, err := newResourceAccessOAuthValues()
	if err != nil {
		t.Fatalf("newResourceAccessOAuthValues returned error: %v", err)
	}
	if len(state) != 43 || stateHash != hashResourceAccessState(state) {
		t.Fatalf("OAuth state length/hash = %d/%q", len(state), stateHash)
	}
	if stateHash == state || strings.Contains(stateHash, state) {
		t.Fatalf("OAuth state was not stored as an opaque hash: state=%q hash=%q", state, stateHash)
	}
}

func TestResourceAccessOAuthCardHandoffRejectsWrongUserAndState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected Feishu API request: %s", r.URL.Path)
	}))
	defer server.Close()
	manager, st, sender := newTestResourceAccessManager(t, server, resourceAccessOAuthConfig{
		ClientID:    "cli_xxx",
		BaseURL:     server.URL,
		CallbackURL: "https://oauth.wulongxin.com/feishu/oauth/callback",
	})
	result, err := manager.RequestAccess(resourceAccessRequestContext(), feishutools.ResourceAccessRequest{
		ResourceType:        "docx",
		ResourceToken:       "doxcn_external",
		Permission:          feishutools.ResourcePermissionRead,
		OnceDurationMinutes: 30,
	})
	if err != nil {
		t.Fatalf("RequestAccess returned error: %v", err)
	}
	cards, _, _ := sender.snapshot()
	if len(cards) != 1 {
		t.Fatalf("sent resource access cards = %#v", cards)
	}
	oauthCard := approveResourceAccessAndWaitForOAuthCard(t, manager, st, sender, result.RequestID, resourceAccessCardActionApproveOnce)
	state := resourceAccessCardState(t, oauthCard)
	callbackURL := manager.oauth.CallbackURL + "?code=auth-code&state=" + url.QueryEscape(state)
	response, err := manager.HandleCardAction(context.Background(), resourceAccessCardSubmitEvent(result.RequestID, "ou_other", "oc_chat", "om_card", callbackURL))
	if err != nil || response == nil || response.Toast == nil || !strings.Contains(response.Toast.Content, "只有") {
		t.Fatalf("wrong-user handoff response = %#v err=%v", response, err)
	}
	wrongStateURL := manager.oauth.CallbackURL + "?code=auth-code&state=wrong-state"
	response, err = manager.HandleCardAction(context.Background(), resourceAccessCardSubmitEvent(result.RequestID, "ou_requester", "oc_chat", "om_card", wrongStateURL))
	if err != nil || response == nil || response.Toast == nil || !strings.Contains(response.Toast.Content, "state") {
		t.Fatalf("wrong-state handoff response = %#v err=%v", response, err)
	}
	pending, err := st.GetFeishuResourceAccessRequest(result.RequestID, "feishu:cli_test")
	if err != nil || pending.State != store.FeishuResourceAccessStatePending || pending.OAuthStateHash == "" || pending.PKCEVerifier != "" {
		t.Fatalf("rejected handoffs changed pending request = %#v err=%v", pending, err)
	}
}

func TestResourceAccessManagerRejectsListenerWithoutCallbackURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer server.Close()
	st := openFeishuApprovalTestStore(t)
	sender := &fakeApprovalSender{}
	cards, err := newCardService(sender)
	if err != nil {
		t.Fatalf("newCardService returned error: %v", err)
	}
	client := lark.NewClient("cli_xxx", "secret", lark.WithOpenBaseUrl(server.URL), lark.WithOAuthBaseUrl(server.URL), lark.WithHttpClient(server.Client()))
	_, err = newResourceAccessManager(context.Background(), st, client, store.Account{
		ID: "feishu:cli_test", Name: "fsbot", Platform: store.PlatformFeishu,
	}, "ou_bot", cards, resourceAccessOAuthConfig{
		ClientID:              "cli_xxx",
		BaseURL:               server.URL,
		CallbackListenAddress: "127.0.0.1:18080",
	})
	if err == nil || !strings.Contains(err.Error(), "requires oauth_callback_url") {
		t.Fatalf("listener-only config error = %v", err)
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

func TestResourceAccessOAuthServerSkipsListenerFreeMode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected Feishu API request: %s", r.URL.Path)
	}))
	defer server.Close()
	manager, _, _ := newTestResourceAccessManager(t, server, resourceAccessOAuthConfig{
		ClientID:    "cli_xxx",
		BaseURL:     server.URL,
		CallbackURL: "https://oauth.wulongxin.com/feishu/oauth/callback",
	})
	if !manager.oauthEnabled() || manager.oauthHTTPCallbackEnabled() {
		t.Fatalf("oauth modes enabled=%t http=%t", manager.oauthEnabled(), manager.oauthHTTPCallbackEnabled())
	}
	callbackServer, err := startResourceAccessOAuthServer(context.Background(), manager)
	if err != nil {
		t.Fatalf("startResourceAccessOAuthServer returned error: %v", err)
	}
	if callbackServer != nil {
		t.Fatalf("listener-free mode started callback server: %#v", callbackServer)
	}
}

func newTestResourceAccessManager(t *testing.T, server *httptest.Server, oauth resourceAccessOAuthConfig) (*resourceAccessManager, *store.Store, *fakeApprovalSender) {
	t.Helper()
	if oauth.CallbackURL != "" && oauth.CredentialSecret == "" {
		oauth.CredentialSecret = "secret"
	}
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
	}, "ou_bot", cards, oauth)
	if err != nil {
		t.Fatalf("newResourceAccessManager returned error: %v", err)
	}
	now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	manager.now = func() time.Time { return now }
	return manager, st, sender
}

func waitForResourceAccessCompletion(t *testing.T, st *store.Store, sender *fakeApprovalSender, requestID string) store.FeishuResourceAccessRequest {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var last store.FeishuResourceAccessRequest
	var lastErr error
	for time.Now().Before(deadline) {
		last, lastErr = st.GetFeishuResourceAccessRequest(requestID, "feishu:cli_test")
		_, updates, _ := sender.snapshot()
		if lastErr == nil && last.State == store.FeishuResourceAccessStateSucceeded && len(updates) > 0 && strings.Contains(updates[len(updates)-1].text, "权限已授予") {
			return last
		}
		if lastErr == nil && (last.State == store.FeishuResourceAccessStateFailed || last.State == store.FeishuResourceAccessStateExpired || last.State == store.FeishuResourceAccessStateDenied) {
			t.Fatalf("resource access reached terminal state %s while waiting for success: %#v", last.State, last)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for resource access completion: request=%#v err=%v", last, lastErr)
	return store.FeishuResourceAccessRequest{}
}

func approveResourceAccessAndWaitForOAuthCard(
	t *testing.T,
	manager *resourceAccessManager,
	st *store.Store,
	sender *fakeApprovalSender,
	requestID, action string,
) string {
	t.Helper()
	event := resourceAccessCardEvent(requestID, "ou_requester", "oc_chat", "om_card")
	event.Event.Action.Value["action"] = action
	response, err := manager.HandleCardAction(context.Background(), event)
	if err != nil || response == nil || response.Toast == nil || response.Toast.Type != "success" {
		t.Fatalf("resource authorization approval response = %#v err=%v", response, err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		request, loadErr := st.GetFeishuResourceAccessRequest(requestID, "feishu:cli_test")
		_, updates, _ := sender.snapshot()
		if loadErr == nil && request.OAuthStateHash != "" && len(updates) > 0 {
			for i := len(updates) - 1; i >= 0; i-- {
				if strings.Contains(updates[i].text, "前往飞书官方授权页面") {
					return updates[i].text
				}
			}
		}
		if loadErr == nil && request.State != store.FeishuResourceAccessStatePending {
			t.Fatalf("resource authorization reached state %s before OAuth handoff: %#v", request.State, request)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for OAuth handoff card for request %s", requestID)
	return ""
}

func resourceAccessCardURL(t *testing.T, raw string) string {
	t.Helper()
	var card map[string]any
	if err := json.Unmarshal([]byte(raw), &card); err != nil {
		t.Fatalf("unmarshal resource access card: %v", err)
	}
	body, _ := card["body"].(map[string]any)
	elements, _ := body["elements"].([]any)
	if len(elements) < 3 {
		t.Fatalf("resource access card elements = %#v", elements)
	}
	button, _ := elements[2].(map[string]any)
	behaviors, _ := button["behaviors"].([]any)
	if len(behaviors) != 1 {
		t.Fatalf("resource access OAuth button behaviors = %#v", behaviors)
	}
	behavior, _ := behaviors[0].(map[string]any)
	value, _ := behavior["default_url"].(string)
	if behavior["type"] != "open_url" || value == "" {
		t.Fatalf("resource access OAuth button behavior = %#v", behavior)
	}
	return value
}

func resourceAccessCardState(t *testing.T, raw string) string {
	t.Helper()
	authURL, err := url.Parse(resourceAccessCardURL(t, raw))
	if err != nil {
		t.Fatalf("parse resource access authorization URL: %v", err)
	}
	state := authURL.Query().Get("state")
	if state == "" {
		t.Fatalf("resource access authorization URL has no state: %s", authURL.String())
	}
	return state
}

func findCardElementByName(value any, name string) map[string]any {
	switch typed := value.(type) {
	case map[string]any:
		if typed["name"] == name {
			return typed
		}
		for _, child := range typed {
			if found := findCardElementByName(child, name); found != nil {
				return found
			}
		}
	case []any:
		for _, child := range typed {
			if found := findCardElementByName(child, name); found != nil {
				return found
			}
		}
	}
	return nil
}

func cardButtonAction(button map[string]any) string {
	behaviors, _ := button["behaviors"].([]any)
	if len(behaviors) != 1 {
		return ""
	}
	behavior, _ := behaviors[0].(map[string]any)
	value, _ := behavior["value"].(map[string]any)
	action, _ := value["action"].(string)
	return action
}

func assertResourceAccessCardButtonBinding(t *testing.T, button map[string]any, requestID, action string) {
	t.Helper()
	behaviors, _ := button["behaviors"].([]any)
	if len(behaviors) != 1 {
		t.Fatalf("resource access button behaviors = %#v", behaviors)
	}
	behavior, _ := behaviors[0].(map[string]any)
	value, _ := behavior["value"].(map[string]any)
	if behavior["type"] != "callback" || value["kind"] != resourceAccessCardActionKind || value["request_id"] != requestID || value["action"] != action {
		t.Fatalf("resource access button callback = %#v", behavior)
	}
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

func resourceAccessCardSubmitEvent(requestID, openID, chatID, messageID, oauthResult string) *callback.CardActionTriggerEvent {
	event := resourceAccessCardEvent(requestID, openID, chatID, messageID)
	event.Event.Action.Value["action"] = resourceAccessCardActionSubmitOAuth
	event.Event.Action.FormValue = map[string]any{resourceAccessOAuthResultField: oauthResult}
	return event
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
