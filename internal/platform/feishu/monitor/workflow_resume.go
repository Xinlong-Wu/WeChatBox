package monitor

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"lingobridge/internal/core"
	"lingobridge/internal/store"
	tooltypes "lingobridge/internal/tools"
)

const (
	defaultWorkflowResumePollInterval = time.Second
	defaultWorkflowResumeLease        = 5 * time.Minute
	defaultWorkflowResumeTimeout      = 3 * time.Minute
	defaultWorkflowResumeBatchSize    = 20
	defaultWorkflowOriginRecoverySize = 200
	defaultWorkflowResumeMaxAttempts  = 5
)

var defaultWorkflowResumeRetryDelays = []time.Duration{
	2 * time.Second,
	5 * time.Second,
	15 * time.Second,
	30 * time.Second,
}

type workflowResumeStore interface {
	ListUncommittedWorkflowContinuationsAfter(accountID string, afterCreatedAt time.Time, afterRequestID string, limit int) ([]store.WorkflowContinuation, error)
	LoadConversation(userID, sessionID string) (*store.Conversation, error)
	CommitWorkflowContinuation(requestID, accountID string, committedRevision int64, now time.Time) (store.WorkflowContinuation, bool, error)
	ListResumableWorkflowContinuations(accountID string, now time.Time, limit int) ([]store.WorkflowContinuation, error)
	ClaimWorkflowContinuation(requestID, accountID, leaseToken string, now time.Time, leaseDuration time.Duration) (store.WorkflowContinuation, error)
	GetWorkflowResult(requestID, accountID string) (store.WorkflowResult, error)
	GetWorkflowCardReference(requestID, accountID string) (store.WorkflowCardReference, error)
	RetryWorkflowContinuation(requestID, accountID, leaseToken string, availableAt time.Time, lastError string, now time.Time) error
	ReleaseWorkflowContinuation(requestID, accountID, leaseToken, lastError string, now time.Time) error
	CompleteWorkflowContinuation(requestID, accountID, leaseToken, state, lastError string, now time.Time) error
	MarkFeishuCardDeliveryDelivered(accountID, requestID, purpose string, revision int64, now time.Time) error
}

type workflowResumeTextSender interface {
	CreateTextWithUUID(ctx context.Context, chatID, text, uuid string) (string, error)
}

type workflowResumeCardUpdater interface {
	UpdateByMessageID(ctx context.Context, messageID string, card Card) error
}

type workflowContinuationWorker struct {
	store             workflowResumeStore
	resumer           core.WorkflowResumer
	sender            workflowResumeTextSender
	cards             workflowResumeCardUpdater
	account           store.Account
	tools             []tooltypes.Tool
	toolOptions       tooltypes.Options
	pollInterval      time.Duration
	lease             time.Duration
	resumeTimeout     time.Duration
	batchSize         int
	maxAttempts       int
	retryDelays       []time.Duration
	cardUpdateTimeout time.Duration
	now               func() time.Time

	originRecoveryMu      sync.Mutex
	originCursorCreatedAt time.Time
	originCursorRequestID string
}

func newWorkflowContinuationWorker(st workflowResumeStore, resumer core.WorkflowResumer, sender workflowResumeTextSender, cards workflowResumeCardUpdater, account store.Account, tools []tooltypes.Tool) (*workflowContinuationWorker, error) {
	if st == nil {
		return nil, fmt.Errorf("workflow continuation store is required")
	}
	if resumer == nil {
		return nil, fmt.Errorf("workflow resumer is required")
	}
	if sender == nil {
		return nil, fmt.Errorf("workflow resume sender is required")
	}
	if cards == nil {
		return nil, fmt.Errorf("workflow resume card updater is required")
	}
	if strings.TrimSpace(account.ID) == "" {
		return nil, fmt.Errorf("workflow resume account is required")
	}
	return &workflowContinuationWorker{
		store:             st,
		resumer:           resumer,
		sender:            sender,
		cards:             cards,
		account:           account,
		tools:             append([]tooltypes.Tool(nil), tools...),
		pollInterval:      defaultWorkflowResumePollInterval,
		lease:             defaultWorkflowResumeLease,
		resumeTimeout:     defaultWorkflowResumeTimeout,
		batchSize:         defaultWorkflowResumeBatchSize,
		maxAttempts:       defaultWorkflowResumeMaxAttempts,
		retryDelays:       append([]time.Duration(nil), defaultWorkflowResumeRetryDelays...),
		cardUpdateTimeout: defaultFeishuCardUpdateTimeout,
		now:               time.Now,
	}, nil
}

func (w *workflowContinuationWorker) Run(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	feishuLog.Info(ctx, "started feishu workflow continuation worker account=%s", w.account.ID)
	w.processAvailable(ctx)
	ticker := time.NewTicker(w.effectivePollInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			feishuLog.Debug(ctx, "stopped feishu workflow continuation worker account=%s", w.account.ID)
			return
		case <-ticker.C:
			w.processAvailable(ctx)
		}
	}
}

func (w *workflowContinuationWorker) processAvailable(ctx context.Context) {
	if err := ctx.Err(); err != nil {
		return
	}
	now := w.currentTime()
	w.reconcileUncommittedOriginTurns(ctx, now)
	continuations, err := w.store.ListResumableWorkflowContinuations(w.account.ID, now, w.effectiveBatchSize())
	if err != nil {
		feishuLog.Warn(ctx, "list resumable feishu workflows failed account=%s: %v", w.account.ID, err)
		return
	}
	for _, continuation := range continuations {
		if err := ctx.Err(); err != nil {
			return
		}
		w.processOne(ctx, continuation)
	}
}

func (w *workflowContinuationWorker) reconcileUncommittedOriginTurns(ctx context.Context, now time.Time) {
	w.originRecoveryMu.Lock()
	defer w.originRecoveryMu.Unlock()
	continuations, err := w.store.ListUncommittedWorkflowContinuationsAfter(
		w.account.ID,
		w.originCursorCreatedAt,
		w.originCursorRequestID,
		defaultWorkflowOriginRecoverySize,
	)
	if err != nil {
		feishuLog.Warn(ctx, "list uncommitted feishu workflow origins failed account=%s: %v", w.account.ID, err)
		return
	}
	if len(continuations) == 0 {
		w.originCursorCreatedAt = time.Time{}
		w.originCursorRequestID = ""
		return
	}
	for _, continuation := range continuations {
		if err := ctx.Err(); err != nil {
			return
		}
		conversation, loadErr := w.store.LoadConversation(continuation.UserKey, continuation.SessionID)
		if loadErr != nil {
			feishuLog.Warn(ctx, "load conversation for feishu workflow origin recovery failed request=%s account=%s session=%s: %v",
				shortRequestID(continuation.RequestID), continuation.AccountID, continuation.SessionID, loadErr)
			continue
		}
		committedRevision := continuation.OriginRevision + 1
		if conversation == nil || conversation.Revision < committedRevision {
			continue
		}
		if !conversationContainsCommittedWorkflowOrigin(conversation, continuation, committedRevision) {
			feishuLog.Debug(ctx, "skip unverified feishu workflow origin recovery request=%s account=%s session=%s expected_revision=%d current_revision=%d",
				shortRequestID(continuation.RequestID), continuation.AccountID, continuation.SessionID, committedRevision, conversationRevision(conversation))
			continue
		}
		recovered, ready, commitErr := w.store.CommitWorkflowContinuation(
			continuation.RequestID,
			continuation.AccountID,
			committedRevision,
			now,
		)
		if commitErr != nil {
			if errors.Is(commitErr, store.ErrWorkflowContinuationResolved) {
				continue
			}
			feishuLog.Warn(ctx, "recover feishu workflow origin commit failed request=%s account=%s session=%s revision=%d: %v",
				shortRequestID(continuation.RequestID), continuation.AccountID, continuation.SessionID, committedRevision, commitErr)
			continue
		}
		feishuLog.Info(ctx, "recovered feishu workflow origin commit request=%s account=%s session=%s revision=%d ready=%t state=%s",
			shortRequestID(continuation.RequestID), continuation.AccountID, continuation.SessionID, committedRevision, ready, recovered.State)
	}
	if len(continuations) < defaultWorkflowOriginRecoverySize {
		w.originCursorCreatedAt = time.Time{}
		w.originCursorRequestID = ""
		return
	}
	last := continuations[len(continuations)-1]
	w.originCursorCreatedAt = last.CreatedAt
	w.originCursorRequestID = last.RequestID
	feishuLog.Debug(ctx, "advanced feishu workflow origin recovery cursor account=%s count=%d last_request=%s",
		w.account.ID, len(continuations), shortRequestID(last.RequestID))
}

func conversationContainsCommittedWorkflowOrigin(conversation *store.Conversation, continuation store.WorkflowContinuation, committedRevision int64) bool {
	if conversation == nil || committedRevision <= continuation.OriginRevision || conversation.Revision < committedRevision {
		return false
	}
	if receipt, ok := conversation.WorkflowOriginReceipts[continuation.RequestID]; ok &&
		receipt.CommittedRevision == committedRevision &&
		strings.TrimSpace(receipt.ToolCallID) == continuation.ToolCallID &&
		strings.TrimSpace(receipt.ToolName) == continuation.ToolName {
		return true
	}
	for _, message := range conversation.Messages {
		if message.Role != "assistant" {
			continue
		}
		for _, trace := range message.ToolTraces {
			if strings.TrimSpace(trace.PendingWorkflowID) == continuation.RequestID &&
				strings.TrimSpace(trace.CallID) == continuation.ToolCallID &&
				strings.TrimSpace(trace.Name) == continuation.ToolName {
				return true
			}
		}
	}
	return false
}

func conversationRevision(conversation *store.Conversation) int64 {
	if conversation == nil {
		return 0
	}
	return conversation.Revision
}

func (w *workflowContinuationWorker) processOne(ctx context.Context, candidate store.WorkflowContinuation) {
	leaseToken, err := newWorkflowResumeLeaseToken()
	if err != nil {
		feishuLog.Error(ctx, "generate feishu workflow lease failed request=%s account=%s: %v", shortRequestID(candidate.RequestID), candidate.AccountID, err)
		return
	}
	claimedAt := w.currentTime()
	continuation, err := w.store.ClaimWorkflowContinuation(
		candidate.RequestID,
		candidate.AccountID,
		leaseToken,
		claimedAt,
		w.effectiveLease(),
	)
	if err != nil {
		if errors.Is(err, store.ErrWorkflowContinuationNotReady) || errors.Is(err, store.ErrWorkflowContinuationResolved) {
			feishuLog.Debug(ctx, "skip unavailable feishu workflow continuation request=%s account=%s: %v", shortRequestID(candidate.RequestID), candidate.AccountID, err)
			return
		}
		feishuLog.Warn(ctx, "claim feishu workflow continuation failed request=%s account=%s: %v", shortRequestID(candidate.RequestID), candidate.AccountID, err)
		return
	}
	result, err := w.store.GetWorkflowResult(continuation.RequestID, continuation.AccountID)
	if err != nil {
		w.handleFailure(ctx, continuation, leaseToken, fmt.Errorf("load workflow result: %w", err))
		return
	}
	resumeCtx, cancel := context.WithTimeout(ctx, w.effectiveResumeTimeout())
	responder := &workflowResumeResponder{
		sender:    w.sender,
		chatID:    continuation.ChatID,
		requestID: continuation.RequestID,
	}
	started := time.Now()
	err = w.resumer.ResumeWorkflow(resumeCtx, core.WorkflowResumeRequest{
		Continuation: continuation,
		Result:       result,
		AccountName:  strings.TrimSpace(w.account.Name),
		Tools:        w.tools,
		ToolOptions:  w.toolOptions,
	}, responder)
	cancel()
	if err != nil {
		w.handleFailure(ctx, continuation, leaseToken, err)
		return
	}
	completedAt := w.currentTime()
	if err := w.store.CompleteWorkflowContinuation(
		continuation.RequestID,
		continuation.AccountID,
		leaseToken,
		store.WorkflowContinuationStateDelivered,
		"",
		completedAt,
	); err != nil {
		feishuLog.Warn(ctx, "mark feishu workflow continuation delivered failed request=%s account=%s session=%s: %v",
			shortRequestID(continuation.RequestID), continuation.AccountID, continuation.SessionID, err)
		return
	}
	feishuLog.Info(ctx, "delivered feishu workflow continuation request=%s account=%s session=%s attempts=%d duration_ms=%d",
		shortRequestID(continuation.RequestID), continuation.AccountID, continuation.SessionID,
		continuation.Attempts, time.Since(started).Milliseconds())
}

func (w *workflowContinuationWorker) handleFailure(ctx context.Context, continuation store.WorkflowContinuation, leaseToken string, cause error) {
	now := w.currentTime()
	lastError := "workflow resume failed"
	if cause != nil {
		lastError = truncateApprovalRunes(strings.TrimSpace(cause.Error()), richTextLogPreviewRunes)
	}
	if errors.Is(cause, store.ErrSessionNotFound) {
		if err := w.store.CompleteWorkflowContinuation(
			continuation.RequestID,
			continuation.AccountID,
			leaseToken,
			store.WorkflowContinuationStateCanceled,
			lastError,
			now,
		); err != nil {
			feishuLog.Warn(ctx, "cancel unavailable feishu workflow continuation failed request=%s account=%s: %v", shortRequestID(continuation.RequestID), continuation.AccountID, err)
			return
		}
		feishuLog.Info(ctx, "canceled feishu workflow continuation for unavailable session request=%s account=%s session=%s",
			shortRequestID(continuation.RequestID), continuation.AccountID, continuation.SessionID)
		w.updateTerminalWorkflowCard(ctx, continuation, "unavailable_session", statusCard{
			title:    "原会话已不可用",
			template: "orange",
			message:  "授权或操作结果已保存，但原会话已归档或删除，LingoBridge 无法自动继续原任务。请在当前对话中重新发送原请求；已经生效的授权不会因此丢失。",
		})
		return
	}
	if ctx.Err() != nil {
		if err := w.store.ReleaseWorkflowContinuation(
			continuation.RequestID,
			continuation.AccountID,
			leaseToken,
			lastError,
			now,
		); err != nil {
			feishuLog.Warn(ctx, "release feishu workflow continuation during runtime shutdown failed request=%s account=%s: %v",
				shortRequestID(continuation.RequestID), continuation.AccountID, err)
			return
		}
		feishuLog.Debug(ctx, "released feishu workflow continuation for retry during runtime shutdown request=%s account=%s session=%s attempts=%d",
			shortRequestID(continuation.RequestID), continuation.AccountID, continuation.SessionID, continuation.Attempts)
		return
	}
	terminal := continuation.Attempts >= w.effectiveMaxAttempts() || errors.Is(cause, core.ErrWorkflowResumeInvalid)
	if terminal {
		if err := w.store.CompleteWorkflowContinuation(
			continuation.RequestID,
			continuation.AccountID,
			leaseToken,
			store.WorkflowContinuationStateFailed,
			lastError,
			now,
		); err != nil {
			feishuLog.Warn(ctx, "mark feishu workflow continuation failed request=%s account=%s: %v", shortRequestID(continuation.RequestID), continuation.AccountID, err)
			return
		}
		feishuLog.Error(ctx, "feishu workflow continuation exhausted request=%s account=%s session=%s attempts=%d error=%s",
			shortRequestID(continuation.RequestID), continuation.AccountID, continuation.SessionID, continuation.Attempts, lastError)
		w.updateTerminalWorkflowCard(ctx, continuation, "resume_exhausted", statusCard{
			title:    "原任务未自动继续",
			template: "orange",
			message:  "授权或操作结果已保存，但 LingoBridge 未能自动继续原任务。请在当前对话中重新发送原请求；已经生效的授权不会因此丢失。",
		})
		return
	}
	availableAt := now.Add(w.retryDelay(continuation.Attempts))
	if err := w.store.RetryWorkflowContinuation(
		continuation.RequestID,
		continuation.AccountID,
		leaseToken,
		availableAt,
		lastError,
		now,
	); err != nil {
		feishuLog.Warn(ctx, "schedule feishu workflow continuation retry failed request=%s account=%s: %v", shortRequestID(continuation.RequestID), continuation.AccountID, err)
		return
	}
	feishuLog.Warn(ctx, "scheduled feishu workflow continuation retry request=%s account=%s session=%s attempt=%d available_at=%s error=%s",
		shortRequestID(continuation.RequestID), continuation.AccountID, continuation.SessionID,
		continuation.Attempts, availableAt.Format(time.RFC3339), lastError)
}

func (w *workflowContinuationWorker) updateTerminalWorkflowCard(
	ctx context.Context,
	continuation store.WorkflowContinuation,
	outcome string,
	card statusCard,
) {
	reference, err := w.store.GetWorkflowCardReference(continuation.RequestID, continuation.AccountID)
	if err != nil {
		feishuLog.Warn(ctx, "resolve terminal feishu workflow card failed request=%s account=%s outcome=%s: %v",
			shortRequestID(continuation.RequestID), continuation.AccountID, outcome, err)
		return
	}
	if reference.CardMessageID == "" {
		feishuLog.Warn(ctx, "skip terminal feishu workflow card update without original card request=%s account=%s kind=%s outcome=%s",
			shortRequestID(continuation.RequestID), continuation.AccountID, reference.Kind, outcome)
		return
	}
	card = w.cardWithWorkflowResultDetail(ctx, continuation, card)
	err = updateFeishuCardByMessageIDWithTimeout(ctx, w.cards, reference.CardMessageID, card, w.effectiveCardUpdateTimeout())
	if err != nil {
		feishuLog.Warn(ctx, "update terminal feishu workflow card failed request=%s account=%s kind=%s outcome=%s: %v",
			shortRequestID(continuation.RequestID), continuation.AccountID, reference.Kind, outcome, err)
		return
	}
	purpose := ""
	switch outcome {
	case "unavailable_session":
		purpose = store.FeishuCardDeliveryPurposeWorkflowUnavailable
	case "resume_exhausted":
		purpose = store.FeishuCardDeliveryPurposeWorkflowExhausted
	}
	if purpose != "" {
		markErr := w.store.MarkFeishuCardDeliveryDelivered(
			continuation.AccountID,
			continuation.RequestID,
			purpose,
			store.FeishuCardDeliveryRevisionContinuation,
			w.currentTime(),
		)
		if markErr != nil && !errors.Is(markErr, store.ErrFeishuCardDeliveryNotFound) &&
			!errors.Is(markErr, store.ErrFeishuCardDeliveryNotReady) && !errors.Is(markErr, store.ErrFeishuCardDeliveryResolved) {
			feishuLog.Warn(ctx, "mark terminal feishu workflow card delivery failed request=%s account=%s outcome=%s: %v",
				shortRequestID(continuation.RequestID), continuation.AccountID, outcome, markErr)
		}
	}
	feishuLog.Info(ctx, "updated terminal feishu workflow card request=%s account=%s kind=%s outcome=%s attempts=%d",
		shortRequestID(continuation.RequestID), continuation.AccountID, reference.Kind, outcome, continuation.Attempts)
}

func (w *workflowContinuationWorker) cardWithWorkflowResultDetail(
	ctx context.Context,
	continuation store.WorkflowContinuation,
	card statusCard,
) statusCard {
	result, err := w.store.GetWorkflowResult(continuation.RequestID, continuation.AccountID)
	if err != nil {
		feishuLog.Warn(ctx, "load terminal feishu workflow result for card failed request=%s account=%s: %v",
			shortRequestID(continuation.RequestID), continuation.AccountID, err)
		return card
	}
	var payload struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(result.Payload, &payload); err != nil {
		feishuLog.Warn(ctx, "parse terminal feishu workflow result for card failed request=%s account=%s: %v",
			shortRequestID(continuation.RequestID), continuation.AccountID, err)
		return card
	}
	detail := strings.TrimSpace(payload.Message)
	if detail == "" {
		return card
	}
	card.message = detail + "\n\n---\n\n" + strings.TrimSpace(card.message)
	return card
}

func (w *workflowContinuationWorker) currentTime() time.Time {
	if w.now == nil {
		return time.Now().UTC()
	}
	return w.now().UTC()
}

func (w *workflowContinuationWorker) effectivePollInterval() time.Duration {
	if w.pollInterval <= 0 {
		return defaultWorkflowResumePollInterval
	}
	return w.pollInterval
}

func (w *workflowContinuationWorker) effectiveLease() time.Duration {
	if w.lease <= 0 {
		return defaultWorkflowResumeLease
	}
	return w.lease
}

func (w *workflowContinuationWorker) effectiveResumeTimeout() time.Duration {
	if w.resumeTimeout <= 0 {
		return defaultWorkflowResumeTimeout
	}
	return w.resumeTimeout
}

func (w *workflowContinuationWorker) effectiveBatchSize() int {
	if w.batchSize <= 0 || w.batchSize > 1000 {
		return defaultWorkflowResumeBatchSize
	}
	return w.batchSize
}

func (w *workflowContinuationWorker) effectiveMaxAttempts() int {
	if w.maxAttempts <= 0 {
		return defaultWorkflowResumeMaxAttempts
	}
	return w.maxAttempts
}

func (w *workflowContinuationWorker) effectiveCardUpdateTimeout() time.Duration {
	if w.cardUpdateTimeout <= 0 {
		return defaultFeishuCardUpdateTimeout
	}
	return w.cardUpdateTimeout
}

func (w *workflowContinuationWorker) retryDelay(attempt int) time.Duration {
	if len(w.retryDelays) == 0 {
		return defaultWorkflowResumeRetryDelays[len(defaultWorkflowResumeRetryDelays)-1]
	}
	index := attempt - 1
	if index < 0 {
		index = 0
	}
	if index >= len(w.retryDelays) {
		index = len(w.retryDelays) - 1
	}
	return w.retryDelays[index]
}

type workflowResumeResponder struct {
	sender       workflowResumeTextSender
	chatID       string
	requestID    string
	messageIndex int
}

func (r *workflowResumeResponder) Send(ctx context.Context, message core.OutboundMessage) error {
	if message.Text != "" {
		uuid := workflowResumeMessageUUID(r.requestID, r.messageIndex)
		if _, err := r.sender.CreateTextWithUUID(ctx, r.chatID, message.Text, uuid); err != nil {
			return err
		}
		r.messageIndex++
		return nil
	}
	if len(message.Image.Data) > 0 || message.Image.Filename != "" || message.Image.LocalPath != "" {
		return core.ErrUnsupportedImage
	}
	return nil
}

func (r *workflowResumeResponder) StartTyping(context.Context) func() {
	return func() {}
}

func workflowResumeMessageUUID(requestID string, messageIndex int) string {
	// This value is part of the durable delivery protocol. Keep the original
	// seed format so continuations created before an upgrade retain the same
	// Feishu idempotency key when they are replayed afterwards.
	digest := sha256.Sum256([]byte(strings.TrimSpace(requestID) + ":" + strconv.Itoa(messageIndex)))
	hexValue := hex.EncodeToString(digest[:16])
	return hexValue[0:8] + "-" + hexValue[8:12] + "-" + hexValue[12:16] + "-" + hexValue[16:20] + "-" + hexValue[20:32]
}

func newWorkflowResumeLeaseToken() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return "lease_" + hex.EncodeToString(value[:]), nil
}

var _ core.Sender = (*workflowResumeResponder)(nil)
