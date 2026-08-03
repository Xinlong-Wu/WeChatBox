package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestToolApprovalApproveAndCompleteIsSingleUse(t *testing.T) {
	st := openTestStore(t)
	now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	created, err := st.CreateToolApproval(ToolApproval{
		AccountID:       "feishu:cli_test",
		ToolName:        "feishu_docs_create",
		ActorOpenID:     "ou_requester",
		ActorUserID:     "u_requester",
		ChatID:          "oc_chat",
		SourceMessageID: "om_source",
		Payload:         `{"title":"Quarterly plan"}`,
		CreatedAt:       now,
		ExpiresAt:       now.Add(10 * time.Minute),
	})
	if err != nil {
		t.Fatalf("CreateToolApproval returned error: %v", err)
	}
	if !strings.HasPrefix(created.ID, "req_") || created.State != ToolApprovalStatePending {
		t.Fatalf("created approval = %#v, want generated pending approval", created)
	}
	workflow, err := st.GetWorkflowRequest(created.ID, created.AccountID)
	if err != nil {
		t.Fatalf("GetWorkflowRequest returned error: %v", err)
	}
	if workflow.Kind != WorkflowRequestKindToolApproval || workflow.State != WorkflowRequestStatePending {
		t.Fatalf("created workflow = %#v", workflow)
	}
	if err := st.SetToolApprovalCardMessageID(created.ID, created.AccountID, "om_card", now.Add(time.Second)); err != nil {
		t.Fatalf("SetToolApprovalCardMessageID returned error: %v", err)
	}

	approved, err := st.DecideToolApproval(
		created.ID,
		created.AccountID,
		ToolApprovalDecisionApprove,
		ToolApprovalMatch{
			ActorOpenID:   "ou_requester",
			ActorUserID:   "u_requester",
			ChatID:        "oc_chat",
			CardMessageID: "om_card",
		},
		now.Add(2*time.Second),
	)
	if err != nil {
		t.Fatalf("DecideToolApproval approve returned error: %v", err)
	}
	if approved.State != ToolApprovalStateExecuting || approved.Payload == "" {
		t.Fatalf("approved = %#v, want executing with payload", approved)
	}

	if _, err := st.DecideToolApproval(
		created.ID,
		created.AccountID,
		ToolApprovalDecisionApprove,
		ToolApprovalMatch{ActorOpenID: "ou_requester", ChatID: "oc_chat", CardMessageID: "om_card"},
		now.Add(3*time.Second),
	); !errors.Is(err, ErrToolApprovalResolved) {
		t.Fatalf("second decision error = %v, want ErrToolApprovalResolved", err)
	}

	if err := st.CompleteToolApproval(created.ID, created.AccountID, ToolApprovalStateSucceeded, now.Add(4*time.Second)); err != nil {
		t.Fatalf("CompleteToolApproval returned error: %v", err)
	}
	final, err := st.GetToolApproval(created.ID, created.AccountID)
	if err != nil {
		t.Fatalf("GetToolApproval returned error: %v", err)
	}
	if final.State != ToolApprovalStateSucceeded || final.Payload != "" {
		t.Fatalf("final approval = %#v, want succeeded with cleared payload", final)
	}
	workflow, err = st.GetWorkflowRequest(created.ID, created.AccountID)
	if err != nil || workflow.State != WorkflowRequestStateSucceeded {
		t.Fatalf("completed workflow = %#v err=%v", workflow, err)
	}
	if err := st.CompleteToolApproval(created.ID, created.AccountID, ToolApprovalStateSucceeded, now.Add(5*time.Second)); !errors.Is(err, ErrToolApprovalResolved) {
		t.Fatalf("second completion error = %v, want ErrToolApprovalResolved", err)
	}
}

func TestToolApprovalRejectsWrongActorAndCardContext(t *testing.T) {
	st := openTestStore(t)
	now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	approval := createBoundToolApproval(t, st, now)

	_, err := st.DecideToolApproval(
		approval.ID,
		approval.AccountID,
		ToolApprovalDecisionApprove,
		ToolApprovalMatch{ActorOpenID: "ou_other", ChatID: approval.ChatID, CardMessageID: "om_card"},
		now.Add(time.Second),
	)
	if !errors.Is(err, ErrToolApprovalForbidden) {
		t.Fatalf("wrong actor error = %v, want ErrToolApprovalForbidden", err)
	}

	_, err = st.DecideToolApproval(
		approval.ID,
		approval.AccountID,
		ToolApprovalDecisionApprove,
		ToolApprovalMatch{ActorOpenID: approval.ActorOpenID, ChatID: approval.ChatID, CardMessageID: "om_other"},
		now.Add(2*time.Second),
	)
	if !errors.Is(err, ErrToolApprovalContextMismatch) {
		t.Fatalf("wrong card error = %v, want ErrToolApprovalContextMismatch", err)
	}

	pending, err := st.GetToolApproval(approval.ID, approval.AccountID)
	if err != nil {
		t.Fatalf("GetToolApproval returned error: %v", err)
	}
	if pending.State != ToolApprovalStatePending || pending.Payload == "" {
		t.Fatalf("pending approval = %#v, want unchanged pending record", pending)
	}
}

func TestToolApprovalDenyAndExpiryClearPayload(t *testing.T) {
	st := openTestStore(t)
	now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	denied := createBoundToolApproval(t, st, now)

	denied, err := st.DecideToolApproval(
		denied.ID,
		denied.AccountID,
		ToolApprovalDecisionDeny,
		ToolApprovalMatch{ActorOpenID: denied.ActorOpenID, ChatID: denied.ChatID, CardMessageID: "om_card"},
		now.Add(time.Second),
	)
	if err != nil {
		t.Fatalf("DecideToolApproval deny returned error: %v", err)
	}
	if denied.State != ToolApprovalStateDenied || denied.Payload != "" {
		t.Fatalf("denied approval = %#v, want denied with cleared payload", denied)
	}

	expiring, err := st.CreateToolApproval(ToolApproval{
		AccountID:   "feishu:cli_test",
		ToolName:    "feishu_docs_create",
		ActorOpenID: "ou_requester",
		ChatID:      "oc_chat",
		Payload:     `{"title":"Expired"}`,
		CreatedAt:   now,
		ExpiresAt:   now.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("CreateToolApproval expiring returned error: %v", err)
	}
	count, err := st.ExpireToolApprovals(expiring.AccountID, now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("ExpireToolApprovals returned error: %v", err)
	}
	if count != 1 {
		t.Fatalf("expired count = %d, want 1", count)
	}
	expired, err := st.GetToolApproval(expiring.ID, expiring.AccountID)
	if err != nil {
		t.Fatalf("GetToolApproval expired returned error: %v", err)
	}
	if expired.State != ToolApprovalStateExpired || expired.Payload != "" {
		t.Fatalf("expired approval = %#v, want expired with cleared payload", expired)
	}
	workflow, err := st.GetWorkflowRequest(expiring.ID, expiring.AccountID)
	if err != nil || workflow.State != WorkflowRequestStateExpired {
		t.Fatalf("expired workflow = %#v err=%v", workflow, err)
	}
}

func TestDeleteToolApprovalsRemovesOnlyMatchingAccount(t *testing.T) {
	st := openTestStore(t)
	now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	first, err := st.CreateToolApproval(ToolApproval{
		AccountID:   "feishu:first",
		ToolName:    "tool",
		ActorOpenID: "ou_first",
		ChatID:      "oc_first",
		Payload:     `{}`,
		CreatedAt:   now,
		ExpiresAt:   now.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("CreateToolApproval first returned error: %v", err)
	}
	second, err := st.CreateToolApproval(ToolApproval{
		AccountID:   "feishu:second",
		ToolName:    "tool",
		ActorOpenID: "ou_second",
		ChatID:      "oc_second",
		Payload:     `{}`,
		CreatedAt:   now,
		ExpiresAt:   now.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("CreateToolApproval second returned error: %v", err)
	}
	for _, approval := range []ToolApproval{first, second} {
		continuation := attachWorkflowContinuationForTest(t, st, approval.ID, approval.AccountID, now, 0)
		if _, _, err := st.CommitWorkflowContinuation(continuation.RequestID, continuation.AccountID, 1, now.Add(time.Second)); err != nil {
			t.Fatalf("CommitWorkflowContinuation returned error: %v", err)
		}
		if _, _, _, err := st.StoreWorkflowResult(WorkflowResult{
			RequestID: continuation.RequestID,
			AccountID: continuation.AccountID,
			State:     WorkflowResultStateDenied,
			CreatedAt: now.Add(2 * time.Second),
		}); err != nil {
			t.Fatalf("StoreWorkflowResult returned error: %v", err)
		}
	}

	if err := st.DeleteToolApprovals(first.AccountID); err != nil {
		t.Fatalf("DeleteToolApprovals returned error: %v", err)
	}
	if _, err := st.GetToolApproval(first.ID, first.AccountID); !errors.Is(err, ErrToolApprovalNotFound) {
		t.Fatalf("deleted approval error = %v, want ErrToolApprovalNotFound", err)
	}
	if _, err := st.GetWorkflowRequest(first.ID, first.AccountID); !errors.Is(err, ErrWorkflowRequestNotFound) {
		t.Fatalf("deleted workflow error = %v, want ErrWorkflowRequestNotFound", err)
	}
	if _, err := st.GetWorkflowContinuation(first.ID, first.AccountID); !errors.Is(err, ErrWorkflowContinuationNotFound) {
		t.Fatalf("deleted workflow continuation error = %v, want ErrWorkflowContinuationNotFound", err)
	}
	if _, err := st.GetWorkflowResult(first.ID, first.AccountID); !errors.Is(err, ErrWorkflowResultNotFound) {
		t.Fatalf("deleted workflow result error = %v, want ErrWorkflowResultNotFound", err)
	}
	if _, err := st.GetToolApproval(second.ID, second.AccountID); err != nil {
		t.Fatalf("other account approval was deleted: %v", err)
	}
	if _, err := st.GetWorkflowContinuation(second.ID, second.AccountID); err != nil {
		t.Fatalf("other account workflow continuation was deleted: %v", err)
	}
	if _, err := st.GetWorkflowResult(second.ID, second.AccountID); err != nil {
		t.Fatalf("other account workflow result was deleted: %v", err)
	}
}

func TestFailExecutingToolApprovalsClearsInterruptedPayload(t *testing.T) {
	st := openTestStore(t)
	now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	approval := createBoundToolApproval(t, st, now)
	if _, err := st.DecideToolApproval(
		approval.ID,
		approval.AccountID,
		ToolApprovalDecisionApprove,
		ToolApprovalMatch{ActorOpenID: approval.ActorOpenID, ChatID: approval.ChatID, CardMessageID: "om_card"},
		now.Add(time.Second),
	); err != nil {
		t.Fatalf("DecideToolApproval returned error: %v", err)
	}

	count, err := st.FailExecutingToolApprovals(approval.AccountID, now.Add(2*time.Second))
	if err != nil {
		t.Fatalf("FailExecutingToolApprovals returned error: %v", err)
	}
	if count != 1 {
		t.Fatalf("interrupted count = %d, want 1", count)
	}
	failed, err := st.GetToolApproval(approval.ID, approval.AccountID)
	if err != nil {
		t.Fatalf("GetToolApproval returned error: %v", err)
	}
	if failed.State != ToolApprovalStateFailed || failed.Payload != "" {
		t.Fatalf("failed approval = %#v, want failed with cleared payload", failed)
	}
}

func createBoundToolApproval(t *testing.T, st *Store, now time.Time) ToolApproval {
	t.Helper()
	approval, err := st.CreateToolApproval(ToolApproval{
		AccountID:   "feishu:cli_test",
		ToolName:    "feishu_docs_create",
		ActorOpenID: "ou_requester",
		ChatID:      "oc_chat",
		Payload:     `{"title":"Quarterly plan"}`,
		CreatedAt:   now,
		ExpiresAt:   now.Add(10 * time.Minute),
	})
	if err != nil {
		t.Fatalf("CreateToolApproval returned error: %v", err)
	}
	if err := st.SetToolApprovalCardMessageID(approval.ID, approval.AccountID, "om_card", now); err != nil {
		t.Fatalf("SetToolApprovalCardMessageID returned error: %v", err)
	}
	approval.CardMessageID = "om_card"
	return approval
}
