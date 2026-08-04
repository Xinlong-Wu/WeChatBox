package monitor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"lingobridge/internal/store"
)

const (
	defaultFeishuAccountRuntimeLeaseTTL       = 30 * time.Second
	defaultFeishuAccountRuntimeLeaseHeartbeat = 10 * time.Second
)

type feishuAccountRuntimeLeaseStore interface {
	AcquireFeishuAccountRuntimeLease(accountID, ownerID string, now time.Time, ttl time.Duration) (store.FeishuAccountRuntimeLease, error)
	RenewFeishuAccountRuntimeLease(accountID, ownerID string, now time.Time, ttl time.Duration) (store.FeishuAccountRuntimeLease, error)
	ReleaseFeishuAccountRuntimeLease(accountID, ownerID string) error
}

type feishuAccountRuntimeLeaseOptions struct {
	OwnerID           string
	TTL               time.Duration
	HeartbeatInterval time.Duration
	Now               func() time.Time
}

type feishuAccountRuntimeLease struct {
	store             feishuAccountRuntimeLeaseStore
	accountID         string
	ownerID           string
	ttl               time.Duration
	heartbeatInterval time.Duration
	now               func() time.Time
	lease             store.FeishuAccountRuntimeLease
}

type feishuAccountRuntimeHeartbeat struct {
	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

func acquireFeishuAccountRuntimeLease(st feishuAccountRuntimeLeaseStore, accountID string, opts feishuAccountRuntimeLeaseOptions) (*feishuAccountRuntimeLease, error) {
	if st == nil {
		return nil, fmt.Errorf("feishu account runtime lease store is required")
	}
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return nil, fmt.Errorf("feishu account runtime lease account is required")
	}
	ownerID := strings.TrimSpace(opts.OwnerID)
	if ownerID == "" {
		var err error
		ownerID, err = newFeishuAccountRuntimeOwnerID()
		if err != nil {
			return nil, fmt.Errorf("generate feishu account runtime owner: %w", err)
		}
	}
	ttl := opts.TTL
	if ttl <= 0 {
		ttl = defaultFeishuAccountRuntimeLeaseTTL
	}
	heartbeatInterval := opts.HeartbeatInterval
	if heartbeatInterval <= 0 {
		heartbeatInterval = defaultFeishuAccountRuntimeLeaseHeartbeat
	}
	if heartbeatInterval >= ttl {
		return nil, fmt.Errorf("feishu account runtime heartbeat must be shorter than lease ttl")
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	acquiredAt := now()
	lease, err := st.AcquireFeishuAccountRuntimeLease(accountID, ownerID, acquiredAt, ttl)
	if err != nil {
		return nil, err
	}
	checkedAt := now()
	if lease.AccountID != accountID || lease.OwnerID != ownerID || !lease.LeaseExpiresAt.After(checkedAt) {
		return nil, fmt.Errorf("%w: acquired feishu account runtime lease changed or expired before use", store.ErrFeishuAccountRuntimeLeaseLost)
	}
	runtimeLease := &feishuAccountRuntimeLease{
		store:             st,
		accountID:         accountID,
		ownerID:           ownerID,
		ttl:               ttl,
		heartbeatInterval: heartbeatInterval,
		now:               now,
		lease:             lease,
	}
	feishuLog.Info(context.Background(), "acquired feishu account runtime lease account=%s owner_ref=%s expires_at=%s",
		accountID, shortResourceRef(ownerID), lease.LeaseExpiresAt.Format(time.RFC3339))
	return runtimeLease, nil
}

func (l *feishuAccountRuntimeLease) startHeartbeat(cancelRuntime context.CancelFunc) *feishuAccountRuntimeHeartbeat {
	ctx, cancel := context.WithCancel(context.Background())
	heartbeat := &feishuAccountRuntimeHeartbeat{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(heartbeat.done)
		heartbeat.err = l.maintain(ctx)
		if heartbeat.err != nil && cancelRuntime != nil {
			cancelRuntime()
		}
	}()
	return heartbeat
}

func (l *feishuAccountRuntimeLease) maintain(ctx context.Context) error {
	if l == nil || l.store == nil {
		return fmt.Errorf("feishu account runtime lease is required")
	}
	timer := time.NewTimer(l.heartbeatInterval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
			attemptedAt := l.now()
			lease, err := l.store.RenewFeishuAccountRuntimeLease(l.accountID, l.ownerID, attemptedAt, l.ttl)
			if err != nil {
				if errors.Is(err, store.ErrFeishuAccountRuntimeLeaseLost) {
					return fmt.Errorf("renew feishu account runtime lease: %w", err)
				}
				remaining := l.lease.LeaseExpiresAt.Sub(attemptedAt)
				retryDelay := feishuAccountRuntimeLeaseRetryDelay(l.heartbeatInterval, remaining)
				if retryDelay <= 0 {
					return fmt.Errorf("renew feishu account runtime lease before known expiry: %w", err)
				}
				feishuLog.Warn(ctx, "retrying transient feishu account runtime lease renewal account=%s owner_ref=%s remaining_ms=%d retry_ms=%d error_type=%T",
					l.accountID, shortResourceRef(l.ownerID), remaining.Milliseconds(), retryDelay.Milliseconds(), err)
				timer.Reset(retryDelay)
				continue
			}
			l.lease = lease
			feishuLog.Debug(ctx, "renewed feishu account runtime lease account=%s owner_ref=%s expires_at=%s",
				l.accountID, shortResourceRef(l.ownerID), lease.LeaseExpiresAt.Format(time.RFC3339))
			timer.Reset(l.heartbeatInterval)
		}
	}
}

func feishuAccountRuntimeLeaseRetryDelay(heartbeatInterval, remaining time.Duration) time.Duration {
	if heartbeatInterval <= 0 || remaining <= 1 {
		return 0
	}
	retryDelay := heartbeatInterval / 2
	remainingHalf := remaining / 2
	if retryDelay <= 0 || remainingHalf < retryDelay {
		retryDelay = remainingHalf
	}
	return retryDelay
}

func (h *feishuAccountRuntimeHeartbeat) stopAndWait() error {
	if h == nil {
		return nil
	}
	h.cancel()
	<-h.done
	return h.err
}

func (l *feishuAccountRuntimeLease) release() error {
	if l == nil || l.store == nil {
		return nil
	}
	if err := l.store.ReleaseFeishuAccountRuntimeLease(l.accountID, l.ownerID); err != nil {
		return err
	}
	feishuLog.Info(context.Background(), "released feishu account runtime lease account=%s owner_ref=%s",
		l.accountID, shortResourceRef(l.ownerID))
	return nil
}

// shutdownFeishuAccountRuntime keeps the account lease alive until every
// already-admitted workflow task has drained. Only then does it stop the
// heartbeat and release ownership for a replacement runtime.
func shutdownFeishuAccountRuntime(
	cancelRuntime context.CancelFunc,
	continuationDone <-chan struct{},
	cardDeliveryDone <-chan struct{},
	messageBot *bot,
	approvals *operationApprovalService,
	resourceAccess *resourceAccessManager,
	heartbeat *feishuAccountRuntimeHeartbeat,
	runtimeLease *feishuAccountRuntimeLease,
) (heartbeatErr, releaseErr error) {
	// Callback admission must close before lifecycle cancellation becomes
	// observable. Otherwise shutdown can block on a worker while a fresh card or
	// OAuth callback consumes durable one-shot state behind the canceled runtime.
	if messageBot != nil {
		messageBot.tasks.CloseAdmission()
	}
	if approvals != nil {
		approvals.tasks.CloseAdmission()
	}
	if resourceAccess != nil {
		resourceAccess.tasks.CloseAdmission()
	}
	if cancelRuntime != nil {
		cancelRuntime()
	}
	if continuationDone != nil {
		<-continuationDone
	}
	if cardDeliveryDone != nil {
		<-cardDeliveryDone
	}
	if messageBot != nil {
		messageBot.tasks.Wait()
	}
	if approvals != nil {
		approvals.tasks.Wait()
	}
	if resourceAccess != nil {
		resourceAccess.tasks.Wait()
	}
	if heartbeat != nil {
		heartbeatErr = heartbeat.stopAndWait()
	}
	if runtimeLease != nil {
		releaseErr = runtimeLease.release()
	}
	return heartbeatErr, releaseErr
}

func newFeishuAccountRuntimeOwnerID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return "runtime_" + hex.EncodeToString(value[:]), nil
}
