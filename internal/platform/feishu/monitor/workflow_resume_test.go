package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"lingobridge/internal/core"
	"lingobridge/internal/store"
)

type fakeWorkflowResumer struct {
	requests []core.WorkflowResumeRequest
	errs     []error
	text     string
}

func (f *fakeWorkflowResumer) ResumeWorkflow(ctx context.Context, request core.WorkflowResumeRequest, sender core.Sender) error {
	f.requests = append(f.requests, request)
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		if err != nil {
			return err
		}
	}
	text := f.text
	if text == "" {
		text = "workflow resumed"
	}
	return sender.Send(ctx, core.OutboundMessage{Text: text})
}

type workflowResumeSendCall struct {
	chatID string
	text   string
	uuid   string
}

type fakeWorkflowResumeTextSender struct {
	calls []workflowResumeSendCall
	errs  []error
}

func (f *fakeWorkflowResumeTextSender) CreateTextWithUUID(_ context.Context, chatID, text, uuid string) (string, error) {
	f.calls = append(f.calls, workflowResumeSendCall{chatID: chatID, text: text, uuid: uuid})
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		if err != nil {
			return "", err
		}
	}
	return "om_resume", nil
}

func TestWorkflowContinuationWorkerDeliversReadyResult(t *testing.T) {
	st := openFeishuApprovalTestStore(t)
	now := time.Date(2026, time.August, 3, 14, 0, 0, 0, time.UTC)
	continuation := createReadyWorkflowForWorker(t, st, now)
	resumer := &fakeWorkflowResumer{text: "authorization complete"}
	sender := &fakeWorkflowResumeTextSender{}
	worker, err := newWorkflowContinuationWorker(st, resumer, sender, store.Account{ID: continuation.AccountID}, nil)
	if err != nil {
		t.Fatalf("newWorkflowContinuationWorker returned error: %v", err)
	}
	worker.now = func() time.Time { return now.Add(3 * time.Second) }

	worker.processAvailable(t.Context())
	stored, err := st.GetWorkflowContinuation(continuation.RequestID, continuation.AccountID)
	if err != nil || stored.State != store.WorkflowContinuationStateDelivered || stored.Attempts != 1 {
		t.Fatalf("delivered continuation = %#v err=%v", stored, err)
	}
	if len(resumer.requests) != 1 || !resumer.requests[0].Continuation.ChatIsGroup {
		t.Fatalf("resume requests = %#v", resumer.requests)
	}
	if len(sender.calls) != 1 || sender.calls[0].chatID != "oc_chat" || sender.calls[0].text != "authorization complete" {
		t.Fatalf("resume send calls = %#v", sender.calls)
	}
	if sender.calls[0].uuid != workflowResumeMessageUUID(continuation.RequestID, 0) {
		t.Fatalf("resume message uuid = %q", sender.calls[0].uuid)
	}
}

func TestWorkflowContinuationWorkerRetriesDeliveryWithStableUUID(t *testing.T) {
	st := openFeishuApprovalTestStore(t)
	now := time.Date(2026, time.August, 3, 14, 15, 0, 0, time.UTC)
	continuation := createReadyWorkflowForWorker(t, st, now)
	resumer := &fakeWorkflowResumer{text: "stable delivery"}
	sender := &fakeWorkflowResumeTextSender{errs: []error{errors.New("temporary send failure"), nil}}
	worker, err := newWorkflowContinuationWorker(st, resumer, sender, store.Account{ID: continuation.AccountID}, nil)
	if err != nil {
		t.Fatalf("newWorkflowContinuationWorker returned error: %v", err)
	}
	current := now.Add(3 * time.Second)
	worker.now = func() time.Time { return current }
	worker.retryDelays = []time.Duration{time.Second}

	worker.processAvailable(t.Context())
	retrying, err := st.GetWorkflowContinuation(continuation.RequestID, continuation.AccountID)
	if err != nil || retrying.State != store.WorkflowContinuationStateReady || retrying.Attempts != 1 || !retrying.AvailableAt.Equal(current.Add(time.Second)) {
		t.Fatalf("retrying continuation = %#v err=%v", retrying, err)
	}
	current = current.Add(time.Second)
	worker.processAvailable(t.Context())
	delivered, err := st.GetWorkflowContinuation(continuation.RequestID, continuation.AccountID)
	if err != nil || delivered.State != store.WorkflowContinuationStateDelivered || delivered.Attempts != 2 {
		t.Fatalf("retried continuation = %#v err=%v", delivered, err)
	}
	if len(sender.calls) != 2 || sender.calls[0].uuid == "" || sender.calls[0].uuid != sender.calls[1].uuid {
		t.Fatalf("retry send calls = %#v", sender.calls)
	}
}

func TestWorkflowContinuationWorkerCancelsArchivedSessionWithoutRetry(t *testing.T) {
	st := openFeishuApprovalTestStore(t)
	now := time.Date(2026, time.August, 3, 14, 30, 0, 0, time.UTC)
	continuation := createReadyWorkflowForWorker(t, st, now)
	resumer := &fakeWorkflowResumer{errs: []error{fmt.Errorf("load session: %w", store.ErrSessionNotFound)}}
	worker, err := newWorkflowContinuationWorker(st, resumer, &fakeWorkflowResumeTextSender{}, store.Account{ID: continuation.AccountID}, nil)
	if err != nil {
		t.Fatalf("newWorkflowContinuationWorker returned error: %v", err)
	}
	worker.now = func() time.Time { return now.Add(3 * time.Second) }

	worker.processAvailable(t.Context())
	canceled, err := st.GetWorkflowContinuation(continuation.RequestID, continuation.AccountID)
	if err != nil || canceled.State != store.WorkflowContinuationStateCanceled || canceled.Attempts != 1 || canceled.LastError == "" {
		t.Fatalf("canceled continuation = %#v err=%v", canceled, err)
	}
}

func createReadyWorkflowForWorker(t *testing.T, st *store.Store, now time.Time) store.WorkflowContinuation {
	t.Helper()
	request, err := st.CreateWorkflowRequest(store.WorkflowRequest{
		AccountID: "feishu:cli_test",
		Kind:      store.WorkflowRequestKindFeishuResourceAccess,
		State:     store.WorkflowRequestStatePending,
		CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("CreateWorkflowRequest returned error: %v", err)
	}
	continuation, err := st.CreateWorkflowContinuation(store.WorkflowContinuation{
		RequestID:       request.ID,
		AccountID:       request.AccountID,
		Platform:        store.PlatformFeishu,
		UserKey:         "feishu:ou_requester",
		SessionID:       "session-work",
		ChatID:          "oc_chat",
		ChatIsGroup:     true,
		SourceMessageID: "om_source",
		ActorOpenID:     "ou_requester",
		ActorUserID:     "u_requester",
		OriginRevision:  6,
		OriginTurnID:    "turn-origin",
		ToolCallID:      "call-origin",
		ToolName:        "feishu_docs_request_access",
		CreatedAt:       now,
	})
	if err != nil {
		t.Fatalf("CreateWorkflowContinuation returned error: %v", err)
	}
	if _, _, err := st.CommitWorkflowContinuation(request.ID, request.AccountID, 7, now.Add(time.Second)); err != nil {
		t.Fatalf("CommitWorkflowContinuation returned error: %v", err)
	}
	_, continuation, ready, err := st.StoreWorkflowResult(store.WorkflowResult{
		RequestID: request.ID,
		AccountID: request.AccountID,
		State:     store.WorkflowResultStateSucceeded,
		Payload:   json.RawMessage(`{"status":"granted"}`),
		CreatedAt: now.Add(2 * time.Second),
	})
	if err != nil || !ready {
		t.Fatalf("StoreWorkflowResult continuation=%#v ready=%t err=%v", continuation, ready, err)
	}
	return continuation
}

var _ core.WorkflowResumer = (*fakeWorkflowResumer)(nil)
var _ workflowResumeTextSender = (*fakeWorkflowResumeTextSender)(nil)
