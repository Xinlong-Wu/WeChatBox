package store

import (
	"testing"
	"time"
)

func TestToolApprovalGrantMatchesExactScopeAndExpiresLazily(t *testing.T) {
	st := openTestStore(t)
	now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	scope := ToolApprovalGrantScope{
		AccountID: "feishu:cli_test",
		ToolName:  "feishu_docs_create",
		ActorType: ToolApprovalActorTypeOpenID,
		ActorID:   "ou_requester",
		ChatID:    "oc_chat",
	}
	created, err := st.UpsertToolApprovalGrant(ToolApprovalGrant{
		ToolApprovalGrantScope: scope,
		SourceApprovalID:       "approval_123",
		CreatedAt:              now,
		ExpiresAt:              now.Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("UpsertToolApprovalGrant returned error: %v", err)
	}
	if !created.ExpiresAt.Equal(now.Add(24*time.Hour)) || created.UpdatedAt != created.CreatedAt {
		t.Fatalf("created grant = %#v", created)
	}

	active, ok, err := st.ActiveToolApprovalGrant(scope, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("ActiveToolApprovalGrant returned error: %v", err)
	}
	if !ok || active.SourceApprovalID != "approval_123" {
		t.Fatalf("active grant = %#v ok=%v, want exact scope match", active, ok)
	}

	mismatches := []ToolApprovalGrantScope{
		{AccountID: "feishu:other", ToolName: scope.ToolName, ActorType: scope.ActorType, ActorID: scope.ActorID, ChatID: scope.ChatID},
		{AccountID: scope.AccountID, ToolName: "other_tool", ActorType: scope.ActorType, ActorID: scope.ActorID, ChatID: scope.ChatID},
		{AccountID: scope.AccountID, ToolName: scope.ToolName, ActorType: scope.ActorType, ActorID: "ou_other", ChatID: scope.ChatID},
		{AccountID: scope.AccountID, ToolName: scope.ToolName, ActorType: scope.ActorType, ActorID: scope.ActorID, ChatID: "oc_other"},
		{AccountID: scope.AccountID, ToolName: scope.ToolName, ActorType: ToolApprovalActorTypeUserID, ActorID: scope.ActorID, ChatID: scope.ChatID},
	}
	for _, mismatch := range mismatches {
		if _, ok, err := st.ActiveToolApprovalGrant(mismatch, now.Add(time.Hour)); err != nil || ok {
			t.Fatalf("mismatched scope %#v returned ok=%v err=%v", mismatch, ok, err)
		}
	}

	if _, ok, err := st.ActiveToolApprovalGrant(scope, now.Add(24*time.Hour)); err != nil || ok {
		t.Fatalf("expired grant returned ok=%v err=%v", ok, err)
	}
	var remaining int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM tool_approval_grants`).Scan(&remaining); err != nil {
		t.Fatalf("count tool approval grants: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("remaining grants = %d, want lazy expiry cleanup", remaining)
	}
}

func TestToolApprovalGrantUpsertRenewsExactScope(t *testing.T) {
	st := openTestStore(t)
	now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	scope := ToolApprovalGrantScope{
		AccountID: "feishu:cli_test",
		ToolName:  "feishu_docs_create",
		ActorType: ToolApprovalActorTypeOpenID,
		ActorID:   "ou_requester",
		ChatID:    "oc_chat",
	}
	if _, err := st.UpsertToolApprovalGrant(ToolApprovalGrant{
		ToolApprovalGrantScope: scope,
		SourceApprovalID:       "approval_first",
		CreatedAt:              now,
		ExpiresAt:              now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("first UpsertToolApprovalGrant returned error: %v", err)
	}
	renewedAt := now.Add(30 * time.Minute)
	if _, err := st.UpsertToolApprovalGrant(ToolApprovalGrant{
		ToolApprovalGrantScope: scope,
		SourceApprovalID:       "approval_second",
		CreatedAt:              renewedAt,
		ExpiresAt:              renewedAt.Add(24 * time.Hour),
	}); err != nil {
		t.Fatalf("second UpsertToolApprovalGrant returned error: %v", err)
	}

	grant, ok, err := st.ActiveToolApprovalGrant(scope, now.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("ActiveToolApprovalGrant returned error: %v", err)
	}
	if !ok || grant.SourceApprovalID != "approval_second" || !grant.CreatedAt.Equal(renewedAt) || !grant.ExpiresAt.Equal(renewedAt.Add(24*time.Hour)) {
		t.Fatalf("renewed grant = %#v ok=%v", grant, ok)
	}
}

func TestDeleteToolApprovalsAlsoRemovesGrantsForMatchingAccount(t *testing.T) {
	st := openTestStore(t)
	now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	first := ToolApprovalGrantScope{
		AccountID: "feishu:first",
		ToolName:  "feishu_docs_create",
		ActorType: ToolApprovalActorTypeOpenID,
		ActorID:   "ou_first",
		ChatID:    "oc_first",
	}
	second := first
	second.AccountID = "feishu:second"
	second.ActorID = "ou_second"
	second.ChatID = "oc_second"
	for i, scope := range []ToolApprovalGrantScope{first, second} {
		if _, err := st.UpsertToolApprovalGrant(ToolApprovalGrant{
			ToolApprovalGrantScope: scope,
			SourceApprovalID:       []string{"approval_first", "approval_second"}[i],
			CreatedAt:              now,
			ExpiresAt:              now.Add(24 * time.Hour),
		}); err != nil {
			t.Fatalf("UpsertToolApprovalGrant returned error: %v", err)
		}
	}

	if err := st.DeleteToolApprovals(first.AccountID); err != nil {
		t.Fatalf("DeleteToolApprovals returned error: %v", err)
	}
	if _, ok, err := st.ActiveToolApprovalGrant(first, now); err != nil || ok {
		t.Fatalf("deleted account grant returned ok=%v err=%v", ok, err)
	}
	if _, ok, err := st.ActiveToolApprovalGrant(second, now); err != nil || !ok {
		t.Fatalf("other account grant returned ok=%v err=%v", ok, err)
	}
}
