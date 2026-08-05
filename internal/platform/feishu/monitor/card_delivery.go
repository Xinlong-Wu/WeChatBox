package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"lingobridge/internal/store"
)

const (
	defaultFeishuCardDeliveryPollInterval = time.Second
	defaultFeishuCardDeliveryLease        = time.Minute
	defaultFeishuCardDeliveryBatchSize    = 20
)

var (
	defaultFeishuCardDeliveryRetryDelays = []time.Duration{
		time.Second,
		5 * time.Second,
		30 * time.Second,
		2 * time.Minute,
		10 * time.Minute,
	}
	errFeishuCardDeliveryObsolete      = errors.New("feishu card delivery is obsolete")
	errFeishuCardDeliveryResultPending = errors.New("feishu card delivery workflow result is pending")
)

type feishuCardDeliveryStore interface {
	ListAvailableFeishuCardDeliveries(accountID string, now time.Time, limit int) ([]store.FeishuCardDelivery, error)
	ClaimFeishuCardDelivery(id, accountID, leaseToken string, now time.Time, leaseDuration time.Duration) (store.FeishuCardDelivery, error)
	RetryFeishuCardDelivery(id, accountID, leaseToken string, availableAt time.Time, lastError string, now time.Time) error
	CompleteFeishuCardDelivery(id, accountID, leaseToken string, now time.Time) error
	DeadLetterFeishuCardDelivery(id, accountID, leaseToken, lastError string, now time.Time) error
	GetToolApproval(id, accountID string) (store.ToolApproval, error)
	GetFeishuResourceAccessRequest(id, accountID string) (store.FeishuResourceAccessRequest, error)
	GetWorkflowResult(requestID, accountID string) (store.WorkflowResult, error)
}

type feishuCardDeliveryWorker struct {
	store          feishuCardDeliveryStore
	cards          cardMessageUpdater
	resourceAccess *resourceAccessManager
	account        store.Account
	pollInterval   time.Duration
	lease          time.Duration
	batchSize      int
	retryDelays    []time.Duration
	updateTimeout  time.Duration
	now            func() time.Time
}

func newFeishuCardDeliveryWorker(
	st feishuCardDeliveryStore,
	cards cardMessageUpdater,
	resourceAccess *resourceAccessManager,
	account store.Account,
) (*feishuCardDeliveryWorker, error) {
	if st == nil {
		return nil, fmt.Errorf("feishu card delivery store is required")
	}
	if cards == nil {
		return nil, fmt.Errorf("feishu card delivery updater is required")
	}
	if strings.TrimSpace(account.ID) == "" {
		return nil, fmt.Errorf("feishu card delivery account is required")
	}
	return &feishuCardDeliveryWorker{
		store:          st,
		cards:          cards,
		resourceAccess: resourceAccess,
		account:        account,
		pollInterval:   defaultFeishuCardDeliveryPollInterval,
		lease:          defaultFeishuCardDeliveryLease,
		batchSize:      defaultFeishuCardDeliveryBatchSize,
		retryDelays:    append([]time.Duration(nil), defaultFeishuCardDeliveryRetryDelays...),
		updateTimeout:  defaultFeishuCardUpdateTimeout,
		now:            time.Now,
	}, nil
}

func (w *feishuCardDeliveryWorker) Run(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	feishuLog.Info(ctx, "started feishu card delivery worker account=%s", w.account.ID)
	w.processAvailable(ctx)
	ticker := time.NewTicker(w.effectivePollInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			feishuLog.Debug(ctx, "stopped feishu card delivery worker account=%s", w.account.ID)
			return
		case <-ticker.C:
			w.processAvailable(ctx)
		}
	}
}

func (w *feishuCardDeliveryWorker) processAvailable(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	deliveries, err := w.store.ListAvailableFeishuCardDeliveries(w.account.ID, w.currentTime(), w.effectiveBatchSize())
	if err != nil {
		feishuLog.Warn(ctx, "list available feishu card deliveries failed account=%s: %v", w.account.ID, err)
		return
	}
	for _, delivery := range deliveries {
		if ctx.Err() != nil {
			return
		}
		w.processOne(ctx, delivery)
	}
}

func (w *feishuCardDeliveryWorker) processOne(ctx context.Context, candidate store.FeishuCardDelivery) {
	leaseToken, err := newWorkflowResumeLeaseToken()
	if err != nil {
		feishuLog.Error(ctx, "generate feishu card delivery lease failed request=%s account=%s: %v",
			shortRequestID(candidate.RequestID), candidate.AccountID, err)
		return
	}
	now := w.currentTime()
	delivery, err := w.store.ClaimFeishuCardDelivery(candidate.ID, candidate.AccountID, leaseToken, now, w.effectiveLease())
	if err != nil {
		if errors.Is(err, store.ErrFeishuCardDeliveryNotReady) || errors.Is(err, store.ErrFeishuCardDeliveryResolved) {
			feishuLog.Debug(ctx, "skip unavailable feishu card delivery request=%s account=%s purpose=%s: %v",
				shortRequestID(candidate.RequestID), candidate.AccountID, candidate.Purpose, err)
			return
		}
		feishuLog.Warn(ctx, "claim feishu card delivery failed request=%s account=%s purpose=%s: %v",
			shortRequestID(candidate.RequestID), candidate.AccountID, candidate.Purpose, err)
		return
	}
	card, err := w.render(ctx, delivery)
	if err == nil {
		updateCtx, cancel := context.WithTimeout(ctx, w.effectiveUpdateTimeout())
		err = w.cards.UpdateByMessageID(updateCtx, delivery.CardMessageID, card)
		cancel()
	}
	if err != nil {
		w.handleFailure(ctx, delivery, leaseToken, err)
		return
	}
	completedAt := w.currentTime()
	if err := w.store.CompleteFeishuCardDelivery(delivery.ID, delivery.AccountID, leaseToken, completedAt); err != nil {
		if errors.Is(err, store.ErrFeishuCardDeliveryResolved) || errors.Is(err, store.ErrFeishuCardDeliveryLeaseLost) {
			feishuLog.Debug(ctx, "card delivery completion lost ownership request=%s account=%s purpose=%s: %v",
				shortRequestID(delivery.RequestID), delivery.AccountID, delivery.Purpose, err)
			return
		}
		feishuLog.Warn(ctx, "mark feishu card delivery complete failed request=%s account=%s purpose=%s: %v",
			shortRequestID(delivery.RequestID), delivery.AccountID, delivery.Purpose, err)
		return
	}
	feishuLog.Info(ctx, "delivered feishu card update request=%s account=%s purpose=%s revision=%d attempts=%d",
		shortRequestID(delivery.RequestID), delivery.AccountID, delivery.Purpose, delivery.Revision, delivery.Attempts)
}

func (w *feishuCardDeliveryWorker) handleFailure(ctx context.Context, delivery store.FeishuCardDelivery, leaseToken string, cause error) {
	now := w.currentTime()
	lastError := "feishu card delivery failed"
	if cause != nil {
		lastError = truncateApprovalRunes(strings.TrimSpace(cause.Error()), richTextLogPreviewRunes)
	}
	if errors.Is(cause, errFeishuCardDeliveryObsolete) {
		if err := w.store.DeadLetterFeishuCardDelivery(delivery.ID, delivery.AccountID, leaseToken, lastError, now); err != nil &&
			!errors.Is(err, store.ErrFeishuCardDeliveryResolved) && !errors.Is(err, store.ErrFeishuCardDeliveryLeaseLost) {
			feishuLog.Warn(ctx, "dead-letter obsolete feishu card delivery failed request=%s account=%s purpose=%s: %v",
				shortRequestID(delivery.RequestID), delivery.AccountID, delivery.Purpose, err)
		}
		return
	}
	delay := w.retryDelay(delivery.Attempts)
	availableAt := now.Add(delay)
	if !availableAt.Before(delivery.ExpiresAt) {
		if err := w.store.DeadLetterFeishuCardDelivery(delivery.ID, delivery.AccountID, leaseToken, lastError, now); err != nil &&
			!errors.Is(err, store.ErrFeishuCardDeliveryResolved) && !errors.Is(err, store.ErrFeishuCardDeliveryLeaseLost) {
			feishuLog.Warn(ctx, "dead-letter expired feishu card delivery failed request=%s account=%s purpose=%s: %v",
				shortRequestID(delivery.RequestID), delivery.AccountID, delivery.Purpose, err)
		}
		feishuLog.Warn(ctx, "feishu card delivery exhausted retry window request=%s account=%s purpose=%s attempts=%d error=%s",
			shortRequestID(delivery.RequestID), delivery.AccountID, delivery.Purpose, delivery.Attempts, lastError)
		return
	}
	if err := w.store.RetryFeishuCardDelivery(delivery.ID, delivery.AccountID, leaseToken, availableAt, lastError, now); err != nil {
		if errors.Is(err, store.ErrFeishuCardDeliveryResolved) || errors.Is(err, store.ErrFeishuCardDeliveryLeaseLost) {
			return
		}
		feishuLog.Warn(ctx, "schedule feishu card delivery retry failed request=%s account=%s purpose=%s: %v",
			shortRequestID(delivery.RequestID), delivery.AccountID, delivery.Purpose, err)
		return
	}
	if errors.Is(cause, errFeishuCardDeliveryResultPending) {
		feishuLog.Debug(ctx, "waiting for feishu workflow result before terminal card delivery request=%s account=%s purpose=%s revision=%d attempt=%d available_at=%s",
			shortRequestID(delivery.RequestID), delivery.AccountID, delivery.Purpose, delivery.Revision,
			delivery.Attempts, availableAt.Format(time.RFC3339))
		return
	}
	feishuLog.Warn(ctx, "scheduled feishu card delivery retry request=%s account=%s purpose=%s revision=%d attempt=%d available_at=%s error=%s",
		shortRequestID(delivery.RequestID), delivery.AccountID, delivery.Purpose, delivery.Revision,
		delivery.Attempts, availableAt.Format(time.RFC3339), lastError)
}

func (w *feishuCardDeliveryWorker) render(ctx context.Context, delivery store.FeishuCardDelivery) (Card, error) {
	switch delivery.Purpose {
	case store.FeishuCardDeliveryPurposeResourceOAuthHandoff:
		return w.renderResourceOAuthHandoff(delivery)
	case store.FeishuCardDeliveryPurposeToolApprovalTerminal:
		return w.renderToolApprovalTerminal(delivery)
	case store.FeishuCardDeliveryPurposeResourceTerminal:
		return w.renderResourceAccessTerminal(delivery)
	case store.FeishuCardDeliveryPurposeWorkflowUnavailable:
		return w.renderContinuationTerminal(ctx, delivery, statusCard{
			title:    "原会话已不可用",
			template: "orange",
			message:  "授权或操作结果已保存，但原会话已归档或删除，LingoBridge 无法自动继续原任务。请在当前对话中重新发送原请求；已经生效的授权不会因此丢失。",
		})
	case store.FeishuCardDeliveryPurposeWorkflowExhausted:
		return w.renderContinuationTerminal(ctx, delivery, statusCard{
			title:    "原任务未自动继续",
			template: "orange",
			message:  "授权或操作结果已保存，但 LingoBridge 未能自动继续原任务。请在当前对话中重新发送原请求；已经生效的授权不会因此丢失。",
		})
	default:
		return nil, fmt.Errorf("%w: unsupported purpose %q", errFeishuCardDeliveryObsolete, delivery.Purpose)
	}
}

func (w *feishuCardDeliveryWorker) renderResourceOAuthHandoff(delivery store.FeishuCardDelivery) (Card, error) {
	if w.resourceAccess == nil {
		return nil, fmt.Errorf("render resource OAuth handoff: resource access manager is unavailable")
	}
	request, err := w.store.GetFeishuResourceAccessRequest(delivery.RequestID, delivery.AccountID)
	if err != nil {
		return nil, fmt.Errorf("load resource OAuth handoff request: %w", err)
	}
	if request.State != store.FeishuResourceAccessStatePending || request.OAuthStateHash == "" || request.OAuthStateCiphertext == "" || !w.currentTime().Before(request.ExpiresAt) {
		return nil, fmt.Errorf("%w: resource OAuth handoff request state=%s", errFeishuCardDeliveryObsolete, request.State)
	}
	state, err := w.resourceAccess.decryptResourceAccessOAuthState(request)
	if err != nil {
		return nil, fmt.Errorf("decrypt resource OAuth handoff state: %w", err)
	}
	if hashResourceAccessState(state) != request.OAuthStateHash {
		return nil, fmt.Errorf("decrypt resource OAuth handoff state: authenticated state hash mismatch")
	}
	authURL, err := w.resourceAccess.authorizationURL(state)
	if err != nil {
		return nil, err
	}
	return pendingResourceAccessCard{request: request, authURL: authURL}, nil
}

func (w *feishuCardDeliveryWorker) renderToolApprovalTerminal(delivery store.FeishuCardDelivery) (Card, error) {
	approval, err := w.store.GetToolApproval(delivery.RequestID, delivery.AccountID)
	if err != nil {
		return nil, fmt.Errorf("load tool approval for card delivery: %w", err)
	}
	result, err := w.store.GetWorkflowResult(delivery.RequestID, delivery.AccountID)
	if errors.Is(err, store.ErrWorkflowResultNotFound) {
		return nil, fmt.Errorf("%w: request=%s", errFeishuCardDeliveryResultPending, shortRequestID(delivery.RequestID))
	}
	if err != nil {
		return nil, fmt.Errorf("load tool approval workflow result: %w", err)
	}
	resultState, _, defaultMessage, warning, _, err := recoveredApprovalResult(approval.State)
	if err != nil {
		return nil, fmt.Errorf("%w: tool approval state=%s", errFeishuCardDeliveryObsolete, approval.State)
	}
	if result.State != resultState {
		return nil, fmt.Errorf("%w: tool approval state=%s result_state=%s", errFeishuCardDeliveryObsolete, approval.State, result.State)
	}
	message := defaultMessage
	var payload struct {
		Message string `json:"message"`
		Warning bool   `json:"warning"`
	}
	if json.Unmarshal(result.Payload, &payload) == nil {
		if strings.TrimSpace(payload.Message) != "" {
			message = strings.TrimSpace(payload.Message)
		}
		warning = payload.Warning
	}
	switch resultState {
	case store.WorkflowResultStateDenied:
		return statusCard{title: "已拒绝授权", template: "grey", message: message}, nil
	case store.WorkflowResultStateExpired:
		return statusCard{title: "授权已过期", template: "grey", message: message}, nil
	case store.WorkflowResultStateFailed:
		return statusCard{title: "执行失败", template: "red", message: message}, nil
	case store.WorkflowResultStateSucceeded:
		if approval.State == store.ToolApprovalStatePartial || warning {
			return statusCard{title: "执行完成（有警告）", template: "orange", message: message}, nil
		}
		return statusCard{title: "执行完成", template: "green", message: message}, nil
	default:
		return nil, fmt.Errorf("%w: tool approval result state=%s", errFeishuCardDeliveryObsolete, resultState)
	}
}

func (w *feishuCardDeliveryWorker) renderResourceAccessTerminal(delivery store.FeishuCardDelivery) (Card, error) {
	request, err := w.store.GetFeishuResourceAccessRequest(delivery.RequestID, delivery.AccountID)
	if err != nil {
		return nil, fmt.Errorf("load resource access for card delivery: %w", err)
	}
	resultState, _, _, defaultMessage, err := recoveredResourceAccessResult(request, request.State)
	if err != nil {
		return nil, fmt.Errorf("%w: resource access state=%s", errFeishuCardDeliveryObsolete, request.State)
	}
	message := defaultMessage
	if result, loadErr := w.store.GetWorkflowResult(delivery.RequestID, delivery.AccountID); loadErr == nil {
		var payload struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(result.Payload, &payload) == nil && strings.TrimSpace(payload.Message) != "" {
			message = strings.TrimSpace(payload.Message)
		}
	} else if !errors.Is(loadErr, store.ErrWorkflowResultNotFound) {
		return nil, fmt.Errorf("load resource access workflow result: %w", loadErr)
	}
	switch resultState {
	case store.WorkflowResultStateDenied:
		return statusCard{title: "已拒绝授权", template: "grey", message: message}, nil
	case store.WorkflowResultStateExpired:
		return statusCard{title: "授权已过期", template: "grey", message: message}, nil
	case store.WorkflowResultStateFailed:
		return statusCard{title: "资源授权未完成", template: "red", message: message}, nil
	case store.WorkflowResultStateSucceeded:
		return statusCard{title: "权限已授予", template: "green", message: message}, nil
	default:
		return nil, fmt.Errorf("%w: resource access result state=%s", errFeishuCardDeliveryObsolete, resultState)
	}
}

func (w *feishuCardDeliveryWorker) renderContinuationTerminal(ctx context.Context, delivery store.FeishuCardDelivery, card statusCard) (Card, error) {
	result, err := w.store.GetWorkflowResult(delivery.RequestID, delivery.AccountID)
	if err != nil {
		if errors.Is(err, store.ErrWorkflowResultNotFound) {
			return card, nil
		}
		return nil, fmt.Errorf("load terminal continuation result: %w", err)
	}
	var payload struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(result.Payload, &payload); err != nil {
		feishuLog.Warn(ctx, "parse workflow result for durable terminal card failed request=%s account=%s: %v",
			shortRequestID(delivery.RequestID), delivery.AccountID, err)
		return card, nil
	}
	if detail := strings.TrimSpace(payload.Message); detail != "" {
		card.message = detail + "\n\n---\n\n" + strings.TrimSpace(card.message)
	}
	return card, nil
}

func (w *feishuCardDeliveryWorker) retryDelay(attempt int) time.Duration {
	if len(w.retryDelays) == 0 {
		return time.Hour
	}
	index := attempt - 1
	if index < 0 {
		index = 0
	}
	if index >= len(w.retryDelays) {
		return time.Hour
	}
	if w.retryDelays[index] <= 0 {
		return time.Second
	}
	return w.retryDelays[index]
}

func (w *feishuCardDeliveryWorker) currentTime() time.Time {
	if w.now == nil {
		return time.Now().UTC()
	}
	return w.now().UTC()
}

func (w *feishuCardDeliveryWorker) effectivePollInterval() time.Duration {
	if w.pollInterval <= 0 {
		return defaultFeishuCardDeliveryPollInterval
	}
	return w.pollInterval
}

func (w *feishuCardDeliveryWorker) effectiveLease() time.Duration {
	if w.lease <= 0 {
		return defaultFeishuCardDeliveryLease
	}
	return w.lease
}

func (w *feishuCardDeliveryWorker) effectiveBatchSize() int {
	if w.batchSize <= 0 || w.batchSize > 1000 {
		return defaultFeishuCardDeliveryBatchSize
	}
	return w.batchSize
}

func (w *feishuCardDeliveryWorker) effectiveUpdateTimeout() time.Duration {
	if w.updateTimeout <= 0 {
		return defaultFeishuCardUpdateTimeout
	}
	return w.updateTimeout
}
