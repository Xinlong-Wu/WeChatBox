package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"lingobridge/internal/store"
)

func (b *Bot) commitPendingWorkflows(ctx context.Context, accountID string, requestIDs []string, committedRevision int64) error {
	requestIDs = normalizedPendingWorkflowIDs(requestIDs)
	if len(requestIDs) == 0 {
		return nil
	}
	if b.Workflows == nil {
		return fmt.Errorf("workflow continuation manager is unavailable")
	}
	now := time.Now().UTC()
	var commitErrors []error
	for _, requestID := range requestIDs {
		continuation, ready, err := b.Workflows.CommitWorkflowContinuation(requestID, accountID, committedRevision, now)
		if err != nil {
			commitErrors = append(commitErrors, fmt.Errorf("commit workflow %s: %w", requestID, err))
			if cancelErr := b.Workflows.CancelWorkflowContinuation(requestID, accountID, "origin workflow commit failed", now); cancelErr != nil && !errors.Is(cancelErr, store.ErrWorkflowContinuationResolved) {
				commitErrors = append(commitErrors, fmt.Errorf("cancel workflow %s after commit failure: %w", requestID, cancelErr))
			}
			continue
		}
		coreLog.Debug(ctx, "committed workflow continuation request=%s account=%s session=%s revision=%d ready=%t",
			requestID, accountID, continuation.SessionID, committedRevision, ready)
	}
	return errors.Join(commitErrors...)
}

func (b *Bot) cancelPendingWorkflows(ctx context.Context, accountID string, requestIDs []string, reason string) {
	requestIDs = normalizedPendingWorkflowIDs(requestIDs)
	if len(requestIDs) == 0 {
		return
	}
	if b.Workflows == nil {
		coreLog.Error(ctx, "cannot cancel pending workflow continuations account=%s count=%d: manager unavailable", accountID, len(requestIDs))
		return
	}
	now := time.Now().UTC()
	for _, requestID := range requestIDs {
		if err := b.Workflows.CancelWorkflowContinuation(requestID, accountID, reason, now); err != nil {
			if errors.Is(err, store.ErrWorkflowContinuationResolved) {
				coreLog.Debug(ctx, "pending workflow continuation already resolved request=%s account=%s", requestID, accountID)
				continue
			}
			coreLog.Warn(ctx, "cancel pending workflow continuation failed request=%s account=%s: %v", requestID, accountID, err)
			continue
		}
		coreLog.Debug(ctx, "canceled pending workflow continuation request=%s account=%s reason=%s", requestID, accountID, reason)
	}
}

func normalizedPendingWorkflowIDs(requestIDs []string) []string {
	if len(requestIDs) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(requestIDs))
	normalized := make([]string, 0, len(requestIDs))
	for _, requestID := range requestIDs {
		requestID = strings.TrimSpace(requestID)
		if requestID == "" {
			continue
		}
		if _, exists := seen[requestID]; exists {
			continue
		}
		seen[requestID] = struct{}{}
		normalized = append(normalized, requestID)
	}
	return normalized
}
