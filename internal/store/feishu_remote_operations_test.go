package store

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestFeishuRemoteOperationLifecycleAndIdempotentReplay(t *testing.T) {
	st := openFeishuDocsTestStore(t)
	now := time.Date(2026, time.August, 4, 15, 0, 0, 0, time.UTC)
	request, err := st.CreateWorkflowRequest(WorkflowRequest{
		ID:        "req_remote_document",
		AccountID: "feishu:cli_test",
		Kind:      WorkflowRequestKindFeishuDocsCreate,
		State:     WorkflowRequestStateExecuting,
		CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("CreateWorkflowRequest returned error: %v", err)
	}
	want := FeishuRemoteOperation{
		RequestID:               request.ID,
		AccountID:               request.AccountID,
		OperationKind:           FeishuRemoteOperationKindDocumentCreate,
		ChatID:                  "oc_chat",
		ActorOpenID:             "ou_requester",
		ParentResourceType:      "folder",
		ParentResourceToken:     "fld_parent",
		BindingParentToken:      "fld_parent",
		RequestedName:           "Quarterly plan",
		PayloadHash:             "payload_hash",
		RemoteResourceType:      "docx",
		InitialContentRequested: true,
		CreatedAt:               now,
	}
	prepared, err := st.PrepareFeishuRemoteOperation(want)
	if err != nil || prepared.State != FeishuRemoteOperationStatePrepared {
		t.Fatalf("prepared operation = %#v err=%v", prepared, err)
	}
	replayed, err := st.PrepareFeishuRemoteOperation(want)
	if err != nil || replayed.RequestID != prepared.RequestID || replayed.PayloadHash != prepared.PayloadHash {
		t.Fatalf("idempotent prepare = %#v err=%v", replayed, err)
	}
	conflict := want
	conflict.PayloadHash = "different_payload_hash"
	if _, err := st.PrepareFeishuRemoteOperation(conflict); !errors.Is(err, ErrFeishuRemoteOperationConflict) {
		t.Fatalf("conflicting prepare error = %v, want ErrFeishuRemoteOperationConflict", err)
	}

	started, claimed, err := st.StartFeishuRemoteOperation(request.ID, request.AccountID, now.Add(time.Second))
	if err != nil || !claimed || started.State != FeishuRemoteOperationStateRemoteStarted || !started.RemoteCallStartedAt.Equal(now.Add(time.Second)) {
		t.Fatalf("started operation = %#v claimed=%t err=%v", started, claimed, err)
	}
	replayedStart, claimed, err := st.StartFeishuRemoteOperation(request.ID, request.AccountID, now.Add(2*time.Second))
	if err != nil || claimed || replayedStart.State != FeishuRemoteOperationStateRemoteStarted || !replayedStart.RemoteCallStartedAt.Equal(started.RemoteCallStartedAt) {
		t.Fatalf("replayed start = %#v claimed=%t err=%v", replayedStart, claimed, err)
	}

	reconcile, err := st.MarkFeishuRemoteOperationReconcileRequired(request.ID, request.AccountID, "transport_error", now.Add(3*time.Second))
	if err != nil || reconcile.State != FeishuRemoteOperationStateReconcileRequired || reconcile.LastErrorCategory != "transport_error" {
		t.Fatalf("reconcile-required operation = %#v err=%v", reconcile, err)
	}
	unknown, err := st.MarkFeishuRemoteOperationOutcomeUnknown(request.ID, request.AccountID, "no_unique_candidate", now.Add(4*time.Second))
	if err != nil || unknown.State != FeishuRemoteOperationStateOutcomeUnknown {
		t.Fatalf("unknown operation = %#v err=%v", unknown, err)
	}
	succeeded, err := st.RecordFeishuRemoteOperationSuccess(
		request.ID,
		request.AccountID,
		"docx",
		"doxcn_created",
		"https://docs.feishu.cn/docx/doxcn_created",
		now.Add(5*time.Second),
	)
	if err != nil || succeeded.State != FeishuRemoteOperationStateRemoteSucceeded || succeeded.RemoteResourceToken != "doxcn_created" {
		t.Fatalf("remote-succeeded operation = %#v err=%v", succeeded, err)
	}
	persisted, err := st.MarkFeishuRemoteOperationPersisted(request.ID, request.AccountID, now.Add(6*time.Second))
	if err != nil || persisted.State != FeishuRemoteOperationStatePersisted || !persisted.RemoteResultAt.Equal(now.Add(5*time.Second)) {
		t.Fatalf("persisted operation = %#v err=%v", persisted, err)
	}
	idempotent, err := st.RecordFeishuRemoteOperationSuccess(
		request.ID,
		request.AccountID,
		"docx",
		"doxcn_created",
		"https://docs.feishu.cn/docx/doxcn_created",
		now.Add(7*time.Second),
	)
	if err != nil || idempotent.State != FeishuRemoteOperationStatePersisted || idempotent.RemoteResourceToken != persisted.RemoteResourceToken {
		t.Fatalf("idempotent success replay = %#v err=%v", idempotent, err)
	}
}

func TestFeishuRemoteOperationConcurrentStartHasSingleCaller(t *testing.T) {
	first, second := openSharedFeishuTestStores(t)
	now := time.Date(2026, time.August, 4, 15, 30, 0, 0, time.UTC)
	request, err := first.CreateWorkflowRequest(WorkflowRequest{
		ID:        "req_remote_concurrent",
		AccountID: "feishu:cli_test",
		Kind:      WorkflowRequestKindFeishuFolderCreate,
		State:     WorkflowRequestStateExecuting,
		CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("CreateWorkflowRequest returned error: %v", err)
	}
	if _, err := first.PrepareFeishuRemoteOperation(FeishuRemoteOperation{
		RequestID:           request.ID,
		AccountID:           request.AccountID,
		OperationKind:       FeishuRemoteOperationKindFolderCreate,
		ChatID:              "oc_chat",
		ActorOpenID:         "ou_requester",
		ParentResourceType:  "folder",
		ParentResourceToken: "fld_root",
		RequestedName:       "Project",
		PayloadHash:         "payload_hash",
		RemoteResourceType:  "folder",
		ShareMemberType:     "openid",
		ShareMemberID:       "ou_requester",
		CreatedAt:           now,
	}); err != nil {
		t.Fatalf("PrepareFeishuRemoteOperation returned error: %v", err)
	}

	start := make(chan struct{})
	type outcome struct {
		claimed bool
		err     error
	}
	outcomes := make(chan outcome, 2)
	var wg sync.WaitGroup
	for _, candidate := range []*Store{first, second} {
		wg.Add(1)
		go func(candidate *Store) {
			defer wg.Done()
			<-start
			_, claimed, startErr := candidate.StartFeishuRemoteOperation(request.ID, request.AccountID, now.Add(time.Second))
			outcomes <- outcome{claimed: claimed, err: startErr}
		}(candidate)
	}
	close(start)
	wg.Wait()
	close(outcomes)
	winners := 0
	for result := range outcomes {
		if result.err != nil {
			t.Fatalf("StartFeishuRemoteOperation returned error: %v", result.err)
		}
		if result.claimed {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("concurrent start winners = %d, want 1", winners)
	}
}

func TestFeishuRemoteOperationSuccessRejectsResourceClaimedByAnotherRequest(t *testing.T) {
	st := openFeishuDocsTestStore(t)
	now := time.Date(2026, time.August, 4, 16, 0, 0, 0, time.UTC)
	const (
		accountID     = "feishu:cli_test"
		resourceToken = "doxcn_shared_remote_result"
	)

	prepareStarted := func(requestID string) {
		t.Helper()
		request, err := st.CreateWorkflowRequest(WorkflowRequest{
			ID:        requestID,
			AccountID: accountID,
			Kind:      WorkflowRequestKindFeishuDocsCreate,
			State:     WorkflowRequestStateExecuting,
			CreatedAt: now,
		})
		if err != nil {
			t.Fatalf("CreateWorkflowRequest(%q) returned error: %v", requestID, err)
		}
		if _, err := st.PrepareFeishuRemoteOperation(FeishuRemoteOperation{
			RequestID:           request.ID,
			AccountID:           request.AccountID,
			OperationKind:       FeishuRemoteOperationKindDocumentCreate,
			ChatID:              "oc_chat",
			ActorOpenID:         "ou_requester",
			ParentResourceType:  "folder",
			ParentResourceToken: "fld_parent",
			BindingParentToken:  "fld_parent",
			RequestedName:       requestID,
			PayloadHash:         "payload_" + requestID,
			RemoteResourceType:  "docx",
			CreatedAt:           now,
		}); err != nil {
			t.Fatalf("PrepareFeishuRemoteOperation(%q) returned error: %v", requestID, err)
		}
		if _, claimed, err := st.StartFeishuRemoteOperation(request.ID, request.AccountID, now.Add(time.Second)); err != nil || !claimed {
			t.Fatalf("StartFeishuRemoteOperation(%q) claimed=%t err=%v", requestID, claimed, err)
		}
	}

	prepareStarted("req_remote_claim_first")
	prepareStarted("req_remote_claim_second")
	if _, err := st.RecordFeishuRemoteOperationSuccess(
		"req_remote_claim_first",
		accountID,
		"docx",
		resourceToken,
		"https://docs.feishu.cn/docx/"+resourceToken,
		now.Add(2*time.Second),
	); err != nil {
		t.Fatalf("first RecordFeishuRemoteOperationSuccess returned error: %v", err)
	}

	second, err := st.RecordFeishuRemoteOperationSuccess(
		"req_remote_claim_second",
		accountID,
		"docx",
		resourceToken,
		"https://docs.feishu.cn/docx/"+resourceToken,
		now.Add(3*time.Second),
	)
	if !errors.Is(err, ErrFeishuRemoteOperationConflict) {
		t.Fatalf("second RecordFeishuRemoteOperationSuccess operation=%#v err=%v, want ErrFeishuRemoteOperationConflict", second, err)
	}
	if second.State != FeishuRemoteOperationStateRemoteStarted || second.RemoteResourceToken != "" {
		t.Fatalf("second operation mutated despite conflicting resource claim: %#v", second)
	}
}

func TestFeishuRemoteOperationConcurrentSuccessHasSingleResourceClaimant(t *testing.T) {
	first, second := openSharedFeishuTestStores(t)
	now := time.Date(2026, time.August, 4, 16, 30, 0, 0, time.UTC)
	const (
		accountID     = "feishu:cli_test"
		resourceToken = "doxcn_concurrent_remote_result"
	)

	prepareStarted := func(requestID string) {
		t.Helper()
		request, err := first.CreateWorkflowRequest(WorkflowRequest{
			ID:        requestID,
			AccountID: accountID,
			Kind:      WorkflowRequestKindFeishuDocsCreate,
			State:     WorkflowRequestStateExecuting,
			CreatedAt: now,
		})
		if err != nil {
			t.Fatalf("CreateWorkflowRequest(%q) returned error: %v", requestID, err)
		}
		if _, err := first.PrepareFeishuRemoteOperation(FeishuRemoteOperation{
			RequestID:           request.ID,
			AccountID:           request.AccountID,
			OperationKind:       FeishuRemoteOperationKindDocumentCreate,
			ChatID:              "oc_chat",
			ActorOpenID:         "ou_requester",
			ParentResourceType:  "folder",
			ParentResourceToken: "fld_parent",
			BindingParentToken:  "fld_parent",
			RequestedName:       requestID,
			PayloadHash:         "payload_" + requestID,
			RemoteResourceType:  "docx",
			CreatedAt:           now,
		}); err != nil {
			t.Fatalf("PrepareFeishuRemoteOperation(%q) returned error: %v", requestID, err)
		}
		if _, claimed, err := first.StartFeishuRemoteOperation(request.ID, request.AccountID, now.Add(time.Second)); err != nil || !claimed {
			t.Fatalf("StartFeishuRemoteOperation(%q) claimed=%t err=%v", requestID, claimed, err)
		}
	}

	prepareStarted("req_remote_concurrent_claim_first")
	prepareStarted("req_remote_concurrent_claim_second")
	type result struct {
		requestID string
		err       error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for i, candidate := range []*Store{first, second} {
		requestID := "req_remote_concurrent_claim_first"
		if i == 1 {
			requestID = "req_remote_concurrent_claim_second"
		}
		wg.Add(1)
		go func(candidate *Store, requestID string) {
			defer wg.Done()
			<-start
			_, err := candidate.RecordFeishuRemoteOperationSuccess(
				requestID,
				accountID,
				"docx",
				resourceToken,
				"https://docs.feishu.cn/docx/"+resourceToken,
				now.Add(2*time.Second),
			)
			results <- result{requestID: requestID, err: err}
		}(candidate, requestID)
	}
	close(start)
	wg.Wait()
	close(results)

	succeeded := 0
	conflicted := 0
	for got := range results {
		switch {
		case got.err == nil:
			succeeded++
		case errors.Is(got.err, ErrFeishuRemoteOperationConflict):
			conflicted++
		default:
			t.Fatalf("RecordFeishuRemoteOperationSuccess(%q) returned unexpected error: %v", got.requestID, got.err)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("concurrent claim outcomes succeeded=%d conflicted=%d, want 1 and 1", succeeded, conflicted)
	}

	var claimed int
	if err := first.db.QueryRow(
		`SELECT COUNT(*) FROM feishu_remote_operations
		 WHERE account_id=? AND remote_resource_type='docx' AND remote_resource_token=?`,
		accountID,
		resourceToken,
	).Scan(&claimed); err != nil {
		t.Fatalf("count remote resource claimants: %v", err)
	}
	if claimed != 1 {
		t.Fatalf("remote resource claimant count = %d, want 1", claimed)
	}
}
