package monitor

import (
	"context"
	"fmt"
	"time"
)

const (
	defaultFeishuOAuthRefreshAttemptRetention        = 30 * 24 * time.Hour
	defaultFeishuOAuthRefreshAttemptCleanupInterval  = 24 * time.Hour
	defaultFeishuOAuthRefreshAttemptCleanupBatchSize = 500
)

type oauthRefreshRetentionService struct {
	store     oauthRefreshRetentionStore
	accountID string
	now       func() time.Time
	retention time.Duration
	batchSize int
}

func (s oauthRefreshRetentionService) Cleanup(ctx context.Context) (int64, error) {
	if s.store == nil || s.accountID == "" {
		return 0, fmt.Errorf("feishu OAuth refresh attempt cleanup requires a store and account")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	retention := s.retention
	if retention <= 0 {
		retention = defaultFeishuOAuthRefreshAttemptRetention
	}
	batchSize := s.batchSize
	if batchSize <= 0 {
		batchSize = defaultFeishuOAuthRefreshAttemptCleanupBatchSize
	}
	if batchSize > 1000 {
		batchSize = 1000
	}
	now := time.Now
	if s.now != nil {
		now = s.now
	}
	cutoff := now().UTC().Add(-retention)
	started := time.Now()
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		deleted, err := s.store.DeleteTerminalFeishuOAuthRefreshAttempts(s.accountID, cutoff, batchSize)
		if err != nil {
			return total, fmt.Errorf("delete terminal feishu OAuth refresh attempts: %w", err)
		}
		total += deleted
		if deleted < int64(batchSize) {
			break
		}
	}
	if err := ctx.Err(); err != nil {
		return total, err
	}
	unsafeCount, err := s.store.CountUnsafeTerminalFeishuOAuthRefreshAttempts(s.accountID, cutoff)
	if err != nil {
		return total, fmt.Errorf("inspect unsafe terminal feishu OAuth refresh attempts: %w", err)
	}
	if unsafeCount > 0 {
		feishuLog.Warn(ctx, "retained unsafe terminal feishu OAuth refresh attempts account=%s cutoff=%s count=%d",
			s.accountID, cutoff.Format(time.RFC3339), unsafeCount)
	}
	feishuLog.Debug(ctx, "cleaned terminal feishu OAuth refresh attempts account=%s cutoff=%s deleted=%d unsafe_retained=%d duration_ms=%d",
		s.accountID, cutoff.Format(time.RFC3339), total, unsafeCount, time.Since(started).Milliseconds())
	return total, nil
}

func (m *resourceAccessManager) cleanupTerminalFeishuOAuthRefreshAttempts(ctx context.Context) (int64, error) {
	if m == nil {
		return 0, fmt.Errorf("feishu OAuth refresh attempt cleanup requires a manager")
	}
	service := oauthRefreshRetentionService{
		store:     m.store,
		accountID: m.account.ID,
		now:       m.currentTime,
		retention: m.refreshAttemptRetention,
		batchSize: m.refreshAttemptCleanupBatchSize,
	}
	return service.Cleanup(ctx)
}

func (m *resourceAccessManager) startFeishuOAuthRefreshAttemptCleanup() error {
	if m == nil || m.runCtx == nil {
		return fmt.Errorf("feishu OAuth refresh attempt cleanup requires a runtime context")
	}
	interval := m.refreshAttemptCleanupInterval
	if interval <= 0 {
		interval = defaultFeishuOAuthRefreshAttemptCleanupInterval
	}
	accepted := m.tasks.Go(func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-m.runCtx.Done():
				return
			case <-ticker.C:
				if _, err := m.cleanupTerminalFeishuOAuthRefreshAttempts(m.runCtx); err != nil {
					feishuLog.Warn(m.runCtx, "periodic terminal feishu OAuth refresh attempt cleanup failed account=%s error_type=%T: %v",
						m.account.ID, err, err)
				}
			}
		}
	})
	if !accepted {
		return fmt.Errorf("feishu OAuth refresh attempt cleanup task group is closed")
	}
	feishuLog.Debug(m.runCtx, "started terminal feishu OAuth refresh attempt cleanup account=%s interval=%s retention=%s batch_size=%d",
		m.account.ID, interval, m.effectiveFeishuOAuthRefreshAttemptRetention(), m.effectiveFeishuOAuthRefreshAttemptCleanupBatchSize())
	return nil
}

func (m *resourceAccessManager) effectiveFeishuOAuthRefreshAttemptRetention() time.Duration {
	if m != nil && m.refreshAttemptRetention > 0 {
		return m.refreshAttemptRetention
	}
	return defaultFeishuOAuthRefreshAttemptRetention
}

func (m *resourceAccessManager) effectiveFeishuOAuthRefreshAttemptCleanupBatchSize() int {
	if m != nil && m.refreshAttemptCleanupBatchSize > 0 {
		if m.refreshAttemptCleanupBatchSize > 1000 {
			return 1000
		}
		return m.refreshAttemptCleanupBatchSize
	}
	return defaultFeishuOAuthRefreshAttemptCleanupBatchSize
}
