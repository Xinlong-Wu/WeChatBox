package store

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

const (
	testFeishuDocxAppendExecutionOwner = "runtime_test"
	testFeishuDocxAppendExecutionLease = time.Minute
)

func TestFeishuDocxAppendOperationLifecycleFreezesEnvelopeAndClearsTerminalPayload(t *testing.T) {
	st := openFeishuDocsTestStore(t)
	now := time.Date(2026, time.August, 4, 18, 0, 0, 0, time.UTC)
	seedFeishuDocxAppendWorkflow(t, st, "req_append_ledger", WorkflowRequestKindFeishuDocsAppend, now)
	want := FeishuDocxAppendOperation{
		RequestID:          "req_append_ledger",
		AccountID:          "feishu:cli_test",
		ChatID:             "oc_chat",
		ActorOpenID:        "ou_requester",
		ActorUserID:        "u_requester",
		DocumentToken:      "doxcn_append_ledger",
		ClientToken:        "stable-client-token",
		InsertionIndex:     2,
		PayloadHash:        "logical-payload-hash",
		EnvelopeHash:       "first-envelope-hash",
		EnvelopeCiphertext: "v1.first-ciphertext",
		CreatedAt:          now,
	}
	prepared, created, err := st.PrepareFeishuDocxAppendOperation(want)
	if err != nil || !created || prepared.State != FeishuDocxAppendOperationStatePrepared {
		t.Fatalf("prepared operation = %#v created=%t err=%v", prepared, created, err)
	}

	concurrentCandidate := want
	concurrentCandidate.InsertionIndex = 9
	concurrentCandidate.EnvelopeHash = "later-envelope-hash"
	concurrentCandidate.EnvelopeCiphertext = "v1.later-ciphertext"
	replayed, created, err := st.PrepareFeishuDocxAppendOperation(concurrentCandidate)
	if err != nil || created {
		t.Fatalf("equivalent replay = %#v created=%t err=%v", replayed, created, err)
	}
	if replayed.InsertionIndex != want.InsertionIndex || replayed.EnvelopeHash != want.EnvelopeHash || replayed.EnvelopeCiphertext != want.EnvelopeCiphertext {
		t.Fatalf("equivalent replay replaced frozen envelope: %#v", replayed)
	}

	conflict := want
	conflict.PayloadHash = "different-logical-payload"
	if _, _, err := st.PrepareFeishuDocxAppendOperation(conflict); !errors.Is(err, ErrFeishuDocxAppendOperationConflict) {
		t.Fatalf("conflicting prepare error = %v, want ErrFeishuDocxAppendOperationConflict", err)
	}

	const firstExecutionToken = "execution_first"
	started, claimed, err := st.StartFeishuDocxAppendOperation(want.RequestID, want.AccountID, testFeishuDocxAppendExecutionOwner, firstExecutionToken, now.Add(time.Second), testFeishuDocxAppendExecutionLease)
	if err != nil || !claimed || started.State != FeishuDocxAppendOperationStateRemoteStarted {
		t.Fatalf("started operation = %#v claimed=%t err=%v", started, claimed, err)
	}
	replayedStart, claimed, err := st.StartFeishuDocxAppendOperation(want.RequestID, want.AccountID, testFeishuDocxAppendExecutionOwner, "execution_second", now.Add(2*time.Second), testFeishuDocxAppendExecutionLease)
	if err != nil || claimed || replayedStart.State != FeishuDocxAppendOperationStateRemoteStarted {
		t.Fatalf("replayed start = %#v claimed=%t err=%v", replayedStart, claimed, err)
	}
	unknown, err := st.MarkFeishuDocxAppendOperationOutcomeUnknown(want.RequestID, want.AccountID, firstExecutionToken, "lost_response", now.Add(3*time.Second))
	if err != nil || unknown.State != FeishuDocxAppendOperationStateOutcomeUnknown || unknown.EnvelopeCiphertext == "" || unknown.ExecutionOwnerID != "" || unknown.ExecutionToken != "" || !unknown.ExecutionLeaseUntil.IsZero() {
		t.Fatalf("unknown operation = %#v err=%v, want recoverable ciphertext", unknown, err)
	}
	const recoveryExecutionToken = "execution_recovery"
	recovering, claimed, err := st.ClaimFeishuDocxAppendOperationRecovery(want.RequestID, want.AccountID, testFeishuDocxAppendExecutionOwner, recoveryExecutionToken, now.Add(4*time.Second), testFeishuDocxAppendExecutionLease)
	if err != nil || !claimed || recovering.State != FeishuDocxAppendOperationStateOutcomeUnknown {
		t.Fatalf("recovery claim = %#v claimed=%t err=%v", recovering, claimed, err)
	}
	succeeded, err := st.MarkFeishuDocxAppendOperationSucceeded(want.RequestID, want.AccountID, recoveryExecutionToken, now.Add(5*time.Second))
	if err != nil || succeeded.State != FeishuDocxAppendOperationStateSucceeded || succeeded.EnvelopeCiphertext != "" || succeeded.ExecutionOwnerID != "" || succeeded.ExecutionToken != "" || !succeeded.ExecutionLeaseUntil.IsZero() {
		t.Fatalf("succeeded operation = %#v err=%v, want terminal ciphertext cleanup", succeeded, err)
	}
	idempotent, err := st.MarkFeishuDocxAppendOperationSucceeded(want.RequestID, want.AccountID, recoveryExecutionToken, now.Add(6*time.Second))
	if err != nil || idempotent.State != FeishuDocxAppendOperationStateSucceeded || idempotent.EnvelopeCiphertext != "" {
		t.Fatalf("idempotent success = %#v err=%v", idempotent, err)
	}
	recoverable, err := st.ListRecoverableFeishuDocxAppendOperations(want.AccountID, 10)
	if err != nil || len(recoverable) != 1 || recoverable[0].State != FeishuDocxAppendOperationStateSucceeded {
		t.Fatalf("terminal workflow gap list = %#v err=%v", recoverable, err)
	}
	if err := st.UpdateWorkflowRequestState(want.RequestID, want.AccountID, WorkflowRequestStateSucceeded, now.Add(7*time.Second)); err != nil {
		t.Fatalf("UpdateWorkflowRequestState returned error: %v", err)
	}
	recoverable, err = st.ListRecoverableFeishuDocxAppendOperations(want.AccountID, 10)
	if err != nil || len(recoverable) != 0 {
		t.Fatalf("recovery list after workflow reconciliation = %#v err=%v, want empty", recoverable, err)
	}
}

func TestFeishuDocxAppendOperationFailureClearsProtectedEnvelope(t *testing.T) {
	st := openFeishuDocsTestStore(t)
	now := time.Date(2026, time.August, 4, 18, 15, 0, 0, time.UTC)
	seedFeishuDocxAppendWorkflow(t, st, "req_append_failed", WorkflowRequestKindFeishuDocsAppend, now)
	operation := testFeishuDocxAppendOperation("req_append_failed", now)
	if _, _, err := st.PrepareFeishuDocxAppendOperation(operation); err != nil {
		t.Fatalf("PrepareFeishuDocxAppendOperation returned error: %v", err)
	}
	const executionToken = "execution_failure"
	if _, claimed, err := st.StartFeishuDocxAppendOperation(operation.RequestID, operation.AccountID, testFeishuDocxAppendExecutionOwner, executionToken, now.Add(time.Second), testFeishuDocxAppendExecutionLease); err != nil || !claimed {
		t.Fatalf("StartFeishuDocxAppendOperation claimed=%t err=%v", claimed, err)
	}
	failed, err := st.MarkFeishuDocxAppendOperationFailed(operation.RequestID, operation.AccountID, executionToken, "permission_denied", now.Add(2*time.Second))
	if err != nil || failed.State != FeishuDocxAppendOperationStateFailed || failed.EnvelopeCiphertext != "" || failed.ExecutionOwnerID != "" || failed.ExecutionToken != "" || !failed.ExecutionLeaseUntil.IsZero() || failed.LastErrorCategory != "permission_denied" {
		t.Fatalf("failed operation = %#v err=%v", failed, err)
	}
}

func TestFeishuDocxAppendOperationOutcomeUnknownCannotBecomeFailed(t *testing.T) {
	st := openFeishuDocsTestStore(t)
	now := time.Date(2026, time.August, 4, 18, 20, 0, 0, time.UTC)
	seedFeishuDocxAppendWorkflow(t, st, "req_append_unknown_not_failed", WorkflowRequestKindFeishuDocsAppend, now)
	operation := testFeishuDocxAppendOperation("req_append_unknown_not_failed", now)
	if _, _, err := st.PrepareFeishuDocxAppendOperation(operation); err != nil {
		t.Fatalf("PrepareFeishuDocxAppendOperation returned error: %v", err)
	}
	const executionToken = "execution_unknown"
	if _, claimed, err := st.StartFeishuDocxAppendOperation(operation.RequestID, operation.AccountID, testFeishuDocxAppendExecutionOwner, executionToken, now.Add(time.Second), testFeishuDocxAppendExecutionLease); err != nil || !claimed {
		t.Fatalf("StartFeishuDocxAppendOperation claimed=%t err=%v", claimed, err)
	}
	unknown, err := st.MarkFeishuDocxAppendOperationOutcomeUnknown(operation.RequestID, operation.AccountID, executionToken, "lost_response", now.Add(2*time.Second))
	if err != nil || unknown.State != FeishuDocxAppendOperationStateOutcomeUnknown || unknown.EnvelopeCiphertext == "" {
		t.Fatalf("unknown operation = %#v err=%v", unknown, err)
	}

	unchanged, err := st.MarkFeishuDocxAppendOperationFailed(operation.RequestID, operation.AccountID, executionToken, "later_rejection", now.Add(3*time.Second))
	if !errors.Is(err, ErrFeishuDocxAppendOperationNotReady) {
		t.Fatalf("MarkFeishuDocxAppendOperationFailed error = %v, want ErrFeishuDocxAppendOperationNotReady", err)
	}
	if unchanged.State != FeishuDocxAppendOperationStateOutcomeUnknown || unchanged.EnvelopeCiphertext == "" {
		t.Fatalf("operation after rejected terminal downgrade = %#v, want recoverable outcome_unknown", unchanged)
	}
}

func TestFeishuDocxAppendOperationConcurrentStartHasSingleCaller(t *testing.T) {
	first, second := openSharedFeishuTestStores(t)
	now := time.Date(2026, time.August, 4, 18, 30, 0, 0, time.UTC)
	seedFeishuDocxAppendWorkflow(t, first, "req_append_concurrent", WorkflowRequestKindFeishuDocsAppend, now)
	operation := testFeishuDocxAppendOperation("req_append_concurrent", now)
	if _, _, err := first.PrepareFeishuDocxAppendOperation(operation); err != nil {
		t.Fatalf("PrepareFeishuDocxAppendOperation returned error: %v", err)
	}

	start := make(chan struct{})
	type outcome struct {
		claimed bool
		err     error
	}
	outcomes := make(chan outcome, 2)
	var wg sync.WaitGroup
	for index, candidate := range []*Store{first, second} {
		wg.Add(1)
		go func(candidate *Store, executionToken string) {
			defer wg.Done()
			<-start
			_, claimed, err := candidate.StartFeishuDocxAppendOperation(operation.RequestID, operation.AccountID, testFeishuDocxAppendExecutionOwner, executionToken, now.Add(time.Second), testFeishuDocxAppendExecutionLease)
			outcomes <- outcome{claimed: claimed, err: err}
		}(candidate, fmt.Sprintf("execution_%d", index))
	}
	close(start)
	wg.Wait()
	close(outcomes)
	winners := 0
	for result := range outcomes {
		if result.err != nil {
			t.Fatalf("StartFeishuDocxAppendOperation returned error: %v", result.err)
		}
		if result.claimed {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("concurrent append start winners = %d, want 1", winners)
	}
}

func TestFeishuDocxAppendOperationStartDoesNotRequireAccountRuntimeLease(t *testing.T) {
	st := openFeishuDocsTestStore(t)
	now := time.Date(2026, time.August, 4, 18, 32, 0, 0, time.UTC)
	seedFeishuDocxAppendWorkflow(t, st, "req_append_execution_owner", WorkflowRequestKindFeishuDocsAppend, now)
	operation := testFeishuDocxAppendOperation("req_append_execution_owner", now)
	if _, _, err := st.PrepareFeishuDocxAppendOperation(operation); err != nil {
		t.Fatalf("PrepareFeishuDocxAppendOperation returned error: %v", err)
	}
	started, claimed, err := st.StartFeishuDocxAppendOperation(operation.RequestID, operation.AccountID, "runtime_independent", "execution_first", now.Add(time.Second), time.Minute)
	if err != nil || !claimed || started.State != FeishuDocxAppendOperationStateRemoteStarted || started.ExecutionOwnerID != "runtime_independent" || started.ExecutionToken != "execution_first" {
		t.Fatalf("start without account runtime lease = %#v claimed=%t err=%v", started, claimed, err)
	}
}

func TestFeishuDocxAppendOperationRecoveryClaimFencesStaleOwner(t *testing.T) {
	st := openFeishuDocsTestStore(t)
	now := time.Date(2026, time.August, 4, 18, 35, 0, 0, time.UTC)
	seedFeishuDocxAppendWorkflow(t, st, "req_append_recovery_fence", WorkflowRequestKindFeishuDocsAppend, now)
	operation := testFeishuDocxAppendOperation("req_append_recovery_fence", now)
	if _, _, err := st.PrepareFeishuDocxAppendOperation(operation); err != nil {
		t.Fatalf("PrepareFeishuDocxAppendOperation returned error: %v", err)
	}
	if _, claimed, err := st.StartFeishuDocxAppendOperation(operation.RequestID, operation.AccountID, "runtime_old", "execution_old", now.Add(time.Second), time.Minute); err != nil || !claimed {
		t.Fatalf("StartFeishuDocxAppendOperation claimed=%t err=%v", claimed, err)
	}
	blocked, claimed, err := st.ClaimFeishuDocxAppendOperationRecovery(operation.RequestID, operation.AccountID, "runtime_old", "execution_same_owner", now.Add(2*time.Second), time.Minute)
	if err != nil || claimed || blocked.ExecutionToken != "execution_old" || blocked.State != FeishuDocxAppendOperationStateRemoteStarted {
		t.Fatalf("same-owner live recovery claim = %#v claimed=%t err=%v", blocked, claimed, err)
	}
	blocked, claimed, err = st.ClaimFeishuDocxAppendOperationRecovery(operation.RequestID, operation.AccountID, "runtime_new", "execution_new", now.Add(3*time.Second), time.Minute)
	if err != nil || claimed || blocked.ExecutionToken != "execution_old" || blocked.State != FeishuDocxAppendOperationStateRemoteStarted {
		t.Fatalf("different-owner live recovery claim = %#v claimed=%t err=%v", blocked, claimed, err)
	}

	takenOver, claimed, err := st.ClaimFeishuDocxAppendOperationRecovery(operation.RequestID, operation.AccountID, "runtime_new", "execution_new", now.Add(62*time.Second), time.Minute)
	if err != nil || !claimed || takenOver.State != FeishuDocxAppendOperationStateOutcomeUnknown || takenOver.ExecutionOwnerID != "runtime_new" || takenOver.ExecutionToken != "execution_new" {
		t.Fatalf("new-owner recovery claim = %#v claimed=%t err=%v", takenOver, claimed, err)
	}
	staleClaim, claimed, err := st.ClaimFeishuDocxAppendOperationRecovery(operation.RequestID, operation.AccountID, "runtime_old", "execution_stale", now.Add(63*time.Second), time.Minute)
	if err != nil || claimed || staleClaim.ExecutionOwnerID != "runtime_new" || staleClaim.ExecutionToken != "execution_new" {
		t.Fatalf("stale runtime recovery claim = %#v claimed=%t err=%v, want current owner preserved", staleClaim, claimed, err)
	}
	stale, err := st.MarkFeishuDocxAppendOperationSucceeded(operation.RequestID, operation.AccountID, "execution_old", now.Add(64*time.Second))
	if !errors.Is(err, ErrFeishuDocxAppendOperationNotReady) || stale.State != FeishuDocxAppendOperationStateOutcomeUnknown || stale.ExecutionToken != "execution_new" || stale.EnvelopeCiphertext == "" {
		t.Fatalf("stale owner success = %#v err=%v, want fenced recoverable operation", stale, err)
	}
	stale, err = st.MarkFeishuDocxAppendOperationFailed(operation.RequestID, operation.AccountID, "execution_old", "late_rejection", now.Add(64*time.Second))
	if !errors.Is(err, ErrFeishuDocxAppendOperationNotReady) || stale.State != FeishuDocxAppendOperationStateOutcomeUnknown || stale.ExecutionToken != "execution_new" || stale.EnvelopeCiphertext == "" {
		t.Fatalf("stale owner failure = %#v err=%v, want fenced recoverable operation", stale, err)
	}
	completed, err := st.MarkFeishuDocxAppendOperationSucceeded(operation.RequestID, operation.AccountID, "execution_new", now.Add(65*time.Second))
	if err != nil || completed.State != FeishuDocxAppendOperationStateSucceeded || completed.EnvelopeCiphertext != "" || completed.ExecutionOwnerID != "" || completed.ExecutionToken != "" || !completed.ExecutionLeaseUntil.IsZero() {
		t.Fatalf("new owner completion = %#v err=%v", completed, err)
	}
}

func TestFeishuDocxAppendOperationRecoveryLeaseAllowsSameOwnerTakeoverAfterExpiry(t *testing.T) {
	st := openFeishuDocsTestStore(t)
	now := time.Date(2026, time.August, 4, 18, 40, 0, 0, time.UTC)
	seedFeishuDocxAppendWorkflow(t, st, "req_append_recovery_lease", WorkflowRequestKindFeishuDocsAppend, now)
	operation := testFeishuDocxAppendOperation("req_append_recovery_lease", now)
	if _, _, err := st.PrepareFeishuDocxAppendOperation(operation); err != nil {
		t.Fatalf("PrepareFeishuDocxAppendOperation returned error: %v", err)
	}
	if _, claimed, err := st.StartFeishuDocxAppendOperation(operation.RequestID, operation.AccountID, "runtime_same", "execution_old", now.Add(time.Second), 10*time.Second); err != nil || !claimed {
		t.Fatalf("StartFeishuDocxAppendOperation claimed=%t err=%v", claimed, err)
	}
	takenOver, claimed, err := st.ClaimFeishuDocxAppendOperationRecovery(operation.RequestID, operation.AccountID, "runtime_same", "execution_new", now.Add(12*time.Second), time.Minute)
	if err != nil || !claimed || takenOver.State != FeishuDocxAppendOperationStateOutcomeUnknown || takenOver.ExecutionToken != "execution_new" {
		t.Fatalf("expired same-owner recovery claim = %#v claimed=%t err=%v", takenOver, claimed, err)
	}
}

func TestDeleteFeishuDocsDataRemovesDocxAppendOperations(t *testing.T) {
	st := openFeishuDocsTestStore(t)
	now := time.Date(2026, time.August, 4, 18, 45, 0, 0, time.UTC)
	seedFeishuDocxAppendWorkflow(t, st, "req_append_delete", WorkflowRequestKindFeishuDocsAppend, now)
	operation := testFeishuDocxAppendOperation("req_append_delete", now)
	if _, _, err := st.PrepareFeishuDocxAppendOperation(operation); err != nil {
		t.Fatalf("PrepareFeishuDocxAppendOperation returned error: %v", err)
	}
	if err := st.DeleteFeishuDocsData(operation.AccountID); err != nil {
		t.Fatalf("DeleteFeishuDocsData returned error: %v", err)
	}
	if _, err := st.GetFeishuDocxAppendOperation(operation.RequestID, operation.AccountID); !errors.Is(err, ErrFeishuDocxAppendOperationNotFound) {
		t.Fatalf("GetFeishuDocxAppendOperation after delete error = %v, want not found", err)
	}
}

func TestListRecoverableFeishuDocxAppendOperationsUsesStableKeysetPagination(t *testing.T) {
	st := openFeishuDocsTestStore(t)
	now := time.Date(2026, time.August, 4, 18, 50, 0, 0, time.UTC)
	for index, requestID := range []string{"req_append_page_a", "req_append_page_b", "req_append_page_c"} {
		createdAt := now
		if index == 2 {
			createdAt = now.Add(time.Second)
		}
		seedFeishuDocxAppendWorkflow(t, st, requestID, WorkflowRequestKindFeishuDocsAppend, createdAt)
		operation := testFeishuDocxAppendOperation(requestID, createdAt)
		operation.PayloadHash = "payload-" + requestID
		if _, _, err := st.PrepareFeishuDocxAppendOperation(operation); err != nil {
			t.Fatalf("PrepareFeishuDocxAppendOperation(%q): %v", requestID, err)
		}
	}
	first, err := st.ListRecoverableFeishuDocxAppendOperationsAfter("feishu:cli_test", time.Time{}, "", 2)
	if err != nil || len(first) != 2 || first[0].RequestID != "req_append_page_a" || first[1].RequestID != "req_append_page_b" {
		t.Fatalf("first recovery page = %#v err=%v", first, err)
	}
	second, err := st.ListRecoverableFeishuDocxAppendOperationsAfter(
		"feishu:cli_test",
		first[len(first)-1].CreatedAt,
		first[len(first)-1].RequestID,
		2,
	)
	if err != nil || len(second) != 1 || second[0].RequestID != "req_append_page_c" {
		t.Fatalf("second recovery page = %#v err=%v", second, err)
	}
}

func TestReconcileFeishuDocxAppendWorkflowStateUsesCurrentLedgerAndSkipsToolApproval(t *testing.T) {
	st := openFeishuDocsTestStore(t)
	now := time.Date(2026, time.August, 4, 18, 55, 0, 0, time.UTC)
	seedFeishuDocxAppendWorkflow(t, st, "req_create_reconcile", WorkflowRequestKindFeishuDocsCreate, now)
	operation := testFeishuDocxAppendOperation("req_create_reconcile", now)
	if _, _, err := st.PrepareFeishuDocxAppendOperation(operation); err != nil {
		t.Fatalf("PrepareFeishuDocxAppendOperation returned error: %v", err)
	}
	if _, claimed, err := st.StartFeishuDocxAppendOperation(operation.RequestID, operation.AccountID, testFeishuDocxAppendExecutionOwner, "execution_old", now.Add(time.Second), time.Minute); err != nil || !claimed {
		t.Fatalf("StartFeishuDocxAppendOperation claimed=%t err=%v", claimed, err)
	}
	if _, err := st.MarkFeishuDocxAppendOperationOutcomeUnknown(operation.RequestID, operation.AccountID, "execution_old", "lost_response", now.Add(2*time.Second)); err != nil {
		t.Fatalf("MarkFeishuDocxAppendOperationOutcomeUnknown returned error: %v", err)
	}
	if err := st.UpdateWorkflowRequestState(operation.RequestID, operation.AccountID, WorkflowRequestStateSucceeded, now.Add(3*time.Second)); err != nil {
		t.Fatalf("seed stale workflow success: %v", err)
	}
	reconciled, managed, err := st.ReconcileFeishuDocxAppendWorkflowState(operation.RequestID, operation.AccountID, now.Add(4*time.Second))
	if err != nil || !managed || reconciled.State != WorkflowRequestStatePartial {
		t.Fatalf("unknown append reconciliation = %#v managed=%t err=%v, want partial", reconciled, managed, err)
	}
	if _, claimed, err := st.ClaimFeishuDocxAppendOperationRecovery(operation.RequestID, operation.AccountID, testFeishuDocxAppendExecutionOwner, "execution_new", now.Add(5*time.Second), time.Minute); err != nil || !claimed {
		t.Fatalf("ClaimFeishuDocxAppendOperationRecovery claimed=%t err=%v", claimed, err)
	}
	if _, err := st.MarkFeishuDocxAppendOperationSucceeded(operation.RequestID, operation.AccountID, "execution_new", now.Add(6*time.Second)); err != nil {
		t.Fatalf("MarkFeishuDocxAppendOperationSucceeded returned error: %v", err)
	}
	reconciled, managed, err = st.ReconcileFeishuDocxAppendWorkflowState(operation.RequestID, operation.AccountID, now.Add(7*time.Second))
	if err != nil || !managed || reconciled.State != WorkflowRequestStateSucceeded {
		t.Fatalf("succeeded append reconciliation = %#v managed=%t err=%v, want succeeded", reconciled, managed, err)
	}

	seedFeishuDocxAppendWorkflow(t, st, "req_approval_reconcile", WorkflowRequestKindToolApproval, now.Add(8*time.Second))
	approvalOperation := testFeishuDocxAppendOperation("req_approval_reconcile", now.Add(8*time.Second))
	approvalOperation.PayloadHash = "approval-payload-hash"
	if _, _, err := st.PrepareFeishuDocxAppendOperation(approvalOperation); err != nil {
		t.Fatalf("prepare approval append operation: %v", err)
	}
	unchanged, managed, err := st.ReconcileFeishuDocxAppendWorkflowState(approvalOperation.RequestID, approvalOperation.AccountID, now.Add(9*time.Second))
	if err != nil || managed || unchanged.Kind != WorkflowRequestKindToolApproval || unchanged.State != WorkflowRequestStateExecuting {
		t.Fatalf("tool approval reconciliation = %#v managed=%t err=%v, want unchanged", unchanged, managed, err)
	}

	seedFeishuDocxAppendWorkflow(t, st, "req_create_without_ledger", WorkflowRequestKindFeishuDocsCreate, now.Add(10*time.Second))
	if err := st.UpdateWorkflowRequestState("req_create_without_ledger", "feishu:cli_test", WorkflowRequestStateSucceeded, now.Add(11*time.Second)); err != nil {
		t.Fatalf("seed legacy create success: %v", err)
	}
	unchanged, managed, err = st.ReconcileFeishuDocxAppendWorkflowState("req_create_without_ledger", "feishu:cli_test", now.Add(12*time.Second))
	if err != nil || managed || unchanged.Kind != WorkflowRequestKindFeishuDocsCreate || unchanged.State != WorkflowRequestStateSucceeded {
		t.Fatalf("no-ledger create reconciliation = %#v managed=%t err=%v, want unchanged", unchanged, managed, err)
	}
}

func TestUpdateWorkflowRequestStatePreservesDocxAppendLedgerAuthorityAndLegacyCompatibility(t *testing.T) {
	st := openFeishuDocsTestStore(t)
	now := time.Date(2026, time.August, 4, 19, 0, 0, 0, time.UTC)

	type ledgerCase struct {
		name     string
		kind     string
		state    string
		stale    string
		expected string
	}
	cases := []ledgerCase{
		{name: "append_prepared", kind: WorkflowRequestKindFeishuDocsAppend, state: FeishuDocxAppendOperationStatePrepared, stale: WorkflowRequestStateSucceeded, expected: WorkflowRequestStatePartial},
		{name: "append_remote_started", kind: WorkflowRequestKindFeishuDocsAppend, state: FeishuDocxAppendOperationStateRemoteStarted, stale: WorkflowRequestStateSucceeded, expected: WorkflowRequestStatePartial},
		{name: "append_outcome_unknown", kind: WorkflowRequestKindFeishuDocsAppend, state: FeishuDocxAppendOperationStateOutcomeUnknown, stale: WorkflowRequestStateSucceeded, expected: WorkflowRequestStatePartial},
		{name: "append_succeeded", kind: WorkflowRequestKindFeishuDocsAppend, state: FeishuDocxAppendOperationStateSucceeded, stale: WorkflowRequestStateFailed, expected: WorkflowRequestStateSucceeded},
		{name: "append_failed", kind: WorkflowRequestKindFeishuDocsAppend, state: FeishuDocxAppendOperationStateFailed, stale: WorkflowRequestStateSucceeded, expected: WorkflowRequestStateFailed},
		{name: "create_prepared", kind: WorkflowRequestKindFeishuDocsCreate, state: FeishuDocxAppendOperationStatePrepared, stale: WorkflowRequestStateSucceeded, expected: WorkflowRequestStatePartial},
		{name: "create_remote_started", kind: WorkflowRequestKindFeishuDocsCreate, state: FeishuDocxAppendOperationStateRemoteStarted, stale: WorkflowRequestStateSucceeded, expected: WorkflowRequestStatePartial},
		{name: "create_outcome_unknown", kind: WorkflowRequestKindFeishuDocsCreate, state: FeishuDocxAppendOperationStateOutcomeUnknown, stale: WorkflowRequestStateSucceeded, expected: WorkflowRequestStatePartial},
		{name: "create_succeeded", kind: WorkflowRequestKindFeishuDocsCreate, state: FeishuDocxAppendOperationStateSucceeded, stale: WorkflowRequestStateFailed, expected: WorkflowRequestStateSucceeded},
		{name: "create_failed", kind: WorkflowRequestKindFeishuDocsCreate, state: FeishuDocxAppendOperationStateFailed, stale: WorkflowRequestStateSucceeded, expected: WorkflowRequestStatePartial},
	}

	seedLedger := func(t *testing.T, requestID, kind, state string, offset time.Duration) {
		t.Helper()
		createdAt := now.Add(offset)
		seedFeishuDocxAppendWorkflow(t, st, requestID, kind, createdAt)
		operation := testFeishuDocxAppendOperation(requestID, createdAt)
		operation.PayloadHash = "payload-" + requestID
		if _, _, err := st.PrepareFeishuDocxAppendOperation(operation); err != nil {
			t.Fatalf("PrepareFeishuDocxAppendOperation(%q): %v", requestID, err)
		}
		if state == FeishuDocxAppendOperationStatePrepared {
			return
		}
		executionToken := "execution-" + requestID
		if _, claimed, err := st.StartFeishuDocxAppendOperation(requestID, operation.AccountID, testFeishuDocxAppendExecutionOwner, executionToken, createdAt.Add(time.Second), time.Minute); err != nil || !claimed {
			t.Fatalf("StartFeishuDocxAppendOperation(%q) claimed=%t err=%v", requestID, claimed, err)
		}
		switch state {
		case FeishuDocxAppendOperationStateRemoteStarted:
			return
		case FeishuDocxAppendOperationStateOutcomeUnknown:
			if _, err := st.MarkFeishuDocxAppendOperationOutcomeUnknown(requestID, operation.AccountID, executionToken, "lost_response", createdAt.Add(2*time.Second)); err != nil {
				t.Fatalf("MarkFeishuDocxAppendOperationOutcomeUnknown(%q): %v", requestID, err)
			}
		case FeishuDocxAppendOperationStateSucceeded:
			if _, err := st.MarkFeishuDocxAppendOperationSucceeded(requestID, operation.AccountID, executionToken, createdAt.Add(2*time.Second)); err != nil {
				t.Fatalf("MarkFeishuDocxAppendOperationSucceeded(%q): %v", requestID, err)
			}
		case FeishuDocxAppendOperationStateFailed:
			if _, err := st.MarkFeishuDocxAppendOperationFailed(requestID, operation.AccountID, executionToken, "remote_rejected", createdAt.Add(2*time.Second)); err != nil {
				t.Fatalf("MarkFeishuDocxAppendOperationFailed(%q): %v", requestID, err)
			}
		default:
			t.Fatalf("unsupported test append state %q", state)
		}
	}

	for index, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			requestID := "req_workflow_matrix_" + testCase.name
			offset := time.Duration(index*10) * time.Second
			seedLedger(t, requestID, testCase.kind, testCase.state, offset)
			if err := st.UpdateWorkflowRequestState(requestID, "feishu:cli_test", testCase.stale, now.Add(offset+5*time.Second)); err != nil {
				t.Fatalf("UpdateWorkflowRequestState returned error: %v", err)
			}
			workflow, err := st.GetWorkflowRequest(requestID, "feishu:cli_test")
			if err != nil || workflow.State != testCase.expected {
				t.Fatalf("workflow after stale update = %#v err=%v, want %s", workflow, err, testCase.expected)
			}
		})
	}

	seedLedger(t, "req_workflow_matrix_approval", WorkflowRequestKindToolApproval, FeishuDocxAppendOperationStateSucceeded, 2*time.Minute)
	if err := st.UpdateWorkflowRequestState("req_workflow_matrix_approval", "feishu:cli_test", WorkflowRequestStatePartial, now.Add(2*time.Minute+5*time.Second)); err != nil {
		t.Fatalf("UpdateWorkflowRequestState(tool approval): %v", err)
	}
	approval, err := st.GetWorkflowRequest("req_workflow_matrix_approval", "feishu:cli_test")
	if err != nil || approval.State != WorkflowRequestStatePartial {
		t.Fatalf("tool approval workflow = %#v err=%v, want independently managed partial", approval, err)
	}

	for index, legacy := range []struct {
		requestID string
		kind      string
		state     string
	}{
		{requestID: "req_workflow_matrix_legacy_append", kind: WorkflowRequestKindFeishuDocsAppend, state: WorkflowRequestStateFailed},
		{requestID: "req_workflow_matrix_legacy_create", kind: WorkflowRequestKindFeishuDocsCreate, state: WorkflowRequestStateSucceeded},
	} {
		createdAt := now.Add(3*time.Minute + time.Duration(index)*time.Second)
		seedFeishuDocxAppendWorkflow(t, st, legacy.requestID, legacy.kind, createdAt)
		if err := st.UpdateWorkflowRequestState(legacy.requestID, "feishu:cli_test", legacy.state, createdAt.Add(time.Second)); err != nil {
			t.Fatalf("UpdateWorkflowRequestState(%q): %v", legacy.requestID, err)
		}
		workflow, err := st.GetWorkflowRequest(legacy.requestID, "feishu:cli_test")
		if err != nil || workflow.State != legacy.state {
			t.Fatalf("legacy no-ledger workflow = %#v err=%v, want %s", workflow, err, legacy.state)
		}
	}
}

func seedFeishuDocxAppendWorkflow(t *testing.T, st *Store, requestID, kind string, now time.Time) {
	t.Helper()
	if _, err := st.CreateWorkflowRequest(WorkflowRequest{
		ID:        requestID,
		AccountID: "feishu:cli_test",
		Kind:      kind,
		State:     WorkflowRequestStateExecuting,
		CreatedAt: now,
	}); err != nil {
		t.Fatalf("CreateWorkflowRequest returned error: %v", err)
	}
}

func testFeishuDocxAppendOperation(requestID string, now time.Time) FeishuDocxAppendOperation {
	return FeishuDocxAppendOperation{
		RequestID:          requestID,
		AccountID:          "feishu:cli_test",
		ChatID:             "oc_chat",
		ActorOpenID:        "ou_requester",
		ActorUserID:        "u_requester",
		DocumentToken:      "doxcn_append_operation",
		ClientToken:        "stable-client-token",
		InsertionIndex:     1,
		PayloadHash:        "logical-payload-hash",
		EnvelopeHash:       "envelope-hash",
		EnvelopeCiphertext: "v1.encrypted-envelope",
		CreatedAt:          now,
	}
}
