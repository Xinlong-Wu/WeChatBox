package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"lingobridge/internal/config"
	"lingobridge/internal/llm"
	"lingobridge/internal/store"
	tooltypes "lingobridge/internal/tools"
)

type failingWorkflowResumeSender struct {
	sent []OutboundMessage
	err  error
}

func (f *failingWorkflowResumeSender) Send(_ context.Context, message OutboundMessage) error {
	f.sent = append(f.sent, message)
	return f.err
}

func (f *failingWorkflowResumeSender) StartTyping(context.Context) func() {
	return func() {}
}

type sequencedWorkflowResumeSender struct {
	sent []OutboundMessage
	errs []error
}

func (s *sequencedWorkflowResumeSender) Send(_ context.Context, message OutboundMessage) error {
	s.sent = append(s.sent, message)
	index := len(s.sent) - 1
	if index < len(s.errs) {
		return s.errs[index]
	}
	return nil
}

func (s *sequencedWorkflowResumeSender) StartTyping(context.Context) func() {
	return func() {}
}

func workflowResumeTextMessages(messages []OutboundMessage) []string {
	texts := make([]string, 0, len(messages))
	for _, message := range messages {
		if message.Text != "" {
			texts = append(texts, message.Text)
		}
	}
	return texts
}

func TestResumeWorkflowInjectsAuthenticatedEventAndSavesTargetSession(t *testing.T) {
	sessions := &fakeSessions{
		sess: &store.Session{ID: "current-other", UserID: "feishu:ou_requester"},
		conv: &store.Conversation{Revision: 4, Messages: []store.Message{{Role: "user", Content: "original task"}}},
	}
	client := &fakeLLM{resp: llm.Response{Text: "continued after authorization"}}
	bot := testBot(sessions, client)
	sender := &fakeSender{}
	request := testWorkflowResumeRequest()

	if err := bot.ResumeWorkflow(t.Context(), request, sender); err != nil {
		t.Fatalf("ResumeWorkflow returned error: %v", err)
	}
	if len(client.systemPrompts) != 1 || len(client.messages) == 0 {
		t.Fatalf("LLM prompts/messages = %#v/%#v", client.systemPrompts, client.messages)
	}
	event := client.messages[len(client.messages)-1]
	if event.Internal == nil || event.Internal.Kind != workflowResultInternalEventKind || event.Internal.ID != request.Continuation.RequestID {
		t.Fatalf("workflow event marker = %#v", event.Internal)
	}
	start := strings.IndexByte(event.Content, '{')
	end := strings.LastIndexByte(event.Content, '}')
	if start < 0 || end < start {
		t.Fatalf("workflow event content = %q", event.Content)
	}
	var envelope map[string]any
	if err := json.Unmarshal([]byte(event.Content[start:end+1]), &envelope); err != nil {
		t.Fatalf("decode workflow event envelope: %v", err)
	}
	attestation, _ := envelope["attestation"].(string)
	if len(sender.sent) != 1 {
		t.Fatalf("sent = %#v, want one response", sender.sent)
	}
	if attestation == "" || !strings.Contains(client.systemPrompts[0], attestation) || strings.Contains(sender.sent[0].Text, attestation) {
		t.Fatalf("attestation prompt/output mismatch prompt=%q output=%#v", client.systemPrompts[0], sender.sent)
	}
	if sessions.saved == nil || sessions.saved.Revision != 5 || len(sessions.saved.Messages) != 3 {
		t.Fatalf("saved conversation = %#v", sessions.saved)
	}
	if got := sessions.saved.Messages[1].Internal; got == nil || got.ID != request.Continuation.RequestID {
		t.Fatalf("saved internal event = %#v", got)
	} else if got.CommittedRevision != 5 {
		t.Fatalf("saved internal event revision = %d, want 5", got.CommittedRevision)
	}
	if len(sender.sent) != 1 || sender.sent[0].Text != "continued after authorization" {
		t.Fatalf("sent = %#v", sender.sent)
	}
}

func TestResumeWorkflowRejectsArchivedTargetSession(t *testing.T) {
	sessions := &fakeSessions{
		sessionErr: store.ErrSessionNotFound,
		conv:       &store.Conversation{Revision: 4, Messages: []store.Message{{Role: "user", Content: "archived task"}}},
	}
	client := &fakeLLM{resp: llm.Response{Text: "must not resume archived session"}}
	bot := testBot(sessions, client)
	err := bot.ResumeWorkflow(t.Context(), testWorkflowResumeRequest(), &fakeSender{})
	if !errors.Is(err, store.ErrSessionNotFound) {
		t.Fatalf("ResumeWorkflow error = %v, want ErrSessionNotFound", err)
	}
	if client.called || len(client.messages) != 0 || sessions.saved != nil {
		t.Fatalf("archived workflow was resumed client=%#v saved=%#v", client, sessions.saved)
	}
}

func TestResumeWorkflowPreservesAccountNameForProviderToolScope(t *testing.T) {
	sessions := &fakeSessions{conv: &store.Conversation{Revision: 4}}
	client := &fakeToolLLM{
		callTurns: [][]tooltypes.Call{{{
			ID:        "call_account_tool",
			Name:      "mcp_account_tool",
			Arguments: json.RawMessage(`{}`),
		}}},
		finalText: "account-scoped workflow resumed",
	}
	tool := &fakeTool{spec: tooltypes.Spec{Name: "mcp_account_tool"}, result: `{"status":"ok"}`}
	provider := &fakeToolProvider{tools: []tooltypes.Tool{tool}}
	bot := &Bot{
		Sessions:       sessions,
		LLMConfig:      testLLMConfig(),
		LLMClients:     map[string]llm.Client{},
		NewLLM:         func(config.ResolvedModel) llm.Client { return client },
		TextChunkLimit: testTextChunkLimit,
		ToolProvider:   provider,
	}
	request := testWorkflowResumeRequest()
	request.AccountName = "fsbot"

	if err := bot.ResumeWorkflow(t.Context(), request, &fakeSender{}); err != nil {
		t.Fatalf("ResumeWorkflow returned error: %v", err)
	}
	if len(provider.scopes) != 1 || provider.scopes[0].Platform != store.PlatformFeishu ||
		provider.scopes[0].AccountID != request.Continuation.AccountID || provider.scopes[0].AccountName != "fsbot" {
		t.Fatalf("workflow provider scopes = %#v", provider.scopes)
	}
	if len(tool.executions) != 1 {
		t.Fatalf("account-scoped workflow tool executions = %#v", tool.executions)
	}
}

func TestResumeWorkflowReplaysCommittedResponseWithoutCallingModelAgain(t *testing.T) {
	sessions := &fakeSessions{conv: &store.Conversation{Revision: 2}}
	client := &fakeLLM{resp: llm.Response{Text: "resume once"}}
	bot := testBot(sessions, client)
	request := testWorkflowResumeRequest()
	firstSender := &fakeSender{}
	if err := bot.ResumeWorkflow(t.Context(), request, firstSender); err != nil {
		t.Fatalf("first ResumeWorkflow returned error: %v", err)
	}
	if len(client.systemPrompts) != 1 {
		t.Fatalf("first LLM calls = %d, want 1", len(client.systemPrompts))
	}
	sessions.conv.Messages = append(sessions.conv.Messages,
		store.Message{Role: "user", Content: "later message"},
		store.Message{Role: "assistant", Content: "later response"},
	)
	sessions.conv.Revision++

	secondSender := &fakeSender{}
	if err := bot.ResumeWorkflow(t.Context(), request, secondSender); err != nil {
		t.Fatalf("second ResumeWorkflow returned error: %v", err)
	}
	if len(client.systemPrompts) != 1 {
		t.Fatalf("LLM calls after replay = %d, want still 1", len(client.systemPrompts))
	}
	if len(secondSender.sent) != 1 || secondSender.sent[0].Text != "resume once" {
		t.Fatalf("replayed response = %#v", secondSender.sent)
	}
}

func TestResumeWorkflowReplaysCommittedResponseAfterConversationCompaction(t *testing.T) {
	sessions := &fakeSessions{conv: &store.Conversation{Revision: 2}}
	client := &fakeLLM{resp: llm.Response{Text: "resume once before compaction"}}
	bot := testBot(sessions, client)
	request := testWorkflowResumeRequest()
	if err := bot.ResumeWorkflow(t.Context(), request, &fakeSender{}); err != nil {
		t.Fatalf("first ResumeWorkflow returned error: %v", err)
	}
	for index := 0; index < nativeContextKeepRecentMessages+2; index++ {
		role := "user"
		if index%2 == 1 {
			role = "assistant"
		}
		sessions.conv.Messages = append(sessions.conv.Messages, store.Message{
			Role:    role,
			Content: fmt.Sprintf("later message %d", index),
		})
	}
	sessions.conv.Messages = retainRecentMessages(sessions.conv.Messages, nativeContextKeepRecentMessages)
	sessions.conv.Revision++

	secondSender := &fakeSender{}
	if err := bot.ResumeWorkflow(t.Context(), request, secondSender); err != nil {
		t.Fatalf("ResumeWorkflow after compaction returned error: %v", err)
	}
	if len(client.systemPrompts) != 1 {
		t.Fatalf("LLM calls after compaction replay = %d, want still 1", len(client.systemPrompts))
	}
	if len(secondSender.sent) != 1 || secondSender.sent[0].Text != "resume once before compaction" {
		t.Fatalf("compacted replay response = %#v", secondSender.sent)
	}
}

func TestResumeWorkflowDeliveryRetryDoesNotCallModelAgain(t *testing.T) {
	sessions := &fakeSessions{conv: &store.Conversation{Revision: 2}}
	client := &fakeLLM{resp: llm.Response{Text: "persist before delivery"}}
	bot := testBot(sessions, client)
	request := testWorkflowResumeRequest()
	firstSender := &failingWorkflowResumeSender{err: errors.New("send failed")}
	if err := bot.ResumeWorkflow(t.Context(), request, firstSender); err == nil || !strings.Contains(err.Error(), "deliver workflow resume response") {
		t.Fatalf("first ResumeWorkflow error = %v", err)
	}
	if sessions.saved == nil || len(client.systemPrompts) != 1 {
		t.Fatalf("saved/model calls = %#v/%d", sessions.saved, len(client.systemPrompts))
	}

	secondSender := &fakeSender{}
	if err := bot.ResumeWorkflow(t.Context(), request, secondSender); err != nil {
		t.Fatalf("retry ResumeWorkflow returned error: %v", err)
	}
	if len(client.systemPrompts) != 1 {
		t.Fatalf("LLM calls after delivery retry = %d, want still 1", len(client.systemPrompts))
	}
	if len(secondSender.sent) != 1 || secondSender.sent[0].Text != "persist before delivery" {
		t.Fatalf("retried delivery = %#v", secondSender.sent)
	}
}

func TestResumeWorkflowDeliveryRetryPreservesOriginalChunkBoundaries(t *testing.T) {
	sessions := &fakeSessions{conv: &store.Conversation{Revision: 2}}
	client := &fakeLLM{resp: llm.Response{Text: "abcdefghij"}}
	bot := testBot(sessions, client)
	bot.TextChunkLimit = 5
	request := testWorkflowResumeRequest()
	firstSender := &sequencedWorkflowResumeSender{errs: []error{nil, errors.New("second chunk failed")}}

	if err := bot.ResumeWorkflow(t.Context(), request, firstSender); err == nil || !strings.Contains(err.Error(), "deliver workflow resume response") {
		t.Fatalf("first ResumeWorkflow error = %v", err)
	}
	if got := workflowResumeTextMessages(firstSender.sent); len(got) != 2 || got[0] != "abcde" || got[1] != "fghij" {
		t.Fatalf("first delivery chunks = %#v", got)
	}

	bot.TextChunkLimit = 20
	secondSender := &fakeSender{}
	if err := bot.ResumeWorkflow(t.Context(), request, secondSender); err != nil {
		t.Fatalf("retry ResumeWorkflow returned error: %v", err)
	}
	if got := workflowResumeTextMessages(secondSender.sent); len(got) != 2 || got[0] != "abcde" || got[1] != "fghij" {
		t.Fatalf("retried delivery chunks = %#v, want the original UUID-to-text mapping", got)
	}
	if len(client.systemPrompts) != 1 {
		t.Fatalf("LLM calls after delivery retry = %d, want still 1", len(client.systemPrompts))
	}
}

func TestRememberWorkflowResumeReceiptRetainsOnlyChainedWorkflowTraceIdentity(t *testing.T) {
	conv := &store.Conversation{}
	event := store.Message{
		Role: "user",
		Internal: &store.InternalEvent{
			Kind:              workflowResultInternalEventKind,
			ID:                "req_resume",
			CommittedRevision: 3,
		},
	}
	assistant := store.Message{
		Role:    "assistant",
		Content: "continued",
		Attachments: []store.Attachment{{
			Type: "image", MIMEType: "image/png", RefProvider: "openai", RefType: "file", RefID: "file_1",
		}},
		ToolTraces: []store.ToolTrace{
			{
				CallID: "call_regular", Name: "regular_tool", Status: "ok",
				Arguments: `{"private":"argument"}`, Result: `{"private":"result"}`, DurationMillis: 12,
			},
			{
				CallID: "call_next", Name: "feishu_docs_request_access", Status: "ok",
				Arguments: `{"private":"next argument"}`, Result: `{"status":"pending"}`,
				Error: "private error", PendingWorkflowID: "req_next", DurationMillis: 34,
			},
		},
	}

	rememberWorkflowResumeReceipt(conv, event, assistant, []string{"continued"})

	receipt, ok := conv.WorkflowResumeReceipts["req_resume"]
	if !ok {
		t.Fatal("workflow resume receipt was not recorded")
	}
	if receipt.Assistant.Content != assistant.Content || len(receipt.Assistant.Attachments) != 1 {
		t.Fatalf("receipt delivery payload = %#v, want original content and attachments", receipt.Assistant)
	}
	if len(receipt.Assistant.ToolTraces) != 1 {
		t.Fatalf("receipt tool traces = %#v, want only the chained workflow trace", receipt.Assistant.ToolTraces)
	}
	trace := receipt.Assistant.ToolTraces[0]
	if trace.CallID != "call_next" || trace.Name != "feishu_docs_request_access" || trace.PendingWorkflowID != "req_next" {
		t.Fatalf("receipt chained workflow trace = %#v", trace)
	}
	if trace.Arguments != "" || trace.Result != "" || trace.Error != "" || trace.Status != "" || trace.DurationMillis != 0 {
		t.Fatalf("receipt retained nonessential tool trace data = %#v", trace)
	}
	if got := pendingWorkflowIDsFromTraces(receipt.Assistant.ToolTraces); len(got) != 1 || got[0] != "req_next" {
		t.Fatalf("receipt chained workflow ids = %#v, want req_next", got)
	}
}

func TestResumeWorkflowPreservesTrustedContextAndCommitsChainedWorkflow(t *testing.T) {
	sessions := &fakeSessions{conv: &store.Conversation{Revision: 9}}
	client := &fakeToolLLM{
		callTurns: [][]tooltypes.Call{{{
			ID:        "call_next",
			Name:      "next_tool",
			Arguments: json.RawMessage(`{}`),
		}}},
		finalText: "next authorization requested",
	}
	tool := &fakeTool{
		spec:              tooltypes.Spec{Name: "next_tool"},
		result:            `{"status":"pending"}`,
		pendingWorkflowID: "req_next",
	}
	workflows := &fakeWorkflowContinuationManager{}
	bot := &Bot{
		Sessions:       sessions,
		LLMConfig:      testLLMConfig(),
		LLMClients:     map[string]llm.Client{},
		NewLLM:         func(config.ResolvedModel) llm.Client { return client },
		TextChunkLimit: testTextChunkLimit,
		Workflows:      workflows,
	}
	request := testWorkflowResumeRequest()
	request.Continuation.ChatIsGroup = true
	request.Tools = []tooltypes.Tool{tool}

	if err := bot.ResumeWorkflow(t.Context(), request, &fakeSender{}); err != nil {
		t.Fatalf("ResumeWorkflow returned error: %v", err)
	}
	if len(tool.executions) != 1 {
		t.Fatalf("tool executions = %#v", tool.executions)
	}
	execution := tool.executions[0]
	if execution.AccountID != request.Continuation.AccountID || execution.UserKey != request.Continuation.UserKey ||
		execution.SessionID != request.Continuation.SessionID || execution.ChatID != request.Continuation.ChatID ||
		execution.ActorOpenID != request.Continuation.ActorOpenID || !execution.ChatIsGroup || execution.ConversationRevision != 9 {
		t.Fatalf("trusted resumed execution = %#v", execution)
	}
	if len(workflows.commits) != 1 || workflows.commits[0].requestID != "req_next" || workflows.commits[0].committedRevision != 10 {
		t.Fatalf("chained workflow commits = %#v", workflows.commits)
	}
	assistant := sessions.saved.Messages[len(sessions.saved.Messages)-1]
	if len(assistant.ToolTraces) != 1 || assistant.ToolTraces[0].PendingWorkflowID != "req_next" {
		t.Fatalf("saved chained workflow trace = %#v", assistant.ToolTraces)
	}
	sessions.conv.Messages = append(sessions.conv.Messages,
		store.Message{Role: "user", Content: "later message"},
		store.Message{Role: "assistant", Content: "later response"},
	)
	sessions.conv.Revision++
	if err := bot.ResumeWorkflow(t.Context(), request, &fakeSender{}); err != nil {
		t.Fatalf("replayed ResumeWorkflow returned error: %v", err)
	}
	if len(workflows.commits) != 2 || workflows.commits[1].committedRevision != 10 {
		t.Fatalf("replayed chained workflow commits = %#v, want original revision 10", workflows.commits)
	}
}

func testWorkflowResumeRequest() WorkflowResumeRequest {
	continuation := store.WorkflowContinuation{
		RequestID:         "req_resume",
		AccountID:         "feishu:cli_test",
		Platform:          store.PlatformFeishu,
		UserKey:           "feishu:ou_requester",
		SessionID:         "session-work",
		ChatID:            "oc_chat",
		SourceMessageID:   "om_source",
		ActorOpenID:       "ou_requester",
		ActorUserID:       "u_requester",
		CommittedRevision: 4,
		ToolCallID:        "call_origin",
		ToolName:          "feishu_docs_request_access",
		State:             store.WorkflowContinuationStateProcessing,
	}
	return WorkflowResumeRequest{
		Continuation: continuation,
		AccountName:  "fsbot",
		Result: store.WorkflowResult{
			RequestID: continuation.RequestID,
			AccountID: continuation.AccountID,
			State:     store.WorkflowResultStateSucceeded,
			Payload:   json.RawMessage(`{"status":"granted","permission":"write"}`),
		},
	}
}
