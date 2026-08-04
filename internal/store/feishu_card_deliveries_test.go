package store

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestFeishuCardDeliveryRevisionSupersedesOlderWork(t *testing.T) {
	st := openFeishuDocsTestStore(t)
	now := time.Date(2026, time.August, 4, 11, 0, 0, 0, time.UTC)
	older, err := st.EnqueueFeishuCardDelivery(FeishuCardDelivery{
		AccountID:     "feishu:cli_test",
		RequestID:     "req_card",
		Purpose:       FeishuCardDeliveryPurposeResourceOAuthHandoff,
		Revision:      FeishuCardDeliveryRevisionOAuthHandoff,
		CardMessageID: "om_card",
		CreatedAt:     now,
		ExpiresAt:     now.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("enqueue older delivery: %v", err)
	}
	newer, err := st.EnqueueFeishuCardDelivery(FeishuCardDelivery{
		AccountID:     older.AccountID,
		RequestID:     older.RequestID,
		Purpose:       FeishuCardDeliveryPurposeResourceTerminal,
		Revision:      FeishuCardDeliveryRevisionTerminal,
		CardMessageID: older.CardMessageID,
		CreatedAt:     now.Add(time.Minute),
		ExpiresAt:     now.Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("enqueue newer delivery: %v", err)
	}
	storedOlder, err := st.GetFeishuCardDelivery(older.ID, older.AccountID)
	if err != nil || storedOlder.State != FeishuCardDeliveryStateSuperseded {
		t.Fatalf("older delivery = %#v err=%v, want superseded", storedOlder, err)
	}
	available, err := st.ListAvailableFeishuCardDeliveries(older.AccountID, now.Add(2*time.Minute), 10)
	if err != nil {
		t.Fatalf("ListAvailableFeishuCardDeliveries returned error: %v", err)
	}
	if len(available) != 1 || available[0].ID != newer.ID {
		t.Fatalf("available deliveries = %#v, want only newer revision", available)
	}
}

func TestFeishuCardDeliveryNewRevisionWaitsForClaimedOlderUpdate(t *testing.T) {
	st := openFeishuDocsTestStore(t)
	now := time.Date(2026, time.August, 4, 11, 15, 0, 0, time.UTC)
	older, err := st.EnqueueFeishuCardDelivery(FeishuCardDelivery{
		AccountID:     "feishu:cli_test",
		RequestID:     "req_ordered_card",
		Purpose:       FeishuCardDeliveryPurposeResourceOAuthHandoff,
		Revision:      FeishuCardDeliveryRevisionOAuthHandoff,
		CardMessageID: "om_card",
		CreatedAt:     now,
		ExpiresAt:     now.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("enqueue older delivery: %v", err)
	}
	claimed, err := st.ClaimFeishuCardDelivery(older.ID, older.AccountID, "lease_old", now, time.Minute)
	if err != nil {
		t.Fatalf("claim older delivery: %v", err)
	}
	newer, err := st.EnqueueFeishuCardDelivery(FeishuCardDelivery{
		AccountID:     older.AccountID,
		RequestID:     older.RequestID,
		Purpose:       FeishuCardDeliveryPurposeResourceTerminal,
		Revision:      FeishuCardDeliveryRevisionTerminal,
		CardMessageID: older.CardMessageID,
		CreatedAt:     now.Add(time.Second),
		ExpiresAt:     now.Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("enqueue newer delivery: %v", err)
	}
	storedOlder, err := st.GetFeishuCardDelivery(older.ID, older.AccountID)
	if err != nil || storedOlder.State != FeishuCardDeliveryStateProcessing || storedOlder.LeaseToken != claimed.LeaseToken {
		t.Fatalf("claimed older delivery after newer enqueue = %#v err=%v", storedOlder, err)
	}
	available, err := st.ListAvailableFeishuCardDeliveries(older.AccountID, now.Add(2*time.Second), 10)
	if err != nil {
		t.Fatalf("list while older update is in flight: %v", err)
	}
	if len(available) != 0 {
		t.Fatalf("available deliveries while older update is in flight = %#v", available)
	}
	if _, err := st.ClaimFeishuCardDelivery(
		older.ID,
		older.AccountID,
		"competing_lease",
		now.Add(2*time.Second),
		time.Minute,
	); !errors.Is(err, ErrFeishuCardDeliveryNotReady) {
		t.Fatalf("competing older claim error = %v, want not ready", err)
	}
	storedOlder, err = st.GetFeishuCardDelivery(older.ID, older.AccountID)
	if err != nil || storedOlder.State != FeishuCardDeliveryStateProcessing || storedOlder.LeaseToken != claimed.LeaseToken {
		t.Fatalf("claimed older delivery after competing claim = %#v err=%v", storedOlder, err)
	}
	if err := st.MarkFeishuCardDeliveryDelivered(
		newer.AccountID,
		newer.RequestID,
		newer.Purpose,
		newer.Revision,
		now.Add(2*time.Second),
	); !errors.Is(err, ErrFeishuCardDeliveryNotReady) {
		t.Fatalf("newer fast-path completion error = %v, want not ready", err)
	}
	if err := st.CompleteFeishuCardDelivery(older.ID, older.AccountID, claimed.LeaseToken, now.Add(3*time.Second)); !errors.Is(err, ErrFeishuCardDeliveryResolved) {
		t.Fatalf("older completion after newer enqueue = %v, want resolved", err)
	}
	storedOlder, err = st.GetFeishuCardDelivery(older.ID, older.AccountID)
	if err != nil || storedOlder.State != FeishuCardDeliveryStateSuperseded {
		t.Fatalf("older delivery after completion = %#v err=%v", storedOlder, err)
	}
	available, err = st.ListAvailableFeishuCardDeliveries(older.AccountID, now.Add(4*time.Second), 10)
	if err != nil || len(available) != 1 || available[0].ID != newer.ID {
		t.Fatalf("available deliveries after older completion = %#v err=%v", available, err)
	}
}

func TestFeishuCardDeliveryConcurrentClaimHasSingleOwnerAndExpiredTakeover(t *testing.T) {
	first, second := openSharedFeishuRuntimeLeaseStores(t)
	now := time.Date(2026, time.August, 4, 11, 30, 0, 0, time.UTC)
	delivery, err := first.EnqueueFeishuCardDelivery(FeishuCardDelivery{
		AccountID:     "feishu:cli_test",
		RequestID:     "req_claim",
		Purpose:       FeishuCardDeliveryPurposeToolApprovalTerminal,
		Revision:      FeishuCardDeliveryRevisionTerminal,
		CardMessageID: "om_card",
		CreatedAt:     now,
		ExpiresAt:     now.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("EnqueueFeishuCardDelivery returned error: %v", err)
	}
	start := make(chan struct{})
	type outcome struct {
		lease string
		err   error
	}
	outcomes := make(chan outcome, 2)
	var wg sync.WaitGroup
	for _, candidate := range []struct {
		store *Store
		lease string
	}{{first, "lease_a"}, {second, "lease_b"}} {
		wg.Add(1)
		go func(candidateStore *Store, lease string) {
			defer wg.Done()
			<-start
			_, claimErr := candidateStore.ClaimFeishuCardDelivery(delivery.ID, delivery.AccountID, lease, now, time.Minute)
			outcomes <- outcome{lease: lease, err: claimErr}
		}(candidate.store, candidate.lease)
	}
	close(start)
	wg.Wait()
	close(outcomes)
	winner := ""
	for result := range outcomes {
		if result.err == nil {
			if winner != "" {
				t.Fatalf("multiple claim winners: %s and %s", winner, result.lease)
			}
			winner = result.lease
			continue
		}
		if !errors.Is(result.err, ErrFeishuCardDeliveryNotReady) {
			t.Fatalf("losing claim error = %v", result.err)
		}
	}
	if winner == "" {
		t.Fatal("no delivery claim winner")
	}
	takeoverLease := "lease_takeover"
	takenOver, err := second.ClaimFeishuCardDelivery(delivery.ID, delivery.AccountID, takeoverLease, now.Add(time.Minute), time.Minute)
	if err != nil || takenOver.LeaseToken != takeoverLease || takenOver.Attempts != 2 {
		t.Fatalf("taken-over delivery = %#v err=%v", takenOver, err)
	}
	if err := first.CompleteFeishuCardDelivery(delivery.ID, delivery.AccountID, winner, now.Add(time.Minute)); !errors.Is(err, ErrFeishuCardDeliveryLeaseLost) {
		t.Fatalf("stale completion error = %v, want ErrFeishuCardDeliveryLeaseLost", err)
	}
	if err := second.CompleteFeishuCardDelivery(delivery.ID, delivery.AccountID, takeoverLease, now.Add(time.Minute)); err != nil {
		t.Fatalf("takeover completion returned error: %v", err)
	}
}

func TestFeishuCardDeliveryFastPathCompletionIsIdempotent(t *testing.T) {
	st := openFeishuDocsTestStore(t)
	now := time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC)
	delivery, err := st.EnqueueFeishuCardDelivery(FeishuCardDelivery{
		AccountID:     "feishu:cli_test",
		RequestID:     "req_fast",
		Purpose:       FeishuCardDeliveryPurposeResourceTerminal,
		Revision:      FeishuCardDeliveryRevisionTerminal,
		CardMessageID: "om_card",
		CreatedAt:     now,
		ExpiresAt:     now.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("EnqueueFeishuCardDelivery returned error: %v", err)
	}
	for range 2 {
		if err := st.MarkFeishuCardDeliveryDelivered(delivery.AccountID, delivery.RequestID, delivery.Purpose, delivery.Revision, now.Add(time.Second)); err != nil {
			t.Fatalf("MarkFeishuCardDeliveryDelivered returned error: %v", err)
		}
	}
	stored, err := st.GetFeishuCardDelivery(delivery.ID, delivery.AccountID)
	if err != nil || stored.State != FeishuCardDeliveryStateDelivered || stored.DeliveredAt.IsZero() {
		t.Fatalf("delivered row = %#v err=%v", stored, err)
	}
}

func TestFeishuCardDeliveryExpiryDoesNotRevokeActiveClaim(t *testing.T) {
	st := openFeishuDocsTestStore(t)
	now := time.Date(2026, time.August, 4, 12, 10, 0, 0, time.UTC)
	delivery, err := st.EnqueueFeishuCardDelivery(FeishuCardDelivery{
		AccountID:     "feishu:cli_test",
		RequestID:     "req_expiring_claim",
		Purpose:       FeishuCardDeliveryPurposeResourceOAuthHandoff,
		Revision:      FeishuCardDeliveryRevisionOAuthHandoff,
		CardMessageID: "om_card",
		CreatedAt:     now,
		ExpiresAt:     now.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("enqueue delivery: %v", err)
	}
	claimed, err := st.ClaimFeishuCardDelivery(delivery.ID, delivery.AccountID, "active_lease", now, 2*time.Minute)
	if err != nil {
		t.Fatalf("claim delivery: %v", err)
	}
	if _, err := st.ClaimFeishuCardDelivery(
		delivery.ID,
		delivery.AccountID,
		"competing_lease",
		now.Add(90*time.Second),
		time.Minute,
	); !errors.Is(err, ErrFeishuCardDeliveryNotReady) {
		t.Fatalf("claim after delivery expiry error = %v, want not ready", err)
	}
	stored, err := st.GetFeishuCardDelivery(delivery.ID, delivery.AccountID)
	if err != nil || stored.State != FeishuCardDeliveryStateProcessing || stored.LeaseToken != claimed.LeaseToken {
		t.Fatalf("active claim after delivery expiry = %#v err=%v", stored, err)
	}
}

func TestFeishuCardDeliveryExpiredWorkTransitionsToDead(t *testing.T) {
	st := openFeishuDocsTestStore(t)
	now := time.Date(2026, time.August, 4, 12, 15, 0, 0, time.UTC)
	delivery, err := st.EnqueueFeishuCardDelivery(FeishuCardDelivery{
		AccountID:     "feishu:cli_test",
		RequestID:     "req_expired",
		Purpose:       FeishuCardDeliveryPurposeResourceOAuthHandoff,
		Revision:      FeishuCardDeliveryRevisionOAuthHandoff,
		CardMessageID: "om_card",
		CreatedAt:     now,
		ExpiresAt:     now.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("EnqueueFeishuCardDelivery returned error: %v", err)
	}
	available, err := st.ListAvailableFeishuCardDeliveries(delivery.AccountID, now.Add(2*time.Minute), 10)
	if err != nil || len(available) != 1 || available[0].ID != delivery.ID {
		t.Fatalf("expired available deliveries = %#v err=%v", available, err)
	}
	if _, err := st.ClaimFeishuCardDelivery(delivery.ID, delivery.AccountID, "lease_expired", now.Add(2*time.Minute), time.Minute); !errors.Is(err, ErrFeishuCardDeliveryResolved) {
		t.Fatalf("expired claim error = %v, want ErrFeishuCardDeliveryResolved", err)
	}
	stored, err := st.GetFeishuCardDelivery(delivery.ID, delivery.AccountID)
	if err != nil || stored.State != FeishuCardDeliveryStateDead {
		t.Fatalf("expired delivery = %#v err=%v, want dead", stored, err)
	}
}

func TestToolApprovalTerminalStateAndCardDeliveryAreAtomic(t *testing.T) {
	st := openFeishuDocsTestStore(t)
	now := time.Date(2026, time.August, 4, 12, 30, 0, 0, time.UTC)
	approval, err := st.CreateToolApproval(ToolApproval{
		AccountID:       "feishu:cli_test",
		ToolName:        "feishu_docs_create",
		ActionKey:       "create",
		ResourceType:    "folder",
		ResourceToken:   "fld_parent",
		ActorOpenID:     "ou_requester",
		ChatID:          "oc_chat",
		SourceMessageID: "om_source",
		Payload:         `{"title":"Plan"}`,
		CreatedAt:       now,
		ExpiresAt:       now.Add(10 * time.Minute),
	})
	if err != nil {
		t.Fatalf("CreateToolApproval returned error: %v", err)
	}
	if err := st.SetToolApprovalCardMessageID(approval.ID, approval.AccountID, "om_card", now); err != nil {
		t.Fatalf("SetToolApprovalCardMessageID returned error: %v", err)
	}
	approval, err = st.DecideToolApproval(approval.ID, approval.AccountID, ToolApprovalDecisionApprove, ToolApprovalMatch{
		ActorOpenID: "ou_requester", CardMessageID: "om_card", ChatID: "oc_chat",
	}, now.Add(time.Second))
	if err != nil {
		t.Fatalf("DecideToolApproval returned error: %v", err)
	}
	if _, err := st.db.Exec(`CREATE TRIGGER reject_card_delivery BEFORE INSERT ON feishu_card_deliveries BEGIN SELECT RAISE(ABORT, 'injected card delivery failure'); END`); err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}
	if err := st.CompleteToolApproval(approval.ID, approval.AccountID, ToolApprovalStateSucceeded, now.Add(2*time.Second)); err == nil {
		t.Fatal("CompleteToolApproval returned nil error with rejected outbox insert")
	}
	stored, err := st.GetToolApproval(approval.ID, approval.AccountID)
	if err != nil || stored.State != ToolApprovalStateExecuting {
		t.Fatalf("approval after rolled-back outbox failure = %#v err=%v, want executing", stored, err)
	}
	if _, err := st.db.Exec(`DROP TRIGGER reject_card_delivery`); err != nil {
		t.Fatalf("drop failure trigger: %v", err)
	}
	if err := st.CompleteToolApproval(approval.ID, approval.AccountID, ToolApprovalStateSucceeded, now.Add(3*time.Second)); err != nil {
		t.Fatalf("CompleteToolApproval returned error: %v", err)
	}
	delivery, err := st.GetFeishuCardDeliveryByKey(
		approval.AccountID,
		approval.ID,
		FeishuCardDeliveryPurposeToolApprovalTerminal,
		FeishuCardDeliveryRevisionTerminal,
	)
	if err != nil || delivery.State != FeishuCardDeliveryStatePending || delivery.CardMessageID != "om_card" {
		t.Fatalf("tool terminal delivery = %#v err=%v", delivery, err)
	}
}

func TestResourceOAuthPreparationAndHandoffDeliveryAreDurable(t *testing.T) {
	st := openFeishuDocsTestStore(t)
	now := time.Date(2026, time.August, 4, 13, 0, 0, 0, time.UTC)
	request, err := st.CreateFeishuResourceAccessRequest(FeishuResourceAccessRequest{
		AccountID:           "feishu:cli_test",
		ActorOpenID:         "ou_requester",
		ChatID:              "oc_chat",
		SourceMessageID:     "om_source",
		ResourceType:        "docx",
		ResourceToken:       "doc_token",
		Permission:          FeishuResourcePermissionWrite,
		OnceDurationMinutes: 30,
		CreatedAt:           now,
		ExpiresAt:           now.Add(10 * time.Minute),
	})
	if err != nil {
		t.Fatalf("CreateFeishuResourceAccessRequest returned error: %v", err)
	}
	if err := st.SetFeishuResourceAccessCardMessageID(request.ID, request.AccountID, "om_card", now); err != nil {
		t.Fatalf("SetFeishuResourceAccessCardMessageID returned error: %v", err)
	}
	request, err = st.ApproveFeishuResourceAccessRequest(request.ID, request.AccountID, FeishuResourceGrantModeOnce, FeishuResourceAccessMatch{
		ActorOpenID: "ou_requester", ChatID: "oc_chat", CardMessageID: "om_card",
	}, now.Add(time.Second))
	if err != nil {
		t.Fatalf("ApproveFeishuResourceAccessRequest returned error: %v", err)
	}
	if _, err := st.db.Exec(`CREATE TRIGGER reject_card_delivery BEFORE INSERT ON feishu_card_deliveries BEGIN SELECT RAISE(ABORT, 'injected card delivery failure'); END`); err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}
	if err := st.PrepareFeishuResourceAccessOAuth(
		request.ID, request.AccountID, "state_hash", "encrypted_state", "", "openid", "ou_bot", now.Add(2*time.Second),
	); err == nil {
		t.Fatal("PrepareFeishuResourceAccessOAuth returned nil error with rejected outbox insert")
	}
	rolledBack, err := st.GetFeishuResourceAccessRequest(request.ID, request.AccountID)
	if err != nil || rolledBack.OAuthStateHash != "" || rolledBack.OAuthStateCiphertext != "" {
		t.Fatalf("resource request after rolled-back outbox failure = %#v err=%v", rolledBack, err)
	}
	if _, err := st.db.Exec(`DROP TRIGGER reject_card_delivery`); err != nil {
		t.Fatalf("drop failure trigger: %v", err)
	}
	if err := st.PrepareFeishuResourceAccessOAuth(
		request.ID, request.AccountID, "state_hash", "encrypted_state", "", "openid", "ou_bot", now.Add(3*time.Second),
	); err != nil {
		t.Fatalf("PrepareFeishuResourceAccessOAuth returned error: %v", err)
	}
	delivery, err := st.GetFeishuCardDeliveryByKey(
		request.AccountID,
		request.ID,
		FeishuCardDeliveryPurposeResourceOAuthHandoff,
		FeishuCardDeliveryRevisionOAuthHandoff,
	)
	if err != nil || delivery.State != FeishuCardDeliveryStatePending {
		t.Fatalf("OAuth handoff delivery = %#v err=%v", delivery, err)
	}
	if err := st.MarkFeishuResourceAccessOAuthHandoffDelivered(request.ID, request.AccountID, "state_hash", now.Add(4*time.Second)); err != nil {
		t.Fatalf("MarkFeishuResourceAccessOAuthHandoffDelivered returned error: %v", err)
	}
	delivered, err := st.GetFeishuCardDelivery(delivery.ID, delivery.AccountID)
	if err != nil || delivered.State != FeishuCardDeliveryStateDelivered {
		t.Fatalf("delivered OAuth handoff = %#v err=%v", delivered, err)
	}
	updated, err := st.GetFeishuResourceAccessRequest(request.ID, request.AccountID)
	if err != nil || updated.OAuthStateHash != "state_hash" || updated.OAuthStateCiphertext != "" || updated.OAuthHandoffDeliveredAt.IsZero() {
		t.Fatalf("resource request after handoff delivery = %#v err=%v", updated, err)
	}
}

func TestWorkflowContinuationTerminalStateAndCardDeliveryAreAtomic(t *testing.T) {
	st := openFeishuDocsTestStore(t)
	now := time.Date(2026, time.August, 4, 13, 30, 0, 0, time.UTC)
	approval, err := st.CreateToolApproval(ToolApproval{
		AccountID:       "feishu:cli_test",
		ToolName:        "feishu_docs_create",
		ActionKey:       "create",
		ResourceType:    "folder",
		ResourceToken:   "fld_parent",
		ActorOpenID:     "ou_requester",
		ChatID:          "oc_chat",
		SourceMessageID: "om_source",
		Payload:         `{"title":"Plan"}`,
		CreatedAt:       now,
		ExpiresAt:       now.Add(10 * time.Minute),
	})
	if err != nil {
		t.Fatalf("CreateToolApproval returned error: %v", err)
	}
	if err := st.SetToolApprovalCardMessageID(approval.ID, approval.AccountID, "om_card", now); err != nil {
		t.Fatalf("SetToolApprovalCardMessageID returned error: %v", err)
	}
	continuation := attachWorkflowContinuationForTest(t, st, approval.ID, approval.AccountID, now, 0)
	if _, _, err := st.CommitWorkflowContinuation(continuation.RequestID, continuation.AccountID, 1, now.Add(time.Second)); err != nil {
		t.Fatalf("CommitWorkflowContinuation returned error: %v", err)
	}
	if _, _, _, err := st.StoreWorkflowResult(WorkflowResult{
		RequestID: continuation.RequestID,
		AccountID: continuation.AccountID,
		State:     WorkflowResultStateSucceeded,
		Payload:   []byte(`{"message":"operation completed"}`),
		CreatedAt: now.Add(2 * time.Second),
	}); err != nil {
		t.Fatalf("StoreWorkflowResult returned error: %v", err)
	}
	claimed, err := st.ClaimWorkflowContinuation(
		continuation.RequestID, continuation.AccountID, "continuation-lease", now.Add(3*time.Second), time.Minute,
	)
	if err != nil {
		t.Fatalf("ClaimWorkflowContinuation returned error: %v", err)
	}
	if _, err := st.db.Exec(`CREATE TRIGGER reject_card_delivery BEFORE INSERT ON feishu_card_deliveries BEGIN SELECT RAISE(ABORT, 'injected card delivery failure'); END`); err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}
	if err := st.CompleteWorkflowContinuation(
		claimed.RequestID, claimed.AccountID, claimed.LeaseToken, WorkflowContinuationStateFailed, "model unavailable", now.Add(4*time.Second),
	); err == nil {
		t.Fatal("CompleteWorkflowContinuation returned nil error with rejected outbox insert")
	}
	rolledBack, err := st.GetWorkflowContinuation(claimed.RequestID, claimed.AccountID)
	if err != nil || rolledBack.State != WorkflowContinuationStateProcessing || rolledBack.LeaseToken != claimed.LeaseToken {
		t.Fatalf("continuation after rolled-back outbox failure = %#v err=%v", rolledBack, err)
	}
	if _, err := st.db.Exec(`DROP TRIGGER reject_card_delivery`); err != nil {
		t.Fatalf("drop failure trigger: %v", err)
	}
	if err := st.CompleteWorkflowContinuation(
		claimed.RequestID, claimed.AccountID, claimed.LeaseToken, WorkflowContinuationStateFailed, "model unavailable", now.Add(5*time.Second),
	); err != nil {
		t.Fatalf("CompleteWorkflowContinuation returned error: %v", err)
	}
	delivery, err := st.GetFeishuCardDeliveryByKey(
		claimed.AccountID,
		claimed.RequestID,
		FeishuCardDeliveryPurposeWorkflowExhausted,
		FeishuCardDeliveryRevisionContinuation,
	)
	if err != nil || delivery.State != FeishuCardDeliveryStatePending || delivery.CardMessageID != "om_card" {
		t.Fatalf("continuation terminal delivery = %#v err=%v", delivery, err)
	}
}
