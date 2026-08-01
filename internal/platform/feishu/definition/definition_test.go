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

func TestDeleteAccountClearsPendingToolApprovals(t *testing.T) {
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

	def := Definition()
	if err := def.DeleteAccount(platform.AccountDeleteContext{Platform: platformCtx, Account: account}); err != nil {
		t.Fatalf("DeleteAccount returned error: %v", err)
	}
	if _, err := st.GetToolApproval(approval.ID, account.ID); !errors.Is(err, store.ErrToolApprovalNotFound) {
		t.Fatalf("GetToolApproval error = %v, want ErrToolApprovalNotFound", err)
	}
	loaded, err := feishu.LoadConfig(platformCtx)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}
	if _, ok := loaded.Accounts[account.Name]; ok {
		t.Fatalf("deleted account remains in config: %#v", loaded.Accounts)
	}
}
