package monitor

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
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
	defaultWorkflowResumeMaxAttempts  = 5
)

var defaultWorkflowResumeRetryDelays = []time.Duration{
	2 * time.Second,
	5 * time.Second,
	15 * time.Second,
	30 * time.Second,
}

type workflowResumeStore interface {
	ListResumableWorkflowContinuations(accountID string, now time.Time, limit int) ([]store.WorkflowContinuation, error)
	ClaimWorkflowContinuation(requestID, accountID, leaseToken string, now time.Time, leaseDuration time.Duration) (store.WorkflowContinuation, error)
	GetWorkflowResult(requestID, accountID string) (store.WorkflowResult, error)
	RetryWorkflowContinuation(requestID, accountID, leaseToken string, availableAt time.Time, lastError string, now time.Time) error
	CompleteWorkflowContinuation(requestID, accountID, leaseToken, state, lastError string, now time.Time) error
}

type workflowResumeTextSender interface {
	CreateTextWithUUID(ctx context.Context, chatID, text, uuid string) (string, error)
}

type workflowContinuationWorker struct {
	store         workflowResumeStore
	resumer       core.WorkflowResumer
	sender        workflowResumeTextSender
	account       store.Account
	tools         []tooltypes.Tool
	toolOptions   tooltypes.Options
	pollInterval  time.Duration
	lease         time.Duration
	resumeTimeout time.Duration
	batchSize     int
	maxAttempts   int
	retryDelays   []time.Duration
	now           func() time.Time
}

func newWorkflowContinuationWorker(st workflowResumeStore, resumer core.WorkflowResumer, sender workflowResumeTextSender, account store.Account, tools []tooltypes.Tool) (*workflowContinuationWorker, error) {
	if st == nil {
		return nil, fmt.Errorf("workflow continuation store is required")
	}
	if resumer == nil {
		return nil, fmt.Errorf("workflow resumer is required")
	}
	if sender == nil {
		return nil, fmt.Errorf("workflow resume sender is required")
	}
	if strings.TrimSpace(account.ID) == "" {
		return nil, fmt.Errorf("workflow resume account is required")
	}
	return &workflowContinuationWorker{
		store:         st,
		resumer:       resumer,
		sender:        sender,
		account:       account,
		tools:         append([]tooltypes.Tool(nil), tools...),
		pollInterval:  defaultWorkflowResumePollInterval,
		lease:         defaultWorkflowResumeLease,
		resumeTimeout: defaultWorkflowResumeTimeout,
		batchSize:     defaultWorkflowResumeBatchSize,
		maxAttempts:   defaultWorkflowResumeMaxAttempts,
		retryDelays:   append([]time.Duration(nil), defaultWorkflowResumeRetryDelays...),
		now:           time.Now,
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
	continuations, err := w.store.ListResumableWorkflowContinuations(w.account.ID, w.currentTime(), w.effectiveBatchSize())
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
		return
	}
	availableAt := now.Add(w.retryDelay(continuation.Attempts))
	if ctx.Err() != nil {
		availableAt = now
	}
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
