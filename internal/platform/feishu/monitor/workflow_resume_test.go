package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lingobridge/internal/core"
	feishutools "lingobridge/internal/platform/feishu/tools"
	"lingobridge/internal/store"
)

type fakeWorkflowResumer struct {
	requests []core.WorkflowResumeRequest
	errs     []error
	text     string
	resume   func(context.Context, core.WorkflowResumeRequest, core.Sender) error
}

func (f *fakeWorkflowResumer) ResumeWorkflow(ctx context.Context, request core.WorkflowResumeRequest, sender core.Sender) error {
	f.requests = append(f.requests, request)
	if f.resume != nil {
		return f.resume(ctx, request, sender)
	}
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

type workflowResumeCardUpdateCall struct {
	messageID string
	cardJSON  string
}

type fakeWorkflowResumeCardUpdater struct {
	calls  []workflowResumeCardUpdateCall
	err    error
	update func(context.Context, string, Card) error
}

func (f *fakeWorkflowResumeCardUpdater) UpdateByMessageID(ctx context.Context, messageID string, card Card) error {
	cardJSON, err := card.JSON()
	if err != nil {
		return err
	}
	f.calls = append(f.calls, workflowResumeCardUpdateCall{messageID: messageID, cardJSON: cardJSON})
	if f.update != nil {
		return f.update(ctx, messageID, card)
	}
	return f.err
}

func TestWorkflowContinuationWorkerDeliversReadyResult(t *testing.T) {
	st := openFeishuApprovalTestStore(t)
	now := time.Date(2026, time.August, 3, 14, 0, 0, 0, time.UTC)
	continuation := createReadyWorkflowForWorker(t, st, now)
	resumer := &fakeWorkflowResumer{text: "authorization complete"}
	sender := &fakeWorkflowResumeTextSender{}
	worker, err := newWorkflowContinuationWorker(st, resumer, sender, &fakeWorkflowResumeCardUpdater{}, store.Account{ID: continuation.AccountID, Name: "fsbot"}, nil)
	if err != nil {
		t.Fatalf("newWorkflowContinuationWorker returned error: %v", err)
	}
	worker.now = func() time.Time { return now.Add(3 * time.Second) }

	worker.processAvailable(t.Context())
	stored, err := st.GetWorkflowContinuation(continuation.RequestID, continuation.AccountID)
	if err != nil || stored.State != store.WorkflowContinuationStateDelivered || stored.Attempts != 1 {
		t.Fatalf("delivered continuation = %#v err=%v", stored, err)
	}
	if len(resumer.requests) != 1 || !resumer.requests[0].Continuation.ChatIsGroup || resumer.requests[0].AccountName != "fsbot" {
		t.Fatalf("resume requests = %#v", resumer.requests)
	}
	if len(sender.calls) != 1 || sender.calls[0].chatID != "oc_chat" || sender.calls[0].text != "authorization complete" {
		t.Fatalf("resume send calls = %#v", sender.calls)
	}
	if sender.calls[0].uuid != workflowResumeMessageUUID(continuation.RequestID, 0) {
		t.Fatalf("resume message uuid = %q", sender.calls[0].uuid)
	}
}

func TestWorkflowContinuationWorkerRecoversOriginCommitAfterConversationSave(t *testing.T) {
	st := openFeishuApprovalTestStore(t)
	now := time.Date(2026, time.August, 3, 14, 5, 0, 0, time.UTC)
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
		SourceMessageID: "om_source",
		ActorOpenID:     "ou_requester",
		OriginRevision:  0,
		OriginTurnID:    "turn-origin",
		ToolCallID:      "call-origin",
		ToolName:        "feishu_docs_request_access",
		CreatedAt:       now,
	})
	if err != nil {
		t.Fatalf("CreateWorkflowContinuation returned error: %v", err)
	}
	conversation := &store.Conversation{Messages: []store.Message{
		{Role: "user", Content: "request access"},
		{
			Role:    "assistant",
			Content: "authorization pending",
			ToolTraces: []store.ToolTrace{{
				CallID:            continuation.ToolCallID,
				Name:              continuation.ToolName,
				Status:            "ok",
				PendingWorkflowID: continuation.RequestID,
			}},
		},
	}}
	if revision, saveErr := st.SaveConversationCAS(continuation.UserKey, continuation.SessionID, 0, conversation); saveErr != nil || revision != 1 {
		t.Fatalf("SaveConversationCAS revision=%d err=%v, want saved origin turn at revision 1", revision, saveErr)
	}
	resumer := &fakeWorkflowResumer{text: "authorization complete"}
	sender := &fakeWorkflowResumeTextSender{}
	worker, err := newWorkflowContinuationWorker(st, resumer, sender, &fakeWorkflowResumeCardUpdater{}, store.Account{ID: continuation.AccountID}, nil)
	if err != nil {
		t.Fatalf("newWorkflowContinuationWorker returned error: %v", err)
	}
	current := now.Add(time.Second)
	worker.now = func() time.Time { return current }
	worker.processAvailable(t.Context())
	committed, err := st.GetWorkflowContinuation(continuation.RequestID, continuation.AccountID)
	if err != nil || committed.State != store.WorkflowContinuationStateWaiting || committed.CommittedRevision != 1 {
		t.Fatalf("recovered origin before terminal result = %#v err=%v, want waiting revision 1", committed, err)
	}
	if len(resumer.requests) != 0 || len(sender.calls) != 0 {
		t.Fatalf("resume requests/sends before terminal result = %d/%d, want none", len(resumer.requests), len(sender.calls))
	}

	_, waiting, ready, err := st.StoreWorkflowResult(store.WorkflowResult{
		RequestID: continuation.RequestID,
		AccountID: continuation.AccountID,
		State:     store.WorkflowResultStateSucceeded,
		Payload:   json.RawMessage(`{"status":"granted"}`),
		CreatedAt: now.Add(2 * time.Second),
	})
	if err != nil || !ready || waiting.State != store.WorkflowContinuationStateReady || waiting.CommittedRevision != 1 {
		t.Fatalf("committed continuation after result = %#v ready=%t err=%v", waiting, ready, err)
	}

	current = now.Add(3 * time.Second)
	worker.processAvailable(t.Context())

	recovered, err := st.GetWorkflowContinuation(continuation.RequestID, continuation.AccountID)
	if err != nil || recovered.State != store.WorkflowContinuationStateDelivered || recovered.CommittedRevision != 1 {
		t.Fatalf("recovered continuation = %#v err=%v, want delivered revision 1", recovered, err)
	}
	if len(resumer.requests) != 1 || len(sender.calls) != 1 {
		t.Fatalf("resume requests/sends = %d/%d, want one recovered delivery", len(resumer.requests), len(sender.calls))
	}
}

func TestWorkflowContinuationWorkerRecoversOriginCommitAfterTraceCompaction(t *testing.T) {
	st := openFeishuApprovalTestStore(t)
	now := time.Date(2026, time.August, 4, 20, 0, 0, 0, time.UTC)
	request, err := st.CreateWorkflowRequest(store.WorkflowRequest{
		ID: "req_origin_compacted", AccountID: "feishu:cli_test",
		Kind: store.WorkflowRequestKindFeishuResourceAccess, State: store.WorkflowRequestStatePending, CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("CreateWorkflowRequest returned error: %v", err)
	}
	continuation, err := st.CreateWorkflowContinuation(store.WorkflowContinuation{
		RequestID: request.ID, AccountID: request.AccountID, Platform: store.PlatformFeishu,
		UserKey: "feishu:ou_requester", SessionID: "session-compacted", ChatID: "oc_chat",
		SourceMessageID: "om_source", ActorOpenID: "ou_requester", OriginRevision: 0,
		OriginTurnID: "turn-origin", ToolCallID: "call-origin", ToolName: "feishu_docs_request_access", CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("CreateWorkflowContinuation returned error: %v", err)
	}
	conversation := &store.Conversation{
		Messages: []store.Message{{Role: "user", Content: "later compacted history"}, {Role: "assistant", Content: "later response"}},
		WorkflowOriginReceipts: map[string]store.WorkflowOriginReceipt{
			continuation.RequestID: {
				ToolCallID: continuation.ToolCallID, ToolName: continuation.ToolName, CommittedRevision: 1,
			},
		},
	}
	if revision, saveErr := st.SaveConversationCAS(continuation.UserKey, continuation.SessionID, 0, conversation); saveErr != nil || revision != 1 {
		t.Fatalf("SaveConversationCAS revision=%d err=%v", revision, saveErr)
	}
	worker, err := newWorkflowContinuationWorker(
		st, &fakeWorkflowResumer{}, &fakeWorkflowResumeTextSender{}, &fakeWorkflowResumeCardUpdater{},
		store.Account{ID: continuation.AccountID}, nil,
	)
	if err != nil {
		t.Fatalf("newWorkflowContinuationWorker returned error: %v", err)
	}
	worker.now = func() time.Time { return now.Add(time.Second) }
	worker.processAvailable(t.Context())

	recovered, err := st.GetWorkflowContinuation(continuation.RequestID, continuation.AccountID)
	if err != nil || recovered.CommittedRevision != 1 || recovered.State != store.WorkflowContinuationStateWaiting {
		t.Fatalf("compacted origin recovery = %#v err=%v, want committed waiting continuation", recovered, err)
	}
}

func TestWorkflowContinuationWorkerOriginRecoveryDoesNotStarveAfterFirstBatch(t *testing.T) {
	st := openFeishuApprovalTestStore(t)
	now := time.Date(2026, time.August, 4, 20, 15, 0, 0, time.UTC)
	const total = defaultWorkflowOriginRecoverySize + 1
	var last store.WorkflowContinuation
	for index := 0; index < total; index++ {
		requestID := fmt.Sprintf("req_origin_batch_%03d", index)
		request, err := st.CreateWorkflowRequest(store.WorkflowRequest{
			ID: requestID, AccountID: "feishu:cli_test", Kind: store.WorkflowRequestKindFeishuResourceAccess,
			State: store.WorkflowRequestStatePending, CreatedAt: now.Add(time.Duration(index) * time.Millisecond),
		})
		if err != nil {
			t.Fatalf("CreateWorkflowRequest(%d) returned error: %v", index, err)
		}
		continuation, err := st.CreateWorkflowContinuation(store.WorkflowContinuation{
			RequestID: request.ID, AccountID: request.AccountID, Platform: store.PlatformFeishu,
			UserKey: fmt.Sprintf("feishu:user_%03d", index), SessionID: fmt.Sprintf("session-%03d", index), ChatID: "oc_chat",
			SourceMessageID: "om_source", ActorOpenID: "ou_requester", OriginRevision: 0,
			OriginTurnID: "turn-origin", ToolCallID: "call-origin", ToolName: "feishu_docs_request_access",
			CreatedAt: now.Add(time.Duration(index) * time.Millisecond),
		})
		if err != nil {
			t.Fatalf("CreateWorkflowContinuation(%d) returned error: %v", index, err)
		}
		last = continuation
	}
	conversation := &store.Conversation{Messages: []store.Message{
		{Role: "user", Content: "request access"},
		{Role: "assistant", Content: "authorization pending", ToolTraces: []store.ToolTrace{{
			CallID: last.ToolCallID, Name: last.ToolName, Status: "ok", PendingWorkflowID: last.RequestID,
		}}},
	}}
	if revision, saveErr := st.SaveConversationCAS(last.UserKey, last.SessionID, 0, conversation); saveErr != nil || revision != 1 {
		t.Fatalf("SaveConversationCAS last origin revision=%d err=%v", revision, saveErr)
	}
	worker, err := newWorkflowContinuationWorker(
		st, &fakeWorkflowResumer{}, &fakeWorkflowResumeTextSender{}, &fakeWorkflowResumeCardUpdater{},
		store.Account{ID: last.AccountID}, nil,
	)
	if err != nil {
		t.Fatalf("newWorkflowContinuationWorker returned error: %v", err)
	}
	worker.now = func() time.Time { return now.Add(time.Second) }
	worker.processAvailable(t.Context())
	worker.processAvailable(t.Context())

	recovered, err := st.GetWorkflowContinuation(last.RequestID, last.AccountID)
	if err != nil || recovered.CommittedRevision != 1 {
		t.Fatalf("continuation after two origin-recovery batches = %#v err=%v, want the record after the first batch to make progress", recovered, err)
	}
}

func TestWorkflowContinuationWorkerRetriesDeliveryWithStableUUID(t *testing.T) {
	st := openFeishuApprovalTestStore(t)
	now := time.Date(2026, time.August, 3, 14, 15, 0, 0, time.UTC)
	continuation := createReadyWorkflowForWorker(t, st, now)
	resumer := &fakeWorkflowResumer{text: "stable delivery"}
	sender := &fakeWorkflowResumeTextSender{errs: []error{errors.New("temporary send failure"), nil}}
	worker, err := newWorkflowContinuationWorker(st, resumer, sender, &fakeWorkflowResumeCardUpdater{}, store.Account{ID: continuation.AccountID}, nil)
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

func TestWorkflowResumeMessageUUIDPreservesPersistedDeliveryProtocol(t *testing.T) {
	const want = "5a861760-2e92-2c8d-d448-b055c0a1b5de"
	if got := workflowResumeMessageUUID("req_compat", 0); got != want {
		t.Fatalf("workflowResumeMessageUUID = %q, want legacy-compatible %q", got, want)
	}
}

func TestWorkflowContinuationWorkerUpdatesOriginalCardAfterFinalFailure(t *testing.T) {
	st := openFeishuApprovalTestStore(t)
	now := time.Date(2026, time.August, 3, 14, 25, 0, 0, time.UTC)
	continuation := createReadyApprovalWorkflowForWorker(t, st, now)
	resumer := &fakeWorkflowResumer{errs: []error{errors.New("model unavailable")}}
	sender := &fakeWorkflowResumeTextSender{}
	cards := &fakeWorkflowResumeCardUpdater{}
	worker, err := newWorkflowContinuationWorker(st, resumer, sender, cards, store.Account{ID: continuation.AccountID}, nil)
	if err != nil {
		t.Fatalf("newWorkflowContinuationWorker returned error: %v", err)
	}
	worker.now = func() time.Time { return now.Add(3 * time.Second) }
	worker.maxAttempts = 1

	worker.processAvailable(t.Context())
	worker.processAvailable(t.Context())
	failed, err := st.GetWorkflowContinuation(continuation.RequestID, continuation.AccountID)
	if err != nil || failed.State != store.WorkflowContinuationStateFailed || failed.Attempts != 1 {
		t.Fatalf("failed continuation = %#v err=%v", failed, err)
	}
	if len(resumer.requests) != 1 || len(sender.calls) != 0 {
		t.Fatalf("resume requests/text sends = %d/%d, want 1/0", len(resumer.requests), len(sender.calls))
	}
	if len(cards.calls) != 1 || cards.calls[0].messageID != "om_card" {
		t.Fatalf("card updates = %#v, want one update of original card", cards.calls)
	}
	if !strings.Contains(cards.calls[0].cardJSON, "授权或操作结果已保存") ||
		!strings.Contains(cards.calls[0].cardJSON, "未能自动继续原任务") ||
		!strings.Contains(cards.calls[0].cardJSON, "已追加飞书文档内容") ||
		strings.Contains(cards.calls[0].cardJSON, "授权失败") {
		t.Fatalf("terminal failure card = %s", cards.calls[0].cardJSON)
	}
}

func TestWorkflowContinuationWorkerKeepsFinalStateWhenCardUpdateFails(t *testing.T) {
	st := openFeishuApprovalTestStore(t)
	now := time.Date(2026, time.August, 3, 14, 27, 0, 0, time.UTC)
	continuation := createReadyApprovalWorkflowForWorker(t, st, now)
	resumer := &fakeWorkflowResumer{errs: []error{errors.New("model unavailable")}}
	cards := &fakeWorkflowResumeCardUpdater{err: errors.New("card update unavailable")}
	worker, err := newWorkflowContinuationWorker(st, resumer, &fakeWorkflowResumeTextSender{}, cards, store.Account{ID: continuation.AccountID}, nil)
	if err != nil {
		t.Fatalf("newWorkflowContinuationWorker returned error: %v", err)
	}
	worker.now = func() time.Time { return now.Add(3 * time.Second) }
	worker.maxAttempts = 1

	worker.processAvailable(t.Context())
	worker.processAvailable(t.Context())
	failed, err := st.GetWorkflowContinuation(continuation.RequestID, continuation.AccountID)
	if err != nil || failed.State != store.WorkflowContinuationStateFailed || failed.Attempts != 1 {
		t.Fatalf("failed continuation after card error = %#v err=%v", failed, err)
	}
	if len(resumer.requests) != 1 || len(cards.calls) != 1 {
		t.Fatalf("resume/card calls = %d/%d, want one synchronous terminal-card attempt", len(resumer.requests), len(cards.calls))
	}
	delivery, err := st.GetFeishuCardDeliveryByKey(
		continuation.AccountID,
		continuation.RequestID,
		store.FeishuCardDeliveryPurposeWorkflowExhausted,
		store.FeishuCardDeliveryRevisionContinuation,
	)
	if err != nil || delivery.State != store.FeishuCardDeliveryStatePending {
		t.Fatalf("durable terminal card delivery = %#v err=%v", delivery, err)
	}
	cards.err = nil
	deliveryWorker, err := newFeishuCardDeliveryWorker(st, cards, nil, store.Account{ID: continuation.AccountID})
	if err != nil {
		t.Fatalf("newFeishuCardDeliveryWorker returned error: %v", err)
	}
	deliveryWorker.now = func() time.Time { return now.Add(4 * time.Second) }
	deliveryWorker.processAvailable(t.Context())
	delivery, err = st.GetFeishuCardDelivery(delivery.ID, delivery.AccountID)
	if err != nil || delivery.State != store.FeishuCardDeliveryStateDelivered || len(cards.calls) != 2 {
		t.Fatalf("retried terminal card delivery = %#v calls=%d err=%v", delivery, len(cards.calls), err)
	}
}

func TestWorkflowContinuationWorkerBoundsTerminalCardUpdate(t *testing.T) {
	st := openFeishuApprovalTestStore(t)
	now := time.Date(2026, time.August, 3, 14, 28, 0, 0, time.UTC)
	continuation := createReadyApprovalWorkflowForWorker(t, st, now)
	resumer := &fakeWorkflowResumer{errs: []error{core.ErrWorkflowResumeInvalid}}
	cards := &fakeWorkflowResumeCardUpdater{
		update: func(ctx context.Context, _ string, _ Card) error {
			<-ctx.Done()
			return ctx.Err()
		},
	}
	worker, err := newWorkflowContinuationWorker(st, resumer, &fakeWorkflowResumeTextSender{}, cards, store.Account{ID: continuation.AccountID}, nil)
	if err != nil {
		t.Fatalf("newWorkflowContinuationWorker returned error: %v", err)
	}
	worker.now = func() time.Time { return now.Add(3 * time.Second) }
	worker.cardUpdateTimeout = 10 * time.Millisecond

	started := time.Now()
	worker.processAvailable(t.Context())
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("terminal card update blocked workflow worker for %s", elapsed)
	}
	if len(cards.calls) != 1 {
		t.Fatalf("terminal card update calls = %#v, want one bounded attempt", cards.calls)
	}
}

func TestWorkflowContinuationWorkerRuntimeCancellationDoesNotExhaustContinuation(t *testing.T) {
	st := openFeishuApprovalTestStore(t)
	now := time.Date(2026, time.August, 3, 14, 29, 0, 0, time.UTC)
	continuation := createReadyApprovalWorkflowForWorker(t, st, now)
	resumer := &fakeWorkflowResumer{}
	cards := &fakeWorkflowResumeCardUpdater{}
	worker, err := newWorkflowContinuationWorker(st, resumer, &fakeWorkflowResumeTextSender{}, cards, store.Account{ID: continuation.AccountID}, nil)
	if err != nil {
		t.Fatalf("newWorkflowContinuationWorker returned error: %v", err)
	}
	worker.now = func() time.Time { return now.Add(3 * time.Second) }
	worker.maxAttempts = 2

	for shutdown := 0; shutdown < 3; shutdown++ {
		ctx, cancel := context.WithCancel(context.Background())
		resumer.resume = func(context.Context, core.WorkflowResumeRequest, core.Sender) error {
			cancel()
			return context.Canceled
		}
		worker.processAvailable(ctx)
		stored, err := st.GetWorkflowContinuation(continuation.RequestID, continuation.AccountID)
		if err != nil || stored.State != store.WorkflowContinuationStateReady || stored.Attempts != 0 {
			t.Fatalf("continuation after runtime cancellation %d = %#v err=%v, want retryable ready state without a consumed attempt", shutdown+1, stored, err)
		}
	}
	resumer.resume = func(context.Context, core.WorkflowResumeRequest, core.Sender) error {
		return errors.New("temporary model failure")
	}
	worker.processAvailable(context.Background())
	stored, err := st.GetWorkflowContinuation(continuation.RequestID, continuation.AccountID)
	if err != nil || stored.State != store.WorkflowContinuationStateReady || stored.Attempts != 1 {
		t.Fatalf("continuation after first real failure = %#v err=%v, want one retryable attempt", stored, err)
	}
	if len(cards.calls) != 0 {
		t.Fatalf("runtime cancellation terminal card updates = %#v, want none", cards.calls)
	}
}

func TestWorkflowContinuationWorkerCancelsArchivedSessionWithoutRetry(t *testing.T) {
	st := openFeishuApprovalTestStore(t)
	now := time.Date(2026, time.August, 3, 14, 30, 0, 0, time.UTC)
	continuation := createReadyApprovalWorkflowForWorker(t, st, now)
	resumer := &fakeWorkflowResumer{errs: []error{fmt.Errorf("load session: %w", store.ErrSessionNotFound)}}
	cards := &fakeWorkflowResumeCardUpdater{}
	worker, err := newWorkflowContinuationWorker(st, resumer, &fakeWorkflowResumeTextSender{}, cards, store.Account{ID: continuation.AccountID}, nil)
	if err != nil {
		t.Fatalf("newWorkflowContinuationWorker returned error: %v", err)
	}
	worker.now = func() time.Time { return now.Add(3 * time.Second) }

	worker.processAvailable(t.Context())
	canceled, err := st.GetWorkflowContinuation(continuation.RequestID, continuation.AccountID)
	if err != nil || canceled.State != store.WorkflowContinuationStateCanceled || canceled.Attempts != 1 || canceled.LastError == "" {
		t.Fatalf("canceled continuation = %#v err=%v", canceled, err)
	}
	if len(cards.calls) != 1 || cards.calls[0].messageID != "om_card" ||
		!strings.Contains(cards.calls[0].cardJSON, "原会话已不可用") ||
		!strings.Contains(cards.calls[0].cardJSON, "已追加飞书文档内容") ||
		!strings.Contains(cards.calls[0].cardJSON, "重新发送原请求") {
		t.Fatalf("canceled session card updates = %#v, want one actionable terminal update", cards.calls)
	}
}

func TestOperationApprovalDuplicateCallbackResumesContinuationOnce(t *testing.T) {
	st := openFeishuApprovalTestStore(t)
	cardSender := &fakeApprovalSender{}
	manager := newTestApprovalManager(t, st, cardSender)
	executor := &fakeApprovalExecutor{
		name:   "feishu_docs_create",
		result: feishutools.OperationApprovalExecution{Message: "document created"},
		done:   make(chan struct{}),
	}
	if err := manager.registerExecutor(executor); err != nil {
		t.Fatalf("registerExecutor returned error: %v", err)
	}
	pending := requestTestApproval(t, manager)
	if _, _, err := st.CommitWorkflowContinuation(pending.RequestID, "feishu:cli_test", 8, manager.currentTime()); err != nil {
		t.Fatalf("CommitWorkflowContinuation returned error: %v", err)
	}
	event := approvalCardEvent(pending.RequestID, approvalCardActionApproveOnce, "ou_requester", "oc_chat", "om_card", "")
	response, err := manager.HandleCardAction(t.Context(), event)
	if err != nil || response == nil || response.Toast == nil || response.Toast.Type != "success" {
		t.Fatalf("first approval callback response = %#v err=%v", response, err)
	}
	duplicate, err := manager.HandleCardAction(t.Context(), event)
	if err != nil || duplicate == nil || duplicate.Toast == nil || duplicate.Toast.Type != "info" {
		t.Fatalf("duplicate approval callback response = %#v err=%v", duplicate, err)
	}
	select {
	case <-executor.done:
	case <-time.After(time.Second):
		t.Fatal("approved executor was not called")
	}
	waitForApprovalState(t, st, pending.RequestID, store.ToolApprovalStateSucceeded)
	waitForApprovalWorkflowResult(t, st, pending.RequestID, store.WorkflowResultStateSucceeded)

	resumer := &fakeWorkflowResumer{text: "operation workflow resumed"}
	resumeSender := &fakeWorkflowResumeTextSender{}
	worker, err := newWorkflowContinuationWorker(st, resumer, resumeSender, &fakeWorkflowResumeCardUpdater{}, store.Account{ID: "feishu:cli_test"}, nil)
	if err != nil {
		t.Fatalf("newWorkflowContinuationWorker returned error: %v", err)
	}
	worker.now = func() time.Time { return manager.currentTime().Add(time.Second) }
	worker.processAvailable(t.Context())
	worker.processAvailable(t.Context())

	if calls, _ := executor.snapshot(); calls != 1 {
		t.Fatalf("executor calls = %d, want 1", calls)
	}
	if len(resumer.requests) != 1 || len(resumeSender.calls) != 1 || resumeSender.calls[0].text != "operation workflow resumed" {
		t.Fatalf("resume requests/sends = %#v/%#v, want one delivery", resumer.requests, resumeSender.calls)
	}
	continuation, err := st.GetWorkflowContinuation(pending.RequestID, "feishu:cli_test")
	if err != nil || continuation.State != store.WorkflowContinuationStateDelivered || continuation.Attempts != 1 {
		t.Fatalf("operation continuation = %#v err=%v", continuation, err)
	}
}

func TestResourceApprovalDuplicateCallbackResumesContinuationOnce(t *testing.T) {
	var authCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			writeResourceAccessJSON(t, w, tenantTokenResponseForResourceAccess())
		case "/open-apis/drive/v1/permissions/doxcn_resume/members/auth":
			authCalls++
			writeResourceAccessJSON(t, w, map[string]any{"code": 0, "msg": "Success", "data": map[string]any{"auth_result": true}})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	manager, st, cardSender := newTestResourceAccessManager(t, server, resourceAccessOAuthConfig{})
	now := manager.currentTime()
	if _, err := st.UpsertFeishuResourceCapability(store.FeishuResourceCapability{
		AccountID: "feishu:cli_test", ResourceType: "docx", ResourceToken: "doxcn_resume",
		SubjectType: "openid", SubjectID: "ou_bot", Permission: store.FeishuResourcePermissionWrite,
		SourceActorOpenID: "ou_requester", SourceRequestID: "req_capability",
		State: store.FeishuResourceCapabilityStateActive, CreatedAt: now, VerifiedAt: now,
	}); err != nil {
		t.Fatalf("UpsertFeishuResourceCapability returned error: %v", err)
	}
	result, err := manager.RequestAccess(resourceAccessRequestContext(), feishutools.ResourceAccessRequest{
		ResourceType: "docx", ResourceToken: "doxcn_resume", Permission: feishutools.ResourcePermissionRead,
		OnceDurationMinutes: 30, Reason: "resume original workflow",
	})
	if err != nil || result.Status != feishutools.ResourceAccessStatusPending {
		t.Fatalf("RequestAccess = %#v err=%v", result, err)
	}
	if _, _, err := st.CommitWorkflowContinuation(result.RequestID, "feishu:cli_test", 8, now); err != nil {
		t.Fatalf("CommitWorkflowContinuation returned error: %v", err)
	}
	event := resourceAccessCardEvent(result.RequestID, "ou_requester", "oc_chat", "om_card")
	event.Event.Action.Value["action"] = resourceAccessCardActionApproveOnce
	response, err := manager.HandleCardAction(t.Context(), event)
	if err != nil || response == nil || response.Toast == nil || response.Toast.Type != "success" {
		t.Fatalf("first resource callback response = %#v err=%v", response, err)
	}
	duplicate, err := manager.HandleCardAction(t.Context(), event)
	if err != nil || duplicate == nil || duplicate.Toast == nil || duplicate.Toast.Type != "info" {
		t.Fatalf("duplicate resource callback response = %#v err=%v", duplicate, err)
	}
	waitForResourceAccessCompletion(t, st, cardSender, result.RequestID)

	resumer := &fakeWorkflowResumer{text: "resource workflow resumed"}
	resumeSender := &fakeWorkflowResumeTextSender{}
	worker, err := newWorkflowContinuationWorker(st, resumer, resumeSender, &fakeWorkflowResumeCardUpdater{}, store.Account{ID: "feishu:cli_test"}, nil)
	if err != nil {
		t.Fatalf("newWorkflowContinuationWorker returned error: %v", err)
	}
	worker.now = func() time.Time { return now.Add(time.Second) }
	worker.processAvailable(t.Context())
	worker.processAvailable(t.Context())

	if authCalls != 1 {
		t.Fatalf("live permission checks = %d, want 1", authCalls)
	}
	if len(resumer.requests) != 1 || len(resumeSender.calls) != 1 || resumeSender.calls[0].text != "resource workflow resumed" {
		t.Fatalf("resume requests/sends = %#v/%#v, want one delivery", resumer.requests, resumeSender.calls)
	}
	continuation, err := st.GetWorkflowContinuation(result.RequestID, "feishu:cli_test")
	if err != nil || continuation.State != store.WorkflowContinuationStateDelivered || continuation.Attempts != 1 {
		t.Fatalf("resource continuation = %#v err=%v", continuation, err)
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

func createReadyApprovalWorkflowForWorker(t *testing.T, st *store.Store, now time.Time) store.WorkflowContinuation {
	t.Helper()
	approval, err := st.CreateToolApproval(store.ToolApproval{
		AccountID:       "feishu:cli_test",
		ToolName:        "feishu_docs_append",
		ActionKey:       "append",
		ResourceType:    "docx",
		ResourceToken:   "doxcn_resume_failure",
		ActorOpenID:     "ou_requester",
		ActorUserID:     "u_requester",
		ChatID:          "oc_chat",
		SourceMessageID: "om_source",
		Payload:         `{}`,
		CreatedAt:       now,
		ExpiresAt:       now.Add(10 * time.Minute),
	})
	if err != nil {
		t.Fatalf("CreateToolApproval returned error: %v", err)
	}
	if err := st.SetToolApprovalCardMessageID(approval.ID, approval.AccountID, "om_card", now.Add(time.Second)); err != nil {
		t.Fatalf("SetToolApprovalCardMessageID returned error: %v", err)
	}
	continuation, err := st.CreateWorkflowContinuation(store.WorkflowContinuation{
		RequestID:       approval.ID,
		AccountID:       approval.AccountID,
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
		ToolName:        "feishu_docs_append",
		CreatedAt:       now,
	})
	if err != nil {
		t.Fatalf("CreateWorkflowContinuation returned error: %v", err)
	}
	if _, _, err := st.CommitWorkflowContinuation(approval.ID, approval.AccountID, 7, now.Add(time.Second)); err != nil {
		t.Fatalf("CommitWorkflowContinuation returned error: %v", err)
	}
	_, continuation, ready, err := st.StoreWorkflowResult(store.WorkflowResult{
		RequestID: approval.ID,
		AccountID: approval.AccountID,
		State:     store.WorkflowResultStateSucceeded,
		Payload:   json.RawMessage(`{"status":"succeeded","message":"✅ 已追加飞书文档内容：[目标文档](https://docs.feishu.cn/docx/test)"}`),
		CreatedAt: now.Add(2 * time.Second),
	})
	if err != nil || !ready {
		t.Fatalf("StoreWorkflowResult continuation=%#v ready=%t err=%v", continuation, ready, err)
	}
	return continuation
}

var _ core.WorkflowResumer = (*fakeWorkflowResumer)(nil)
var _ workflowResumeTextSender = (*fakeWorkflowResumeTextSender)(nil)
var _ workflowResumeCardUpdater = (*fakeWorkflowResumeCardUpdater)(nil)
