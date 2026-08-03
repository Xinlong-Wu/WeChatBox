package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"lingobridge/internal/commands"
	"lingobridge/internal/config"
	"lingobridge/internal/llm"
	"lingobridge/internal/logging"
	"lingobridge/internal/store"
	tooltypes "lingobridge/internal/tools"
)

var coreLog = logging.For("core")

const llmTurnMaxAttempts = 3

var llmTurnRetryDelays = []time.Duration{2 * time.Second, 5 * time.Second}

type Platform interface {
	Run(ctx context.Context, handler Handler) error
}

type Handler interface {
	Handle(ctx context.Context, msg InboundMessage, sender Sender) error
}

type Sender interface {
	Send(ctx context.Context, msg OutboundMessage) error
	StartTyping(ctx context.Context) func()
}

type TextStreamSender interface {
	StartTextStream(ctx context.Context) (TextStream, error)
}

type TextStream interface {
	Update(ctx context.Context, text string) error
	Finish(ctx context.Context, text string) error
}

type InboundMessage struct {
	Platform             string
	AccountID            string
	AccountName          string
	UserKey              string
	Model                string
	CommandText          string
	CommandPolicy        commands.Policy
	LLMText              string
	SystemPromptSuffix   string
	PrepareUserMessage   PrepareUserMessageFunc
	PrepareErrorNotice   func(error) string
	MutateResponse       ResponseMutator
	ErrorNotice          func(error) string
	Metadata             map[string]string
	Tools                []tooltypes.Tool
	ToolOptions          tooltypes.Options
	DisableProviderTools bool
}

type OutboundMessage struct {
	Text  string
	Image llm.Image
}

type PrepareUserMessageFunc func(ctx context.Context, userID, sessionID string, client llm.Client) (store.Message, error)

type ConversationManager interface {
	commands.SessionManager
	GetOrCreateCurrentSession(userID string) (*store.Session, error)
	LoadHistory(userID, sessionID string) (*store.Conversation, error)
	SaveHistoryCAS(userID, sessionID string, expectedRevision int64, conv *store.Conversation) (int64, error)
}

type WorkflowContinuationManager interface {
	CommitWorkflowContinuation(requestID, accountID string, committedRevision int64, now time.Time) (store.WorkflowContinuation, bool, error)
	CancelWorkflowContinuation(requestID, accountID, reason string, now time.Time) error
}

type LLMFactory func(config.ResolvedModel) llm.Client
type ResponseMutator func(userID, sessionID string, resp llm.Response) llm.Response

type Bot struct {
	Sessions            ConversationManager
	LLMConfig           config.LLMConfig
	LLMClients          map[string]llm.Client
	mu                  sync.Mutex
	laneMu              sync.Mutex
	lanes               *sessionLaneSet
	NewLLM              LLMFactory
	MutateResponse      ResponseMutator
	ErrorNotice         func(error) string
	TextChunkLimit      int
	EnableTextStreaming bool
	ToolProvider        tooltypes.Provider
	Workflows           WorkflowContinuationManager
}

func New(sessions ConversationManager, cfg config.LLMConfig) *Bot {
	return &Bot{
		Sessions:            sessions,
		LLMConfig:           cfg,
		LLMClients:          map[string]llm.Client{},
		lanes:               newSessionLaneSet(),
		NewLLM:              defaultLLMFactory,
		EnableTextStreaming: false,
	}
}

func (b *Bot) Handle(ctx context.Context, msg InboundMessage, sender Sender) error {
	if msg.UserKey == "" {
		return nil
	}
	if isCompactCommand(msg.CommandText) {
		return b.handleCompactCommand(ctx, msg, sender)
	}
	selection := b.resolveToolsForMessage(ctx, msg)
	msg.Tools = selection.Tools
	commandTools := commandToolSummaries(msg.Tools)
	commandOptions := commands.HandleOptions{
		Policy: msg.CommandPolicy,
		Tools:  commandTools,
	}
	if commandUsesCurrentSessionLane(msg.CommandText) {
		sess, err := b.Sessions.GetOrCreateCurrentSession(msg.UserKey)
		if err != nil {
			coreLog.Error(ctx, "get session for command: %v", err)
			_ = sender.Send(ctx, OutboundMessage{Text: "❌ 会话加载失败，请重试。"})
			return err
		}
		var handled bool
		err = b.withSessionLane(ctx, msg, sess.ID, func(laneCtx context.Context) error {
			var commandErr error
			handled, commandErr = b.handleSharedCommand(laneCtx, msg, sender, commandOptions, len(commandTools))
			return commandErr
		})
		if handled || err != nil {
			return err
		}
	}
	if handled, err := b.handleSharedCommand(ctx, msg, sender, commandOptions, len(commandTools)); handled || err != nil {
		return err
	}
	if strings.TrimSpace(msg.LLMText) == "" && msg.PrepareUserMessage == nil {
		return nil
	}
	return b.reply(ctx, msg, sender, selection.Options)
}

func (b *Bot) handleSharedCommand(ctx context.Context, msg InboundMessage, sender Sender, opts commands.HandleOptions, toolCount int) (bool, error) {
	resp, handled, err := commands.HandleWithOptions(msg.CommandText, msg.UserKey, b.Sessions, opts)
	if !handled {
		return false, nil
	}
	if err != nil {
		coreLog.Warn(ctx, "command error: %v", err)
		_ = sender.Send(ctx, OutboundMessage{Text: fmt.Sprintf("❌ 错误：%v", err)})
		return true, nil
	}
	coreLog.Debug(ctx, "command handled command=%s tools=%d", commandName(msg.CommandText), toolCount)
	return true, sender.Send(ctx, OutboundMessage{Text: resp})
}

func commandUsesCurrentSessionLane(text string) bool {
	switch commandName(text) {
	case "/current", "/rename", "/archive", "/clear", "/model":
		return true
	default:
		return false
	}
}

func (b *Bot) resolveToolsForMessage(ctx context.Context, msg InboundMessage) tooltypes.Selection {
	var providerSelection tooltypes.Selection
	if msg.DisableProviderTools {
		coreLog.Debug(ctx, "provider tools disabled platform=%s account=%s platform_tools=%d", msg.Platform, msg.AccountName, len(msg.Tools))
		return tooltypes.Selection{Tools: mergeTools(ctx, msg.Tools, nil), Options: msg.ToolOptions}
	}
	if b.ToolProvider != nil {
		providerSelection = b.ToolProvider.Resolve(tooltypes.Scope{
			Platform:    msg.Platform,
			AccountID:   msg.AccountID,
			AccountName: msg.AccountName,
		})
	}
	tools := mergeTools(ctx, msg.Tools, providerSelection.Tools)
	if len(tools) != len(msg.Tools)+len(providerSelection.Tools) {
		coreLog.Debug(ctx, "merged tools platform=%d provider=%d effective=%d", len(msg.Tools), len(providerSelection.Tools), len(tools))
	}
	return tooltypes.Selection{Tools: tools, Options: mergeToolOptions(providerSelection.Options, msg.ToolOptions)}
}

func effectiveSystemPrompt(base, suffix string) string {
	suffix = strings.TrimSpace(suffix)
	if suffix == "" {
		return base
	}
	base = strings.TrimRight(base, "\n")
	if strings.TrimSpace(base) == "" {
		return suffix
	}
	return base + "\n\n" + suffix
}

func retryLLMTurn[T any](ctx context.Context, provider, modelName, operation string, onChunk func(string) error, call func(func(string) error) (T, error)) (T, error) {
	var zero T
	var lastErr error
	for attempt := 1; attempt <= llmTurnMaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		chunkEmitted := false
		attemptOnChunk := onChunk
		if onChunk != nil {
			attemptOnChunk = func(chunk string) error {
				if chunk != "" {
					chunkEmitted = true
				}
				return onChunk(chunk)
			}
		}
		resp, err := call(attemptOnChunk)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if chunkEmitted || !llm.IsRetryableError(err) || attempt == llmTurnMaxAttempts {
			return zero, err
		}
		delay := llmTurnRetryDelay(attempt)
		coreLog.Warn(ctx, "retrying LLM turn provider=%s model=%s operation=%s attempt=%d max_attempts=%d delay=%s error=%s",
			provider, modelName, operation, attempt+1, llmTurnMaxAttempts, delay, truncateText(err.Error(), defaultToolTraceTextLimit))
		if !sleepLLMRetry(ctx, delay) {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return zero, ctxErr
			}
			return zero, err
		}
	}
	return zero, lastErr
}

func sleepLLMRetry(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		return true
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func llmTurnRetryDelay(failedAttempt int) time.Duration {
	index := failedAttempt - 1
	if index < 0 || index >= len(llmTurnRetryDelays) {
		return 0
	}
	return llmTurnRetryDelays[index]
}

func (b *Bot) reply(ctx context.Context, msg InboundMessage, sender Sender, toolOptions tooltypes.Options) error {
	sess, err := b.Sessions.GetOrCreateCurrentSession(msg.UserKey)
	if err != nil {
		coreLog.Error(ctx, "get session: %v", err)
		_ = sender.Send(ctx, OutboundMessage{Text: "❌ 会话加载失败，请重试。"})
		return err
	}
	return b.withSessionLane(ctx, msg, sess.ID, func(laneCtx context.Context) error {
		return b.replyInSession(laneCtx, msg, sender, toolOptions, sess)
	})
}

func (b *Bot) replyInSession(ctx context.Context, msg InboundMessage, sender Sender, toolOptions tooltypes.Options, sess *store.Session) error {
	model, llmClient, err := b.llmForMessage(ctx, msg, sess.ID)
	if err != nil {
		coreLog.Error(ctx, "resolve LLM: %v", err)
		_ = sender.Send(ctx, OutboundMessage{Text: "❌ 模型配置不可用，请检查配置。"})
		return err
	}
	modelName, provider := model.Name, model.Provider
	systemPrompt := effectiveSystemPrompt(b.LLMConfig.SystemPrompt, msg.SystemPromptSuffix)

	userMsg, err := b.prepareUserMessage(ctx, msg, sess.ID, llmClient)
	if err != nil {
		coreLog.Warn(ctx, "prepare user message failed provider=%s model=%s: %v", provider, modelName, err)
		_ = sender.Send(ctx, OutboundMessage{Text: b.prepareErrorNotice(msg, err)})
		return err
	}
	if userMsg.Content == "" && len(userMsg.Attachments) == 0 {
		return nil
	}

	conv, err := b.Sessions.LoadHistory(msg.UserKey, sess.ID)
	if err != nil {
		coreLog.Warn(ctx, "load history: %v", err)
		conv = &store.Conversation{}
	}
	if conv == nil {
		conv = &store.Conversation{}
	}
	expectedRevision := conv.Revision
	if len(msg.Tools) > 0 {
		var execution tooltypes.ExecutionContext
		ctx, execution, err = bindToolExecutionContext(ctx, msg, sess.ID, expectedRevision)
		if err != nil {
			coreLog.Error(ctx, "bind tool execution context: %v", err)
			_ = sender.Send(ctx, OutboundMessage{Text: "❌ 工具执行上下文初始化失败，请重试。"})
			return err
		}
		coreLog.Debug(ctx, "bound trusted tool context platform=%s account=%s session=%s turn=%s chat_present=%t actor_present=%t revision=%d",
			execution.Platform, execution.AccountID, execution.SessionID, execution.TurnID,
			execution.ChatID != "", execution.ActorOpenID != "" || execution.ActorUserID != "", execution.ConversationRevision)
	}

	compact := llm.CompactConfig{
		Mode:          string(model.Compact.Mode),
		ContextWindow: model.ContextWindow,
		Threshold:     model.Compact.Threshold,
		Instructions:  model.Compact.Instructions,
	}
	historyForRequest := conv.Messages
	providerContext := providerContextForModel(conv, modelName)
	preCompacted := false
	var compactNoticeHandle CompactNoticeHandle
	var preCompactNotice CompactNotice
	compactAllowed := automaticCompactAllowed(compact)
	if compactAllowed {
		var compactErr error
		historyForRequest, providerContext, preCompacted, compactErr = b.prepareNativeContext(systemPrompt, historyForRequest, userMsg, providerContext, compact, llmClient, func(compactedMessages, retainedMessages int) {
			preCompactNotice = CompactNotice{
				ModelName:         modelName,
				Manual:            false,
				CompactedMessages: compactedMessages,
				RetainedMessages:  retainedMessages,
			}
			compactNoticeHandle = startCompactNotice(ctx, sender, preCompactNotice)
		})
		if compactErr != nil {
			coreLog.Error(ctx, "compact context failed provider=%s model=%s: %v", provider, modelName, compactErr)
			_ = sender.Send(ctx, OutboundMessage{Text: b.errorNotice(msg, compactErr)})
			return compactErr
		}
	}

	msgs := ToLLMMessagesWithUserMessage(systemPrompt, &store.Conversation{Messages: historyForRequest}, userMsg, b.LLMConfig.MaxHistory)

	var textStream *replyTextStream
	if b.EnableTextStreaming {
		if streamSender, ok := sender.(TextStreamSender); ok {
			textStream = newReplyTextStream(streamSender, b.chunkLimit())
		}
	}
	var onChunk func(string) error
	if textStream != nil {
		onChunk = func(chunk string) error {
			return textStream.OnChunk(ctx, chunk)
		}
	}

	stopTyping := sender.StartTyping(ctx)
	var llmResponse llm.Response
	var toolTraces []store.ToolTrace
	var pendingWorkflowIDs []string
	if len(msg.Tools) > 0 {
		if toolClient, ok := llmClient.(llm.ToolCallingClient); ok {
			llmResponse, toolTraces, pendingWorkflowIDs, err = b.chatWithTools(ctx, toolClient, provider, modelName, systemPrompt, msgs, providerContext, compact, compactAllowed, msg.Tools, toolOptions, onChunk)
		} else {
			coreLog.Warn(ctx, "model provider=%s model=%s does not support tool calling; continuing without tools", provider, modelName)
			llmResponse, err = b.chatWithoutTools(ctx, llmClient, provider, modelName, systemPrompt, compactAllowed, providerContext, compact, msgs, onChunk)
		}
	} else {
		llmResponse, err = b.chatWithoutTools(ctx, llmClient, provider, modelName, systemPrompt, compactAllowed, providerContext, compact, msgs, onChunk)
	}
	stopTyping()
	if err != nil {
		b.cancelPendingWorkflows(ctx, msg.AccountID, pendingWorkflowIDs, "origin turn did not commit")
		if ctxErr := ctx.Err(); ctxErr != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			coreLog.Warn(ctx, "reply canceled provider=%s model=%s: %v", provider, modelName, err)
			return err
		}
		coreLog.Error(ctx, "LLM error provider=%s model=%s: %v", provider, modelName, err)
		notice := b.errorNotice(msg, err)
		if textStream != nil && textStream.Started() {
			if streamErr := textStream.Finish(ctx, notice); streamErr == nil {
				return err
			}
		}
		_ = sender.Send(ctx, OutboundMessage{Text: notice})
		return err
	}
	if msg.MutateResponse != nil {
		llmResponse = msg.MutateResponse(msg.UserKey, sess.ID, llmResponse)
	} else if b.MutateResponse != nil {
		llmResponse = b.MutateResponse(msg.UserKey, sess.ID, llmResponse)
	}

	assistantHistory, err := llmClient.AssistantMessage(llmResponse)
	if err != nil {
		b.cancelPendingWorkflows(ctx, msg.AccountID, pendingWorkflowIDs, "origin turn did not commit")
		coreLog.Error(ctx, "prepare assistant history failed provider=%s model=%s: %v", provider, modelName, err)
		_ = sender.Send(ctx, OutboundMessage{Text: b.errorNotice(msg, err)})
		return err
	}
	assistantHistory.ToolTraces = toolTraces
	historyForSave := conv.Messages
	if compactAllowed {
		if preCompacted {
			historyForSave = historyForRequest
		}
		if llmResponse.Compacted {
			historyForSave = retainRecentMessages(historyForSave, nativeContextKeepRecentMessages)
		}
		if !llmResponse.ProviderContext.IsEmpty() {
			providerContext = llmResponse.ProviderContext
		}
		if preCompacted || llmResponse.Compacted || !providerContext.IsEmpty() {
			if conv.ProviderContexts == nil {
				conv.ProviderContexts = map[string]store.ProviderContext{}
			}
			if !providerContext.IsEmpty() {
				conv.ProviderContexts[modelName] = providerContext
			}
		}
	}
	conv.Messages = append(historyForSave, userMsg, assistantHistory)

	newRevision, err := b.Sessions.SaveHistoryCAS(msg.UserKey, sess.ID, expectedRevision, conv)
	if err != nil {
		b.cancelPendingWorkflows(ctx, msg.AccountID, pendingWorkflowIDs, "origin conversation save failed")
		coreLog.Error(ctx, "save history with revision cas session=%s expected_revision=%d: %v", sess.ID, expectedRevision, err)
		notice := "❌ 会话保存失败，本次回复可能未写入历史，请重试。"
		if errors.Is(err, store.ErrConversationConflict) {
			notice = "❌ 会话在处理期间发生变化，本次回复未写入历史，请重试。"
		}
		_ = sender.Send(ctx, OutboundMessage{Text: notice})
		return err
	}
	coreLog.Debug(ctx, "saved conversation session=%s revision=%d messages=%d", sess.ID, newRevision, len(conv.Messages))
	if err := b.commitPendingWorkflows(ctx, msg.AccountID, pendingWorkflowIDs, newRevision); err != nil {
		coreLog.Error(ctx, "commit pending workflow continuations session=%s revision=%d count=%d: %v", sess.ID, newRevision, len(pendingWorkflowIDs), err)
		_ = sender.Send(ctx, OutboundMessage{Text: "❌ 异步授权状态保存失败，请重新发起操作。"})
		return err
	}
	if preCompacted {
		if err := finishCompactNotice(ctx, sender, compactNoticeHandle, preCompactNotice); err != nil {
			return err
		}
	}

	if llmResponse.Text != "" {
		chunks := SplitTextChunks(llmResponse.Text, b.chunkLimit())
		start := 0
		if textStream != nil {
			if err := textStream.Finish(ctx, llmResponse.Text); err != nil {
				return err
			}
			start = textStream.FinishedChunks(len(chunks))
		}
		for _, chunk := range chunks[start:] {
			if err := sender.Send(ctx, OutboundMessage{Text: chunk}); err != nil {
				return err
			}
		}
	}

	for i, image := range llmResponse.Images {
		if err := sender.Send(ctx, OutboundMessage{Image: image}); err != nil {
			coreLog.Error(ctx, "send image failed image=%d/%d: %v", i+1, len(llmResponse.Images), err)
			_ = sender.Send(ctx, OutboundMessage{Text: b.errorNotice(msg, err)})
			return err
		}
	}

	coreLog.Debug(ctx, "reply to=%s provider=%s model=%s len=%d images=%d", msg.UserKey, provider, modelName, len(llmResponse.Text), len(llmResponse.Images))
	return nil
}

func (b *Bot) chatWithoutTools(ctx context.Context, client llm.Client, provider, modelName, systemPrompt string, compactAllowed bool, providerContext store.ProviderContext, compact llm.CompactConfig, msgs []store.Message, onChunk func(string) error) (llm.Response, error) {
	return retryLLMTurn(ctx, provider, modelName, "chat", onChunk, func(attemptOnChunk func(string) error) (llm.Response, error) {
		if compactAllowed {
			if contextClient, ok := client.(llm.ContextStreamingClient); ok {
				return contextClient.ChatStreamWithContext(systemPrompt, msgs, providerContext, compact, attemptOnChunk)
			}
		}
		return client.ChatStream(systemPrompt, msgs, attemptOnChunk)
	})
}

func (b *Bot) chatWithTools(ctx context.Context, client llm.ToolCallingClient, provider, modelName, systemPrompt string, msgs []store.Message, providerContext store.ProviderContext, compact llm.CompactConfig, compactAllowed bool, tools []tooltypes.Tool, options tooltypes.Options, onChunk func(string) error) (llm.Response, []store.ToolTrace, []string, error) {
	specs := toolSpecs(tools)
	if len(specs) == 0 {
		coreLog.Warn(ctx, "tool calling requested with %d tool entries but no valid tool specs; falling back to plain chat", len(tools))
		baseClient, ok := client.(llm.Client)
		if !ok {
			return llm.Response{}, nil, nil, fmt.Errorf("tool-capable client does not implement base chat")
		}
		resp, err := b.chatWithoutTools(ctx, baseClient, provider, modelName, systemPrompt, compactAllowed, providerContext, compact, msgs, onChunk)
		return resp, nil, nil, err
	}

	maxCalls := effectiveMaxToolCalls(options.MaxCalls)
	lookup := toolMap(tools)
	coreLog.Debug(ctx, "tool loop start tools=%d max_calls=%d timeout=%s result_limit=%d", len(specs), maxCalls, effectiveToolTimeout(options.Timeout), effectiveToolResultLimit(options.ResultLimit))
	var traces []store.ToolTrace
	var pendingWorkflowIDs []string
	pendingWorkflowSet := map[string]struct{}{}
	var previous llm.ToolState
	var results []tooltypes.Result
	compacted := false
	totalCalls := 0
	pendingBudgetReminder := toolBudgetReminderNone
	pendingBudgetRemaining := maxCalls
	sentBudget10 := false
	sentBudget5 := false
	effectiveCompact := llm.CompactConfig{}
	if compactAllowed {
		effectiveCompact = compact
	}

	for {
		if err := ctx.Err(); err != nil {
			return llm.Response{}, traces, pendingWorkflowIDs, err
		}
		turnSystemPrompt := toolBudgetSystemPrompt(systemPrompt, maxCalls, pendingBudgetReminder, pendingBudgetRemaining)
		pendingBudgetReminder = toolBudgetReminderNone
		resp, err := retryLLMTurn(ctx, provider, modelName, "tool_turn", onChunk, func(attemptOnChunk func(string) error) (llm.ToolResponse, error) {
			return client.ChatStreamWithTools(turnSystemPrompt, msgs, providerContext, effectiveCompact, specs, previous, results, attemptOnChunk)
		})
		if err != nil {
			return llm.Response{}, traces, pendingWorkflowIDs, err
		}
		if len(resp.ToolCalls) == 0 {
			coreLog.Debug(ctx, "tool loop finish calls=%d traces=%d text_len=%d images=%d", totalCalls, len(traces), len(resp.Text), len(resp.Images))
			finalResp := resp.Response
			if compacted {
				finalResp.Compacted = true
			}
			if !providerContext.IsEmpty() && finalResp.ProviderContext.IsEmpty() {
				finalResp.ProviderContext = providerContext
			}
			return finalResp, traces, pendingWorkflowIDs, nil
		}
		if totalCalls+len(resp.ToolCalls) > maxCalls {
			err := fmt.Errorf("tool call limit exceeded: %d > %d", totalCalls+len(resp.ToolCalls), maxCalls)
			coreLog.Warn(ctx, "%v", err)
			return llm.Response{}, traces, pendingWorkflowIDs, err
		}

		previous = appendToolState(previous, resp.ToolState)
		if !resp.Response.ProviderContext.IsEmpty() {
			providerContext = resp.Response.ProviderContext
		}
		if resp.Response.Compacted {
			compacted = true
		}
		coreLog.Debug(ctx, "model requested tool calls count=%d total_before=%d", len(resp.ToolCalls), totalCalls)
		for _, call := range resp.ToolCalls {
			if err := ctx.Err(); err != nil {
				return llm.Response{}, traces, pendingWorkflowIDs, err
			}
			call.Name = strings.TrimSpace(call.Name)
			if _, ok := lookup[call.Name]; !ok {
				coreLog.Warn(ctx, "model requested unavailable tool name=%s call_id=%s", call.Name, call.ID)
			}
			coreLog.Debug(ctx, "tool call start name=%s call_id=%s args=%s", call.Name, call.ID, summarizeToolArgumentsForLog(call.Arguments, defaultToolTraceTextLimit))
			totalCalls++
			result, trace, toolErr := runTool(ctx, lookup[call.Name], call, options.Timeout, options.ResultLimit)
			if requestID := strings.TrimSpace(result.PendingWorkflowID); requestID != "" {
				if _, exists := pendingWorkflowSet[requestID]; !exists {
					pendingWorkflowSet[requestID] = struct{}{}
					pendingWorkflowIDs = append(pendingWorkflowIDs, requestID)
				}
			}
			if toolErr != nil {
				traces = append(traces, trace)
				coreLog.Warn(ctx, "tool loop canceled name=%s call_id=%s duration_ms=%d error=%s", trace.Name, trace.CallID, trace.DurationMillis, truncateText(trace.Error, defaultToolTraceTextLimit))
				return llm.Response{}, traces, pendingWorkflowIDs, toolErr
			}
			results = append(results, result)
			traces = append(traces, trace)
			if trace.Status == "error" {
				coreLog.Warn(ctx, "tool call failed name=%s call_id=%s duration_ms=%d error=%s", trace.Name, trace.CallID, trace.DurationMillis, truncateText(trace.Error, defaultToolTraceTextLimit))
			} else {
				coreLog.Debug(ctx, "tool call finished name=%s call_id=%s status=%s duration_ms=%d", trace.Name, trace.CallID, trace.Status, trace.DurationMillis)
			}
		}
		remaining := maxCalls - totalCalls
		switch nextToolBudgetReminder(maxCalls, remaining, sentBudget10, sentBudget5) {
		case toolBudgetReminderFivePercent:
			pendingBudgetReminder = toolBudgetReminderFivePercent
			pendingBudgetRemaining = remaining
			sentBudget5 = true
			sentBudget10 = true
			coreLog.Debug(ctx, "tool budget reminder queued severity=5%% remaining=%d max_calls=%d", remaining, maxCalls)
		case toolBudgetReminderTenPercent:
			pendingBudgetReminder = toolBudgetReminderTenPercent
			pendingBudgetRemaining = remaining
			sentBudget10 = true
			coreLog.Debug(ctx, "tool budget reminder queued severity=10%% remaining=%d max_calls=%d", remaining, maxCalls)
		}
	}
}

func appendToolState(previous, next llm.ToolState) llm.ToolState {
	if next.IsEmpty() {
		return previous
	}
	if previous.IsEmpty() || previous.Provider != next.Provider || previous.Endpoint != next.Endpoint {
		return next
	}
	previous.Items = append(previous.Items, next.Items...)
	return previous
}

func effectiveMaxToolCalls(maxCalls int) int {
	if maxCalls <= 0 {
		return defaultMaxToolCalls
	}
	return maxCalls
}

func effectiveToolTimeout(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return defaultToolTimeout
	}
	return timeout
}

func effectiveToolResultLimit(limit int) int {
	if limit <= 0 {
		return defaultToolResultLimit
	}
	return limit
}

func (b *Bot) prepareUserMessage(ctx context.Context, msg InboundMessage, sessionID string, llmClient llm.Client) (store.Message, error) {
	if msg.PrepareUserMessage != nil {
		return msg.PrepareUserMessage(ctx, msg.UserKey, sessionID, llmClient)
	}
	return llmClient.PrepareUserMessage(msg.LLMText, nil)
}

func (b *Bot) chunkLimit() int {
	if b.TextChunkLimit <= 0 {
		return -1
	}
	return b.TextChunkLimit
}

func (b *Bot) prepareErrorNotice(msg InboundMessage, err error) string {
	if msg.PrepareErrorNotice != nil {
		return msg.PrepareErrorNotice(err)
	}
	return b.errorNotice(msg, err)
}

func (b *Bot) errorNotice(msg InboundMessage, err error) string {
	if msg.ErrorNotice != nil {
		return msg.ErrorNotice(err)
	}
	if b.ErrorNotice != nil {
		return b.ErrorNotice(err)
	}
	return AIErrorNotice(err)
}
