package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrFeishuAccountRuntimeLeaseNotFound = errors.New("feishu account runtime lease not found")
	ErrFeishuAccountRuntimeLeaseHeld     = errors.New("feishu account runtime lease is held by another owner")
	ErrFeishuAccountRuntimeLeaseLost     = errors.New("feishu account runtime lease ownership was lost")
)

// FeishuAccountRuntimeLease is the durable single-active owner for one Feishu
// bot account. OwnerID is an opaque random runtime identifier.
type FeishuAccountRuntimeLease struct {
	AccountID      string
	OwnerID        string
	AcquiredAt     time.Time
	HeartbeatAt    time.Time
	LeaseExpiresAt time.Time
}

const feishuAccountRuntimeLeaseSelect = `SELECT
 account_id, owner_id, acquired_at_ms, heartbeat_at_ms, lease_expires_at_ms
 FROM feishu_account_runtime_leases`

// AcquireFeishuAccountRuntimeLease atomically acquires an absent or expired
// account lease. Re-acquiring with the same owner is idempotent and retains the
// original acquisition time.
func (s *Store) AcquireFeishuAccountRuntimeLease(accountID, ownerID string, now time.Time, ttl time.Duration) (FeishuAccountRuntimeLease, error) {
	if err := s.requireFeishuDocsStore(); err != nil {
		return FeishuAccountRuntimeLease{}, err
	}
	accountID = strings.TrimSpace(accountID)
	ownerID = strings.TrimSpace(ownerID)
	now = normalizedWorkflowTime(now)
	if accountID == "" || ownerID == "" || ttl <= 0 {
		return FeishuAccountRuntimeLease{}, fmt.Errorf("feishu account runtime lease account, owner, and positive ttl are required")
	}
	leaseExpiresAt := now.Add(ttl)
	for attempt := 0; attempt < 2; attempt++ {
		s.mu.Lock()
		lease, err := scanFeishuAccountRuntimeLease(s.db.QueryRow(
			`INSERT INTO feishu_account_runtime_leases (
			 account_id, owner_id, acquired_at_ms, heartbeat_at_ms, lease_expires_at_ms
			) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(account_id) DO UPDATE SET
			 owner_id=excluded.owner_id,
			 acquired_at_ms=CASE
			  WHEN feishu_account_runtime_leases.owner_id=excluded.owner_id
			  THEN feishu_account_runtime_leases.acquired_at_ms
			  ELSE excluded.acquired_at_ms
			 END,
			 heartbeat_at_ms=excluded.heartbeat_at_ms,
			 lease_expires_at_ms=excluded.lease_expires_at_ms
			 WHERE feishu_account_runtime_leases.owner_id=excluded.owner_id
			   OR feishu_account_runtime_leases.lease_expires_at_ms<=excluded.heartbeat_at_ms
			 RETURNING account_id, owner_id, acquired_at_ms, heartbeat_at_ms, lease_expires_at_ms`,
			accountID,
			ownerID,
			now.UnixMilli(),
			now.UnixMilli(),
			leaseExpiresAt.UnixMilli(),
		))
		s.mu.Unlock()
		if err == nil {
			return lease, nil
		}
		if !errors.Is(err, ErrFeishuAccountRuntimeLeaseNotFound) {
			return FeishuAccountRuntimeLease{}, fmt.Errorf("acquire feishu account runtime lease: %w", err)
		}
		lease, loadErr := s.GetFeishuAccountRuntimeLease(accountID)
		if loadErr == nil {
			return lease, ErrFeishuAccountRuntimeLeaseHeld
		}
		if !errors.Is(loadErr, ErrFeishuAccountRuntimeLeaseNotFound) {
			return FeishuAccountRuntimeLease{}, loadErr
		}
	}
	return FeishuAccountRuntimeLease{}, ErrFeishuAccountRuntimeLeaseHeld
}

// RenewFeishuAccountRuntimeLease extends a live lease owned by ownerID. An
// expired lease cannot be renewed; the runtime must stop and acquire a new
// owner identity on its next start.
func (s *Store) RenewFeishuAccountRuntimeLease(accountID, ownerID string, now time.Time, ttl time.Duration) (FeishuAccountRuntimeLease, error) {
	if err := s.requireFeishuDocsStore(); err != nil {
		return FeishuAccountRuntimeLease{}, err
	}
	accountID = strings.TrimSpace(accountID)
	ownerID = strings.TrimSpace(ownerID)
	now = normalizedWorkflowTime(now)
	if accountID == "" || ownerID == "" || ttl <= 0 {
		return FeishuAccountRuntimeLease{}, fmt.Errorf("feishu account runtime lease account, owner, and positive ttl are required")
	}
	s.mu.Lock()
	result, err := s.db.Exec(
		`UPDATE feishu_account_runtime_leases
		 SET heartbeat_at_ms=?, lease_expires_at_ms=?
		 WHERE account_id=? AND owner_id=? AND lease_expires_at_ms>?`,
		now.UnixMilli(),
		now.Add(ttl).UnixMilli(),
		accountID,
		ownerID,
		now.UnixMilli(),
	)
	s.mu.Unlock()
	if err != nil {
		return FeishuAccountRuntimeLease{}, fmt.Errorf("renew feishu account runtime lease: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return FeishuAccountRuntimeLease{}, fmt.Errorf("inspect feishu account runtime lease renewal: %w", err)
	}
	if count != 1 {
		return FeishuAccountRuntimeLease{}, ErrFeishuAccountRuntimeLeaseLost
	}
	lease, err := s.GetFeishuAccountRuntimeLease(accountID)
	if err != nil {
		return FeishuAccountRuntimeLease{}, err
	}
	if lease.AccountID != accountID || lease.OwnerID != ownerID || !lease.LeaseExpiresAt.After(now) {
		return lease, ErrFeishuAccountRuntimeLeaseLost
	}
	return lease, nil
}

// ReleaseFeishuAccountRuntimeLease removes only the lease still owned by
// ownerID. A stale owner cannot release a lease acquired by a replacement.
func (s *Store) ReleaseFeishuAccountRuntimeLease(accountID, ownerID string) error {
	if err := s.requireFeishuDocsStore(); err != nil {
		return err
	}
	accountID = strings.TrimSpace(accountID)
	ownerID = strings.TrimSpace(ownerID)
	if accountID == "" || ownerID == "" {
		return fmt.Errorf("feishu account runtime lease account and owner are required")
	}
	s.mu.Lock()
	result, err := s.db.Exec(
		`DELETE FROM feishu_account_runtime_leases WHERE account_id=? AND owner_id=?`,
		accountID,
		ownerID,
	)
	s.mu.Unlock()
	if err != nil {
		return fmt.Errorf("release feishu account runtime lease: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect feishu account runtime lease release: %w", err)
	}
	if count != 1 {
		return ErrFeishuAccountRuntimeLeaseLost
	}
	return nil
}

// GetFeishuAccountRuntimeLease returns the current durable owner for accountID.
func (s *Store) GetFeishuAccountRuntimeLease(accountID string) (FeishuAccountRuntimeLease, error) {
	if err := s.requireFeishuDocsStore(); err != nil {
		return FeishuAccountRuntimeLease{}, err
	}
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return FeishuAccountRuntimeLease{}, fmt.Errorf("feishu account runtime lease account is required")
	}
	return scanFeishuAccountRuntimeLease(s.db.QueryRow(
		feishuAccountRuntimeLeaseSelect+` WHERE account_id=?`,
		accountID,
	))
}

type feishuAccountRuntimeLeaseScanner interface {
	Scan(dest ...any) error
}

func scanFeishuAccountRuntimeLease(scanner feishuAccountRuntimeLeaseScanner) (FeishuAccountRuntimeLease, error) {
	var lease FeishuAccountRuntimeLease
	var acquiredAtMS, heartbeatAtMS, leaseExpiresAtMS int64
	if err := scanner.Scan(
		&lease.AccountID,
		&lease.OwnerID,
		&acquiredAtMS,
		&heartbeatAtMS,
		&leaseExpiresAtMS,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return FeishuAccountRuntimeLease{}, ErrFeishuAccountRuntimeLeaseNotFound
		}
		return FeishuAccountRuntimeLease{}, fmt.Errorf("scan feishu account runtime lease: %w", err)
	}
	lease.AcquiredAt = time.UnixMilli(acquiredAtMS).UTC()
	lease.HeartbeatAt = time.UnixMilli(heartbeatAtMS).UTC()
	lease.LeaseExpiresAt = time.UnixMilli(leaseExpiresAtMS).UTC()
	return lease, nil
}
