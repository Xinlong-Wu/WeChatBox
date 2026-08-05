package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"

	feishutools "lingobridge/internal/platform/feishu/tools"
	"lingobridge/internal/store"
	tooltypes "lingobridge/internal/tools"
)

type fakeApprovalSender struct {
	mu                    sync.Mutex
	cards                 []sentText
	updates               []updatedText
	messages              []sentText
	createErr             error
	updateErr             error
	interactionUpdateErr  error
	updateCardFunc        func(context.Context, string, string) error
	interactionUpdateFunc func(context.Context, string, string) error
	sendErr               error
}

func (f *fakeApprovalSender) SendText(_ context.Context, chatID, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.messages = append(f.messages, sentText{chatID: chatID, text: text})
	return f.sendErr
}

func (f *fakeApprovalSender) CreateCard(_ context.Context, chatID, cardJSON string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cards = append(f.cards, sentText{chatID: chatID, text: cardJSON})
	if f.createErr != nil {
		return "", f.createErr
	}
	return "om_card", nil
}

func (f *fakeApprovalSender) UpdateCard(ctx context.Context, messageID, cardJSON string) error {
	f.mu.Lock()
	f.updates = append(f.updates, updatedText{messageID: messageID, text: cardJSON})
	update := f.updateCardFunc
	err := f.updateErr
	f.mu.Unlock()
	if update != nil {
		return update(ctx, messageID, cardJSON)
	}
	return err
}

func (f *fakeApprovalSender) UpdateCardAfterInteraction(ctx context.Context, callbackToken, cardJSON string) error {
	f.mu.Lock()
	f.updates = append(f.updates, updatedText{messageID: callbackToken, text: cardJSON})
	update := f.interactionUpdateFunc
	err := f.interactionUpdateErr
	f.mu.Unlock()
	if update != nil {
		return update(ctx, callbackToken, cardJSON)
	}
	return err
}

func (f *fakeApprovalSender) snapshot() (cards []sentText, updates []updatedText, messages []sentText) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sentText(nil), f.cards...), append([]updatedText(nil), f.updates...), append([]sentText(nil), f.messages...)
}

type fakeApprovalExecutor struct {
	mu         sync.Mutex
	name       string
	actionKey  string
	action     string
	disableAll bool
	recover    bool
	result     feishutools.OperationApprovalExecution
	err        error
	payload    json.RawMessage
	calls      int
	done       chan struct{}
}

type blockingApprovalExecutor struct {
	started     chan struct{}
	release     chan struct{}
	startedOnce sync.Once
	recover     bool
}

func (e *blockingApprovalExecutor) OperationApprovalPolicy() feishutools.OperationApprovalPolicy {
	return feishutools.OperationApprovalPolicy{
		ToolName:           "feishu_docs_create",
		ActionKey:          "create",
		Action:             "创建飞书文档",
		SupportsAll:        true,
		RecoverInterrupted: e.recover,
	}
}

func (e *blockingApprovalExecutor) ExecuteApproved(ctx context.Context, _ string, _ json.RawMessage) (feishutools.OperationApprovalExecution, error) {
	e.startedOnce.Do(func() { close(e.started) })
	select {
	case <-e.release:
		return feishutools.OperationApprovalExecution{Message: "文档已创建"}, nil
	case <-ctx.Done():
		return feishutools.OperationApprovalExecution{}, ctx.Err()
	}
}

type failingGrantStore struct {
	toolApprovalStore
	err error
}

func (f failingGrantStore) UpsertToolApprovalGrant(store.ToolApprovalGrant) (store.ToolApprovalGrant, error) {
	return store.ToolApprovalGrant{}, f.err
}

type failingCompleteToolApprovalStore struct {
	toolApprovalStore
	err error
}

func (f failingCompleteToolApprovalStore) CompleteToolApproval(_, _, _ string, _ time.Time) error {
	return f.err
}

type recordingToolApprovalStore struct {
	toolApprovalStore
	canceled []string
}

func (s *recordingToolApprovalStore) CancelWorkflowContinuation(requestID, accountID, reason string, now time.Time) error {
	s.canceled = append(s.canceled, requestID)
	return s.toolApprovalStore.CancelWorkflowContinuation(requestID, accountID, reason, now)
}

type observingToolApprovalResultStore struct {
	toolApprovalStore
	onStoreResult func()
}

type resolveExecutingApprovalDuringRecoveryListStore struct {
	toolApprovalStore
	now time.Time
}

func (s *resolveExecutingApprovalDuringRecoveryListStore) ListExecutingToolApprovals(accountID string) ([]store.ToolApproval, error) {
	approvals, err := s.toolApprovalStore.ListExecutingToolApprovals(accountID)
	if err != nil || len(approvals) == 0 {
		return approvals, err
	}
	if err := s.toolApprovalStore.CompleteToolApproval(approvals[0].ID, approvals[0].AccountID, store.ToolApprovalStateSucceeded, s.now); err != nil {
		return nil, err
	}
	return approvals, nil
}

func (s *observingToolApprovalResultStore) StoreWorkflowResult(result store.WorkflowResult) (store.WorkflowResult, store.WorkflowContinuation, bool, error) {
	if s.onStoreResult != nil {
		s.onStoreResult()
	}
	return s.toolApprovalStore.StoreWorkflowResult(result)
}

func (f *fakeApprovalExecutor) OperationApprovalPolicy() feishutools.OperationApprovalPolicy {
	actionKey := f.actionKey
	if actionKey == "" {
		actionKey = "create"
	}
	action := f.action
	if action == "" {
		action = "创建飞书文档"
	}
	return feishutools.OperationApprovalPolicy{
		ToolName:           f.name,
		ActionKey:          actionKey,
		Action:             action,
		SupportsAll:        !f.disableAll,
		RecoverInterrupted: f.recover,
	}
}

func (f *fakeApprovalExecutor) ExecuteApproved(_ context.Context, _ string, payload json.RawMessage) (feishutools.OperationApprovalExecution, error) {
	f.mu.Lock()
	f.calls++
	f.payload = append(json.RawMessage(nil), payload...)
	done := f.done
	result := f.result
	err := f.err
	f.mu.Unlock()
	if done != nil {
		select {
		case <-done:
		default:
			close(done)
		}
	}
	return result, err
}

func (f *fakeApprovalExecutor) snapshot() (int, json.RawMessage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, append(json.RawMessage(nil), f.payload...)
}

func TestApprovalManagerRequestsBoundCardWithoutDisplayingPayload(t *testing.T) {
	st := openFeishuApprovalTestStore(t)
	sender := &fakeApprovalSender{}
	manager := newTestApprovalManager(t, st, sender)
	executor := &fakeApprovalExecutor{name: "feishu_docs_create"}
	if err := manager.registerExecutor(executor); err != nil {
		t.Fatalf("registerExecutor returned error: %v", err)
	}

	payload := json.RawMessage(`{"title":"Quarterly plan","content":"private document body"}`)
	pending, err := manager.CheckOrRequest(approvalRequestContext(), feishutools.OperationApprovalRequest{
		ToolName:      "feishu_docs_create",
		ActionKey:     "create",
		ResourceType:  "folder",
		ResourceToken: "fld_token",
		Fields: []feishutools.ApprovalField{
			{Label: "文档标题", Value: "Quarterly plan"},
			{Label: "目标文件夹", Value: "fld_token"},
		},
		Payload: payload,
	})
	if err != nil {
		t.Fatalf("CheckOrRequest returned error: %v", err)
	}
	if pending.Status != feishutools.OperationApprovalStatusPending || pending.RequestID == "" || !pending.ExpiresAt.Equal(manager.currentTime().Add(manager.ttl)) {
		t.Fatalf("pending = %#v, want generated ID and configured expiry", pending)
	}
	cards, _, _ := sender.snapshot()
	if len(cards) != 1 || cards[0].chatID != "oc_chat" {
		t.Fatalf("cards = %#v, want one card in trusted chat", cards)
	}
	if !strings.Contains(cards[0].text, "Quarterly plan") || !strings.Contains(cards[0].text, `fld\\_token`) {
		t.Fatalf("approval card = %s, want displayed approval fields", cards[0].text)
	}
	if strings.Contains(cards[0].text, "private document body") {
		t.Fatalf("approval card leaked payload content: %s", cards[0].text)
	}
	assertApprovalCardActions(t, cards[0].text, pending.RequestID)

	record, err := st.GetToolApproval(pending.RequestID, "feishu:cli_test")
	if err != nil {
		t.Fatalf("GetToolApproval returned error: %v", err)
	}
	if record.State != store.ToolApprovalStatePending || record.ActionKey != "create" || record.ResourceType != "folder" || record.ResourceToken != "fld_token" || !record.SupportsAll || record.ActorOpenID != "ou_requester" || record.ChatID != "oc_chat" || record.CardMessageID != "om_card" || record.Payload != string(payload) {
		t.Fatalf("stored approval = %#v, want actor/chat/card-bound pending record", record)
	}
	continuation, err := st.GetWorkflowContinuation(pending.RequestID, "feishu:cli_test")
	if err != nil {
		t.Fatalf("GetWorkflowContinuation returned error: %v", err)
	}
	if continuation.State != store.WorkflowContinuationStateWaiting || continuation.CommittedRevision != -1 ||
		continuation.UserKey != "feishu:ou_requester" || continuation.SessionID != "session-work" ||
		continuation.ChatID != "oc_chat" || continuation.SourceMessageID != "om_source" ||
		continuation.ActorOpenID != "ou_requester" || continuation.OriginRevision != 7 ||
		continuation.OriginTurnID != "turn-test" || continuation.ToolCallID != "call-test" ||
		continuation.ToolName != "feishu_docs_create" {
		t.Fatalf("stored continuation = %#v", continuation)
	}
}

func TestApprovalCardActionParsesFormCallbackAndReason(t *testing.T) {
	event := approvalCardEvent("req_123", approvalCardActionApproveAll, "ou_requester", "oc_chat", "om_card", "请保持标题简洁")
	requestID, action, ok := parseApprovalCardAction(event)
	if !ok || requestID != "req_123" || action != approvalCardActionApproveAll {
		t.Fatalf("parseApprovalCardAction = %q %q %v", requestID, action, ok)
	}
	if reason := approvalCardReason(event); reason != "请保持标题简洁" {
		t.Fatalf("approvalCardReason = %q", reason)
	}
}

func TestOperationApprovalPolicyCanDisableReusableGrant(t *testing.T) {
	st := openFeishuApprovalTestStore(t)
	sender := &fakeApprovalSender{}
	manager := newTestApprovalManager(t, st, sender)
	executor := &fakeApprovalExecutor{name: "feishu_docs_create", disableAll: true}
	if err := manager.registerExecutor(executor); err != nil {
		t.Fatalf("registerExecutor returned error: %v", err)
	}
	pending := requestTestApproval(t, manager)
	cards, _, _ := sender.snapshot()
	if len(cards) != 1 || strings.Contains(cards[0].text, approvalCardActionApproveAll) || strings.Contains(cards[0].text, "全部同意") {
		t.Fatalf("single-use approval card = %#v", cards)
	}

	response, err := manager.HandleCardAction(t.Context(), approvalCardEvent(
		pending.RequestID, approvalCardActionApproveAll, "ou_requester", "oc_chat", "om_card", "",
	))
	if err != nil || response == nil || response.Toast == nil || response.Toast.Type != "error" || !strings.Contains(response.Toast.Content, "不支持永久授权") {
		t.Fatalf("forged approve-all response = %#v err=%v", response, err)
	}
	record, err := st.GetToolApproval(pending.RequestID, "feishu:cli_test")
	if err != nil || record.State != store.ToolApprovalStatePending || record.SupportsAll {
		t.Fatalf("single-use approval record = %#v err=%v", record, err)
	}
	if calls, _ := executor.snapshot(); calls != 0 {
		t.Fatalf("executor calls = %d, want 0", calls)
	}
}

func TestApprovalManagerPermanentGrantRequiresExactOperationScope(t *testing.T) {
	st := openFeishuApprovalTestStore(t)
	sender := &fakeApprovalSender{}
	manager := newTestApprovalManager(t, st, sender)
	if err := manager.registerExecutor(&fakeApprovalExecutor{name: "feishu_docs_create"}); err != nil {
		t.Fatalf("registerExecutor returned error: %v", err)
	}
	now := manager.currentTime()
	request := testOperationApprovalRequest()
	if _, err := st.UpsertToolApprovalGrant(store.ToolApprovalGrant{
		ToolApprovalGrantScope: store.ToolApprovalGrantScope{
			AccountID:     "feishu:cli_test",
			ToolName:      request.ToolName,
			ActionKey:     request.ActionKey,
			ResourceType:  request.ResourceType,
			ResourceToken: request.ResourceToken,
			ActorType:     store.ToolApprovalActorTypeOpenID,
			ActorID:       "ou_requester",
			ChatID:        "oc_chat",
		},
		SourceRequestID: "req_all",
		CreatedAt:       now,
	}); err != nil {
		t.Fatalf("UpsertToolApprovalGrant returned error: %v", err)
	}

	active, err := manager.hasActiveGrant(approvalRequestContext(), request)
	if err != nil || !active {
		t.Fatalf("exact grant returned active=%v err=%v", active, err)
	}
	decision, err := manager.CheckOrRequest(approvalRequestContext(), request)
	if err != nil || decision.Status != feishutools.OperationApprovalStatusGranted || decision.RequestID != "" {
		t.Fatalf("CheckOrRequest decision = %#v err=%v", decision, err)
	}
	if cards, _, _ := sender.snapshot(); len(cards) != 0 {
		t.Fatalf("active operation grant sent cards = %#v", cards)
	}
	wrongUser := feishutools.WithActor(context.Background(), feishutools.Actor{OpenID: "ou_other", UserID: "u_requester"})
	wrongUser = feishutools.WithChatContext(wrongUser, feishutools.ChatContext{ChatID: "oc_chat"})
	if active, err := manager.hasActiveGrant(wrongUser, request); err != nil || active {
		t.Fatalf("wrong-user grant returned active=%v err=%v", active, err)
	}
	wrongChat := feishutools.WithActor(context.Background(), feishutools.Actor{OpenID: "ou_requester"})
	wrongChat = feishutools.WithChatContext(wrongChat, feishutools.ChatContext{ChatID: "oc_other"})
	if active, err := manager.hasActiveGrant(wrongChat, request); err != nil || active {
		t.Fatalf("wrong-chat grant returned active=%v err=%v", active, err)
	}
	mismatches := []feishutools.OperationApprovalRequest{
		{ToolName: "other_tool", ActionKey: request.ActionKey, ResourceType: request.ResourceType, ResourceToken: request.ResourceToken},
		{ToolName: request.ToolName, ActionKey: "other_action", ResourceType: request.ResourceType, ResourceToken: request.ResourceToken},
		{ToolName: request.ToolName, ActionKey: request.ActionKey, ResourceType: "docx", ResourceToken: request.ResourceToken},
		{ToolName: request.ToolName, ActionKey: request.ActionKey, ResourceType: request.ResourceType, ResourceToken: "fld_other"},
	}
	for _, mismatch := range mismatches {
		if active, err := manager.hasActiveGrant(approvalRequestContext(), mismatch); err != nil || active {
			t.Fatalf("mismatched operation %#v returned active=%v err=%v", mismatch, active, err)
		}
	}

	manager.now = func() time.Time { return now.Add(365 * 24 * time.Hour) }
	if active, err := manager.hasActiveGrant(approvalRequestContext(), request); err != nil || !active {
		t.Fatalf("permanent grant returned active=%v err=%v after time advance", active, err)
	}
}

func TestApprovalManagerOnlyRequesterCanApprove(t *testing.T) {
	st := openFeishuApprovalTestStore(t)
	sender := &fakeApprovalSender{}
	manager := newTestApprovalManager(t, st, sender)
	executor := &fakeApprovalExecutor{name: "feishu_docs_create"}
	if err := manager.registerExecutor(executor); err != nil {
		t.Fatalf("registerExecutor returned error: %v", err)
	}
	pending := requestTestApproval(t, manager)

	resp, err := manager.HandleCardAction(t.Context(), approvalCardEvent(pending.RequestID, approvalCardActionApproveOnce, "ou_other", "oc_chat", "om_card", ""))
	if err != nil {
		t.Fatalf("HandleCardAction returned error: %v", err)
	}
	if resp == nil || resp.Toast == nil || resp.Toast.Type != "error" || !strings.Contains(resp.Toast.Content, "发起请求的用户") {
		t.Fatalf("response = %#v, want requester-only error toast", resp)
	}
	if calls, _ := executor.snapshot(); calls != 0 {
		t.Fatalf("executor calls = %d, want 0", calls)
	}
	record, err := st.GetToolApproval(pending.RequestID, "feishu:cli_test")
	if err != nil {
		t.Fatalf("GetToolApproval returned error: %v", err)
	}
	if record.State != store.ToolApprovalStatePending || record.Payload == "" {
		t.Fatalf("approval = %#v, want unchanged pending record", record)
	}
}

func TestApprovalManagerShutdownRejectsApprovalBeforeConsumingRequest(t *testing.T) {
	st := openFeishuApprovalTestStore(t)
	sender := &fakeApprovalSender{}
	manager := newTestApprovalManager(t, st, sender)
	executor := &fakeApprovalExecutor{name: "feishu_docs_create"}
	if err := manager.registerExecutor(executor); err != nil {
		t.Fatalf("registerExecutor returned error: %v", err)
	}
	pending := requestTestApproval(t, manager)
	manager.tasks.CloseAndWait()

	response, err := manager.HandleCardAction(t.Context(), approvalCardEvent(
		pending.RequestID,
		approvalCardActionApproveOnce,
		"ou_requester",
		"oc_chat",
		"om_card",
		"",
	))
	if err != nil {
		t.Fatalf("HandleCardAction returned error: %v", err)
	}
	if response == nil || response.Toast == nil || response.Toast.Type != "error" {
		t.Fatalf("response = %#v, want shutdown error toast", response)
	}
	approval, err := st.GetToolApproval(pending.RequestID, manager.account.ID)
	if err != nil {
		t.Fatalf("GetToolApproval returned error: %v", err)
	}
	if approval.State != store.ToolApprovalStatePending || approval.Payload == "" {
		t.Fatalf("approval after rejected shutdown callback = %#v, want actionable pending request", approval)
	}
	if calls, _ := executor.snapshot(); calls != 0 {
		t.Fatalf("executor calls = %d, want 0", calls)
	}
}

func TestApprovalManagerCanceledRuntimeRejectsApprovalBeforeConsumingRequest(t *testing.T) {
	st := openFeishuApprovalTestStore(t)
	sender := &fakeApprovalSender{}
	manager := newTestApprovalManager(t, st, sender)
	executor := &fakeApprovalExecutor{name: "feishu_docs_create"}
	if err := manager.registerExecutor(executor); err != nil {
		t.Fatalf("registerExecutor returned error: %v", err)
	}
	pending := requestTestApproval(t, manager)
	runCtx, cancelRun := context.WithCancel(context.Background())
	manager.runCtx = runCtx
	cancelRun()

	response, err := manager.HandleCardAction(t.Context(), approvalCardEvent(
		pending.RequestID,
		approvalCardActionApproveOnce,
		"ou_requester",
		"oc_chat",
		"om_card",
		"",
	))
	if err != nil {
		t.Fatalf("HandleCardAction returned error: %v", err)
	}
	if response == nil || response.Toast == nil || response.Toast.Type != "error" {
		t.Fatalf("response = %#v, want shutdown error toast", response)
	}
	manager.tasks.CloseAndWait()
	approval, err := st.GetToolApproval(pending.RequestID, manager.account.ID)
	if err != nil {
		t.Fatalf("GetToolApproval returned error: %v", err)
	}
	if approval.State != store.ToolApprovalStatePending || approval.Payload == "" {
		t.Fatalf("approval after canceled-runtime callback = %#v, want actionable pending request", approval)
	}
	if calls, _ := executor.snapshot(); calls != 0 {
		t.Fatalf("executor calls = %d, want 0", calls)
	}
}

func TestApprovalManagerDrainsAdmittedExecutionAfterRuntimeCancellation(t *testing.T) {
	st := openFeishuApprovalTestStore(t)
	sender := &fakeApprovalSender{}
	manager := newTestApprovalManager(t, st, sender)
	runCtx, cancelRun := context.WithCancel(context.Background())
	manager.runCtx = runCtx
	executor := &blockingApprovalExecutor{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	t.Cleanup(func() {
		select {
		case <-executor.release:
		default:
			close(executor.release)
		}
	})
	if err := manager.registerExecutor(executor); err != nil {
		t.Fatalf("registerExecutor returned error: %v", err)
	}
	pending := requestTestApproval(t, manager)

	response, err := manager.HandleCardAction(t.Context(), approvalCardEvent(
		pending.RequestID,
		approvalCardActionApproveOnce,
		"ou_requester",
		"oc_chat",
		"om_card",
		"",
	))
	if err != nil || response == nil || response.Toast == nil || response.Toast.Type != "success" {
		t.Fatalf("HandleCardAction response = %#v err=%v", response, err)
	}
	select {
	case <-executor.started:
	case <-time.After(time.Second):
		t.Fatal("approved executor did not start")
	}
	cancelRun()
	time.Sleep(20 * time.Millisecond)
	approval, err := st.GetToolApproval(pending.RequestID, manager.account.ID)
	if err != nil || approval.State != store.ToolApprovalStateExecuting {
		t.Fatalf("approval after runtime cancellation = %#v err=%v, want executing until admitted task drains", approval, err)
	}
	close(executor.release)
	manager.tasks.CloseAndWait()
	completed, err := st.GetToolApproval(pending.RequestID, manager.account.ID)
	if err != nil || completed.State != store.ToolApprovalStateSucceeded {
		t.Fatalf("approval after admitted task drain = %#v err=%v, want succeeded", completed, err)
	}
}

func TestApprovalManagerLeaseOwnershipLossPreservesAdmittedExecutionForRecovery(t *testing.T) {
	st := openFeishuApprovalTestStore(t)
	sender := &fakeApprovalSender{}
	manager := newTestApprovalManager(t, st, sender)
	lifecycleCtx, cancelLifecycle := context.WithCancel(context.Background())
	defer cancelLifecycle()
	ownershipCtx, cancelOwnership := context.WithCancel(context.Background())
	manager.runCtx = withFeishuRuntimeOwnership(lifecycleCtx, ownershipCtx)
	executor := &blockingApprovalExecutor{
		started: make(chan struct{}),
		release: make(chan struct{}),
		recover: true,
	}
	releaseExecutor := func() {
		select {
		case <-executor.release:
		default:
			close(executor.release)
		}
	}
	t.Cleanup(releaseExecutor)
	if err := manager.registerExecutor(executor); err != nil {
		t.Fatalf("registerExecutor returned error: %v", err)
	}
	pending := requestTestApproval(t, manager)

	response, err := manager.HandleCardAction(t.Context(), approvalCardEvent(
		pending.RequestID,
		approvalCardActionApproveOnce,
		"ou_requester",
		"oc_chat",
		"om_card",
		"",
	))
	if err != nil || response == nil || response.Toast == nil || response.Toast.Type != "success" {
		t.Fatalf("HandleCardAction response = %#v err=%v", response, err)
	}
	select {
	case <-executor.started:
	case <-time.After(time.Second):
		t.Fatal("approved executor did not start")
	}

	cancelOwnership()
	tasksDone := make(chan struct{})
	go func() {
		manager.tasks.CloseAndWait()
		close(tasksDone)
	}()
	select {
	case <-tasksDone:
	case <-time.After(200 * time.Millisecond):
		releaseExecutor()
		<-tasksDone
		t.Fatal("admitted approval execution ignored runtime ownership cancellation")
	}
	preserved, err := st.GetToolApproval(pending.RequestID, manager.account.ID)
	if err != nil || preserved.State != store.ToolApprovalStateExecuting || preserved.Payload == "" {
		t.Fatalf("approval after runtime ownership cancellation = %#v err=%v, want recoverable executing payload", preserved, err)
	}
	workflow, err := st.GetWorkflowRequest(pending.RequestID, manager.account.ID)
	if err != nil || workflow.State != store.WorkflowRequestStateExecuting {
		t.Fatalf("workflow after runtime ownership cancellation = %#v err=%v, want executing", workflow, err)
	}
	if result, err := st.GetWorkflowResult(pending.RequestID, manager.account.ID); !errors.Is(err, store.ErrWorkflowResultNotFound) {
		t.Fatalf("workflow result after runtime ownership cancellation = %#v err=%v, want none", result, err)
	}
	_, updates, messages := sender.snapshot()
	if len(updates) != 0 || len(messages) != 0 {
		t.Fatalf("terminal outputs after runtime ownership cancellation = updates:%#v messages:%#v", updates, messages)
	}
}

func TestApprovalManagerApprovesExecutesUpdatesCardAndStoresResult(t *testing.T) {
	st := openFeishuApprovalTestStore(t)
	sender := &fakeApprovalSender{}
	manager := newTestApprovalManager(t, st, sender)
	executor := &fakeApprovalExecutor{
		name:   "feishu_docs_create",
		result: feishutools.OperationApprovalExecution{Message: "✅ 文档已创建：[Quarterly plan](https://docs.feishu.cn/docx/doc123)"},
		done:   make(chan struct{}),
	}
	if err := manager.registerExecutor(executor); err != nil {
		t.Fatalf("registerExecutor returned error: %v", err)
	}
	pending := requestTestApproval(t, manager)

	resp, err := manager.HandleCardAction(t.Context(), approvalCardEvent(pending.RequestID, approvalCardActionApproveOnce, "ou_requester", "oc_chat", "om_card", ""))
	if err != nil {
		t.Fatalf("HandleCardAction returned error: %v", err)
	}
	if resp == nil || resp.Toast == nil || resp.Toast.Type != "success" || resp.Card != nil {
		t.Fatalf("response = %#v, want toast-only response before official delayed update", resp)
	}
	select {
	case <-executor.done:
	case <-time.After(time.Second):
		t.Fatal("approved executor was not called")
	}
	waitForApprovalState(t, st, pending.RequestID, store.ToolApprovalStateSucceeded)
	waitForApprovalUpdates(t, sender, 1)

	calls, payload := executor.snapshot()
	if calls != 1 || !strings.Contains(string(payload), "Quarterly plan") {
		t.Fatalf("executor calls/payload = %d/%s, want one approved payload", calls, payload)
	}
	record, err := st.GetToolApproval(pending.RequestID, "feishu:cli_test")
	if err != nil {
		t.Fatalf("GetToolApproval returned error: %v", err)
	}
	if record.Payload != "" {
		t.Fatalf("completed approval retained payload: %#v", record)
	}
	_, updates, messages := sender.snapshot()
	if len(updates) != 1 || updates[0].messageID != "c_callback" || !strings.Contains(updates[0].text, "执行完成") {
		t.Fatalf("card updates = %#v, want one callback-token delayed completion update", updates)
	}
	if len(messages) != 0 {
		t.Fatalf("messages = %#v, want card-only immediate result", messages)
	}
	workflowResult := waitForApprovalWorkflowResult(t, st, pending.RequestID, store.WorkflowResultStateSucceeded)
	if !strings.Contains(string(workflowResult.Payload), "docs.feishu.cn/docx/doc123") {
		t.Fatalf("workflow result = %#v", workflowResult)
	}

	second, err := manager.HandleCardAction(t.Context(), approvalCardEvent(pending.RequestID, approvalCardActionApproveOnce, "ou_requester", "oc_chat", "om_card", ""))
	if err != nil {
		t.Fatalf("second HandleCardAction returned error: %v", err)
	}
	if second == nil || second.Toast == nil || !strings.Contains(second.Toast.Content, "已经处理") {
		t.Fatalf("second response = %#v, want already handled toast", second)
	}
	if calls, _ := executor.snapshot(); calls != 1 {
		t.Fatalf("executor calls after duplicate click = %d, want 1", calls)
	}
	if active, err := manager.hasActiveGrant(approvalRequestContext(), testOperationApprovalRequest()); err != nil || active {
		t.Fatalf("approve-once grant returned active=%v err=%v", active, err)
	}
}

func TestApprovalManagerPersistsOutcomeUnknownExecutionAsPartialWarning(t *testing.T) {
	st := openFeishuApprovalTestStore(t)
	sender := &fakeApprovalSender{}
	manager := newTestApprovalManager(t, st, sender)
	executor := &fakeApprovalExecutor{
		name: "feishu_docs_create",
		result: feishutools.OperationApprovalExecution{
			Message:       "⚠️ 追加结果无法确认，请检查文档并请勿重复追加。",
			Warning:       true,
			WarningReason: "append outcome is unknown",
		},
		done: make(chan struct{}),
	}
	if err := manager.registerExecutor(executor); err != nil {
		t.Fatalf("registerExecutor returned error: %v", err)
	}
	pending := requestTestApproval(t, manager)

	response, err := manager.HandleCardAction(t.Context(), approvalCardEvent(
		pending.RequestID,
		approvalCardActionApproveOnce,
		"ou_requester",
		"oc_chat",
		"om_card",
		"",
	))
	if err != nil || response == nil || response.Toast == nil || response.Toast.Type != "success" {
		t.Fatalf("HandleCardAction response = %#v err=%v", response, err)
	}
	select {
	case <-executor.done:
	case <-time.After(time.Second):
		t.Fatal("warning executor was not called")
	}
	waitForApprovalState(t, st, pending.RequestID, store.ToolApprovalStatePartial)
	waitForApprovalUpdates(t, sender, 1)
	result := waitForApprovalWorkflowResult(t, st, pending.RequestID, store.WorkflowResultStateSucceeded)
	if !strings.Contains(string(result.Payload), `"status":"succeeded_with_warning"`) ||
		!strings.Contains(string(result.Payload), `"warning":true`) ||
		!strings.Contains(string(result.Payload), "append outcome is unknown") {
		t.Fatalf("warning workflow result = %#v", result)
	}
	record, err := st.GetToolApproval(pending.RequestID, manager.account.ID)
	if err != nil || record.Payload != "" {
		t.Fatalf("partial approval retained payload = %#v err=%v", record, err)
	}
	_, updates, messages := sender.snapshot()
	if len(updates) != 1 || !strings.Contains(updates[0].text, "执行完成（有警告）") || len(messages) != 0 {
		t.Fatalf("warning outputs = updates:%#v messages:%#v", updates, messages)
	}
}

func TestApprovalManagerAttemptsTerminalCardBeforeMakingContinuationReady(t *testing.T) {
	st := openFeishuApprovalTestStore(t)
	sender := &fakeApprovalSender{}
	manager := newTestApprovalManager(t, st, sender)
	cardUpdatedBeforeResult := make(chan bool, 1)
	manager.store = &observingToolApprovalResultStore{
		toolApprovalStore: manager.store,
		onStoreResult: func() {
			_, updates, _ := sender.snapshot()
			cardUpdatedBeforeResult <- len(updates) > 0
		},
	}
	executor := &fakeApprovalExecutor{
		name:   "feishu_docs_create",
		result: feishutools.OperationApprovalExecution{Message: "文档已创建"},
	}
	if err := manager.registerExecutor(executor); err != nil {
		t.Fatalf("registerExecutor returned error: %v", err)
	}
	pending := requestTestApproval(t, manager)

	response, err := manager.HandleCardAction(t.Context(), approvalCardEvent(
		pending.RequestID,
		approvalCardActionApproveOnce,
		"ou_requester",
		"oc_chat",
		"om_card",
		"",
	))
	if err != nil || response == nil || response.Toast == nil || response.Toast.Type != "success" {
		t.Fatalf("HandleCardAction response = %#v err=%v", response, err)
	}
	select {
	case updated := <-cardUpdatedBeforeResult:
		if !updated {
			t.Fatal("workflow continuation became ready before the terminal approval card update was attempted")
		}
	case <-time.After(time.Second):
		t.Fatal("approved result was not persisted")
	}
}

func TestApprovalManagerFallsBackToMessageIDWhenCallbackUpdateFails(t *testing.T) {
	st := openFeishuApprovalTestStore(t)
	sender := &fakeApprovalSender{interactionUpdateErr: errors.New("callback token expired")}
	manager := newTestApprovalManager(t, st, sender)
	executor := &fakeApprovalExecutor{
		name:   "feishu_docs_create",
		result: feishutools.OperationApprovalExecution{Message: "文档已创建"},
		done:   make(chan struct{}),
	}
	if err := manager.registerExecutor(executor); err != nil {
		t.Fatalf("registerExecutor returned error: %v", err)
	}
	pending := requestTestApproval(t, manager)

	response, err := manager.HandleCardAction(t.Context(), approvalCardEvent(
		pending.RequestID,
		approvalCardActionApproveOnce,
		"ou_requester",
		"oc_chat",
		"om_card",
		"",
	))
	if err != nil || response == nil || response.Toast == nil || response.Toast.Type != "success" {
		t.Fatalf("HandleCardAction response = %#v err=%v", response, err)
	}
	select {
	case <-executor.done:
	case <-time.After(time.Second):
		t.Fatal("approved executor was not called")
	}
	waitForApprovalState(t, st, pending.RequestID, store.ToolApprovalStateSucceeded)
	waitForApprovalUpdates(t, sender, 2)

	_, updates, messages := sender.snapshot()
	if len(updates) != 2 || updates[0].messageID != "c_callback" || updates[1].messageID != "om_card" ||
		!strings.Contains(updates[1].text, "执行完成") {
		t.Fatalf("fallback card updates = %#v, want callback attempt then message-id update", updates)
	}
	if len(messages) != 0 {
		t.Fatalf("messages = %#v, want card-only result", messages)
	}
}

func TestApprovalManagerDurablyRetriesTerminalCardWhenFastPathsFail(t *testing.T) {
	st := openFeishuApprovalTestStore(t)
	sender := &fakeApprovalSender{
		interactionUpdateErr: errors.New("callback update unavailable"),
		updateErr:            errors.New("message update unavailable"),
	}
	manager := newTestApprovalManager(t, st, sender)
	executor := &fakeApprovalExecutor{
		name:   "feishu_docs_create",
		result: feishutools.OperationApprovalExecution{Message: "文档已创建"},
		done:   make(chan struct{}),
	}
	if err := manager.registerExecutor(executor); err != nil {
		t.Fatalf("registerExecutor returned error: %v", err)
	}
	pending := requestTestApproval(t, manager)

	response, err := manager.HandleCardAction(t.Context(), approvalCardEvent(
		pending.RequestID,
		approvalCardActionApproveOnce,
		"ou_requester",
		"oc_chat",
		"om_card",
		"",
	))
	if err != nil || response == nil || response.Toast == nil || response.Toast.Type != "success" {
		t.Fatalf("HandleCardAction response = %#v err=%v", response, err)
	}
	select {
	case <-executor.done:
	case <-time.After(time.Second):
		t.Fatal("approved executor was not called")
	}
	waitForApprovalState(t, st, pending.RequestID, store.ToolApprovalStateSucceeded)
	waitForApprovalUpdates(t, sender, 2)
	delivery, err := st.GetFeishuCardDeliveryByKey(
		manager.account.ID,
		pending.RequestID,
		store.FeishuCardDeliveryPurposeToolApprovalTerminal,
		store.FeishuCardDeliveryRevisionTerminal,
	)
	if err != nil || delivery.State != store.FeishuCardDeliveryStatePending {
		t.Fatalf("durable approval card delivery = %#v err=%v", delivery, err)
	}

	sender.mu.Lock()
	sender.interactionUpdateErr = nil
	sender.updateErr = nil
	sender.mu.Unlock()
	deliveryWorker, err := newFeishuCardDeliveryWorker(st, manager.cards, nil, manager.account)
	if err != nil {
		t.Fatalf("newFeishuCardDeliveryWorker returned error: %v", err)
	}
	deliveryWorker.now = func() time.Time { return manager.currentTime().Add(time.Second) }
	deliveryWorker.processAvailable(t.Context())
	delivery, err = st.GetFeishuCardDelivery(delivery.ID, delivery.AccountID)
	if err != nil || delivery.State != store.FeishuCardDeliveryStateDelivered {
		t.Fatalf("retried approval card delivery = %#v err=%v", delivery, err)
	}
	_, updates, _ := sender.snapshot()
	if len(updates) != 3 || updates[2].messageID != "om_card" || !strings.Contains(updates[2].text, "执行完成") {
		t.Fatalf("approval card update attempts = %#v", updates)
	}
}

func TestApprovalManagerMessageIDFallbackGetsFreshTimeoutAfterCallbackTimeout(t *testing.T) {
	fallbackContextActive := make(chan bool, 1)
	sender := &fakeApprovalSender{
		interactionUpdateFunc: func(ctx context.Context, _, _ string) error {
			<-ctx.Done()
			return ctx.Err()
		},
		updateCardFunc: func(ctx context.Context, _, _ string) error {
			fallbackContextActive <- ctx.Err() == nil
			return nil
		},
	}
	st := openFeishuApprovalTestStore(t)
	manager := newTestApprovalManager(t, st, sender)
	manager.cardUpdateTimeout = 10 * time.Millisecond
	manager.updateApprovalResultCard(store.ToolApproval{
		ID:            "req_timeout_fallback",
		AccountID:     "feishu:cli_test",
		CardMessageID: "om_card",
	}, "c_callback", statusCard{title: "完成", template: "green", message: "完成"})

	select {
	case active := <-fallbackContextActive:
		if !active {
			t.Fatal("message-id fallback reused the expired callback context")
		}
	case <-time.After(time.Second):
		t.Fatal("message-id fallback was not attempted after callback timeout")
	}
}

func TestApprovalManagerResultCardUsesShutdownIndependentContext(t *testing.T) {
	st := openFeishuApprovalTestStore(t)
	contextActive := make(chan bool, 1)
	sender := &fakeApprovalSender{
		interactionUpdateFunc: func(ctx context.Context, _, _ string) error {
			contextActive <- ctx.Err() == nil
			return nil
		},
	}
	manager := newTestApprovalManager(t, st, sender)
	runCtx, cancelRun := context.WithCancel(context.Background())
	cancelRun()
	manager.runCtx = runCtx

	manager.updateApprovalResultCard(store.ToolApproval{
		ID:            "req_shutdown_card",
		AccountID:     manager.account.ID,
		CardMessageID: "om_card",
	}, "callback-token", statusCard{title: "完成", template: "green", message: "完成"})

	select {
	case active := <-contextActive:
		if !active {
			t.Fatal("approval result card inherited runtime cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("approval result card update was not attempted")
	}
}

func TestApprovalManagerExecutorFailureStoresResultWithoutExtraMessage(t *testing.T) {
	st := openFeishuApprovalTestStore(t)
	sender := &fakeApprovalSender{}
	manager := newTestApprovalManager(t, st, sender)
	executor := &fakeApprovalExecutor{
		name: "feishu_docs_create",
		err:  errors.New("create failed"),
		done: make(chan struct{}),
	}
	if err := manager.registerExecutor(executor); err != nil {
		t.Fatalf("registerExecutor returned error: %v", err)
	}
	pending := requestTestApproval(t, manager)

	response, err := manager.HandleCardAction(t.Context(), approvalCardEvent(pending.RequestID, approvalCardActionApproveOnce, "ou_requester", "oc_chat", "om_card", ""))
	if err != nil || response == nil || response.Toast == nil || response.Toast.Type != "success" {
		t.Fatalf("HandleCardAction response = %#v err=%v", response, err)
	}
	select {
	case <-executor.done:
	case <-time.After(time.Second):
		t.Fatal("approved executor was not called")
	}
	waitForApprovalState(t, st, pending.RequestID, store.ToolApprovalStateFailed)
	waitForApprovalUpdates(t, sender, 1)
	workflowResult := waitForApprovalWorkflowResult(t, st, pending.RequestID, store.WorkflowResultStateFailed)
	if !strings.Contains(string(workflowResult.Payload), `"status":"failed"`) {
		t.Fatalf("failed workflow result = %#v", workflowResult)
	}
	_, updates, messages := sender.snapshot()
	if len(updates) != 1 || !strings.Contains(updates[0].text, "执行失败") || len(messages) != 0 {
		t.Fatalf("updates/messages = %#v/%#v", updates, messages)
	}
}

func TestApprovalManagerDoesNotPublishSuccessWhenCompletionPersistenceFails(t *testing.T) {
	st := openFeishuApprovalTestStore(t)
	sender := &fakeApprovalSender{}
	manager := newTestApprovalManager(t, st, sender)
	executor := &fakeApprovalExecutor{
		name:   "feishu_docs_create",
		result: feishutools.OperationApprovalExecution{Message: "document created"},
		done:   make(chan struct{}),
	}
	if err := manager.registerExecutor(executor); err != nil {
		t.Fatalf("registerExecutor returned error: %v", err)
	}
	manager.store = failingCompleteToolApprovalStore{
		toolApprovalStore: manager.store,
		err:               errors.New("completion persistence unavailable"),
	}
	pending := requestTestApproval(t, manager)

	response, err := manager.HandleCardAction(t.Context(), approvalCardEvent(
		pending.RequestID,
		approvalCardActionApproveOnce,
		"ou_requester",
		"oc_chat",
		"om_card",
		"",
	))
	if err != nil || response == nil || response.Toast == nil || response.Toast.Type != "success" {
		t.Fatalf("HandleCardAction response = %#v err=%v", response, err)
	}
	select {
	case <-executor.done:
	case <-time.After(time.Second):
		t.Fatal("approved executor was not called")
	}
	manager.tasks.CloseAndWait()

	record, err := st.GetToolApproval(pending.RequestID, "feishu:cli_test")
	if err != nil || record.State != store.ToolApprovalStateExecuting {
		t.Fatalf("approval after rejected completion persistence = %#v err=%v, want executing", record, err)
	}
	if _, err := st.GetWorkflowResult(pending.RequestID, "feishu:cli_test"); !errors.Is(err, store.ErrWorkflowResultNotFound) {
		t.Fatalf("workflow result error = %v, want ErrWorkflowResultNotFound", err)
	}
	_, updates, messages := sender.snapshot()
	if len(updates) != 0 || len(messages) != 0 {
		t.Fatalf("updates/messages = %#v/%#v, want no terminal publication before durable completion", updates, messages)
	}
}

func TestApprovalManagerExpiredDecisionStoresWorkflowResult(t *testing.T) {
	st := openFeishuApprovalTestStore(t)
	sender := &fakeApprovalSender{}
	manager := newTestApprovalManager(t, st, sender)
	if err := manager.registerExecutor(&fakeApprovalExecutor{name: "feishu_docs_create"}); err != nil {
		t.Fatalf("registerExecutor returned error: %v", err)
	}
	pending := requestTestApproval(t, manager)
	manager.now = func() time.Time { return pending.ExpiresAt }

	response, err := manager.HandleCardAction(t.Context(), approvalCardEvent(pending.RequestID, approvalCardActionApproveOnce, "ou_requester", "oc_chat", "om_card", ""))
	if err != nil || response == nil || response.Toast == nil || !strings.Contains(response.Toast.Content, "过期") {
		t.Fatalf("HandleCardAction response = %#v err=%v", response, err)
	}
	workflowResult, err := st.GetWorkflowResult(pending.RequestID, "feishu:cli_test")
	if err != nil || workflowResult.State != store.WorkflowResultStateExpired || !strings.Contains(string(workflowResult.Payload), `"status":"expired"`) {
		t.Fatalf("expired workflow result = %#v err=%v", workflowResult, err)
	}
}

func TestApprovalManagerRecoveryReconcilesExpiredWorkflowResult(t *testing.T) {
	st := openFeishuApprovalTestStore(t)
	sender := &fakeApprovalSender{}
	manager := newTestApprovalManager(t, st, sender)
	if err := manager.registerExecutor(&fakeApprovalExecutor{name: "feishu_docs_create"}); err != nil {
		t.Fatalf("registerExecutor returned error: %v", err)
	}
	pending := requestTestApproval(t, manager)
	manager.now = func() time.Time { return pending.ExpiresAt }
	cardUpdatedBeforeResult := make(chan bool, 1)
	manager.store = &observingToolApprovalResultStore{
		toolApprovalStore: manager.store,
		onStoreResult: func() {
			_, updates, _ := sender.snapshot()
			cardUpdatedBeforeResult <- len(updates) > 0
		},
	}

	if err := manager.recoverPersistedApprovals(t.Context()); err != nil {
		t.Fatalf("recoverPersistedApprovals returned error: %v", err)
	}
	select {
	case updated := <-cardUpdatedBeforeResult:
		if !updated {
			t.Fatal("recovery made the continuation ready before updating the expired approval card")
		}
	case <-time.After(time.Second):
		t.Fatal("recovery did not persist the expired approval result")
	}
	workflowResult, err := st.GetWorkflowResult(pending.RequestID, "feishu:cli_test")
	if err != nil || workflowResult.State != store.WorkflowResultStateExpired || !strings.Contains(string(workflowResult.Payload), `"status":"expired"`) {
		t.Fatalf("reconciled workflow result = %#v err=%v", workflowResult, err)
	}
	continuation, err := st.GetWorkflowContinuation(pending.RequestID, "feishu:cli_test")
	if err != nil || continuation.State != store.WorkflowContinuationStateWaiting {
		t.Fatalf("reconciled continuation = %#v err=%v", continuation, err)
	}
	_, updates, _ := sender.snapshot()
	if len(updates) != 1 || !strings.Contains(updates[0].text, "授权已过期") {
		t.Fatalf("recovered approval card updates = %#v", updates)
	}
}

func TestApprovalManagerRecoveryResumesRestartSafeExecutingApproval(t *testing.T) {
	st := openFeishuApprovalTestStore(t)
	manager := newTestApprovalManager(t, st, &fakeApprovalSender{})
	executor := &fakeApprovalExecutor{
		name:    "feishu_docs_create",
		recover: true,
		result:  feishutools.OperationApprovalExecution{Message: "document recovered"},
		done:    make(chan struct{}),
	}
	if err := manager.registerExecutor(executor); err != nil {
		t.Fatalf("registerExecutor returned error: %v", err)
	}
	pending := requestTestApproval(t, manager)
	if _, err := st.DecideToolApproval(
		pending.RequestID,
		"feishu:cli_test",
		store.ToolApprovalDecisionApprove,
		store.ToolApprovalMatch{
			ActorOpenID:   "ou_requester",
			ActorUserID:   "u_requester",
			ChatID:        "oc_chat",
			CardMessageID: "om_card",
		},
		manager.currentTime(),
	); err != nil {
		t.Fatalf("DecideToolApproval returned error: %v", err)
	}

	if err := manager.recoverPersistedApprovals(t.Context()); err != nil {
		t.Fatalf("recoverPersistedApprovals returned error: %v", err)
	}
	select {
	case <-executor.done:
	case <-time.After(time.Second):
		t.Fatal("restart-safe executing approval was not resumed")
	}
	waitForApprovalState(t, st, pending.RequestID, store.ToolApprovalStateSucceeded)
	calls, payload := executor.snapshot()
	if calls != 1 || !strings.Contains(string(payload), "Quarterly plan") {
		t.Fatalf("recovered executor calls=%d payload=%s", calls, payload)
	}
}

func TestApprovalManagerRecoveryToleratesExecutingApprovalResolvedAfterListing(t *testing.T) {
	st := openFeishuApprovalTestStore(t)
	manager := newTestApprovalManager(t, st, &fakeApprovalSender{})
	if err := manager.registerExecutor(&fakeApprovalExecutor{name: "feishu_docs_create"}); err != nil {
		t.Fatalf("registerExecutor returned error: %v", err)
	}
	pending := requestTestApproval(t, manager)
	if _, err := st.DecideToolApproval(
		pending.RequestID,
		"feishu:cli_test",
		store.ToolApprovalDecisionApprove,
		store.ToolApprovalMatch{
			ActorOpenID:   "ou_requester",
			ActorUserID:   "u_requester",
			ChatID:        "oc_chat",
			CardMessageID: "om_card",
		},
		manager.currentTime(),
	); err != nil {
		t.Fatalf("DecideToolApproval returned error: %v", err)
	}
	manager.store = &resolveExecutingApprovalDuringRecoveryListStore{
		toolApprovalStore: manager.store,
		now:               manager.currentTime().Add(time.Second),
	}
	if err := manager.recoverPersistedApprovals(t.Context()); err != nil {
		t.Fatalf("recoverPersistedApprovals returned error after concurrent completion: %v", err)
	}
	stored, err := st.GetToolApproval(pending.RequestID, "feishu:cli_test")
	if err != nil || stored.State != store.ToolApprovalStateSucceeded {
		t.Fatalf("concurrently completed approval = %#v err=%v", stored, err)
	}
}

func TestApprovalManagerRecoverySharesTerminalCardTimeoutBudget(t *testing.T) {
	st := openFeishuApprovalTestStore(t)
	sender := &fakeApprovalSender{
		updateCardFunc: func(ctx context.Context, _, _ string) error {
			<-ctx.Done()
			return ctx.Err()
		},
	}
	manager := newTestApprovalManager(t, st, sender)
	manager.cardUpdateTimeout = 15 * time.Millisecond
	if err := manager.registerExecutor(&fakeApprovalExecutor{name: "feishu_docs_create"}); err != nil {
		t.Fatalf("registerExecutor returned error: %v", err)
	}
	first := requestTestApproval(t, manager)
	second := requestTestApproval(t, manager)
	manager.now = func() time.Time { return first.ExpiresAt }

	started := time.Now()
	if err := manager.recoverPersistedApprovals(t.Context()); err != nil {
		t.Fatalf("recoverPersistedApprovals returned error: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("recovery exceeded one shared card timeout budget: %s", elapsed)
	}
	_, updates, _ := sender.snapshot()
	if len(updates) != 1 {
		t.Fatalf("terminal card update attempts = %d, want one attempt before the shared context expires", len(updates))
	}
	waitForApprovalWorkflowResult(t, st, first.RequestID, store.WorkflowResultStateExpired)
	waitForApprovalWorkflowResult(t, st, second.RequestID, store.WorkflowResultStateExpired)
}

func TestApprovalManagerApproveAllCreatesPermanentExactScopedGrant(t *testing.T) {
	st := openFeishuApprovalTestStore(t)
	sender := &fakeApprovalSender{}
	manager := newTestApprovalManager(t, st, sender)
	executor := &fakeApprovalExecutor{
		name:   "feishu_docs_create",
		result: feishutools.OperationApprovalExecution{Message: "✅ 文档已创建"},
		done:   make(chan struct{}),
	}
	if err := manager.registerExecutor(executor); err != nil {
		t.Fatalf("registerExecutor returned error: %v", err)
	}
	pending := requestTestApproval(t, manager)
	clickedAt := manager.currentTime()

	resp, err := manager.HandleCardAction(t.Context(), approvalCardEvent(
		pending.RequestID,
		approvalCardActionApproveAll,
		"ou_requester",
		"oc_chat",
		"om_card",
		"后续同类操作无需重复询问",
	))
	if err != nil {
		t.Fatalf("HandleCardAction returned error: %v", err)
	}
	if resp == nil || resp.Toast == nil || resp.Toast.Type != "success" || !strings.Contains(resp.Toast.Content, "永久授权") {
		t.Fatalf("response = %#v, want reusable-grant toast", resp)
	}
	select {
	case <-executor.done:
	case <-time.After(time.Second):
		t.Fatal("approve-all executor was not called")
	}
	waitForApprovalState(t, st, pending.RequestID, store.ToolApprovalStateSucceeded)

	scope, err := toolApprovalGrantScope(
		"feishu:cli_test",
		"feishu_docs_create",
		"create",
		"folder",
		"fld_token",
		"ou_requester",
		"u_requester",
		"oc_chat",
	)
	if err != nil {
		t.Fatalf("toolApprovalGrantScope returned error: %v", err)
	}
	grant, active, err := st.FindToolApprovalGrant(scope)
	if err != nil {
		t.Fatalf("FindToolApprovalGrant returned error: %v", err)
	}
	if !active || grant.SourceRequestID != pending.RequestID || !grant.CreatedAt.Equal(clickedAt) {
		t.Fatalf("grant = %#v active=%v, want permanent exact scope from click", grant, active)
	}
	request := testOperationApprovalRequest()
	if active, err := manager.hasActiveGrant(approvalRequestContext(), request); err != nil || !active {
		t.Fatalf("same scope returned active=%v err=%v", active, err)
	}
	otherChat := feishutools.WithActor(context.Background(), feishutools.Actor{OpenID: "ou_requester", UserID: "u_requester"})
	otherChat = feishutools.WithChatContext(otherChat, feishutools.ChatContext{ChatID: "oc_other"})
	if active, err := manager.hasActiveGrant(otherChat, request); err != nil || active {
		t.Fatalf("other chat returned active=%v err=%v", active, err)
	}
	otherResource := request
	otherResource.ResourceToken = "fld_other"
	if active, err := manager.hasActiveGrant(approvalRequestContext(), otherResource); err != nil || active {
		t.Fatalf("other resource returned active=%v err=%v", active, err)
	}
}

func TestApprovalManagerApproveAllFallsBackToCurrentRequestWhenGrantSaveFails(t *testing.T) {
	st := openFeishuApprovalTestStore(t)
	sender := &fakeApprovalSender{}
	manager := newTestApprovalManager(t, st, sender)
	executor := &fakeApprovalExecutor{
		name:   "feishu_docs_create",
		result: feishutools.OperationApprovalExecution{Message: "✅ 文档已创建"},
		done:   make(chan struct{}),
	}
	if err := manager.registerExecutor(executor); err != nil {
		t.Fatalf("registerExecutor returned error: %v", err)
	}
	manager.store = failingGrantStore{toolApprovalStore: manager.store, err: errors.New("grant unavailable")}
	pending := requestTestApproval(t, manager)

	resp, err := manager.HandleCardAction(t.Context(), approvalCardEvent(
		pending.RequestID,
		approvalCardActionApproveAll,
		"ou_requester",
		"oc_chat",
		"om_card",
		"",
	))
	if err != nil {
		t.Fatalf("HandleCardAction returned error: %v", err)
	}
	if resp == nil || resp.Toast == nil || resp.Toast.Type != "success" || !strings.Contains(resp.Toast.Content, "保存失败") || !strings.Contains(resp.Toast.Content, "后续调用仍需审批") {
		t.Fatalf("response = %#v, want degraded one-time approval toast", resp)
	}
	select {
	case <-executor.done:
	case <-time.After(time.Second):
		t.Fatal("executor was not called after grant save failure")
	}
	waitForApprovalState(t, st, pending.RequestID, store.ToolApprovalStateSucceeded)
	if active, err := manager.hasActiveGrant(approvalRequestContext(), testOperationApprovalRequest()); err != nil || active {
		t.Fatalf("failed grant returned active=%v err=%v", active, err)
	}
}

func TestApprovalManagerDenyDoesNotExecute(t *testing.T) {
	st := openFeishuApprovalTestStore(t)
	sender := &fakeApprovalSender{}
	manager := newTestApprovalManager(t, st, sender)
	executor := &fakeApprovalExecutor{name: "feishu_docs_create"}
	if err := manager.registerExecutor(executor); err != nil {
		t.Fatalf("registerExecutor returned error: %v", err)
	}
	pending := requestTestApproval(t, manager)

	resp, err := manager.HandleCardAction(t.Context(), approvalCardEvent(pending.RequestID, approvalCardActionReject, "ou_requester", "oc_chat", "om_card", "标题不够清楚"))
	if err != nil {
		t.Fatalf("HandleCardAction returned error: %v", err)
	}
	if resp == nil || resp.Toast == nil || resp.Toast.Type != "success" || !strings.Contains(resp.Toast.Content, "不会执行") || !approvalResponseCardContains(t, resp, "已拒绝授权") {
		t.Fatalf("response = %#v, want denied toast and immediate terminal card", resp)
	}
	waitForApprovalState(t, st, pending.RequestID, store.ToolApprovalStateDenied)
	_, updates, _ := sender.snapshot()
	if len(updates) != 0 {
		t.Fatalf("card updates = %#v, want denial handled in callback response", updates)
	}
	if calls, _ := executor.snapshot(); calls != 0 {
		t.Fatalf("executor calls = %d, want 0", calls)
	}
	record, err := st.GetToolApproval(pending.RequestID, "feishu:cli_test")
	if err != nil {
		t.Fatalf("GetToolApproval returned error: %v", err)
	}
	if record.Payload != "" {
		t.Fatalf("denied approval retained payload: %#v", record)
	}
	workflowResult, err := st.GetWorkflowResult(pending.RequestID, "feishu:cli_test")
	if err != nil || workflowResult.State != store.WorkflowResultStateDenied || !strings.Contains(string(workflowResult.Payload), `"status":"denied"`) {
		t.Fatalf("denied workflow result = %#v err=%v", workflowResult, err)
	}
}

func TestApprovalManagerFailsRequestWhenCardCannotBeSent(t *testing.T) {
	st := openFeishuApprovalTestStore(t)
	sender := &fakeApprovalSender{createErr: errors.New("send failed")}
	manager := newTestApprovalManager(t, st, sender)
	recordingStore := &recordingToolApprovalStore{toolApprovalStore: manager.store}
	manager.store = recordingStore
	if err := manager.registerExecutor(&fakeApprovalExecutor{name: "feishu_docs_create"}); err != nil {
		t.Fatalf("registerExecutor returned error: %v", err)
	}

	_, err := manager.CheckOrRequest(approvalRequestContext(), feishutools.OperationApprovalRequest{
		ToolName:      "feishu_docs_create",
		ActionKey:     "create",
		ResourceType:  "folder",
		ResourceToken: "fld_token",
		Payload:       json.RawMessage(`{"title":"Quarterly plan"}`),
	})
	if err == nil || !strings.Contains(err.Error(), "send feishu tool approval card") {
		t.Fatalf("CheckOrRequest error = %v, want card send error", err)
	}
	if len(recordingStore.canceled) != 1 || recordingStore.canceled[0] == "" {
		t.Fatalf("canceled continuations = %#v, want one request", recordingStore.canceled)
	}
	continuation, loadErr := st.GetWorkflowContinuation(recordingStore.canceled[0], "feishu:cli_test")
	if loadErr != nil || continuation.State != store.WorkflowContinuationStateCanceled {
		t.Fatalf("failed approval continuation = %#v err=%v, want canceled", continuation, loadErr)
	}
}

func TestConfigureEventHandlersRegistersCardApprovalCallback(t *testing.T) {
	st := openFeishuApprovalTestStore(t)
	sender := &fakeApprovalSender{}
	manager := newTestApprovalManager(t, st, sender)
	b := &bot{approvals: manager, cards: manager.cards, eventCommands: map[string][]string{}}

	_, registered, err := b.configureEventHandlers(dispatcher.NewEventDispatcher("", ""), nil)
	if err != nil {
		t.Fatalf("configureEventHandlers returned error: %v", err)
	}
	if got, want := strings.Join(registered, ", "), "im.message.receive_v1, card.action.trigger"; got != want {
		t.Fatalf("registered events = %q, want %q", got, want)
	}
}

func openFeishuApprovalTestStore(t *testing.T) *store.Store {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	st, err := store.Open(store.PlatformFeishu)
	if err != nil {
		t.Fatalf("store.Open returned error: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Fatalf("store.Close returned error: %v", err)
		}
	})
	return st
}

func newTestApprovalManager(t *testing.T, st *store.Store, sender *fakeApprovalSender) *operationApprovalService {
	t.Helper()
	cards, err := newCardService(sender)
	if err != nil {
		t.Fatalf("newCardService returned error: %v", err)
	}
	manager, err := newOperationApprovalService(context.Background(), st, store.Account{
		ID:       "feishu:cli_test",
		Name:     "fsbot",
		Platform: store.PlatformFeishu,
	}, cards)
	if err != nil {
		t.Fatalf("newOperationApprovalService returned error: %v", err)
	}
	t.Cleanup(manager.tasks.CloseAndWait)
	now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	manager.now = func() time.Time { return now }
	return manager
}

func approvalRequestContext() context.Context {
	return workflowRequestContext("feishu_docs_create")
}

func resourceAccessRequestContext() context.Context {
	return workflowRequestContext(feishutools.ResourceAccessToolName)
}

func workflowRequestContext(toolName string) context.Context {
	return tooltypes.WithExecutionContext(context.Background(), tooltypes.ExecutionContext{
		Platform:             store.PlatformFeishu,
		AccountID:            "feishu:cli_test",
		UserKey:              "feishu:ou_requester",
		SessionID:            "session-work",
		ChatID:               "oc_chat",
		SourceMessageID:      "om_source",
		ActorOpenID:          "ou_requester",
		ActorUserID:          "u_requester",
		ChatIsGroup:          true,
		ConversationRevision: 7,
		TurnID:               "turn-test",
		ToolCallID:           "call-test",
		ToolName:             toolName,
	})
}

func requestTestApproval(t *testing.T, manager *operationApprovalService) feishutools.OperationApprovalResult {
	t.Helper()
	pending, err := manager.CheckOrRequest(approvalRequestContext(), testOperationApprovalRequest())
	if err != nil {
		t.Fatalf("CheckOrRequest returned error: %v", err)
	}
	if pending.Status != feishutools.OperationApprovalStatusPending {
		t.Fatalf("CheckOrRequest result = %#v, want pending", pending)
	}
	return pending
}

func testOperationApprovalRequest() feishutools.OperationApprovalRequest {
	return feishutools.OperationApprovalRequest{
		ToolName:      "feishu_docs_create",
		ActionKey:     "create",
		ResourceType:  "folder",
		ResourceToken: "fld_token",
		Fields:        []feishutools.ApprovalField{{Label: "文档标题", Value: "Quarterly plan"}},
		Payload:       json.RawMessage(`{"title":"Quarterly plan"}`),
	}
}

func approvalCardEvent(requestID, action, openID, chatID, messageID, reason string) *callback.CardActionTriggerEvent {
	userID := "u_requester"
	if openID != "ou_requester" {
		userID = "u_other"
	}
	return &callback.CardActionTriggerEvent{
		Event: &callback.CardActionTriggerRequest{
			Operator: &callback.Operator{OpenID: openID, UserID: &userID},
			Token:    "c_callback",
			Context:  &callback.Context{OpenChatID: chatID, OpenMessageID: messageID},
			Action: &callback.CallBackAction{
				Value: map[string]interface{}{
					"kind":       approvalCardActionKind,
					"request_id": requestID,
					"action":     action,
				},
				FormValue: map[string]interface{}{"reason": reason},
			},
		},
	}
}

func approvalResponseCardContains(t *testing.T, response *callback.CardActionTriggerResponse, want string) bool {
	t.Helper()
	if response == nil || response.Card == nil || response.Card.Type != "raw" || response.Card.Data == nil {
		return false
	}
	data, err := json.Marshal(response.Card.Data)
	if err != nil {
		t.Fatalf("marshal approval response card: %v", err)
	}
	return strings.Contains(string(data), want)
}

func assertApprovalCardActions(t *testing.T, cardJSON, requestID string) {
	t.Helper()
	var card map[string]interface{}
	if err := json.Unmarshal([]byte(cardJSON), &card); err != nil {
		t.Fatalf("unmarshal approval card: %v", err)
	}
	if card["schema"] != "2.0" {
		t.Fatalf("card schema = %#v, want 2.0", card["schema"])
	}
	config, _ := card["config"].(map[string]interface{})
	if config["update_multi"] != true {
		t.Fatalf("card config = %#v, want shared Card V2 update_multi=true", config)
	}
	header, _ := card["header"].(map[string]interface{})
	title, _ := header["title"].(map[string]interface{})
	tags, _ := header["text_tag_list"].([]interface{})
	if title["content"] != "权限申请审批" || header["template"] != "blue" || header["padding"] != "12px 8px 12px 8px" || len(tags) != 1 {
		t.Fatalf("card header = %#v", header)
	}
	body, _ := card["body"].(map[string]interface{})
	elements, _ := body["elements"].([]interface{})
	if len(elements) != 1 {
		t.Fatalf("card elements = %#v, want one form", elements)
	}
	form, _ := elements[0].(map[string]interface{})
	if form["tag"] != "form" || form["name"] != "Form_msa8n85x" || form["padding"] != "4px 0px 4px 0px" {
		t.Fatalf("approval form = %#v", form)
	}
	formElements, _ := form["elements"].([]interface{})
	if len(formElements) != 4 {
		t.Fatalf("form elements = %#v, want markdown, two approvals, and reject row", formElements)
	}
	markdown, _ := formElements[0].(map[string]interface{})
	if markdown["tag"] != "markdown" || markdown["element_id"] != "SnLSJiYBwzi2qzhJsFPP" || !strings.Contains(markdown["content"].(string), "永久允许") {
		t.Fatalf("approval markdown = %#v", markdown)
	}
	assertApprovalFormButton(t, formElements[1], requestID, approvalCardActionApproveOnce, "同意一次", "primary_filled", "Button_ruivkstdali")
	assertApprovalFormButton(t, formElements[2], requestID, approvalCardActionApproveAll, "全部同意", "primary", "Button_zrwjazvut3f")

	reasonRow, _ := formElements[3].(map[string]interface{})
	columns, _ := reasonRow["columns"].([]interface{})
	if reasonRow["tag"] != "column_set" || len(columns) != 2 {
		t.Fatalf("reason row = %#v", reasonRow)
	}
	inputColumn, _ := columns[0].(map[string]interface{})
	inputElements, _ := inputColumn["elements"].([]interface{})
	input, _ := inputElements[0].(map[string]interface{})
	placeholder, _ := input["placeholder"].(map[string]interface{})
	if input["tag"] != "input" || input["name"] != "reason" || placeholder["content"] != "请输入建议" {
		t.Fatalf("reason input = %#v", input)
	}
	rejectColumn, _ := columns[1].(map[string]interface{})
	rejectElements, _ := rejectColumn["elements"].([]interface{})
	assertApprovalFormButton(t, rejectElements[0], requestID, approvalCardActionReject, "拒绝", "danger", "Button_k7l2449r9dj")
}

func assertApprovalFormButton(t *testing.T, raw interface{}, requestID, action, text, buttonType, name string) {
	t.Helper()
	button, _ := raw.(map[string]interface{})
	label, _ := button["text"].(map[string]interface{})
	if button["tag"] != "button" || button["width"] != "default" || button["size"] != "medium" || button["type"] != buttonType || button["name"] != name || button["form_action_type"] != "submit" || label["content"] != text {
		t.Fatalf("approval button = %#v", button)
	}
	behaviors, _ := button["behaviors"].([]interface{})
	if len(behaviors) != 1 {
		t.Fatalf("approval button behaviors = %#v", behaviors)
	}
	behavior, _ := behaviors[0].(map[string]interface{})
	value, _ := behavior["value"].(map[string]interface{})
	if behavior["type"] != "callback" || value["request_id"] != requestID || value["action"] != action || value["kind"] != approvalCardActionKind {
		t.Fatalf("approval button callback = %#v", behavior)
	}
}

func waitForApprovalState(t *testing.T, st *store.Store, requestID, want string) {
	t.Helper()
	deadline := time.After(time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		record, err := st.GetToolApproval(requestID, "feishu:cli_test")
		if err == nil && record.State == want {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("approval state = %#v error=%v, want %s", record, err, want)
		case <-ticker.C:
		}
	}
}

func waitForApprovalUpdates(t *testing.T, sender *fakeApprovalSender, updateCount int) {
	t.Helper()
	deadline := time.After(time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		_, updates, _ := sender.snapshot()
		if len(updates) >= updateCount {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("approval card updates = %d, want at least %d", len(updates), updateCount)
		case <-ticker.C:
		}
	}
}

func waitForApprovalWorkflowResult(t *testing.T, st *store.Store, requestID, want string) store.WorkflowResult {
	t.Helper()
	deadline := time.After(time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		result, err := st.GetWorkflowResult(requestID, "feishu:cli_test")
		if err == nil && result.State == want {
			return result
		}
		select {
		case <-deadline:
			t.Fatalf("workflow result = %#v error=%v, want %s", result, err, want)
		case <-ticker.C:
		}
	}
}
