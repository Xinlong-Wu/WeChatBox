package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestWorkflowContinuationResultBeforeOriginCommit(t *testing.T) {
	st := openTestStore(t)
	now := time.Date(2026, time.August, 3, 12, 0, 0, 0, time.UTC)
	continuation := createWorkflowContinuationForTest(t, st, now, 7)

	stored, pending, ready, err := st.StoreWorkflowResult(WorkflowResult{
		RequestID: continuation.RequestID,
		AccountID: continuation.AccountID,
		State:     WorkflowResultStateSucceeded,
		Payload:   json.RawMessage(`{"status":"granted"}`),
		CreatedAt: now.Add(time.Second),
	})
	if err != nil {
		t.Fatalf("StoreWorkflowResult returned error: %v", err)
	}
	if ready || pending.State != WorkflowContinuationStateWaiting || pending.CommittedRevision != -1 {
		t.Fatalf("continuation after early result = %#v ready=%v, want uncommitted waiting", pending, ready)
	}
	if stored.State != WorkflowResultStateSucceeded || string(stored.Payload) != `{"status":"granted"}` {
		t.Fatalf("stored result = %#v", stored)
	}

	committed, ready, err := st.CommitWorkflowContinuation(
		continuation.RequestID,
		continuation.AccountID,
		8,
		now.Add(2*time.Second),
	)
	if err != nil {
		t.Fatalf("CommitWorkflowContinuation returned error: %v", err)
	}
	if !ready || committed.State != WorkflowContinuationStateReady || committed.CommittedRevision != 8 {
		t.Fatalf("committed continuation = %#v ready=%v, want ready revision 8", committed, ready)
	}

	resumable, err := st.ListResumableWorkflowContinuations(continuation.AccountID, now.Add(2*time.Second), 10)
	if err != nil {
		t.Fatalf("ListResumableWorkflowContinuations returned error: %v", err)
	}
	if len(resumable) != 1 || resumable[0].RequestID != continuation.RequestID {
		t.Fatalf("resumable continuations = %#v, want request %s", resumable, continuation.RequestID)
	}
}

func TestWorkflowContinuationOriginCommitBeforeResult(t *testing.T) {
	st := openTestStore(t)
	now := time.Date(2026, time.August, 3, 12, 15, 0, 0, time.UTC)
	continuation := createWorkflowContinuationForTest(t, st, now, 3)

	committed, ready, err := st.CommitWorkflowContinuation(
		continuation.RequestID,
		continuation.AccountID,
		4,
		now.Add(time.Second),
	)
	if err != nil {
		t.Fatalf("CommitWorkflowContinuation returned error: %v", err)
	}
	if ready || committed.State != WorkflowContinuationStateWaiting || committed.CommittedRevision != 4 {
		t.Fatalf("committed continuation = %#v ready=%v, want committed waiting", committed, ready)
	}

	_, resumed, ready, err := st.StoreWorkflowResult(WorkflowResult{
		RequestID: continuation.RequestID,
		AccountID: continuation.AccountID,
		State:     WorkflowResultStateDenied,
		Payload:   json.RawMessage(`{"reason":"user_denied"}`),
		CreatedAt: now.Add(2 * time.Second),
	})
	if err != nil {
		t.Fatalf("StoreWorkflowResult returned error: %v", err)
	}
	if !ready || resumed.State != WorkflowContinuationStateReady || !resumed.AvailableAt.Equal(now.Add(2*time.Second)) {
		t.Fatalf("resumed continuation = %#v ready=%v, want ready at result time", resumed, ready)
	}
}

func TestWorkflowResultIsIdempotentAndRejectsConflicts(t *testing.T) {
	st := openTestStore(t)
	now := time.Date(2026, time.August, 3, 12, 30, 0, 0, time.UTC)
	continuation := createWorkflowContinuationForTest(t, st, now, 10)
	if _, _, err := st.CommitWorkflowContinuation(continuation.RequestID, continuation.AccountID, 11, now.Add(time.Second)); err != nil {
		t.Fatalf("CommitWorkflowContinuation returned error: %v", err)
	}

	first, _, ready, err := st.StoreWorkflowResult(WorkflowResult{
		RequestID: continuation.RequestID,
		AccountID: continuation.AccountID,
		State:     WorkflowResultStateSucceeded,
		Payload:   json.RawMessage(`{"permission":"write","status":"granted"}`),
		CreatedAt: now.Add(2 * time.Second),
	})
	if err != nil || !ready {
		t.Fatalf("first StoreWorkflowResult result=%#v ready=%v err=%v", first, ready, err)
	}

	duplicate, duplicateContinuation, ready, err := st.StoreWorkflowResult(WorkflowResult{
		RequestID: continuation.RequestID,
		AccountID: continuation.AccountID,
		State:     WorkflowResultStateSucceeded,
		Payload:   json.RawMessage("{\n  \"status\": \"granted\", \"permission\": \"write\"\n}"),
		CreatedAt: now.Add(3 * time.Second),
	})
	if err != nil {
		t.Fatalf("duplicate StoreWorkflowResult returned error: %v", err)
	}
	if !ready || duplicateContinuation.State != WorkflowContinuationStateReady {
		t.Fatalf("duplicate continuation = %#v ready=%v, want still ready", duplicateContinuation, ready)
	}
	if !duplicate.CreatedAt.Equal(first.CreatedAt) || string(duplicate.Payload) != string(first.Payload) {
		t.Fatalf("duplicate result = %#v, want original %#v", duplicate, first)
	}

	_, _, _, err = st.StoreWorkflowResult(WorkflowResult{
		RequestID: continuation.RequestID,
		AccountID: continuation.AccountID,
		State:     WorkflowResultStateFailed,
		Payload:   json.RawMessage(`{"status":"failed"}`),
		CreatedAt: now.Add(4 * time.Second),
	})
	if !errors.Is(err, ErrWorkflowResultConflict) {
		t.Fatalf("conflicting state error = %v, want ErrWorkflowResultConflict", err)
	}

	_, _, _, err = st.StoreWorkflowResult(WorkflowResult{
		RequestID: continuation.RequestID,
		AccountID: continuation.AccountID,
		State:     WorkflowResultStateSucceeded,
		Payload:   json.RawMessage(`{"permission":"read","status":"granted"}`),
		CreatedAt: now.Add(5 * time.Second),
	})
	if !errors.Is(err, ErrWorkflowResultConflict) {
		t.Fatalf("conflicting payload error = %v, want ErrWorkflowResultConflict", err)
	}
}

func TestWorkflowContinuationLeaseRetryAndDelivery(t *testing.T) {
	st := openTestStore(t)
	now := time.Date(2026, time.August, 3, 12, 45, 0, 0, time.UTC)
	continuation := createReadyWorkflowContinuationForTest(t, st, now)

	claimed, err := st.ClaimWorkflowContinuation(
		continuation.RequestID,
		continuation.AccountID,
		"lease-one",
		now.Add(2*time.Second),
		time.Minute,
	)
	if err != nil {
		t.Fatalf("ClaimWorkflowContinuation returned error: %v", err)
	}
	if claimed.State != WorkflowContinuationStateProcessing || claimed.Attempts != 1 || claimed.LeaseToken != "lease-one" {
		t.Fatalf("claimed continuation = %#v", claimed)
	}

	err = st.RetryWorkflowContinuation(
		continuation.RequestID,
		continuation.AccountID,
		"wrong-lease",
		now.Add(5*time.Second),
		"temporary failure",
		now.Add(3*time.Second),
	)
	if !errors.Is(err, ErrWorkflowContinuationLeaseLost) {
		t.Fatalf("wrong lease retry error = %v, want ErrWorkflowContinuationLeaseLost", err)
	}

	longError := strings.Repeat("界", maxWorkflowContinuationErrorRunes+20)
	if err := st.RetryWorkflowContinuation(
		continuation.RequestID,
		continuation.AccountID,
		"lease-one",
		now.Add(5*time.Second),
		longError,
		now.Add(3*time.Second),
	); err != nil {
		t.Fatalf("RetryWorkflowContinuation returned error: %v", err)
	}
	retrying, err := st.GetWorkflowContinuation(continuation.RequestID, continuation.AccountID)
	if err != nil {
		t.Fatalf("GetWorkflowContinuation returned error: %v", err)
	}
	if retrying.State != WorkflowContinuationStateReady || retrying.LeaseToken != "" ||
		!retrying.LeaseExpiresAt.IsZero() || len([]rune(retrying.LastError)) != maxWorkflowContinuationErrorRunes {
		t.Fatalf("retrying continuation = %#v", retrying)
	}

	if _, err := st.ClaimWorkflowContinuation(
		continuation.RequestID,
		continuation.AccountID,
		"lease-two",
		now.Add(4*time.Second),
		time.Minute,
	); !errors.Is(err, ErrWorkflowContinuationNotReady) {
		t.Fatalf("early retry claim error = %v, want ErrWorkflowContinuationNotReady", err)
	}

	claimed, err = st.ClaimWorkflowContinuation(
		continuation.RequestID,
		continuation.AccountID,
		"lease-two",
		now.Add(5*time.Second),
		time.Minute,
	)
	if err != nil {
		t.Fatalf("second ClaimWorkflowContinuation returned error: %v", err)
	}
	if claimed.Attempts != 2 {
		t.Fatalf("second claim attempts = %d, want 2", claimed.Attempts)
	}
	if err := st.CompleteWorkflowContinuation(
		continuation.RequestID,
		continuation.AccountID,
		"lease-two",
		WorkflowContinuationStateDelivered,
		"",
		now.Add(6*time.Second),
	); err != nil {
		t.Fatalf("CompleteWorkflowContinuation returned error: %v", err)
	}
	if err := st.CompleteWorkflowContinuation(
		continuation.RequestID,
		continuation.AccountID,
		"lease-two",
		WorkflowContinuationStateDelivered,
		"",
		now.Add(7*time.Second),
	); !errors.Is(err, ErrWorkflowContinuationResolved) {
		t.Fatalf("duplicate completion error = %v, want ErrWorkflowContinuationResolved", err)
	}
}

func TestWorkflowContinuationExpiredLeaseIsResumable(t *testing.T) {
	st := openTestStore(t)
	now := time.Date(2026, time.August, 3, 13, 0, 0, 0, time.UTC)
	continuation := createReadyWorkflowContinuationForTest(t, st, now)
	claimedAt := now.Add(2 * time.Second)
	leaseDuration := time.Minute

	if _, err := st.ClaimWorkflowContinuation(
		continuation.RequestID,
		continuation.AccountID,
		"stale-lease",
		claimedAt,
		leaseDuration,
	); err != nil {
		t.Fatalf("ClaimWorkflowContinuation returned error: %v", err)
	}

	resumable, err := st.ListResumableWorkflowContinuations(
		continuation.AccountID,
		claimedAt.Add(leaseDuration-time.Millisecond),
		10,
	)
	if err != nil {
		t.Fatalf("ListResumableWorkflowContinuations before expiry returned error: %v", err)
	}
	if len(resumable) != 0 {
		t.Fatalf("resumable before lease expiry = %#v, want none", resumable)
	}

	leaseExpiredAt := claimedAt.Add(leaseDuration)
	resumable, err = st.ListResumableWorkflowContinuations(continuation.AccountID, leaseExpiredAt, 10)
	if err != nil {
		t.Fatalf("ListResumableWorkflowContinuations after expiry returned error: %v", err)
	}
	if len(resumable) != 1 || resumable[0].State != WorkflowContinuationStateProcessing {
		t.Fatalf("resumable after lease expiry = %#v, want expired processing request", resumable)
	}

	reclaimed, err := st.ClaimWorkflowContinuation(
		continuation.RequestID,
		continuation.AccountID,
		"fresh-lease",
		leaseExpiredAt,
		time.Minute,
	)
	if err != nil {
		t.Fatalf("reclaim expired workflow continuation returned error: %v", err)
	}
	if reclaimed.Attempts != 2 || reclaimed.LeaseToken != "fresh-lease" {
		t.Fatalf("reclaimed continuation = %#v, want second attempt with fresh lease", reclaimed)
	}
}

func createWorkflowContinuationForTest(t *testing.T, st *Store, now time.Time, originRevision int64) WorkflowContinuation {
	t.Helper()
	request, err := st.CreateWorkflowRequest(WorkflowRequest{
		AccountID: "feishu:cli_test",
		Kind:      WorkflowRequestKindFeishuResourceAccess,
		State:     WorkflowRequestStatePending,
		CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("CreateWorkflowRequest returned error: %v", err)
	}
	return attachWorkflowContinuationForTest(t, st, request.ID, request.AccountID, now, originRevision)
}

func attachWorkflowContinuationForTest(t *testing.T, st *Store, requestID, accountID string, now time.Time, originRevision int64) WorkflowContinuation {
	t.Helper()
	continuation, err := st.CreateWorkflowContinuation(WorkflowContinuation{
		RequestID:       requestID,
		AccountID:       accountID,
		Platform:        PlatformFeishu,
		UserKey:         "feishu:ou_requester",
		SessionID:       "session-work",
		ChatID:          "oc_chat",
		SourceMessageID: "om_source",
		ActorOpenID:     "ou_requester",
		ActorUserID:     "u_requester",
		OriginRevision:  originRevision,
		OriginTurnID:    "turn-" + requestID,
		ToolCallID:      "call-" + requestID,
		ToolName:        "feishu_docs_request_access",
		CreatedAt:       now,
	})
	if err != nil {
		t.Fatalf("CreateWorkflowContinuation returned error: %v", err)
	}
	return continuation
}

func createReadyWorkflowContinuationForTest(t *testing.T, st *Store, now time.Time) WorkflowContinuation {
	t.Helper()
	continuation := createWorkflowContinuationForTest(t, st, now, 20)
	if _, _, err := st.CommitWorkflowContinuation(
		continuation.RequestID,
		continuation.AccountID,
		21,
		now.Add(time.Second),
	); err != nil {
		t.Fatalf("CommitWorkflowContinuation returned error: %v", err)
	}
	_, continuation, ready, err := st.StoreWorkflowResult(WorkflowResult{
		RequestID: continuation.RequestID,
		AccountID: continuation.AccountID,
		State:     WorkflowResultStateSucceeded,
		Payload:   json.RawMessage(`{"status":"ok"}`),
		CreatedAt: now.Add(2 * time.Second),
	})
	if err != nil || !ready {
		t.Fatalf("StoreWorkflowResult continuation=%#v ready=%v err=%v", continuation, ready, err)
	}
	return continuation
}
