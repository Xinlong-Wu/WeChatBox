package store

import (
	"testing"
	"time"
)

func TestToolApprovalGrantMatchesExactPermanentScope(t *testing.T) {
	st := openTestStore(t)
	now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	scope := exactToolApprovalGrantScope("feishu:cli_test", "ou_requester", "oc_chat", "feishu_docs_create", "create", "folder", "fld_target")
	created, err := st.UpsertToolApprovalGrant(ToolApprovalGrant{
		ToolApprovalGrantScope: scope,
		SourceRequestID:        "req_123",
		CreatedAt:              now,
	})
	if err != nil {
		t.Fatalf("UpsertToolApprovalGrant returned error: %v", err)
	}
	if created.UpdatedAt != created.CreatedAt {
		t.Fatalf("created grant = %#v", created)
	}

	active, ok, err := st.FindToolApprovalGrant(scope)
	if err != nil {
		t.Fatalf("FindToolApprovalGrant returned error: %v", err)
	}
	if !ok || active.SourceRequestID != "req_123" {
		t.Fatalf("active grant = %#v ok=%v, want exact scope match", active, ok)
	}

	mismatches := []ToolApprovalGrantScope{
		exactToolApprovalGrantScope("feishu:other", scope.ActorID, scope.ChatID, scope.ToolName, scope.ActionKey, scope.ResourceType, scope.ResourceToken),
		exactToolApprovalGrantScope(scope.AccountID, scope.ActorID, scope.ChatID, "other_tool", scope.ActionKey, scope.ResourceType, scope.ResourceToken),
		exactToolApprovalGrantScope(scope.AccountID, scope.ActorID, scope.ChatID, scope.ToolName, "other_action", scope.ResourceType, scope.ResourceToken),
		exactToolApprovalGrantScope(scope.AccountID, scope.ActorID, scope.ChatID, scope.ToolName, scope.ActionKey, "docx", scope.ResourceToken),
		exactToolApprovalGrantScope(scope.AccountID, scope.ActorID, scope.ChatID, scope.ToolName, scope.ActionKey, scope.ResourceType, "fld_other"),
		exactToolApprovalGrantScope(scope.AccountID, "ou_other", scope.ChatID, scope.ToolName, scope.ActionKey, scope.ResourceType, scope.ResourceToken),
		exactToolApprovalGrantScope(scope.AccountID, scope.ActorID, "oc_other", scope.ToolName, scope.ActionKey, scope.ResourceType, scope.ResourceToken),
	}
	actorTypeMismatch := scope
	actorTypeMismatch.ActorType = ToolApprovalActorTypeUserID
	mismatches = append(mismatches, actorTypeMismatch)
	for _, mismatch := range mismatches {
		if _, ok, err := st.FindToolApprovalGrant(mismatch); err != nil || ok {
			t.Fatalf("mismatched scope %#v returned ok=%v err=%v", mismatch, ok, err)
		}
	}

	if grant, ok, err := st.FindToolApprovalGrant(scope); err != nil || !ok || !grant.CreatedAt.Equal(now) {
		t.Fatalf("permanent grant later lookup = %#v ok=%v err=%v", grant, ok, err)
	}
}

func TestToolApprovalGrantUpsertRenewsExactScope(t *testing.T) {
	st := openTestStore(t)
	now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	scope := exactToolApprovalGrantScope("feishu:cli_test", "ou_requester", "oc_chat", "feishu_docs_append", "append", "docx", "doxcn_target")
	if _, err := st.UpsertToolApprovalGrant(ToolApprovalGrant{
		ToolApprovalGrantScope: scope,
		SourceRequestID:        "req_first",
		CreatedAt:              now,
	}); err != nil {
		t.Fatalf("first UpsertToolApprovalGrant returned error: %v", err)
	}
	renewedAt := now.Add(30 * time.Minute)
	if _, err := st.UpsertToolApprovalGrant(ToolApprovalGrant{
		ToolApprovalGrantScope: scope,
		SourceRequestID:        "req_second",
		CreatedAt:              renewedAt,
	}); err != nil {
		t.Fatalf("second UpsertToolApprovalGrant returned error: %v", err)
	}

	grant, ok, err := st.FindToolApprovalGrant(scope)
	if err != nil {
		t.Fatalf("FindToolApprovalGrant returned error: %v", err)
	}
	if !ok || grant.SourceRequestID != "req_second" || !grant.CreatedAt.Equal(renewedAt) || !grant.UpdatedAt.Equal(renewedAt) {
		t.Fatalf("renewed grant = %#v ok=%v", grant, ok)
	}
}

func TestDeleteToolApprovalsAlsoRemovesGrantsForMatchingAccount(t *testing.T) {
	st := openTestStore(t)
	now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	first := exactToolApprovalGrantScope("feishu:first", "ou_first", "oc_first", "feishu_docs_create", "create", "folder", "fld_first")
	second := exactToolApprovalGrantScope("feishu:second", "ou_second", "oc_second", "feishu_docs_create", "create", "folder", "fld_second")
	for i, scope := range []ToolApprovalGrantScope{first, second} {
		if _, err := st.UpsertToolApprovalGrant(ToolApprovalGrant{
			ToolApprovalGrantScope: scope,
			SourceRequestID:        []string{"req_first", "req_second"}[i],
			CreatedAt:              now,
		}); err != nil {
			t.Fatalf("UpsertToolApprovalGrant returned error: %v", err)
		}
	}

	if err := st.DeleteToolApprovals(first.AccountID); err != nil {
		t.Fatalf("DeleteToolApprovals returned error: %v", err)
	}
	if _, ok, err := st.FindToolApprovalGrant(first); err != nil || ok {
		t.Fatalf("deleted account grant returned ok=%v err=%v", ok, err)
	}
	if _, ok, err := st.FindToolApprovalGrant(second); err != nil || !ok {
		t.Fatalf("other account grant returned ok=%v err=%v", ok, err)
	}
}

func exactToolApprovalGrantScope(accountID, actorID, chatID, toolName, actionKey, resourceType, resourceToken string) ToolApprovalGrantScope {
	return ToolApprovalGrantScope{
		AccountID:     accountID,
		ToolName:      toolName,
		ActionKey:     actionKey,
		ResourceType:  resourceType,
		ResourceToken: resourceToken,
		ActorType:     ToolApprovalActorTypeOpenID,
		ActorID:       actorID,
		ChatID:        chatID,
	}
}
