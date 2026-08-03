package core

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"lingobridge/internal/commands"
	"lingobridge/internal/config"
	"lingobridge/internal/llm"
	"lingobridge/internal/store"
	tooltypes "lingobridge/internal/tools"
)

const testTextChunkLimit = 4000

func testLLMConfig() config.LLMConfig {
	return config.LLMConfig{
		DefaultModel: "deepseek",
		Models: map[string]config.LLMModelConfig{
			"deepseek": {Provider: "openai", BaseURL: "https://llm.test", APIKey: "key", ID: "model"},
		},
		SystemPrompt: "system",
	}
}

type fakeSessions struct {
	sess       *store.Session
	conv       *store.Conversation
	saved      *store.Conversation
	model      string
	commandHit bool
}

func (f *fakeSessions) GetOrCreateCurrentSession(userID string) (*store.Session, error) {
	if f.sess != nil {
		return f.sess, nil
	}
	return &store.Session{ID: "session", UserID: userID, Name: "default", Current: true}, nil
}

func (f *fakeSessions) LoadHistory(userID, sessionID string) (*store.Conversation, error) {
	if f.conv != nil {
		return f.conv, nil
	}
	return &store.Conversation{}, nil
}

func (f *fakeSessions) SaveHistoryCAS(userID, sessionID string, expectedRevision int64, conv *store.Conversation) (int64, error) {
	conv.Revision = expectedRevision + 1
	f.saved = conv
	f.conv = conv
	return conv.Revision, nil
}

func (f *fakeSessions) CurrentSession(userID string) (*store.Session, error) {
	f.commandHit = true
	return f.GetOrCreateCurrentSession(userID)
}

func (f *fakeSessions) CreateSession(userID, name string) (*store.Session, error) {
	return &store.Session{ID: "new", UserID: userID, Name: name, Current: true}, nil
}

func (f *fakeSessions) ListSessions(userID string) ([]store.Session, error) { return nil, nil }
func (f *fakeSessions) SwitchSession(userID, sessionName string) (*store.Session, error) {
	return &store.Session{ID: "switched", UserID: userID, Name: sessionName, Current: true}, nil
}
func (f *fakeSessions) RenameCurrentSession(userID, newName string) (*store.Session, error) {
	return &store.Session{ID: "current", UserID: userID, Name: newName, Current: true}, nil
}
func (f *fakeSessions) ArchiveSession(userID, sessionName string) (*store.ArchiveResult, error) {
	return &store.ArchiveResult{Archived: store.Session{Name: sessionName}}, nil
}
func (f *fakeSessions) ClearSession(userID string) (*store.Session, error) {
	return &store.Session{ID: "cleared", UserID: userID, Name: "session-1", Current: true}, nil
}
func (f *fakeSessions) CurrentModel(userID, sessionID string) (string, error) {
	if f.model != "" {
		return f.model, nil
	}
	return "deepseek", nil
}
func (f *fakeSessions) SetModel(userID, sessionID, modelName string) error { return nil }
func (f *fakeSessions) DefaultModelName() string                           { return "deepseek" }
func (f *fakeSessions) ListModels() []string                               { return []string{"deepseek"} }

type fakeLLM struct {
	called          bool
	preparedContent string
	messages        []store.Message
	resp            llm.Response
	prepareErr      error
	streamChunks    []string
	streamErrs      []error
	streamErr       error
	systemPrompts   []string
}

type blockingLaneLLM struct {
	entered chan struct{}
	release chan struct{}
	mu      sync.Mutex
	active  int
	max     int
	calls   int
}

func (b *blockingLaneLLM) PrepareUserMessage(content string, attachments []llm.InputAttachment) (store.Message, error) {
	return store.Message{Role: "user", Content: content}, nil
}

func (b *blockingLaneLLM) Chat(systemPrompt string, messages []store.Message) (llm.Response, error) {
	return b.ChatStream(systemPrompt, messages, nil)
}

func (b *blockingLaneLLM) ChatStream(systemPrompt string, messages []store.Message, onChunk func(chunk string) error) (llm.Response, error) {
	b.mu.Lock()
	b.active++
	b.calls++
	call := b.calls
	if b.active > b.max {
		b.max = b.active
	}
	b.mu.Unlock()
	b.entered <- struct{}{}
	<-b.release
	b.mu.Lock()
	b.active--
	b.mu.Unlock()
	return llm.Response{Text: "reply-" + strconv.Itoa(call)}, nil
}

func (b *blockingLaneLLM) AssistantMessage(resp llm.Response) (store.Message, error) {
	return store.Message{Role: "assistant", Content: resp.Text}, nil
}

func (b *blockingLaneLLM) snapshot() (calls, maxActive int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls, b.max
}

func (f *fakeLLM) PrepareUserMessage(content string, attachments []llm.InputAttachment) (store.Message, error) {
	f.preparedContent = content
	if f.prepareErr != nil {
		return store.Message{}, f.prepareErr
	}
	return store.Message{Role: "user", Content: content}, nil
}

func (f *fakeLLM) Chat(systemPrompt string, messages []store.Message) (llm.Response, error) {
	return f.ChatStream(systemPrompt, messages, nil)
}

func (f *fakeLLM) ChatStream(systemPrompt string, messages []store.Message, onChunk func(chunk string) error) (llm.Response, error) {
	f.called = true
	f.messages = messages
	f.systemPrompts = append(f.systemPrompts, systemPrompt)
	for _, chunk := range f.streamChunks {
		if onChunk != nil {
			if err := onChunk(chunk); err != nil {
				return llm.Response{}, err
			}
		}
	}
	if len(f.streamErrs) > 0 {
		err := f.streamErrs[0]
		f.streamErrs = f.streamErrs[1:]
		if err != nil {
			return llm.Response{}, err
		}
	}
	if f.streamErr != nil {
		return llm.Response{}, f.streamErr
	}
	if f.resp.Text == "" {
		f.resp.Text = "hello"
	}
	return f.resp, nil
}

func (f *fakeLLM) AssistantMessage(resp llm.Response) (store.Message, error) {
	return store.Message{Role: "assistant", Content: resp.Text}, nil
}

type fakeNativeLLM struct {
	fakeLLM
	compactedMessages []store.Message
	contextMessages   []store.Message
	context           store.ProviderContext
	compactErr        error
}

type fakeToolLLM struct {
	fakeLLM
	turns         int
	calls         []tooltypes.Call
	callTurns     [][]tooltypes.Call
	results       []tooltypes.Result
	toolSpecs     []tooltypes.Spec
	finalText     string
	providerCtx   store.ProviderContext
	systemPrompts []string
	turnErrs      map[int]error
	previousTurns []llm.ToolState
	resultTurns   [][]tooltypes.Result
}

func (f *fakeToolLLM) ChatStreamWithTools(systemPrompt string, messages []store.Message, providerContext store.ProviderContext, compact llm.CompactConfig, tools []tooltypes.Spec, previous llm.ToolState, results []tooltypes.Result, onChunk func(chunk string) error) (llm.ToolResponse, error) {
	f.called = true
	f.messages = messages
	f.providerCtx = providerContext
	f.toolSpecs = tools
	f.systemPrompts = append(f.systemPrompts, systemPrompt)
	f.results = append(f.results, results...)
	f.previousTurns = append(f.previousTurns, previous)
	f.resultTurns = append(f.resultTurns, append([]tooltypes.Result(nil), results...))
	turn := f.turns
	f.turns++
	if f.turnErrs != nil {
		if err := f.turnErrs[turn]; err != nil {
			return llm.ToolResponse{}, err
		}
	}
	var calls []tooltypes.Call
	switch {
	case len(f.callTurns) > 0:
		if turn < len(f.callTurns) {
			calls = f.callTurns[turn]
		}
	case turn == 0:
		calls = f.calls
	}
	if len(calls) > 0 {
		return llm.ToolResponse{
			ToolCalls: calls,
			ToolState: llm.ToolState{
				Provider: "fake",
				Endpoint: "tools",
				Items:    []json.RawMessage{json.RawMessage(`{"type":"function_call"}`)},
			},
		}, nil
	}
	text := f.finalText
	if text == "" {
		text = "tool final"
	}
	return llm.ToolResponse{Response: llm.Response{Text: text}}, nil
}

type fakeTool struct {
	spec       tooltypes.Spec
	result     string
	err        string
	block      bool
	calls      []tooltypes.Call
	executions []tooltypes.ExecutionContext
}

func (f *fakeTool) Spec() tooltypes.Spec {
	if f.spec.Name == "" {
		f.spec.Name = "fake_tool"
	}
	return f.spec
}

func (f *fakeTool) Execute(ctx context.Context, call tooltypes.Call) tooltypes.Result {
	f.calls = append(f.calls, call)
	execution, _ := tooltypes.ExecutionContextFromContext(ctx)
	f.executions = append(f.executions, execution)
	if f.block {
		<-ctx.Done()
		return tooltypes.Result{CallID: call.ID, Name: call.Name, Content: ctx.Err().Error(), IsError: true}
	}
	if f.err != "" {
		return tooltypes.Result{CallID: call.ID, Name: call.Name, Content: f.err, IsError: true}
	}
	return tooltypes.Result{CallID: call.ID, Name: call.Name, Content: f.result}
}

type cancelingTool struct {
	cancel func()
	calls  []tooltypes.Call
}

func (t *cancelingTool) Spec() tooltypes.Spec {
	return tooltypes.Spec{Name: "fake_tool"}
}

func (t *cancelingTool) Execute(ctx context.Context, call tooltypes.Call) tooltypes.Result {
	t.calls = append(t.calls, call)
	if t.cancel != nil {
		t.cancel()
	}
	<-ctx.Done()
	return tooltypes.Result{CallID: call.ID, Name: call.Name, Content: ctx.Err().Error(), IsError: true}
}

func makeToolCalls(count, offset int) []tooltypes.Call {
	calls := make([]tooltypes.Call, 0, count)
	for i := 0; i < count; i++ {
		calls = append(calls, tooltypes.Call{
			ID:        "call_" + strconv.Itoa(offset+i+1),
			Name:      "fake_tool",
			Arguments: json.RawMessage(`{}`),
		})
	}
	return calls
}

func TestSummarizeToolArgumentsForLogRedactsSensitiveKeys(t *testing.T) {
	got := summarizeToolArgumentsForLog(json.RawMessage(`{
		"query":"roadmap",
		"token":"secret-token",
		"nested":{"password":"secret-password","Authorization":"Bearer secret"},
		"items":[{"api_key":"secret-api-key"}]
	}`), 4096)
	for _, secret := range []string{"secret-token", "secret-password", "Bearer secret", "secret-api-key"} {
		if strings.Contains(got, secret) {
			t.Fatalf("summary contains secret %q: %s", secret, got)
		}
	}
	if !strings.Contains(got, `"query":"roadmap"`) || strings.Count(got, "[REDACTED]") != 4 {
		t.Fatalf("summary = %s, want query plus redactions", got)
	}
}

type fakeToolProvider struct {
	tools   []tooltypes.Tool
	options tooltypes.Options
	scopes  []tooltypes.Scope
}

func (f *fakeToolProvider) Resolve(scope tooltypes.Scope) tooltypes.Selection {
	f.scopes = append(f.scopes, scope)
	return tooltypes.Selection{Tools: f.tools, Options: f.options}
}

func (f *fakeNativeLLM) CompactContext(systemPrompt string, messages []store.Message, providerContext store.ProviderContext, compact llm.CompactConfig) (store.ProviderContext, error) {
	f.compactedMessages = append([]store.Message(nil), messages...)
	if f.compactErr != nil {
		return store.ProviderContext{}, f.compactErr
	}
	return store.ProviderContext{
		Provider: "openai",
		Endpoint: "responses",
		Items:    []json.RawMessage{json.RawMessage(`{"type":"compaction","content":"summary"}`)},
	}, nil
}

func (f *fakeNativeLLM) ChatStreamWithContext(systemPrompt string, messages []store.Message, providerContext store.ProviderContext, compact llm.CompactConfig, onChunk func(chunk string) error) (llm.Response, error) {
	f.called = true
	f.messages = messages
	f.contextMessages = append([]store.Message(nil), messages...)
	f.context = providerContext
	if f.resp.Text == "" {
		f.resp.Text = "hello"
	}
	return f.resp, nil
}

type fakeSender struct {
	sent             []OutboundMessage
	typing           int
	stopTyping       int
	compactStarts    []CompactNotice
	compactFinishes  []CompactNotice
	compactStartErr  error
	compactFinishErr error
}

func (f *fakeSender) Send(ctx context.Context, msg OutboundMessage) error {
	f.sent = append(f.sent, msg)
	return nil
}

func (f *fakeSender) StartTyping(ctx context.Context) func() {
	f.typing++
	return func() { f.stopTyping++ }
}

func (f *fakeSender) StartCompactNotice(ctx context.Context, notice CompactNotice) (CompactNoticeHandle, error) {
	f.compactStarts = append(f.compactStarts, notice)
	if f.compactStartErr != nil {
		return CompactNoticeHandle{}, f.compactStartErr
	}
	return CompactNoticeHandle{MessageID: "compact-notice"}, nil
}

func (f *fakeSender) FinishCompactNotice(ctx context.Context, handle CompactNoticeHandle, notice CompactNotice) error {
	f.compactFinishes = append(f.compactFinishes, notice)
	return f.compactFinishErr
}

type fakeStreamingSender struct {
	fakeSender
	stream  *fakeTextStream
	streams []*fakeTextStream
}

func (f *fakeStreamingSender) StartTextStream(ctx context.Context) (TextStream, error) {
	f.stream = &fakeTextStream{}
	f.streams = append(f.streams, f.stream)
	return f.stream, nil
}

type fakeTextStream struct {
	updates  []string
	finishes []string
}

func (f *fakeTextStream) Update(ctx context.Context, text string) error {
	f.updates = append(f.updates, text)
	return nil
}

func (f *fakeTextStream) Finish(ctx context.Context, text string) error {
	f.finishes = append(f.finishes, text)
	return nil
}

func TestHandleCommandDoesNotCallLLM(t *testing.T) {
	sessions := &fakeSessions{}
	client := &fakeLLM{}
	b := testBot(sessions, client)
	sender := &fakeSender{}

	if err := b.Handle(context.Background(), InboundMessage{UserKey: "user", CommandText: "/current", LLMText: "/current"}, sender); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if client.called {
		t.Fatal("LLM was called for command")
	}
	if len(sender.sent) != 1 || sender.sent[0].Text == "" {
		t.Fatalf("sent = %#v, want command text response", sender.sent)
	}
}

func TestHandleHelpIncludesToolSummaries(t *testing.T) {
	sessions := &fakeSessions{}
	client := &fakeLLM{}
	b := testBot(sessions, client)
	sender := &fakeSender{}
	tool := &fakeTool{
		spec: tooltypes.Spec{
			Name:        "feishu_docs_search",
			Description: "Search Feishu Docs and Wiki visible to the configured Feishu app.",
		},
	}

	if err := b.Handle(context.Background(), InboundMessage{UserKey: "user", CommandText: "/help", LLMText: "/help", Tools: []tooltypes.Tool{tool}}, sender); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if client.called {
		t.Fatal("LLM was called for help command")
	}
	if len(sender.sent) != 1 {
		t.Fatalf("sent = %#v, want one help message", sender.sent)
	}
	for _, want := range []string{"## 可用工具", "feishu_docs_search", "Search Feishu Docs"} {
		if !strings.Contains(sender.sent[0].Text, want) {
			t.Fatalf("help = %q, want %s", sender.sent[0].Text, want)
		}
	}
}

func TestHandleHelpIncludesToolProviderSummaries(t *testing.T) {
	sessions := &fakeSessions{}
	client := &fakeLLM{}
	b := testBot(sessions, client)
	b.ToolProvider = &fakeToolProvider{tools: []tooltypes.Tool{&fakeTool{
		spec: tooltypes.Spec{
			Name:        "mcp_files_read_file",
			Description: "Read a file through MCP.",
		},
	}}}
	sender := &fakeSender{}

	if err := b.Handle(context.Background(), InboundMessage{UserKey: "user", CommandText: "/help", LLMText: "/help"}, sender); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if len(sender.sent) != 1 || !strings.Contains(sender.sent[0].Text, "mcp_files_read_file") {
		t.Fatalf("help = %#v, want MCP tool summary", sender.sent)
	}
}

func TestHandleCommandPolicyDisablesCommand(t *testing.T) {
	sessions := &fakeSessions{}
	client := &fakeLLM{}
	b := testBot(sessions, client)
	sender := &fakeSender{}

	if err := b.Handle(context.Background(), InboundMessage{
		UserKey:       "user",
		CommandText:   "/model",
		LLMText:       "/model",
		CommandPolicy: commands.PolicyWithDisabled("/model"),
	}, sender); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if client.called {
		t.Fatal("LLM was called for disabled command")
	}
	if len(sender.sent) != 1 || sender.sent[0].Text == "" {
		t.Fatalf("sent = %#v, want unsupported command response", sender.sent)
	}
}

func TestHandleTextSavesConversationAndSendsReply(t *testing.T) {
	sessions := &fakeSessions{}
	client := &fakeLLM{}
	b := testBot(sessions, client)
	sender := &fakeSender{}

	if err := b.Handle(context.Background(), InboundMessage{UserKey: "user", CommandText: "hi", LLMText: "hi"}, sender); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if !client.called || client.preparedContent != "hi" {
		t.Fatalf("client called=%v prepared=%q", client.called, client.preparedContent)
	}
	if sessions.saved == nil || len(sessions.saved.Messages) != 2 {
		t.Fatalf("saved = %#v, want two history messages", sessions.saved)
	}
	if len(sender.sent) != 1 || sender.sent[0].Text != "hello" {
		t.Fatalf("sent = %#v, want hello", sender.sent)
	}
	if sender.typing != 1 || sender.stopTyping != 1 {
		t.Fatalf("typing start/stop = %d/%d, want 1/1", sender.typing, sender.stopTyping)
	}
}

func testMultiModelConfig() config.LLMConfig {
	return config.LLMConfig{
		DefaultModel: "deepseek",
		Models: map[string]config.LLMModelConfig{
			"deepseek": {Provider: "openai", BaseURL: "https://llm.test", APIKey: "key", ID: "model"},
			"claude":   {Provider: "anthropic", BaseURL: "https://anthropic.test", APIKey: "key", ID: "claude-model"},
		},
		SystemPrompt: "system",
	}
}

func TestHandleTextModelOverrideSelectsConfiguredModel(t *testing.T) {
	sessions := &fakeSessions{}
	client := &fakeLLM{}
	var resolved []string
	b := &Bot{
		Sessions:   sessions,
		LLMConfig:  testMultiModelConfig(),
		LLMClients: map[string]llm.Client{},
		NewLLM: func(m config.ResolvedModel) llm.Client {
			resolved = append(resolved, m.Name)
			return client
		},
		TextChunkLimit: testTextChunkLimit,
	}

	if err := b.Handle(context.Background(), InboundMessage{UserKey: "user", Model: "claude", LLMText: "hi"}, &fakeSender{}); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if len(resolved) != 1 || resolved[0] != "claude" {
		t.Fatalf("resolved models = %#v, want [claude]", resolved)
	}
}

func TestHandleTextModelOverrideFallsBackWhenUnknown(t *testing.T) {
	sessions := &fakeSessions{}
	client := &fakeLLM{}
	var resolved []string
	b := &Bot{
		Sessions:   sessions,
		LLMConfig:  testMultiModelConfig(),
		LLMClients: map[string]llm.Client{},
		NewLLM: func(m config.ResolvedModel) llm.Client {
			resolved = append(resolved, m.Name)
			return client
		},
		TextChunkLimit: testTextChunkLimit,
	}

	if err := b.Handle(context.Background(), InboundMessage{UserKey: "user", Model: "missing", LLMText: "hi"}, &fakeSender{}); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if len(resolved) != 1 || resolved[0] != "deepseek" {
		t.Fatalf("resolved models = %#v, want fallback [deepseek]", resolved)
	}
}

func TestHandleTextRetriesRetryableLLMTurn(t *testing.T) {
	withImmediateLLMRetry(t)
	sessions := &fakeSessions{}
	client := &fakeLLM{
		streamErrs: []error{
			retryableLLMHTTPError(),
			retryableLLMHTTPError(),
			nil,
		},
		resp: llm.Response{Text: "eventual answer"},
	}
	b := testBot(sessions, client)
	sender := &fakeSender{}

	if err := b.Handle(context.Background(), InboundMessage{UserKey: "user", CommandText: "hi", LLMText: "hi"}, sender); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if len(client.systemPrompts) != 3 {
		t.Fatalf("system prompts = %d, want 3 attempts", len(client.systemPrompts))
	}
	if sessions.saved == nil || len(sessions.saved.Messages) != 2 {
		t.Fatalf("saved = %#v, want one user and one assistant message", sessions.saved)
	}
	if len(sender.sent) != 1 || sender.sent[0].Text != "eventual answer" {
		t.Fatalf("sent = %#v, want eventual answer", sender.sent)
	}
}

func TestHandleTextDoesNotRetryNonRetryableLLMError(t *testing.T) {
	withImmediateLLMRetry(t)
	sessions := &fakeSessions{}
	client := &fakeLLM{streamErrs: []error{errors.New("bad request")}}
	b := testBot(sessions, client)

	err := b.Handle(context.Background(), InboundMessage{UserKey: "user", CommandText: "hi", LLMText: "hi"}, &fakeSender{})
	if err == nil || err.Error() != "bad request" {
		t.Fatalf("Handle error = %v, want bad request", err)
	}
	if len(client.systemPrompts) != 1 {
		t.Fatalf("system prompts = %d, want one attempt", len(client.systemPrompts))
	}
	if sessions.saved != nil {
		t.Fatalf("saved = %#v, want no saved history on failed LLM turn", sessions.saved)
	}
}

func TestHandleTextDoesNotRetryAfterStreamingChunk(t *testing.T) {
	withImmediateLLMRetry(t)
	sessions := &fakeSessions{}
	client := &fakeLLM{
		streamChunks: []string{"partial"},
		streamErrs:   []error{retryableLLMHTTPError()},
	}
	b := testBot(sessions, client)
	sender := &fakeStreamingSender{}

	err := b.Handle(context.Background(), InboundMessage{UserKey: "user", CommandText: "hi", LLMText: "hi"}, sender)
	if err == nil {
		t.Fatal("Handle returned nil, want retryable error after streamed chunk")
	}
	if len(client.systemPrompts) != 1 {
		t.Fatalf("system prompts = %d, want no retry after streamed chunk", len(client.systemPrompts))
	}
	if sender.stream == nil || len(sender.stream.updates) == 0 {
		t.Fatalf("stream = %#v, want partial update before failure", sender.stream)
	}
	if sessions.saved != nil {
		t.Fatalf("saved = %#v, want no saved history on failed streamed turn", sessions.saved)
	}
}

func TestHandleTextRunsToolsAndSavesTrace(t *testing.T) {
	sessions := &fakeSessions{}
	llmClient := &fakeToolLLM{
		calls: []tooltypes.Call{{
			ID:        "call_1",
			Name:      "fake_tool",
			Arguments: json.RawMessage(`{"query":"roadmap"}`),
		}},
		finalText: "answer with tool",
	}
	tool := &fakeTool{result: `{"ok":true}`}
	bot := New(sessions, testLLMConfig())
	bot.NewLLM = func(config.ResolvedModel) llm.Client { return llmClient }
	sender := &fakeSender{}

	err := bot.Handle(context.Background(), InboundMessage{
		UserKey:     "u1",
		CommandText: "search roadmap",
		LLMText:     "search roadmap",
		Tools:       []tooltypes.Tool{tool},
	}, sender)
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if len(tool.calls) != 1 || tool.calls[0].Name != "fake_tool" {
		t.Fatalf("tool calls = %#v, want one fake_tool call", tool.calls)
	}
	if len(llmClient.results) != 1 || llmClient.results[0].Content != `{"ok":true}` {
		t.Fatalf("tool results = %#v, want result returned to model", llmClient.results)
	}
	if len(sender.sent) != 1 || sender.sent[0].Text != "answer with tool" {
		t.Fatalf("sent = %#v, want final tool answer", sender.sent)
	}
	if sessions.saved == nil || len(sessions.saved.Messages) != 2 {
		t.Fatalf("saved = %#v, want user and assistant messages", sessions.saved)
	}
	traces := sessions.saved.Messages[1].ToolTraces
	if len(traces) != 1 {
		t.Fatalf("tool traces = %#v, want one trace", traces)
	}
	if traces[0].Name != "fake_tool" || traces[0].Status != "ok" || !strings.Contains(traces[0].Arguments, "roadmap") {
		t.Fatalf("trace = %#v, want ok fake_tool trace", traces[0])
	}
}

func TestHandleTextBindsTrustedExecutionContextOutsideToolArguments(t *testing.T) {
	sessions := &fakeSessions{
		sess: &store.Session{ID: "sess_current", UserID: "trusted_user", Name: "default", Current: true},
		conv: &store.Conversation{Revision: 7},
	}
	llmClient := &fakeToolLLM{
		calls: []tooltypes.Call{{
			ID:        "call_1",
			Name:      "fake_tool",
			Arguments: json.RawMessage(`{"account_id":"model_account","chat_id":"model_chat","session_id":"model_session","actor_open_id":"model_actor"}`),
		}},
		finalText: "done",
	}
	tool := &fakeTool{result: `{"ok":true}`}
	bot := New(sessions, testLLMConfig())
	bot.NewLLM = func(config.ResolvedModel) llm.Client { return llmClient }
	ctx := tooltypes.WithExecutionContext(context.Background(), tooltypes.ExecutionContext{
		Platform:        "untrusted_outer_platform",
		AccountID:       "untrusted_outer_account",
		UserKey:         "untrusted_outer_user",
		SessionID:       "untrusted_outer_session",
		ChatID:          "oc_trusted",
		SourceMessageID: "om_trusted",
		ActorOpenID:     "ou_trusted",
		ActorUserID:     "u_trusted",
		ChatIsGroup:     true,
	})

	err := bot.Handle(ctx, InboundMessage{
		Platform:  "feishu",
		AccountID: "feishu:cli_trusted",
		UserKey:   "trusted_user",
		LLMText:   "run tool",
		Tools:     []tooltypes.Tool{tool},
	}, &fakeSender{})
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if len(tool.executions) != 1 {
		t.Fatalf("execution contexts = %#v, want one", tool.executions)
	}
	execution := tool.executions[0]
	if execution.Platform != "feishu" || execution.AccountID != "feishu:cli_trusted" || execution.UserKey != "trusted_user" || execution.SessionID != "sess_current" {
		t.Fatalf("runtime scope = %#v, want core-owned platform/account/user/session", execution)
	}
	if execution.ChatID != "oc_trusted" || execution.SourceMessageID != "om_trusted" || execution.ActorOpenID != "ou_trusted" || execution.ActorUserID != "u_trusted" || !execution.ChatIsGroup {
		t.Fatalf("platform scope = %#v, want trusted Feishu chat and actor", execution)
	}
	if !strings.HasPrefix(execution.TurnID, "turn_") || execution.ToolCallID != "call_1" || execution.ToolName != "fake_tool" || execution.ConversationRevision != 7 {
		t.Fatalf("call scope = %#v, want generated turn and runtime-bound call", execution)
	}
}

func TestHandleSerializesTurnsForSameSession(t *testing.T) {
	sessions := &fakeSessions{sess: &store.Session{ID: "session", UserID: "user", Name: "default", Current: true}}
	client := &blockingLaneLLM{
		entered: make(chan struct{}, 2),
		release: make(chan struct{}),
	}
	bot := New(sessions, testLLMConfig())
	bot.NewLLM = func(config.ResolvedModel) llm.Client { return client }
	errs := make(chan error, 2)
	go func() {
		errs <- bot.Handle(context.Background(), InboundMessage{Platform: "feishu", AccountID: "feishu:cli", UserKey: "user", LLMText: "first"}, &fakeSender{})
	}()
	select {
	case <-client.entered:
	case <-time.After(time.Second):
		close(client.release)
		t.Fatal("first turn did not enter LLM")
	}
	go func() {
		errs <- bot.Handle(context.Background(), InboundMessage{Platform: "feishu", AccountID: "feishu:cli", UserKey: "user", LLMText: "second"}, &fakeSender{})
	}()
	enteredConcurrently := false
	select {
	case <-client.entered:
		enteredConcurrently = true
	case <-time.After(100 * time.Millisecond):
	}
	close(client.release)
	for i := 0; i < 2; i++ {
		select {
		case err := <-errs:
			if err != nil {
				t.Fatalf("Handle returned error: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for serialized turns")
		}
	}
	if enteredConcurrently {
		t.Fatal("second turn entered the LLM before the first session turn completed")
	}
	calls, maxActive := client.snapshot()
	if calls != 2 || maxActive != 1 {
		t.Fatalf("LLM concurrency = calls:%d max_active:%d, want 2/1", calls, maxActive)
	}
	if sessions.saved == nil || sessions.saved.Revision != 2 || len(sessions.saved.Messages) != 4 {
		t.Fatalf("saved conversation = %#v, want two serialized turns at revision 2", sessions.saved)
	}
}

func TestHandleTextRetriesToolTurnWithoutRepeatingToolCalls(t *testing.T) {
	withImmediateLLMRetry(t)
	sessions := &fakeSessions{}
	llmClient := &fakeToolLLM{
		callTurns: [][]tooltypes.Call{{
			{
				ID:        "call_1",
				Name:      "fake_tool",
				Arguments: json.RawMessage(`{"query":"roadmap"}`),
			},
		}},
		turnErrs:  map[int]error{1: retryableLLMHTTPError()},
		finalText: "answer after retry",
	}
	tool := &fakeTool{result: `{"ok":true}`}
	bot := New(sessions, testLLMConfig())
	bot.NewLLM = func(config.ResolvedModel) llm.Client { return llmClient }

	err := bot.Handle(context.Background(), InboundMessage{
		UserKey:     "u1",
		CommandText: "search roadmap",
		LLMText:     "search roadmap",
		Tools:       []tooltypes.Tool{tool},
	}, &fakeSender{})
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if len(tool.calls) != 1 {
		t.Fatalf("tool calls = %#v, want one execution despite LLM retry", tool.calls)
	}
	if len(llmClient.systemPrompts) != 3 {
		t.Fatalf("system prompts = %d, want initial tool turn, failed final turn, retried final turn", len(llmClient.systemPrompts))
	}
	if len(llmClient.results) != 2 {
		t.Fatalf("tool results sent to LLM = %d, want same result sent on failed turn and retry", len(llmClient.results))
	}
	if sessions.saved == nil || len(sessions.saved.Messages[1].ToolTraces) != 1 {
		t.Fatalf("saved = %#v, want one tool trace", sessions.saved)
	}
}

func TestHandleTextToolLoopAccumulatesToolContext(t *testing.T) {
	sessions := &fakeSessions{}
	llmClient := &fakeToolLLM{
		callTurns: [][]tooltypes.Call{
			{{
				ID:        "call_1",
				Name:      "fake_tool",
				Arguments: json.RawMessage(`{"query":"one"}`),
			}},
			{{
				ID:        "call_2",
				Name:      "fake_tool",
				Arguments: json.RawMessage(`{"query":"two"}`),
			}},
		},
		finalText: "done",
	}
	tool := &fakeTool{result: `{"ok":true}`}
	bot := New(sessions, testLLMConfig())
	bot.NewLLM = func(config.ResolvedModel) llm.Client { return llmClient }

	err := bot.Handle(context.Background(), InboundMessage{
		UserKey: "u1",
		LLMText: "review",
		Tools:   []tooltypes.Tool{tool},
	}, &fakeSender{})
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if len(tool.calls) != 2 {
		t.Fatalf("tool calls = %#v, want two executions", tool.calls)
	}
	if len(llmClient.previousTurns) != 3 || len(llmClient.resultTurns) != 3 {
		t.Fatalf("turns previous=%d results=%d, want 3", len(llmClient.previousTurns), len(llmClient.resultTurns))
	}
	if got := len(llmClient.previousTurns[1].Items); got != 1 {
		t.Fatalf("turn 2 previous items = %d, want first tool-call state", got)
	}
	if got := len(llmClient.resultTurns[1]); got != 1 {
		t.Fatalf("turn 2 results = %d, want first tool result", got)
	}
	if got := len(llmClient.previousTurns[2].Items); got != 2 {
		t.Fatalf("turn 3 previous items = %d, want cumulative tool-call states", got)
	}
	if got := len(llmClient.resultTurns[2]); got != 2 {
		t.Fatalf("turn 3 results = %d, want cumulative tool results", got)
	}
}

func TestHandleTextRetryExhaustedDoesNotSaveOrCallReviewTool(t *testing.T) {
	withImmediateLLMRetry(t)
	sessions := &fakeSessions{}
	llmClient := &fakeToolLLM{
		turnErrs: map[int]error{
			0: retryableLLMHTTPError(),
			1: retryableLLMHTTPError(),
			2: retryableLLMHTTPError(),
		},
	}
	reviewTool := &fakeTool{spec: tooltypes.Spec{Name: "mcp_github_pull_request_review_write"}}
	bot := New(sessions, testLLMConfig())
	bot.NewLLM = func(config.ResolvedModel) llm.Client { return llmClient }

	err := bot.Handle(context.Background(), InboundMessage{
		Platform:             "github",
		UserKey:              "github:repo:pr:1",
		LLMText:              "review",
		Tools:                []tooltypes.Tool{reviewTool},
		DisableProviderTools: true,
	}, &fakeSender{})
	if err == nil {
		t.Fatal("Handle returned nil, want exhausted retry error")
	}
	var httpErr *llm.HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != 504 {
		t.Fatalf("Handle error = %v, want HTTP 504", err)
	}
	if len(llmClient.systemPrompts) != 3 {
		t.Fatalf("system prompts = %d, want 3 attempts", len(llmClient.systemPrompts))
	}
	if len(reviewTool.calls) != 0 {
		t.Fatalf("review tool calls = %#v, want no submit when LLM turn never succeeds", reviewTool.calls)
	}
	if sessions.saved != nil {
		t.Fatalf("saved = %#v, want no saved history on exhausted retry", sessions.saved)
	}
}

func TestHandleTextSystemPromptSuffixReachesToolsAndMessages(t *testing.T) {
	sessions := &fakeSessions{}
	llmClient := &fakeToolLLM{finalText: "done"}
	bot := New(sessions, testLLMConfig())
	bot.NewLLM = func(config.ResolvedModel) llm.Client { return llmClient }

	err := bot.Handle(context.Background(), InboundMessage{
		UserKey:            "u1",
		LLMText:            "review",
		SystemPromptSuffix: "GitHub review policy",
		Tools:              []tooltypes.Tool{&fakeTool{}},
	}, &fakeSender{})
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if len(llmClient.systemPrompts) != 1 {
		t.Fatalf("system prompts = %d, want 1", len(llmClient.systemPrompts))
	}
	for _, want := range []string{"system", "GitHub review policy"} {
		if !strings.Contains(llmClient.systemPrompts[0], want) {
			t.Fatalf("tool system prompt = %q, want %q", llmClient.systemPrompts[0], want)
		}
	}
	if len(llmClient.messages) == 0 || llmClient.messages[0].Role != "system" {
		t.Fatalf("messages = %#v, want first system message", llmClient.messages)
	}
	for _, want := range []string{"system", "GitHub review policy"} {
		if !strings.Contains(llmClient.messages[0].Content, want) {
			t.Fatalf("message system prompt = %q, want %q", llmClient.messages[0].Content, want)
		}
	}
}

func TestHandleTextRunsToolProviderTools(t *testing.T) {
	sessions := &fakeSessions{}
	llmClient := &fakeToolLLM{
		calls: []tooltypes.Call{{
			ID:        "call_1",
			Name:      "mcp_files_read_file",
			Arguments: json.RawMessage(`{"path":"a.txt"}`),
		}},
		finalText: "answer with mcp tool",
	}
	tool := &fakeTool{
		spec:   tooltypes.Spec{Name: "mcp_files_read_file", Description: "Read file"},
		result: `{"content":"ok"}`,
	}
	bot := New(sessions, testLLMConfig())
	bot.NewLLM = func(config.ResolvedModel) llm.Client { return llmClient }
	provider := &fakeToolProvider{tools: []tooltypes.Tool{tool}}
	bot.ToolProvider = provider
	sender := &fakeSender{}

	err := bot.Handle(context.Background(), InboundMessage{
		Platform:    "feishu",
		AccountID:   "feishu:cli_xxx",
		AccountName: "admin-bot",
		UserKey:     "u1",
		CommandText: "read file",
		LLMText:     "read file",
	}, sender)
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if len(tool.calls) != 1 || tool.calls[0].Name != "mcp_files_read_file" {
		t.Fatalf("tool calls = %#v, want provider tool call", tool.calls)
	}
	if len(llmClient.toolSpecs) != 1 || llmClient.toolSpecs[0].Name != "mcp_files_read_file" {
		t.Fatalf("tool specs = %#v, want provider tool spec", llmClient.toolSpecs)
	}
	if len(provider.scopes) != 1 || provider.scopes[0].Platform != "feishu" || provider.scopes[0].AccountID != "feishu:cli_xxx" || provider.scopes[0].AccountName != "admin-bot" {
		t.Fatalf("provider scopes = %#v, want feishu admin-bot scope", provider.scopes)
	}
}

func TestHandleTextDisableProviderToolsUsesMessageToolsOnly(t *testing.T) {
	sessions := &fakeSessions{}
	llmClient := &fakeToolLLM{
		calls: []tooltypes.Call{{
			ID:        "call_1",
			Name:      "platform_tool",
			Arguments: json.RawMessage(`{}`),
		}},
		finalText: "answer with platform tool",
	}
	platformTool := &fakeTool{
		spec:   tooltypes.Spec{Name: "platform_tool", Description: "Platform tool"},
		result: "ok",
	}
	providerTool := &fakeTool{
		spec:   tooltypes.Spec{Name: "provider_tool", Description: "Provider tool"},
		result: "provider",
	}
	provider := &fakeToolProvider{tools: []tooltypes.Tool{providerTool}}
	bot := New(sessions, testLLMConfig())
	bot.NewLLM = func(config.ResolvedModel) llm.Client { return llmClient }
	bot.ToolProvider = provider

	err := bot.Handle(context.Background(), InboundMessage{
		Platform:             "github",
		AccountID:            "github:reviewer",
		AccountName:          "reviewer",
		UserKey:              "u1",
		LLMText:              "review",
		Tools:                []tooltypes.Tool{platformTool},
		DisableProviderTools: true,
	}, &fakeSender{})
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if len(provider.scopes) != 0 {
		t.Fatalf("provider scopes = %#v, want provider not resolved", provider.scopes)
	}
	if len(platformTool.calls) != 1 {
		t.Fatalf("platform tool calls = %#v, want one call", platformTool.calls)
	}
	if len(providerTool.calls) != 0 {
		t.Fatalf("provider tool calls = %#v, want none", providerTool.calls)
	}
	if len(llmClient.toolSpecs) != 1 || llmClient.toolSpecs[0].Name != "platform_tool" {
		t.Fatalf("tool specs = %#v, want only platform_tool", llmClient.toolSpecs)
	}
}

func TestHandleTextPlatformToolWinsDuplicateProviderTool(t *testing.T) {
	sessions := &fakeSessions{}
	llmClient := &fakeToolLLM{
		calls: []tooltypes.Call{{
			ID:        "call_1",
			Name:      "shared_tool",
			Arguments: json.RawMessage(`{"source":"model"}`),
		}},
		finalText: "answer with platform tool",
	}
	platformTool := &fakeTool{
		spec:   tooltypes.Spec{Name: "shared_tool", Description: "Platform version"},
		result: `{"source":"platform"}`,
	}
	providerTool := &fakeTool{
		spec:   tooltypes.Spec{Name: "shared_tool", Description: "Provider version"},
		result: `{"source":"provider"}`,
	}
	bot := New(sessions, testLLMConfig())
	bot.NewLLM = func(config.ResolvedModel) llm.Client { return llmClient }
	bot.ToolProvider = &fakeToolProvider{tools: []tooltypes.Tool{providerTool}}
	sender := &fakeSender{}

	err := bot.Handle(context.Background(), InboundMessage{
		UserKey: "u1",
		LLMText: "use duplicate",
		Tools:   []tooltypes.Tool{platformTool},
	}, sender)
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if len(platformTool.calls) != 1 {
		t.Fatalf("platform tool calls = %#v, want one call", platformTool.calls)
	}
	if len(providerTool.calls) != 0 {
		t.Fatalf("provider tool calls = %#v, want skipped duplicate", providerTool.calls)
	}
	if len(llmClient.toolSpecs) != 1 || llmClient.toolSpecs[0].Description != "Platform version" {
		t.Fatalf("tool specs = %#v, want only platform spec", llmClient.toolSpecs)
	}
}

func TestHandleTextToolErrorsAreReturnedToModel(t *testing.T) {
	sessions := &fakeSessions{}
	llmClient := &fakeToolLLM{
		calls: []tooltypes.Call{{
			ID:        "call_1",
			Name:      "fake_tool",
			Arguments: json.RawMessage(`{}`),
		}},
		finalText: "handled error",
	}
	tool := &fakeTool{err: "permission denied"}
	bot := New(sessions, testLLMConfig())
	bot.NewLLM = func(config.ResolvedModel) llm.Client { return llmClient }
	sender := &fakeSender{}

	err := bot.Handle(context.Background(), InboundMessage{
		UserKey: "u1",
		LLMText: "try it",
		Tools:   []tooltypes.Tool{tool},
	}, sender)
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if len(llmClient.results) != 1 || !llmClient.results[0].IsError || llmClient.results[0].Content != "permission denied" {
		t.Fatalf("tool results = %#v, want error result returned to model", llmClient.results)
	}
	traces := sessions.saved.Messages[1].ToolTraces
	if len(traces) != 1 || traces[0].Status != "error" || traces[0].Error != "permission denied" {
		t.Fatalf("tool traces = %#v, want error trace", traces)
	}
}

func TestHandleTextToolTimeoutIsReturnedToModel(t *testing.T) {
	sessions := &fakeSessions{}
	llmClient := &fakeToolLLM{
		calls: []tooltypes.Call{{
			ID:        "call_1",
			Name:      "fake_tool",
			Arguments: json.RawMessage(`{}`),
		}},
		finalText: "handled timeout",
	}
	tool := &fakeTool{block: true}
	bot := New(sessions, testLLMConfig())
	bot.ToolProvider = &fakeToolProvider{options: tooltypes.Options{Timeout: time.Millisecond}}
	bot.NewLLM = func(config.ResolvedModel) llm.Client { return llmClient }

	err := bot.Handle(context.Background(), InboundMessage{
		UserKey: "u1",
		LLMText: "try it",
		Tools:   []tooltypes.Tool{tool},
	}, &fakeSender{})
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if len(llmClient.results) != 1 || !llmClient.results[0].IsError || !strings.Contains(llmClient.results[0].Content, "timed out") {
		t.Fatalf("tool results = %#v, want timeout result returned to model", llmClient.results)
	}
}

func TestHandleTextToolLoopStopsOnContextCancel(t *testing.T) {
	sessions := &fakeSessions{}
	llmClient := &fakeToolLLM{
		calls: []tooltypes.Call{{
			ID:        "call_1",
			Name:      "fake_tool",
			Arguments: json.RawMessage(`{}`),
		}},
		finalText: "should not be reached",
	}
	ctx, cancel := context.WithCancel(context.Background())
	tool := &cancelingTool{cancel: cancel}
	bot := New(sessions, testLLMConfig())
	bot.NewLLM = func(config.ResolvedModel) llm.Client { return llmClient }
	sender := &fakeSender{}

	err := bot.Handle(ctx, InboundMessage{
		UserKey: "u1",
		LLMText: "try it",
		Tools:   []tooltypes.Tool{tool},
	}, sender)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Handle error = %v, want context.Canceled", err)
	}
	if len(tool.calls) != 1 {
		t.Fatalf("tool calls = %#v, want one tool execution", tool.calls)
	}
	if len(llmClient.systemPrompts) != 1 {
		t.Fatalf("system prompts = %d, want no next model turn after cancellation", len(llmClient.systemPrompts))
	}
	if len(llmClient.results) != 0 {
		t.Fatalf("tool results = %#v, want cancellation not sent back to model", llmClient.results)
	}
	if sessions.saved != nil {
		t.Fatalf("saved = %#v, want no history saved after cancellation", sessions.saved)
	}
	if len(sender.sent) != 0 {
		t.Fatalf("sent = %#v, want no error notice on cancellation", sender.sent)
	}
}

func TestHandleTextToolCallLimit(t *testing.T) {
	sessions := &fakeSessions{}
	llmClient := &fakeToolLLM{
		calls: []tooltypes.Call{
			{ID: "call_1", Name: "fake_tool", Arguments: json.RawMessage(`{}`)},
			{ID: "call_2", Name: "fake_tool", Arguments: json.RawMessage(`{}`)},
		},
	}
	bot := New(sessions, testLLMConfig())
	bot.ToolProvider = &fakeToolProvider{options: tooltypes.Options{MaxCalls: 1}}
	bot.NewLLM = func(config.ResolvedModel) llm.Client { return llmClient }

	err := bot.Handle(context.Background(), InboundMessage{
		UserKey: "u1",
		LLMText: "try it",
		Tools:   []tooltypes.Tool{&fakeTool{}},
	}, &fakeSender{})
	if err == nil || !strings.Contains(err.Error(), "tool call limit exceeded") {
		t.Fatalf("Handle error = %v, want tool call limit exceeded", err)
	}
	if sessions.saved != nil {
		t.Fatalf("saved = %#v, want no history saved when tool limit is exceeded", sessions.saved)
	}
}

func TestHandleTextMessageToolOptionsOverrideProviderOptions(t *testing.T) {
	sessions := &fakeSessions{}
	llmClient := &fakeToolLLM{
		calls: []tooltypes.Call{
			{ID: "call_1", Name: "fake_tool", Arguments: json.RawMessage(`{}`)},
			{ID: "call_2", Name: "fake_tool", Arguments: json.RawMessage(`{}`)},
		},
		finalText: "handled",
	}
	tool := &fakeTool{result: "ok"}
	bot := New(sessions, testLLMConfig())
	bot.ToolProvider = &fakeToolProvider{options: tooltypes.Options{MaxCalls: 1}}
	bot.NewLLM = func(config.ResolvedModel) llm.Client { return llmClient }

	err := bot.Handle(context.Background(), InboundMessage{
		UserKey:     "u1",
		LLMText:     "try it",
		Tools:       []tooltypes.Tool{tool},
		ToolOptions: tooltypes.Options{MaxCalls: 2},
	}, &fakeSender{})
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if len(tool.calls) != 2 {
		t.Fatalf("tool calls = %d, want 2", len(tool.calls))
	}
}

func TestHandleTextToolBudgetPromptIncludesMaxCalls(t *testing.T) {
	sessions := &fakeSessions{}
	llmClient := &fakeToolLLM{finalText: "done"}
	bot := New(sessions, testLLMConfig())
	bot.NewLLM = func(config.ResolvedModel) llm.Client { return llmClient }

	err := bot.Handle(context.Background(), InboundMessage{
		UserKey:     "u1",
		LLMText:     "try it",
		Tools:       []tooltypes.Tool{&fakeTool{}},
		ToolOptions: tooltypes.Options{MaxCalls: 50},
	}, &fakeSender{})
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if len(llmClient.systemPrompts) != 1 {
		t.Fatalf("system prompts = %d, want 1", len(llmClient.systemPrompts))
	}
	if !strings.Contains(llmClient.systemPrompts[0], "at most 50 times") {
		t.Fatalf("system prompt = %q, want max call budget", llmClient.systemPrompts[0])
	}
	if len(llmClient.results) != 0 {
		t.Fatalf("tool results = %#v, want no fake reminder result", llmClient.results)
	}
}

func TestHandleTextToolBudgetReminderThresholds(t *testing.T) {
	sessions := &fakeSessions{}
	llmClient := &fakeToolLLM{
		callTurns: [][]tooltypes.Call{
			makeToolCalls(18, 0),
			makeToolCalls(1, 18),
		},
		finalText: "done",
	}
	tool := &fakeTool{}
	bot := New(sessions, testLLMConfig())
	bot.NewLLM = func(config.ResolvedModel) llm.Client { return llmClient }

	err := bot.Handle(context.Background(), InboundMessage{
		UserKey:     "u1",
		LLMText:     "try it",
		Tools:       []tooltypes.Tool{tool},
		ToolOptions: tooltypes.Options{MaxCalls: 20},
	}, &fakeSender{})
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if len(llmClient.systemPrompts) != 3 {
		t.Fatalf("system prompts = %d, want 3", len(llmClient.systemPrompts))
	}
	if strings.Contains(llmClient.systemPrompts[0], "tool_call_budget_reminder") {
		t.Fatalf("initial prompt has reminder: %q", llmClient.systemPrompts[0])
	}
	if !strings.Contains(llmClient.systemPrompts[1], `severity="10%"`) || !strings.Contains(llmClient.systemPrompts[1], `remaining="2"`) {
		t.Fatalf("second prompt = %q, want 10%% reminder with 2 remaining", llmClient.systemPrompts[1])
	}
	if !strings.Contains(llmClient.systemPrompts[2], `severity="5%"`) || !strings.Contains(llmClient.systemPrompts[2], `remaining="1"`) {
		t.Fatalf("third prompt = %q, want 5%% reminder with 1 remaining", llmClient.systemPrompts[2])
	}
	allPrompts := strings.Join(llmClient.systemPrompts, "\n")
	if strings.Count(allPrompts, `severity="10%"`) != 1 || strings.Count(allPrompts, `severity="5%"`) != 1 {
		t.Fatalf("system prompts = %#v, want each reminder once", llmClient.systemPrompts)
	}
	if len(tool.calls) != 19 {
		t.Fatalf("tool calls = %d, want 19 real tool executions", len(tool.calls))
	}
	lastResults := llmClient.resultTurns[len(llmClient.resultTurns)-1]
	if len(lastResults) != 19 {
		t.Fatalf("last turn tool results = %d, want only 19 real tool results", len(lastResults))
	}
}

func TestHandleTextToolBudgetReminderUsesHighestSeverityWhenThresholdsCrossTogether(t *testing.T) {
	sessions := &fakeSessions{}
	llmClient := &fakeToolLLM{
		callTurns: [][]tooltypes.Call{
			makeToolCalls(48, 0),
		},
		finalText: "done",
	}
	bot := New(sessions, testLLMConfig())
	bot.NewLLM = func(config.ResolvedModel) llm.Client { return llmClient }

	err := bot.Handle(context.Background(), InboundMessage{
		UserKey:     "u1",
		LLMText:     "try it",
		Tools:       []tooltypes.Tool{&fakeTool{}},
		ToolOptions: tooltypes.Options{MaxCalls: 50},
	}, &fakeSender{})
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if len(llmClient.systemPrompts) != 2 {
		t.Fatalf("system prompts = %d, want 2", len(llmClient.systemPrompts))
	}
	if !strings.Contains(llmClient.systemPrompts[1], `severity="5%"`) || !strings.Contains(llmClient.systemPrompts[1], `remaining="2"`) {
		t.Fatalf("second prompt = %q, want 5%% reminder with 2 remaining", llmClient.systemPrompts[1])
	}
	if strings.Contains(llmClient.systemPrompts[1], `severity="10%"`) {
		t.Fatalf("second prompt = %q, want highest severity only", llmClient.systemPrompts[1])
	}
}

func TestHandleTextCompactsNativeContextAndSavesRecentHistory(t *testing.T) {
	var history []store.Message
	for i := 0; i < nativeContextKeepRecentMessages+3; i++ {
		history = append(history, store.Message{Role: "user", Content: "old message " + string(rune('a'+i))})
	}
	sessions := &fakeSessions{
		conv: &store.Conversation{Messages: history},
	}
	client := &fakeNativeLLM{}
	cfg := config.LLMConfig{
		DefaultModel: "deepseek",
		Models: map[string]config.LLMModelConfig{
			"deepseek": {
				Provider:      "openai",
				BaseURL:       "https://llm.test",
				APIKey:        "key",
				ID:            "model",
				Endpoint:      "responses",
				ContextWindow: 4,
				Compact:       config.LLMCompactConfig{Mode: config.CompactModeAuto, Threshold: 0.25},
			},
		},
		SystemPrompt: "system",
	}
	b := &Bot{
		Sessions:            sessions,
		LLMConfig:           cfg,
		LLMClients:          map[string]llm.Client{},
		NewLLM:              func(config.ResolvedModel) llm.Client { return client },
		TextChunkLimit:      testTextChunkLimit,
		EnableTextStreaming: true,
	}
	sender := &fakeSender{}

	if err := b.Handle(context.Background(), InboundMessage{UserKey: "user", CommandText: "new", LLMText: "new"}, sender); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if len(sender.compactStarts) != 1 || sender.compactStarts[0].ModelName != "deepseek" || sender.compactStarts[0].Manual {
		t.Fatalf("compact starts = %#v, want one automatic deepseek notice", sender.compactStarts)
	}
	if len(sender.compactFinishes) != 1 || sender.compactFinishes[0].CompactedMessages != 3 || sender.compactFinishes[0].RetainedMessages != nativeContextKeepRecentMessages {
		t.Fatalf("compact finishes = %#v, want one success notice", sender.compactFinishes)
	}
	if len(client.compactedMessages) != 3 {
		t.Fatalf("compacted messages = %d, want 3 old messages", len(client.compactedMessages))
	}
	if len(client.contextMessages) != nativeContextKeepRecentMessages+2 {
		t.Fatalf("context request messages = %d, want system + recent history + new", len(client.contextMessages))
	}
	for _, msg := range client.contextMessages {
		if msg.Content == "old message a" {
			t.Fatalf("request included compacted old message: %#v", client.contextMessages)
		}
	}
	if client.context.IsEmpty() {
		t.Fatal("provider context was not passed to context-aware request")
	}
	if sessions.saved == nil {
		t.Fatal("history was not saved")
	}
	if got, want := len(sessions.saved.Messages), nativeContextKeepRecentMessages+2; got != want {
		t.Fatalf("saved messages = %d, want %d", got, want)
	}
	if sessions.saved.Messages[0].Content != "old message d" {
		t.Fatalf("first saved message = %#v, want first retained recent message", sessions.saved.Messages[0])
	}
	ctx := sessions.saved.ProviderContexts["deepseek"]
	if ctx.Provider != "openai" || ctx.Endpoint != "responses" || len(ctx.Items) != 1 {
		t.Fatalf("saved provider context = %#v, want openai responses compact context", ctx)
	}
}

func TestHandleTextContinuesWhenNativeCompactNotTriggered(t *testing.T) {
	var history []store.Message
	for i := 0; i < nativeContextKeepRecentMessages+3; i++ {
		history = append(history, store.Message{Role: "user", Content: "old message " + string(rune('a'+i))})
	}
	sessions := &fakeSessions{
		conv: &store.Conversation{Messages: history},
	}
	client := &fakeNativeLLM{compactErr: llm.ErrCompactionNotTriggered}
	cfg := config.LLMConfig{
		DefaultModel: "deepseek",
		Models: map[string]config.LLMModelConfig{
			"deepseek": {
				Provider:      "openai",
				BaseURL:       "https://llm.test",
				APIKey:        "key",
				ID:            "model",
				Endpoint:      "responses",
				ContextWindow: 4,
				Compact:       config.LLMCompactConfig{Mode: config.CompactModeAuto, Threshold: 0.25},
			},
		},
		SystemPrompt: "system",
	}
	b := &Bot{
		Sessions:            sessions,
		LLMConfig:           cfg,
		LLMClients:          map[string]llm.Client{},
		NewLLM:              func(config.ResolvedModel) llm.Client { return client },
		TextChunkLimit:      testTextChunkLimit,
		EnableTextStreaming: true,
	}
	sender := &fakeSender{}

	if err := b.Handle(context.Background(), InboundMessage{UserKey: "user", CommandText: "new", LLMText: "new"}, sender); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if len(sender.compactStarts) != 1 {
		t.Fatalf("compact starts = %#v, want one attempted compact notice", sender.compactStarts)
	}
	if len(sender.compactFinishes) != 0 {
		t.Fatalf("compact finishes = %#v, want none when compaction was not triggered", sender.compactFinishes)
	}
	if len(client.compactedMessages) != 3 {
		t.Fatalf("compacted messages = %d, want attempted compaction of 3 old messages", len(client.compactedMessages))
	}
	if !client.called {
		t.Fatal("LLM request was not sent after compaction was not triggered")
	}
	if sessions.saved == nil {
		t.Fatal("history was not saved")
	}
	if got, want := len(sessions.saved.Messages), len(history)+2; got != want {
		t.Fatalf("saved messages = %d, want original history plus user/assistant %d", got, want)
	}
	if len(sessions.saved.ProviderContexts) != 0 {
		t.Fatalf("provider contexts = %#v, want none when compaction was not triggered", sessions.saved.ProviderContexts)
	}
	if len(sender.sent) != 1 || sender.sent[0].Text != "hello" {
		t.Fatalf("sent = %#v, want normal reply", sender.sent)
	}
}

func TestHandleCompactCommandCompactsAndSavesRecentHistory(t *testing.T) {
	var history []store.Message
	for i := 0; i < nativeContextKeepRecentMessages+2; i++ {
		history = append(history, store.Message{Role: "user", Content: "old message " + string(rune('a'+i))})
	}
	sessions := &fakeSessions{conv: &store.Conversation{Messages: history}}
	client := &fakeNativeLLM{}
	cfg := config.LLMConfig{
		DefaultModel: "deepseek",
		Models: map[string]config.LLMModelConfig{
			"deepseek": {
				Provider:      "openai",
				BaseURL:       "https://llm.test",
				APIKey:        "key",
				ID:            "model",
				Endpoint:      "responses",
				ContextWindow: 128000,
				Compact:       config.LLMCompactConfig{Mode: config.CompactModeAuto, Threshold: 0.9},
			},
		},
		SystemPrompt: "system",
	}
	b := &Bot{
		Sessions:       sessions,
		LLMConfig:      cfg,
		LLMClients:     map[string]llm.Client{},
		NewLLM:         func(config.ResolvedModel) llm.Client { return client },
		TextChunkLimit: testTextChunkLimit,
	}
	sender := &fakeSender{}

	if err := b.Handle(context.Background(), InboundMessage{UserKey: "user", CommandText: "/compact", LLMText: "/compact"}, sender); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if len(sender.compactStarts) != 1 || !sender.compactStarts[0].Manual || sender.compactStarts[0].CompactedMessages != 2 {
		t.Fatalf("compact starts = %#v, want one manual notice", sender.compactStarts)
	}
	if len(sender.compactFinishes) != 1 || sender.compactFinishes[0].RetainedMessages != nativeContextKeepRecentMessages {
		t.Fatalf("compact finishes = %#v, want one manual success notice", sender.compactFinishes)
	}
	if len(client.compactedMessages) != 2 {
		t.Fatalf("compacted messages = %d, want 2", len(client.compactedMessages))
	}
	if sessions.saved == nil {
		t.Fatal("history was not saved")
	}
	if got, want := len(sessions.saved.Messages), nativeContextKeepRecentMessages; got != want {
		t.Fatalf("saved messages = %d, want %d", got, want)
	}
	if sessions.saved.ProviderContexts["deepseek"].IsEmpty() {
		t.Fatalf("saved provider contexts = %#v, want compact context", sessions.saved.ProviderContexts)
	}
	if len(sender.sent) != 0 {
		t.Fatalf("sent = %#v, want compact success handled by notice sender", sender.sent)
	}
}

func TestHandleCompactCommandContinuesWhenNoticeFails(t *testing.T) {
	var history []store.Message
	for i := 0; i < nativeContextKeepRecentMessages+2; i++ {
		history = append(history, store.Message{Role: "user", Content: "old message " + string(rune('a'+i))})
	}
	sessions := &fakeSessions{conv: &store.Conversation{Messages: history}}
	client := &fakeNativeLLM{}
	cfg := config.LLMConfig{
		DefaultModel: "deepseek",
		Models: map[string]config.LLMModelConfig{
			"deepseek": {
				Provider:      "openai",
				BaseURL:       "https://llm.test",
				APIKey:        "key",
				ID:            "model",
				Endpoint:      "responses",
				ContextWindow: 128000,
				Compact:       config.LLMCompactConfig{Mode: config.CompactModeAuto, Threshold: 0.9},
			},
		},
		SystemPrompt: "system",
	}
	b := &Bot{
		Sessions:       sessions,
		LLMConfig:      cfg,
		LLMClients:     map[string]llm.Client{},
		NewLLM:         func(config.ResolvedModel) llm.Client { return client },
		TextChunkLimit: testTextChunkLimit,
	}
	sender := &fakeSender{
		compactStartErr:  errors.New("notice start failed"),
		compactFinishErr: errors.New("notice finish failed"),
	}

	if err := b.Handle(context.Background(), InboundMessage{UserKey: "user", CommandText: "/compact", LLMText: "/compact"}, sender); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if sessions.saved == nil {
		t.Fatal("history was not saved")
	}
	if len(sender.compactStarts) != 1 || len(sender.compactFinishes) != 1 {
		t.Fatalf("compact notices = %#v/%#v, want attempted start and finish", sender.compactStarts, sender.compactFinishes)
	}
	if len(sender.sent) != 0 {
		t.Fatalf("sent = %#v, want no fallback text from failing notice sender", sender.sent)
	}
}

func TestHandleCompactCommandNotTriggeredDoesNotSave(t *testing.T) {
	var history []store.Message
	for i := 0; i < nativeContextKeepRecentMessages+2; i++ {
		history = append(history, store.Message{Role: "user", Content: "old message " + string(rune('a'+i))})
	}
	sessions := &fakeSessions{conv: &store.Conversation{Messages: history}}
	client := &fakeNativeLLM{compactErr: llm.ErrCompactionNotTriggered}
	cfg := config.LLMConfig{
		DefaultModel: "deepseek",
		Models: map[string]config.LLMModelConfig{
			"deepseek": {
				Provider:      "openai",
				BaseURL:       "https://llm.test",
				APIKey:        "key",
				ID:            "model",
				Endpoint:      "responses",
				ContextWindow: 128000,
				Compact:       config.LLMCompactConfig{Mode: config.CompactModeAuto, Threshold: 0.9},
			},
		},
		SystemPrompt: "system",
	}
	b := &Bot{
		Sessions:       sessions,
		LLMConfig:      cfg,
		LLMClients:     map[string]llm.Client{},
		NewLLM:         func(config.ResolvedModel) llm.Client { return client },
		TextChunkLimit: testTextChunkLimit,
	}
	sender := &fakeSender{}

	if err := b.Handle(context.Background(), InboundMessage{UserKey: "user", CommandText: "/compact", LLMText: "/compact"}, sender); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if len(sender.compactStarts) != 1 {
		t.Fatalf("compact starts = %#v, want one attempted manual compact notice", sender.compactStarts)
	}
	if len(sender.compactFinishes) != 0 {
		t.Fatalf("compact finishes = %#v, want none when manual compaction was not triggered", sender.compactFinishes)
	}
	if len(client.compactedMessages) != 2 {
		t.Fatalf("compacted messages = %d, want attempted compact of 2 old messages", len(client.compactedMessages))
	}
	if sessions.saved != nil {
		t.Fatalf("saved = %#v, want no saved history when compaction was not triggered", sessions.saved)
	}
	if len(sender.sent) != 1 || !strings.Contains(sender.sent[0].Text, "未达到供应商原生压缩触发阈值") {
		t.Fatalf("sent = %#v, want not-triggered compact notice", sender.sent)
	}
}

func TestHandleCompactCommandDisabledByMode(t *testing.T) {
	sessions := &fakeSessions{conv: &store.Conversation{Messages: []store.Message{{Role: "user", Content: "old"}}}}
	client := &fakeNativeLLM{}
	cfg := config.LLMConfig{
		DefaultModel: "deepseek",
		Models: map[string]config.LLMModelConfig{
			"deepseek": {
				Provider: "openai",
				BaseURL:  "https://llm.test",
				APIKey:   "key",
				ID:       "model",
				Endpoint: "chat",
				Compact:  config.LLMCompactConfig{Mode: config.CompactModeFalse, Threshold: 0.9},
			},
		},
		SystemPrompt: "system",
	}
	b := &Bot{
		Sessions:   sessions,
		LLMConfig:  cfg,
		LLMClients: map[string]llm.Client{},
		NewLLM:     func(config.ResolvedModel) llm.Client { return client },
	}
	sender := &fakeSender{}

	if err := b.Handle(context.Background(), InboundMessage{UserKey: "user", CommandText: "/compact", LLMText: "/compact"}, sender); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if len(client.compactedMessages) != 0 {
		t.Fatalf("compacted messages = %#v, want no compaction", client.compactedMessages)
	}
	if len(sender.compactStarts) != 0 || len(sender.compactFinishes) != 0 {
		t.Fatalf("compact notices = %#v/%#v, want none when compact is disabled", sender.compactStarts, sender.compactFinishes)
	}
	if len(sender.sent) != 1 || !strings.Contains(sender.sent[0].Text, "已禁用") {
		t.Fatalf("sent = %#v, want disabled compact error", sender.sent)
	}
}

func TestHandleCompactCommandUnsupportedProviderErrors(t *testing.T) {
	sessions := &fakeSessions{conv: &store.Conversation{Messages: []store.Message{{Role: "user", Content: "old"}}}}
	client := &fakeLLM{}
	b := testBot(sessions, client)
	sender := &fakeSender{}

	if err := b.Handle(context.Background(), InboundMessage{UserKey: "user", CommandText: "/compact", LLMText: "/compact"}, sender); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if client.called {
		t.Fatal("LLM chat was called for unsupported /compact")
	}
	if len(sender.compactStarts) != 0 || len(sender.compactFinishes) != 0 {
		t.Fatalf("compact notices = %#v/%#v, want none for unsupported compact", sender.compactStarts, sender.compactFinishes)
	}
	if len(sender.sent) != 1 || !strings.Contains(sender.sent[0].Text, "不支持上下文压缩") {
		t.Fatalf("sent = %#v, want unsupported compact error", sender.sent)
	}
}

func TestNewDisablesTextStreamingByDefault(t *testing.T) {
	b := New(&fakeSessions{}, config.LLMConfig{})
	if b.EnableTextStreaming {
		t.Fatal("EnableTextStreaming = true, want false")
	}
}

func TestHandleTextStreamsFirstChunkWhenSenderSupportsStream(t *testing.T) {
	sessions := &fakeSessions{}
	client := &fakeLLM{
		streamChunks: []string{"hel", "lo"},
		resp:         llm.Response{Text: "hello"},
	}
	b := testBot(sessions, client)
	sender := &fakeStreamingSender{}

	if err := b.Handle(context.Background(), InboundMessage{UserKey: "user", CommandText: "hi", LLMText: "hi"}, sender); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if sender.stream == nil {
		t.Fatal("stream was not started")
	}
	if got, want := sender.stream.updates, []string{"hel", "hello"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("stream updates = %#v, want %#v", got, want)
	}
	if got, want := sender.stream.finishes, []string{"hello"}; len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("stream finishes = %#v, want %#v", got, want)
	}
	if len(sender.sent) != 0 {
		t.Fatalf("sent = %#v, want no duplicate text send", sender.sent)
	}
	if sessions.saved == nil || len(sessions.saved.Messages) != 2 {
		t.Fatalf("saved = %#v, want history saved", sessions.saved)
	}
}

func TestHandleTextStreamsPreserveRawMarkdown(t *testing.T) {
	text := "```bash\nsudo dnf update -y\n```\ninline `code`"
	sessions := &fakeSessions{}
	client := &fakeLLM{
		streamChunks: []string{"```bash\n", "sudo dnf update -y\n```", "\ninline `code`"},
		resp:         llm.Response{Text: text},
	}
	b := testBot(sessions, client)
	sender := &fakeStreamingSender{}

	if err := b.Handle(context.Background(), InboundMessage{UserKey: "user", CommandText: "hi", LLMText: "hi"}, sender); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if sender.stream == nil {
		t.Fatal("stream was not started")
	}
	if got := sender.stream.updates[len(sender.stream.updates)-1]; got != text {
		t.Fatalf("last stream update = %q, want raw markdown %q", got, text)
	}
	if got := sender.stream.finishes; len(got) != 1 || got[0] != text {
		t.Fatalf("stream finishes = %#v, want raw markdown", got)
	}
}

func TestHandleTextStreamingDisabledUsesFinalChunkedSend(t *testing.T) {
	sessions := &fakeSessions{}
	client := &fakeLLM{
		streamChunks: []string{"abc", "def"},
		resp:         llm.Response{Text: "abcdef"},
	}
	b := testBot(sessions, client)
	b.TextChunkLimit = 3
	b.EnableTextStreaming = false
	sender := &fakeStreamingSender{}

	if err := b.Handle(context.Background(), InboundMessage{UserKey: "user", CommandText: "hi", LLMText: "hi"}, sender); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if len(sender.streams) != 0 {
		t.Fatalf("streams = %d, want 0 when streaming disabled", len(sender.streams))
	}
	if got, want := len(sender.sent), 2; got != want {
		t.Fatalf("sent messages = %d, want %d", got, want)
	}
	if sender.sent[0].Text != "abc" || sender.sent[1].Text != "def" {
		t.Fatalf("sent = %#v, want abc/def chunks", sender.sent)
	}
}

func TestHandleTextStreamingDisabledSendsRawMarkdown(t *testing.T) {
	text := "```bash\nsudo dnf update -y\n```\ninline `code`"
	sessions := &fakeSessions{}
	client := &fakeLLM{resp: llm.Response{Text: text}}
	b := testBot(sessions, client)
	b.EnableTextStreaming = false
	sender := &fakeSender{}

	if err := b.Handle(context.Background(), InboundMessage{UserKey: "user", CommandText: "hi", LLMText: "hi"}, sender); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if len(sender.sent) != 1 || sender.sent[0].Text != text {
		t.Fatalf("sent = %#v, want raw markdown", sender.sent)
	}
}

func TestHandleTextStreamsFirstChunkAndSendsOverflow(t *testing.T) {
	sessions := &fakeSessions{}
	client := &fakeLLM{
		streamChunks: []string{"abc", "def"},
		resp:         llm.Response{Text: "abcdef"},
	}
	b := testBot(sessions, client)
	b.TextChunkLimit = 3
	sender := &fakeStreamingSender{}

	if err := b.Handle(context.Background(), InboundMessage{UserKey: "user", CommandText: "hi", LLMText: "hi"}, sender); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if len(sender.streams) != 2 {
		t.Fatalf("streams = %d, want 2", len(sender.streams))
	}
	if got, want := sender.streams[0].finishes, []string{"abc"}; len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("stream finishes = %#v, want %#v", got, want)
	}
	if got, want := sender.streams[1].finishes, []string{"def"}; len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("second stream finishes = %#v, want %#v", got, want)
	}
	if len(sender.sent) != 0 {
		t.Fatalf("sent = %#v, want no duplicate overflow send", sender.sent)
	}
}

func TestHandleTextStreamingUnavailableUsesFinalChunkedSend(t *testing.T) {
	sessions := &fakeSessions{}
	client := &fakeLLM{
		streamChunks: []string{"abc", "def"},
		resp:         llm.Response{Text: "abcdef"},
	}
	b := testBot(sessions, client)
	b.TextChunkLimit = 3
	sender := &fakeSender{}

	if err := b.Handle(context.Background(), InboundMessage{UserKey: "user", CommandText: "hi", LLMText: "hi"}, sender); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if got, want := len(sender.sent), 2; got != want {
		t.Fatalf("sent messages = %d, want %d", got, want)
	}
	if sender.sent[0].Text != "abc" || sender.sent[1].Text != "def" {
		t.Fatalf("sent = %#v, want abc/def chunks", sender.sent)
	}
}

func TestHandleTextStreamingFinalUncreatedTailFallsBackToSend(t *testing.T) {
	sessions := &fakeSessions{}
	client := &fakeLLM{
		streamChunks: []string{"abc"},
		resp:         llm.Response{Text: "abcdef"},
	}
	b := testBot(sessions, client)
	b.TextChunkLimit = 3
	sender := &fakeStreamingSender{}

	if err := b.Handle(context.Background(), InboundMessage{UserKey: "user", CommandText: "hi", LLMText: "hi"}, sender); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if len(sender.streams) != 1 {
		t.Fatalf("streams = %d, want 1", len(sender.streams))
	}
	if got, want := sender.streams[0].finishes, []string{"abc"}; len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("stream finishes = %#v, want %#v", got, want)
	}
	if len(sender.sent) != 1 || sender.sent[0].Text != "def" {
		t.Fatalf("sent = %#v, want uncreated tail def", sender.sent)
	}
}

func TestHandleTextStreamsSplitLinesAndPreserveUTF8(t *testing.T) {
	text := "第一行\n第二行\n第三行"
	sessions := &fakeSessions{}
	client := &fakeLLM{
		streamChunks: []string{text},
		resp:         llm.Response{Text: text},
	}
	b := testBot(sessions, client)
	b.TextChunkLimit = len("第一行\n第二")
	sender := &fakeStreamingSender{}

	if err := b.Handle(context.Background(), InboundMessage{UserKey: "user", CommandText: "hi", LLMText: "hi"}, sender); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if len(sender.streams) != 3 {
		t.Fatalf("streams = %d, want 3", len(sender.streams))
	}
	var got []string
	for i, stream := range sender.streams {
		if len(stream.finishes) != 1 {
			t.Fatalf("stream %d finishes = %#v, want one finish", i+1, stream.finishes)
		}
		if !utf8.ValidString(stream.finishes[0]) {
			t.Fatalf("stream %d finish is invalid UTF-8: %q", i+1, stream.finishes[0])
		}
		got = append(got, stream.finishes[0])
	}
	if joined := strings.Join(got, ""); joined != text {
		t.Fatalf("joined stream text = %q, want %q", joined, text)
	}
	if got[0] != "第一行\n" || got[1] != "第二行\n" || got[2] != "第三行" {
		t.Fatalf("stream chunks = %#v, want line chunks", got)
	}
}

func TestHandleTextStreamsSplitLongLineWithoutBreakingUTF8(t *testing.T) {
	text := "网络端口🙂网络端口"
	sessions := &fakeSessions{}
	client := &fakeLLM{
		streamChunks: []string{text},
		resp:         llm.Response{Text: text},
	}
	b := testBot(sessions, client)
	b.TextChunkLimit = 5
	sender := &fakeStreamingSender{}

	if err := b.Handle(context.Background(), InboundMessage{UserKey: "user", CommandText: "hi", LLMText: "hi"}, sender); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if len(sender.streams) < 2 {
		t.Fatalf("streams = %d, want multiple streams", len(sender.streams))
	}
	var got []string
	for i, stream := range sender.streams {
		if len(stream.finishes) != 1 {
			t.Fatalf("stream %d finishes = %#v, want one finish", i+1, stream.finishes)
		}
		chunk := stream.finishes[0]
		if !utf8.ValidString(chunk) {
			t.Fatalf("stream %d finish is invalid UTF-8: %q", i+1, chunk)
		}
		got = append(got, chunk)
	}
	if joined := strings.Join(got, ""); joined != text {
		t.Fatalf("joined stream text = %q, want %q", joined, text)
	}
}

func TestHandleTextStreamErrorUpdatesNoticeAndDoesNotSaveHistory(t *testing.T) {
	sessions := &fakeSessions{}
	streamErr := errors.New("stream broke")
	client := &fakeLLM{
		streamChunks: []string{"partial"},
		streamErr:    streamErr,
	}
	b := testBot(sessions, client)
	sender := &fakeStreamingSender{}

	err := b.Handle(context.Background(), InboundMessage{UserKey: "user", CommandText: "hi", LLMText: "hi"}, sender)
	if !errors.Is(err, streamErr) {
		t.Fatalf("Handle error = %v, want streamErr", err)
	}
	if sender.stream == nil {
		t.Fatal("stream was not started")
	}
	if len(sender.stream.finishes) != 1 || !strings.Contains(sender.stream.finishes[0], "AI 响应失败") {
		t.Fatalf("stream finishes = %#v, want error notice", sender.stream.finishes)
	}
	if sessions.saved != nil {
		t.Fatalf("saved = %#v, want no failed assistant history", sessions.saved)
	}
	if len(sender.sent) != 0 {
		t.Fatalf("sent = %#v, want stream error notice without fallback send", sender.sent)
	}
}

func TestHandleUsesPrepareHookAndPrepareErrorNotice(t *testing.T) {
	sessions := &fakeSessions{}
	client := &fakeLLM{}
	b := testBot(sessions, client)
	sender := &fakeSender{}
	prepareErr := errors.New("prepare failed")

	err := b.Handle(context.Background(), InboundMessage{
		UserKey: "user",
		PrepareUserMessage: func(ctx context.Context, userID, sessionID string, client llm.Client) (store.Message, error) {
			return store.Message{}, prepareErr
		},
		PrepareErrorNotice: func(error) string { return "custom prepare notice" },
	}, sender)
	if !errors.Is(err, prepareErr) {
		t.Fatalf("Handle error = %v, want prepareErr", err)
	}
	if len(sender.sent) != 1 || sender.sent[0].Text != "custom prepare notice" {
		t.Fatalf("sent = %#v, want custom prepare notice", sender.sent)
	}
}

func TestSplitTextChunksPrefersLineBoundaries(t *testing.T) {
	text := "第一行\n\n第二行\n第三行"
	chunks := SplitTextChunks(text, len("第一行\n第二"))

	if got, want := strings.Join(chunks, ""), text; got != want {
		t.Fatalf("joined chunks = %q, want %q", got, want)
	}
	want := []string{"第一行\n\n", "第二行\n", "第三行"}
	if len(chunks) != len(want) {
		t.Fatalf("chunks = %#v, want %#v", chunks, want)
	}
	for i := range want {
		if chunks[i] != want[i] {
			t.Fatalf("chunk %d = %q, want %q", i+1, chunks[i], want[i])
		}
		if !utf8.ValidString(chunks[i]) {
			t.Fatalf("chunk %d is invalid UTF-8: %q", i+1, chunks[i])
		}
	}
}

func TestSplitTextChunksSplitsLongLineOnUTF8Boundary(t *testing.T) {
	text := "网络端口🙂网络端口"
	chunks := SplitTextChunks(text, 5)

	if len(chunks) < 2 {
		t.Fatalf("chunks = %#v, want multiple chunks", chunks)
	}
	if got, want := strings.Join(chunks, ""), text; got != want {
		t.Fatalf("joined chunks = %q, want %q", got, want)
	}
	for i, chunk := range chunks {
		if !utf8.ValidString(chunk) {
			t.Fatalf("chunk %d is invalid UTF-8: %q", i+1, chunk)
		}
	}
}

func TestSplitTextChunksKeepsRuneWhenLimitSmallerThanRune(t *testing.T) {
	text := "网络"
	chunks := SplitTextChunks(text, 1)

	if got, want := strings.Join(chunks, ""), text; got != want {
		t.Fatalf("joined chunks = %q, want %q", got, want)
	}
	if len(chunks) != 2 || chunks[0] != "网" || chunks[1] != "络" {
		t.Fatalf("chunks = %#v, want individual runes", chunks)
	}
	for i, chunk := range chunks {
		if !utf8.ValidString(chunk) {
			t.Fatalf("chunk %d is invalid UTF-8: %q", i+1, chunk)
		}
	}
}

func TestSplitTextChunksByRunesUsesCharacterLimit(t *testing.T) {
	text := strings.Repeat("界", 5)
	chunks := SplitTextChunksByRunes(text, 2)

	if len(chunks) != 3 {
		t.Fatalf("chunks = %#v, want 3 chunks", chunks)
	}
	for i, chunk := range chunks {
		if !utf8.ValidString(chunk) {
			t.Fatalf("chunk %d is invalid UTF-8: %q", i+1, chunk)
		}
		if got := len([]rune(chunk)); got > 2 {
			t.Fatalf("chunk %d rune length = %d, want <= 2", i+1, got)
		}
	}
	if got := strings.Join(chunks, ""); got != text {
		t.Fatalf("joined chunks = %q, want %q", got, text)
	}
}

func withImmediateLLMRetry(t *testing.T) {
	t.Helper()
	original := llmTurnRetryDelays
	llmTurnRetryDelays = []time.Duration{0, 0}
	t.Cleanup(func() {
		llmTurnRetryDelays = original
	})
}

func retryableLLMHTTPError() error {
	return &llm.HTTPError{Label: "responses", StatusCode: 504, Body: "gateway timeout"}
}

func testBot(sessions *fakeSessions, client *fakeLLM) *Bot {
	cfg := config.LLMConfig{
		DefaultModel: "deepseek",
		Models: map[string]config.LLMModelConfig{
			"deepseek": {Provider: "openai", BaseURL: "https://llm.test", APIKey: "key", ID: "model"},
		},
		SystemPrompt: "system",
	}
	return &Bot{
		Sessions:            sessions,
		LLMConfig:           cfg,
		LLMClients:          map[string]llm.Client{},
		NewLLM:              func(config.ResolvedModel) llm.Client { return client },
		TextChunkLimit:      testTextChunkLimit,
		EnableTextStreaming: true,
	}
}
