package definition

import (
	"errors"
	"testing"
	"time"

	"lingobridge/internal/config"
	"lingobridge/internal/core"
	"lingobridge/internal/platform"
	"lingobridge/internal/platform/feishu"
	"lingobridge/internal/store"
)

func TestCreateOrUpdateAccountPreservesOAuthConfig(t *testing.T) {
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

	cfg := config.DefaultConfig()
	platformCtx, err := core.NewPlatformContext(store.PlatformFeishu, &cfg, st, nil)
	if err != nil {
		t.Fatalf("NewPlatformContext returned error: %v", err)
	}
	if err := feishu.UpsertAccountConfig(platformCtx, "fsbot", feishu.AccountConfig{
		AppID:              "cli_old",
		AppSecret:          "old-secret",
		BaseURL:            "https://open.feishu.cn",
		OAuthBaseURL:       "https://accounts.example.com",
		OAuthRedirectURI:   "https://bridge.example.com/feishu/oauth/callback",
		OAuthListenAddress: "127.0.0.1:8080",
	}); err != nil {
		t.Fatalf("seed account config: %v", err)
	}

	def := Definition()
	if err := def.CreateOrUpdateAccount(platform.AccountNewContext{Platform: platformCtx}, platform.AccountNewOptions{
		Name: "fsbot",
		Values: feishu.AccountNewOptions{
			AppID:     "cli_new",
			AppSecret: "new-secret",
			BaseURL:   "https://open.example.com",
		},
	}); err != nil {
		t.Fatalf("CreateOrUpdateAccount returned error: %v", err)
	}
	account, ok, err := feishu.ResolveAccountConfig(platformCtx, "fsbot")
	if err != nil || !ok {
		t.Fatalf("ResolveAccountConfig account=%#v ok=%t err=%v", account, ok, err)
	}
	if account.AppID != "cli_new" || account.AppSecret != "new-secret" || account.BaseURL != "https://open.example.com" {
		t.Fatalf("updated account credentials = %#v", account)
	}
	if account.OAuthBaseURL != "https://accounts.example.com" ||
		account.OAuthRedirectURI != "https://bridge.example.com/feishu/oauth/callback" ||
		account.OAuthListenAddress != "127.0.0.1:8080" {
		t.Fatalf("updated account lost OAuth config = %#v", account)
	}
}

func TestDeleteAccountClearsPendingToolApprovalsAndDocsBindings(t *testing.T) {
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

	cfg := config.DefaultConfig()
	platformCtx, err := core.NewPlatformContext(store.PlatformFeishu, &cfg, st, nil)
	if err != nil {
		t.Fatalf("NewPlatformContext returned error: %v", err)
	}
	if err := feishu.UpsertAccountConfig(platformCtx, "fsbot", feishu.AccountConfig{AppID: "cli_xxx", AppSecret: "secret"}); err != nil {
		t.Fatalf("UpsertAccountConfig returned error: %v", err)
	}
	account := store.Account{ID: "feishu:cli_xxx", Name: "fsbot", Platform: store.PlatformFeishu}
	now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	approval, err := st.CreateToolApproval(store.ToolApproval{
		AccountID:   account.ID,
		ToolName:    "feishu_docs_create",
		ActorOpenID: "ou_requester",
		ChatID:      "oc_chat",
		Payload:     `{}`,
		CreatedAt:   now,
		ExpiresAt:   now.Add(10 * time.Minute),
	})
	if err != nil {
		t.Fatalf("CreateToolApproval returned error: %v", err)
	}
	folderRequest, err := st.CreateWorkflowRequest(store.WorkflowRequest{
		AccountID: account.ID,
		Kind:      store.WorkflowRequestKindFeishuFolderCreate,
		State:     store.WorkflowRequestStateSucceeded,
		CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("CreateWorkflowRequest returned error: %v", err)
	}
	if _, err := st.SaveFeishuChatFolder(store.FeishuChatFolder{
		AccountID:       account.ID,
		ChatID:          "oc_chat",
		FolderToken:     "fld_token",
		Name:            "Docs",
		ShareMemberType: "openchat",
		ShareMemberID:   "oc_chat",
		ShareState:      store.FeishuFolderShareStateSucceeded,
		CreateRequestID: folderRequest.ID,
		CreatedAt:       now,
	}); err != nil {
		t.Fatalf("SaveFeishuChatFolder returned error: %v", err)
	}

	def := Definition()
	if err := def.DeleteAccount(platform.AccountDeleteContext{Platform: platformCtx, Account: account}); err != nil {
		t.Fatalf("DeleteAccount returned error: %v", err)
	}
	if _, err := st.GetToolApproval(approval.ID, account.ID); !errors.Is(err, store.ErrToolApprovalNotFound) {
		t.Fatalf("GetToolApproval error = %v, want ErrToolApprovalNotFound", err)
	}
	if folders, err := st.ListFeishuChatFolders(account.ID, "oc_chat"); err != nil || len(folders) != 0 {
		t.Fatalf("deleted account folders = %#v err=%v", folders, err)
	}
	loaded, err := feishu.LoadConfig(platformCtx)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}
	if _, ok := loaded.Accounts[account.Name]; ok {
		t.Fatalf("deleted account remains in config: %#v", loaded.Accounts)
	}
}
