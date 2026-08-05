package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lingobridge/internal/store"
)

type fakeCardDeliveryUpdater struct {
	mu      sync.Mutex
	calls   []updatedText
	failFor int
}

type blockingCardDeliveryUpdater struct {
	mu      sync.Mutex
	calls   []updatedText
	started chan struct{}
	release chan struct{}
}

func (f *blockingCardDeliveryUpdater) UpdateByMessageID(ctx context.Context, messageID string, card Card) error {
	cardJSON, err := card.JSON()
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.calls = append(f.calls, updatedText{messageID: messageID, text: cardJSON})
	callCount := len(f.calls)
	f.mu.Unlock()
	if callCount != 1 {
		return nil
	}
	close(f.started)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-f.release:
		return nil
	}
}

func (f *blockingCardDeliveryUpdater) snapshot() []updatedText {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]updatedText(nil), f.calls...)
}

func (f *fakeCardDeliveryUpdater) UpdateByMessageID(ctx context.Context, messageID string, card Card) error {
	cardJSON, err := card.JSON()
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, updatedText{messageID: messageID, text: cardJSON})
	if len(f.calls) <= f.failFor {
		return errors.New("temporary card update failure")
	}
	return nil
}

func (f *fakeCardDeliveryUpdater) snapshot() []updatedText {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]updatedText(nil), f.calls...)
}

func TestFeishuCardDeliveryWorkerRetriesAfterRestart(t *testing.T) {
	st := openFeishuApprovalTestStore(t)
	now := time.Date(2026, time.August, 4, 14, 0, 0, 0, time.UTC)
	approval, err := st.CreateToolApproval(store.ToolApproval{
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
	continuation, err := st.CreateWorkflowContinuation(store.WorkflowContinuation{
		RequestID:       approval.ID,
		AccountID:       approval.AccountID,
		Platform:        store.PlatformFeishu,
		UserKey:         "feishu:ou_requester",
		SessionID:       "session_current",
		ChatID:          approval.ChatID,
		SourceMessageID: approval.SourceMessageID,
		ActorOpenID:     approval.ActorOpenID,
		OriginRevision:  1,
		OriginTurnID:    "turn_origin",
		ToolCallID:      "call_create",
		ToolName:        approval.ToolName,
		CreatedAt:       now,
	})
	if err != nil {
		t.Fatalf("CreateWorkflowContinuation returned error: %v", err)
	}
	if _, _, err := st.CommitWorkflowContinuation(continuation.RequestID, continuation.AccountID, 2, now.Add(time.Second)); err != nil {
		t.Fatalf("CommitWorkflowContinuation returned error: %v", err)
	}
	approval, err = st.DecideToolApproval(approval.ID, approval.AccountID, store.ToolApprovalDecisionApprove, store.ToolApprovalMatch{
		ActorOpenID: "ou_requester", ChatID: "oc_chat", CardMessageID: "om_card",
	}, now.Add(time.Second))
	if err != nil {
		t.Fatalf("DecideToolApproval returned error: %v", err)
	}
	if _, _, _, err := st.CompleteToolApprovalWithResult(
		approval.ID,
		approval.AccountID,
		store.ToolApprovalStateSucceeded,
		store.WorkflowResult{
			RequestID: approval.ID,
			AccountID: approval.AccountID,
			State:     store.WorkflowResultStateSucceeded,
			Payload:   json.RawMessage(`{"message":"文档已创建","warning":false}`),
		},
		now.Add(2*time.Second),
	); err != nil {
		t.Fatalf("CompleteToolApprovalWithResult returned error: %v", err)
	}
	updater := &fakeCardDeliveryUpdater{failFor: 1}
	worker, err := newFeishuCardDeliveryWorker(st, updater, nil, store.Account{ID: approval.AccountID})
	if err != nil {
		t.Fatalf("newFeishuCardDeliveryWorker returned error: %v", err)
	}
	worker.now = func() time.Time { return now.Add(3 * time.Second) }
	worker.retryDelays = []time.Duration{time.Second}
	worker.processAvailable(t.Context())
	delivery, err := st.GetFeishuCardDeliveryByKey(
		approval.AccountID,
		approval.ID,
		store.FeishuCardDeliveryPurposeToolApprovalTerminal,
		store.FeishuCardDeliveryRevisionTerminal,
	)
	if err != nil || delivery.State != store.FeishuCardDeliveryStatePending || delivery.Attempts != 1 {
		t.Fatalf("delivery after first worker = %#v err=%v", delivery, err)
	}

	restarted, err := newFeishuCardDeliveryWorker(st, updater, nil, store.Account{ID: approval.AccountID})
	if err != nil {
		t.Fatalf("new restarted worker: %v", err)
	}
	restarted.now = func() time.Time { return now.Add(5 * time.Second) }
	restarted.processAvailable(t.Context())
	delivery, err = st.GetFeishuCardDelivery(delivery.ID, delivery.AccountID)
	if err != nil || delivery.State != store.FeishuCardDeliveryStateDelivered || delivery.Attempts != 2 {
		t.Fatalf("delivery after restarted worker = %#v err=%v", delivery, err)
	}
	calls := updater.snapshot()
	if len(calls) != 2 || calls[0].messageID != "om_card" || !strings.Contains(calls[1].text, "执行完成") {
		t.Fatalf("card delivery calls = %#v", calls)
	}
}

func TestFeishuCardDeliveryWorkerWaitsForToolApprovalWorkflowResult(t *testing.T) {
	st := openFeishuApprovalTestStore(t)
	now := time.Date(2026, time.August, 5, 13, 24, 0, 0, time.UTC)
	approval, err := st.CreateToolApproval(store.ToolApproval{
		AccountID:       "feishu:cli_test",
		ToolName:        "feishu_docs_append",
		ActionKey:       "append",
		ResourceType:    "docx",
		ResourceToken:   "doxcn_manual",
		ActorOpenID:     "ou_requester",
		ChatID:          "oc_chat",
		SourceMessageID: "om_source",
		Payload:         `{"content":"summary"}`,
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
		SessionID:       "session_current",
		ChatID:          approval.ChatID,
		SourceMessageID: approval.SourceMessageID,
		ActorOpenID:     approval.ActorOpenID,
		OriginRevision:  1,
		OriginTurnID:    "turn_origin",
		ToolCallID:      "call_append",
		ToolName:        approval.ToolName,
		CreatedAt:       now,
	})
	if err != nil {
		t.Fatalf("CreateWorkflowContinuation returned error: %v", err)
	}
	if _, _, err := st.CommitWorkflowContinuation(continuation.RequestID, continuation.AccountID, 2, now.Add(time.Second)); err != nil {
		t.Fatalf("CommitWorkflowContinuation returned error: %v", err)
	}
	approval, err = st.DecideToolApproval(approval.ID, approval.AccountID, store.ToolApprovalDecisionApprove, store.ToolApprovalMatch{
		ActorOpenID: approval.ActorOpenID, ChatID: approval.ChatID, CardMessageID: "om_card",
	}, now.Add(2*time.Second))
	if err != nil {
		t.Fatalf("DecideToolApproval returned error: %v", err)
	}
	if err := st.CompleteToolApproval(approval.ID, approval.AccountID, store.ToolApprovalStateSucceeded, now.Add(3*time.Second)); err != nil {
		t.Fatalf("CompleteToolApproval returned error: %v", err)
	}

	updater := &fakeCardDeliveryUpdater{}
	worker, err := newFeishuCardDeliveryWorker(st, updater, nil, store.Account{ID: approval.AccountID})
	if err != nil {
		t.Fatalf("newFeishuCardDeliveryWorker returned error: %v", err)
	}
	workerNow := now.Add(4 * time.Second)
	worker.now = func() time.Time { return workerNow }
	worker.retryDelays = []time.Duration{time.Second}
	worker.processAvailable(t.Context())
	if calls := updater.snapshot(); len(calls) != 0 {
		t.Fatalf("card worker published terminal fallback before workflow result = %#v", calls)
	}
	delivery, err := st.GetFeishuCardDeliveryByKey(
		approval.AccountID,
		approval.ID,
		store.FeishuCardDeliveryPurposeToolApprovalTerminal,
		store.FeishuCardDeliveryRevisionTerminal,
	)
	if err != nil || delivery.State != store.FeishuCardDeliveryStatePending || delivery.Attempts != 1 {
		t.Fatalf("delivery while result pending = %#v err=%v", delivery, err)
	}

	if _, _, _, err := st.StoreWorkflowResult(store.WorkflowResult{
		RequestID: approval.ID,
		AccountID: approval.AccountID,
		State:     store.WorkflowResultStateSucceeded,
		Payload: json.RawMessage(`{
			"status":"succeeded",
			"tool_name":"feishu_docs_append",
			"action_key":"append",
			"resource_type":"docx",
			"resource_token":"doxcn_manual",
			"message":"✅ 已追加飞书文档内容",
			"warning":false,
			"warning_reason":""
		}`),
		CreatedAt: now.Add(5 * time.Second),
	}); err != nil {
		t.Fatalf("StoreWorkflowResult returned error: %v", err)
	}
	workerNow = now.Add(6 * time.Second)
	worker.processAvailable(t.Context())
	calls := updater.snapshot()
	if len(calls) != 1 || !strings.Contains(calls[0].text, "已追加飞书文档内容") || strings.Contains(calls[0].text, "服务中断") {
		t.Fatalf("card worker result update = %#v", calls)
	}
	delivery, err = st.GetFeishuCardDelivery(delivery.ID, delivery.AccountID)
	if err != nil || delivery.State != store.FeishuCardDeliveryStateDelivered {
		t.Fatalf("delivery after workflow result = %#v err=%v", delivery, err)
	}
}

func TestFeishuCardDeliveryWorkerAppliesNewRevisionAfterClaimedOlderUpdate(t *testing.T) {
	st := openFeishuApprovalTestStore(t)
	base := time.Date(2026, time.August, 4, 15, 0, 0, 0, time.UTC)
	older, err := st.EnqueueFeishuCardDelivery(store.FeishuCardDelivery{
		AccountID:     "feishu:cli_test",
		RequestID:     "req_ordered_worker_card",
		Purpose:       store.FeishuCardDeliveryPurposeWorkflowUnavailable,
		Revision:      store.FeishuCardDeliveryRevisionOAuthHandoff,
		CardMessageID: "om_card",
		CreatedAt:     base,
		ExpiresAt:     base.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("enqueue older delivery: %v", err)
	}
	updater := &blockingCardDeliveryUpdater{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	worker, err := newFeishuCardDeliveryWorker(st, updater, nil, store.Account{ID: older.AccountID})
	if err != nil {
		t.Fatalf("new delivery worker: %v", err)
	}
	var currentMillis atomic.Int64
	currentMillis.Store(base.UnixMilli())
	worker.now = func() time.Time { return time.UnixMilli(currentMillis.Load()).UTC() }
	done := make(chan struct{})
	go func() {
		defer close(done)
		worker.processAvailable(t.Context())
	}()
	select {
	case <-updater.started:
	case <-time.After(5 * time.Second):
		t.Fatal("older card update did not start")
	}
	currentMillis.Store(base.Add(time.Second).UnixMilli())
	newer, err := st.EnqueueFeishuCardDelivery(store.FeishuCardDelivery{
		AccountID:     older.AccountID,
		RequestID:     older.RequestID,
		Purpose:       store.FeishuCardDeliveryPurposeWorkflowExhausted,
		Revision:      store.FeishuCardDeliveryRevisionTerminal,
		CardMessageID: older.CardMessageID,
		CreatedAt:     base.Add(time.Second),
		ExpiresAt:     base.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("enqueue newer delivery: %v", err)
	}
	close(updater.release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("older card delivery did not finish")
	}
	storedOlder, err := st.GetFeishuCardDelivery(older.ID, older.AccountID)
	if err != nil || storedOlder.State != store.FeishuCardDeliveryStateSuperseded {
		t.Fatalf("older delivery after in-flight update = %#v err=%v", storedOlder, err)
	}
	currentMillis.Store(base.Add(2 * time.Second).UnixMilli())
	worker.processAvailable(t.Context())
	storedNewer, err := st.GetFeishuCardDelivery(newer.ID, newer.AccountID)
	if err != nil || storedNewer.State != store.FeishuCardDeliveryStateDelivered {
		t.Fatalf("newer delivery = %#v err=%v", storedNewer, err)
	}
	calls := updater.snapshot()
	if len(calls) != 2 {
		t.Fatalf("card update calls = %#v, want two ordered revisions", calls)
	}
	if !strings.Contains(calls[0].text, "原会话已不可用") || !strings.Contains(calls[1].text, "原任务未自动继续") {
		t.Fatalf("card update order = %#v", calls)
	}
}
