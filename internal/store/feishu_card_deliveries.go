package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrFeishuCardDeliveryNotFound  = errors.New("feishu card delivery not found")
	ErrFeishuCardDeliveryNotReady  = errors.New("feishu card delivery is not ready")
	ErrFeishuCardDeliveryResolved  = errors.New("feishu card delivery is already resolved")
	ErrFeishuCardDeliveryLeaseLost = errors.New("feishu card delivery lease was lost")
	ErrFeishuCardDeliveryConflict  = errors.New("feishu card delivery conflicts with an existing revision")
)

const (
	FeishuCardDeliveryStatePending    = "pending"
	FeishuCardDeliveryStateProcessing = "processing"
	FeishuCardDeliveryStateDelivered  = "delivered"
	FeishuCardDeliveryStateDead       = "dead"
	FeishuCardDeliveryStateSuperseded = "superseded"

	FeishuCardDeliveryPurposeResourceOAuthHandoff = "resource_oauth_handoff"
	FeishuCardDeliveryPurposeToolApprovalTerminal = "tool_approval_terminal"
	FeishuCardDeliveryPurposeResourceTerminal     = "resource_access_terminal"
	FeishuCardDeliveryPurposeWorkflowUnavailable  = "workflow_unavailable_session"
	FeishuCardDeliveryPurposeWorkflowExhausted    = "workflow_resume_exhausted"

	FeishuCardDeliveryRevisionOAuthHandoff = int64(10)
	FeishuCardDeliveryRevisionTerminal     = int64(20)
	FeishuCardDeliveryRevisionContinuation = int64(30)
)

const feishuCardDeliverySelect = `SELECT
 id, account_id, request_id, purpose, revision, card_message_id, state,
 attempts, available_at_ms, lease_token, lease_expires_at_ms, last_error,
 expires_at_ms, created_at_ms, updated_at_ms, delivered_at_ms
 FROM feishu_card_deliveries`

const feishuTerminalCardDeliveryTTL = 24 * time.Hour

// FeishuCardDelivery is one durable request to rebuild and update an existing
// Feishu card. It stores only routing and retry metadata, never rendered card
// JSON, OAuth codes, callback tokens, or access credentials.
type FeishuCardDelivery struct {
	ID             string
	AccountID      string
	RequestID      string
	Purpose        string
	Revision       int64
	CardMessageID  string
	State          string
	Attempts       int
	AvailableAt    time.Time
	LeaseToken     string
	LeaseExpiresAt time.Time
	LastError      string
	ExpiresAt      time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
	DeliveredAt    time.Time
}

// EnqueueFeishuCardDelivery inserts an idempotent delivery revision and
// supersedes every older revision for the same workflow request.
func (s *Store) EnqueueFeishuCardDelivery(delivery FeishuCardDelivery) (FeishuCardDelivery, error) {
	if err := s.requireFeishuDocsStore(); err != nil {
		return FeishuCardDelivery{}, err
	}
	delivery, err := prepareFeishuCardDelivery(delivery)
	if err != nil {
		return FeishuCardDelivery{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return FeishuCardDelivery{}, fmt.Errorf("begin enqueue feishu card delivery: %w", err)
	}
	defer tx.Rollback()
	delivery, err = enqueueFeishuCardDelivery(tx, delivery)
	if err != nil {
		return FeishuCardDelivery{}, err
	}
	if err := tx.Commit(); err != nil {
		return FeishuCardDelivery{}, fmt.Errorf("commit feishu card delivery: %w", err)
	}
	return delivery, nil
}

// ListAvailableFeishuCardDeliveries returns due pending work and processing
// work whose claim lease expired. Expired or superseded revisions are omitted.
func (s *Store) ListAvailableFeishuCardDeliveries(accountID string, now time.Time, limit int) ([]FeishuCardDelivery, error) {
	if err := s.requireFeishuDocsStore(); err != nil {
		return nil, err
	}
	accountID = strings.TrimSpace(accountID)
	now = normalizedWorkflowTime(now)
	if accountID == "" {
		return nil, fmt.Errorf("feishu card delivery account is required")
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.db.Query(
		feishuCardDeliverySelect+`
		 WHERE account_id=?
		 AND (
		  (expires_at_ms<=? AND state IN (?, ?))
		  OR
		  (expires_at_ms>? AND ((state=? AND available_at_ms<=?) OR (state=? AND lease_expires_at_ms<=?)))
		 )
		 AND NOT EXISTS (
		  SELECT 1 FROM feishu_card_deliveries newer
		  WHERE newer.account_id=feishu_card_deliveries.account_id
		    AND newer.request_id=feishu_card_deliveries.request_id
		    AND newer.revision>feishu_card_deliveries.revision
		 )
		 AND NOT EXISTS (
		  SELECT 1 FROM feishu_card_deliveries older
		  WHERE older.account_id=feishu_card_deliveries.account_id
		    AND older.request_id=feishu_card_deliveries.request_id
		    AND older.revision<feishu_card_deliveries.revision
		    AND older.state=? AND older.lease_expires_at_ms>?
		 )
		 ORDER BY available_at_ms, created_at_ms LIMIT ?`,
		accountID,
		now.UnixMilli(),
		FeishuCardDeliveryStatePending,
		FeishuCardDeliveryStateProcessing,
		now.UnixMilli(),
		FeishuCardDeliveryStatePending,
		now.UnixMilli(),
		FeishuCardDeliveryStateProcessing,
		now.UnixMilli(),
		FeishuCardDeliveryStateProcessing,
		now.UnixMilli(),
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list available feishu card deliveries: %w", err)
	}
	defer rows.Close()
	deliveries := make([]FeishuCardDelivery, 0)
	for rows.Next() {
		delivery, err := scanFeishuCardDelivery(rows)
		if err != nil {
			return nil, err
		}
		deliveries = append(deliveries, delivery)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate available feishu card deliveries: %w", err)
	}
	return deliveries, nil
}

// ClaimFeishuCardDelivery owns one due delivery for a bounded interval.
func (s *Store) ClaimFeishuCardDelivery(id, accountID, leaseToken string, now time.Time, leaseDuration time.Duration) (FeishuCardDelivery, error) {
	if err := s.requireFeishuDocsStore(); err != nil {
		return FeishuCardDelivery{}, err
	}
	id = strings.TrimSpace(id)
	accountID = strings.TrimSpace(accountID)
	leaseToken = strings.TrimSpace(leaseToken)
	now = normalizedWorkflowTime(now)
	if id == "" || accountID == "" || leaseToken == "" || leaseDuration <= 0 {
		return FeishuCardDelivery{}, fmt.Errorf("feishu card delivery id, account, lease token, and positive lease duration are required")
	}
	leaseExpiresAt := now.Add(leaseDuration)
	s.mu.Lock()
	result, err := s.db.Exec(
		`UPDATE feishu_card_deliveries
		 SET state=?, attempts=attempts+1, lease_token=?, lease_expires_at_ms=?, last_error='', updated_at_ms=?
		 WHERE id=? AND account_id=? AND expires_at_ms>?
		 AND ((state=? AND available_at_ms<=?) OR (state=? AND lease_expires_at_ms<=?))
		 AND NOT EXISTS (
		  SELECT 1 FROM feishu_card_deliveries newer
		  WHERE newer.account_id=feishu_card_deliveries.account_id
		    AND newer.request_id=feishu_card_deliveries.request_id
		    AND newer.revision>feishu_card_deliveries.revision
		 )
		 AND NOT EXISTS (
		  SELECT 1 FROM feishu_card_deliveries older
		  WHERE older.account_id=feishu_card_deliveries.account_id
		    AND older.request_id=feishu_card_deliveries.request_id
		    AND older.revision<feishu_card_deliveries.revision
		    AND older.state=? AND older.lease_expires_at_ms>?
		 )`,
		FeishuCardDeliveryStateProcessing,
		leaseToken,
		leaseExpiresAt.UnixMilli(),
		now.UnixMilli(),
		id,
		accountID,
		now.UnixMilli(),
		FeishuCardDeliveryStatePending,
		now.UnixMilli(),
		FeishuCardDeliveryStateProcessing,
		now.UnixMilli(),
		FeishuCardDeliveryStateProcessing,
		now.UnixMilli(),
	)
	s.mu.Unlock()
	if err != nil {
		return FeishuCardDelivery{}, fmt.Errorf("claim feishu card delivery: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return FeishuCardDelivery{}, fmt.Errorf("inspect feishu card delivery claim: %w", err)
	}
	if count == 1 {
		delivery, loadErr := s.GetFeishuCardDelivery(id, accountID)
		if loadErr != nil {
			return FeishuCardDelivery{}, loadErr
		}
		if delivery.State != FeishuCardDeliveryStateProcessing || delivery.LeaseToken != leaseToken {
			return delivery, ErrFeishuCardDeliveryResolved
		}
		return delivery, nil
	}
	delivery, err := s.GetFeishuCardDelivery(id, accountID)
	if err != nil {
		return FeishuCardDelivery{}, err
	}
	if !delivery.ExpiresAt.After(now) {
		s.mu.Lock()
		result, updateErr := s.db.Exec(
			`UPDATE feishu_card_deliveries
			 SET state=?, lease_token='', lease_expires_at_ms=0, updated_at_ms=?
			 WHERE id=? AND account_id=?
			 AND (state=? OR (state=? AND lease_expires_at_ms<=?))`,
			FeishuCardDeliveryStateDead,
			now.UnixMilli(),
			delivery.ID,
			delivery.AccountID,
			FeishuCardDeliveryStatePending,
			FeishuCardDeliveryStateProcessing,
			now.UnixMilli(),
		)
		s.mu.Unlock()
		if updateErr != nil {
			return FeishuCardDelivery{}, fmt.Errorf("expire feishu card delivery: %w", updateErr)
		}
		if updated, rowsErr := result.RowsAffected(); rowsErr != nil {
			return FeishuCardDelivery{}, fmt.Errorf("inspect expired feishu card delivery: %w", rowsErr)
		} else if updated == 1 {
			return delivery, ErrFeishuCardDeliveryResolved
		}
		current, loadErr := s.GetFeishuCardDelivery(id, accountID)
		if loadErr != nil {
			return FeishuCardDelivery{}, loadErr
		}
		if feishuCardDeliveryTerminal(current.State) {
			return current, ErrFeishuCardDeliveryResolved
		}
		return current, ErrFeishuCardDeliveryNotReady
	}
	var newer int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM feishu_card_deliveries
		 WHERE account_id=? AND request_id=? AND revision>?`,
		delivery.AccountID, delivery.RequestID, delivery.Revision,
	).Scan(&newer); err != nil {
		return FeishuCardDelivery{}, fmt.Errorf("inspect newer feishu card delivery revision: %w", err)
	}
	if newer > 0 {
		s.mu.Lock()
		result, updateErr := s.db.Exec(
			`UPDATE feishu_card_deliveries
			 SET state=?, lease_token='', lease_expires_at_ms=0, updated_at_ms=?
			 WHERE id=? AND account_id=?
			 AND (state=? OR (state=? AND lease_expires_at_ms<=?))
			 AND EXISTS (
			  SELECT 1 FROM feishu_card_deliveries newer
			  WHERE newer.account_id=feishu_card_deliveries.account_id
			    AND newer.request_id=feishu_card_deliveries.request_id
			    AND newer.revision>feishu_card_deliveries.revision
			 )`,
			FeishuCardDeliveryStateSuperseded,
			now.UnixMilli(),
			delivery.ID,
			delivery.AccountID,
			FeishuCardDeliveryStatePending,
			FeishuCardDeliveryStateProcessing,
			now.UnixMilli(),
		)
		s.mu.Unlock()
		if updateErr != nil {
			return FeishuCardDelivery{}, fmt.Errorf("supersede stale feishu card delivery: %w", updateErr)
		}
		if updated, rowsErr := result.RowsAffected(); rowsErr != nil {
			return FeishuCardDelivery{}, fmt.Errorf("inspect stale feishu card delivery supersede: %w", rowsErr)
		} else if updated == 1 {
			return delivery, ErrFeishuCardDeliveryResolved
		}
		current, loadErr := s.GetFeishuCardDelivery(id, accountID)
		if loadErr != nil {
			return FeishuCardDelivery{}, loadErr
		}
		if feishuCardDeliveryTerminal(current.State) {
			return current, ErrFeishuCardDeliveryResolved
		}
		return current, ErrFeishuCardDeliveryNotReady
	}
	if feishuCardDeliveryTerminal(delivery.State) {
		return delivery, ErrFeishuCardDeliveryResolved
	}
	return delivery, ErrFeishuCardDeliveryNotReady
}

// RetryFeishuCardDelivery releases a claimed delivery for a later attempt.
func (s *Store) RetryFeishuCardDelivery(id, accountID, leaseToken string, availableAt time.Time, lastError string, now time.Time) error {
	availableAt = normalizedWorkflowTime(availableAt)
	return s.updateClaimedFeishuCardDelivery(
		id, accountID, leaseToken, FeishuCardDeliveryStatePending,
		availableAt, lastError, now, time.Time{},
	)
}

// CompleteFeishuCardDelivery marks one worker-owned delivery successful.
func (s *Store) CompleteFeishuCardDelivery(id, accountID, leaseToken string, now time.Time) error {
	if err := s.requireFeishuDocsStore(); err != nil {
		return err
	}
	id = strings.TrimSpace(id)
	accountID = strings.TrimSpace(accountID)
	leaseToken = strings.TrimSpace(leaseToken)
	now = normalizedWorkflowTime(now)
	if id == "" || accountID == "" || leaseToken == "" {
		return fmt.Errorf("feishu card delivery id, account, and lease token are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin complete feishu card delivery: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.Exec(
		`UPDATE feishu_card_deliveries
		 SET state=?, available_at_ms=?, lease_token='', lease_expires_at_ms=0,
		 last_error='', delivered_at_ms=?, updated_at_ms=?
		 WHERE id=? AND account_id=? AND state=? AND lease_token=?
		 AND NOT EXISTS (
		  SELECT 1 FROM feishu_card_deliveries newer
		  WHERE newer.account_id=feishu_card_deliveries.account_id
		    AND newer.request_id=feishu_card_deliveries.request_id
		    AND newer.revision>feishu_card_deliveries.revision
		 )`,
		FeishuCardDeliveryStateDelivered,
		now.UnixMilli(),
		now.UnixMilli(),
		now.UnixMilli(),
		id,
		accountID,
		FeishuCardDeliveryStateProcessing,
		leaseToken,
	)
	if err != nil {
		return fmt.Errorf("complete feishu card delivery: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect feishu card delivery completion: %w", err)
	}
	delivery, err := feishuCardDeliveryByID(tx, id, accountID)
	if err != nil {
		return err
	}
	if count != 1 {
		superseded, updateErr := tx.Exec(
			`UPDATE feishu_card_deliveries
			 SET state=?, lease_token='', lease_expires_at_ms=0, updated_at_ms=?
			 WHERE id=? AND account_id=? AND state=? AND lease_token=?
			 AND EXISTS (
			  SELECT 1 FROM feishu_card_deliveries newer
			  WHERE newer.account_id=feishu_card_deliveries.account_id
			    AND newer.request_id=feishu_card_deliveries.request_id
			    AND newer.revision>feishu_card_deliveries.revision
			 )`,
			FeishuCardDeliveryStateSuperseded,
			now.UnixMilli(),
			id,
			accountID,
			FeishuCardDeliveryStateProcessing,
			leaseToken,
		)
		if updateErr != nil {
			return fmt.Errorf("supersede completed stale feishu card delivery: %w", updateErr)
		}
		if supersededCount, countErr := superseded.RowsAffected(); countErr != nil {
			return fmt.Errorf("inspect stale feishu card delivery supersede: %w", countErr)
		} else if supersededCount == 1 {
			if err := tx.Commit(); err != nil {
				return fmt.Errorf("commit superseded feishu card delivery: %w", err)
			}
			return ErrFeishuCardDeliveryResolved
		}
		if delivery.State != FeishuCardDeliveryStateProcessing {
			return ErrFeishuCardDeliveryResolved
		}
		return ErrFeishuCardDeliveryLeaseLost
	}
	if delivery.Purpose == FeishuCardDeliveryPurposeResourceOAuthHandoff {
		request, err := feishuResourceAccessByID(tx, delivery.RequestID, delivery.AccountID)
		if err != nil {
			return err
		}
		if request.State != FeishuResourceAccessStatePending || request.OAuthStateHash == "" {
			return ErrFeishuCardDeliveryResolved
		}
		result, err := tx.Exec(
			`UPDATE feishu_resource_access_requests
			 SET oauth_state_ciphertext='',
			 oauth_handoff_delivered_at_ms=CASE WHEN oauth_handoff_delivered_at_ms=0 THEN ? ELSE oauth_handoff_delivered_at_ms END,
			 updated_at_ms=?
			 WHERE id=? AND account_id=? AND state=? AND oauth_state_hash=?`,
			now.UnixMilli(),
			now.UnixMilli(),
			request.ID,
			request.AccountID,
			FeishuResourceAccessStatePending,
			request.OAuthStateHash,
		)
		if err != nil {
			return fmt.Errorf("complete durable feishu resource oauth handoff: %w", err)
		}
		if err := requireOneFeishuResourceAccessRow(result); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit completed feishu card delivery: %w", err)
	}
	return nil
}

// DeadLetterFeishuCardDelivery stops retrying one worker-owned delivery.
func (s *Store) DeadLetterFeishuCardDelivery(id, accountID, leaseToken, lastError string, now time.Time) error {
	now = normalizedWorkflowTime(now)
	return s.updateClaimedFeishuCardDelivery(
		id, accountID, leaseToken, FeishuCardDeliveryStateDead,
		now, lastError, now, time.Time{},
	)
}

// MarkFeishuCardDeliveryDelivered marks a still-pending revision delivered by
// a synchronous callback-token or message-ID fast path. A worker claim wins a
// race with this method and remains responsible for completion.
func (s *Store) MarkFeishuCardDeliveryDelivered(accountID, requestID, purpose string, revision int64, now time.Time) error {
	if err := s.requireFeishuDocsStore(); err != nil {
		return err
	}
	accountID = strings.TrimSpace(accountID)
	requestID = strings.TrimSpace(requestID)
	purpose = strings.TrimSpace(purpose)
	now = normalizedWorkflowTime(now)
	if accountID == "" || requestID == "" || purpose == "" || revision <= 0 {
		return fmt.Errorf("feishu card delivery account, request, purpose, and revision are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.db.Exec(
		`UPDATE feishu_card_deliveries
		 SET state=?, delivered_at_ms=?, updated_at_ms=?
		 WHERE account_id=? AND request_id=? AND purpose=? AND revision=? AND state=?
		 AND NOT EXISTS (
		  SELECT 1 FROM feishu_card_deliveries older
		  WHERE older.account_id=feishu_card_deliveries.account_id
		    AND older.request_id=feishu_card_deliveries.request_id
		    AND older.revision<feishu_card_deliveries.revision
		    AND older.state=? AND older.lease_expires_at_ms>?
		 )`,
		FeishuCardDeliveryStateDelivered,
		now.UnixMilli(),
		now.UnixMilli(),
		accountID,
		requestID,
		purpose,
		revision,
		FeishuCardDeliveryStatePending,
		FeishuCardDeliveryStateProcessing,
		now.UnixMilli(),
	)
	if err != nil {
		return fmt.Errorf("mark feishu card delivery delivered: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect feishu card delivery fast completion: %w", err)
	}
	if count == 1 {
		return nil
	}
	delivery, loadErr := s.GetFeishuCardDeliveryByKey(accountID, requestID, purpose, revision)
	if loadErr != nil {
		return loadErr
	}
	if delivery.State == FeishuCardDeliveryStateDelivered {
		return nil
	}
	if feishuCardDeliveryTerminal(delivery.State) {
		return ErrFeishuCardDeliveryResolved
	}
	return ErrFeishuCardDeliveryNotReady
}

func (s *Store) updateClaimedFeishuCardDelivery(
	id, accountID, leaseToken, state string,
	availableAt time.Time,
	lastError string,
	now time.Time,
	deliveredAt time.Time,
) error {
	if err := s.requireFeishuDocsStore(); err != nil {
		return err
	}
	id = strings.TrimSpace(id)
	accountID = strings.TrimSpace(accountID)
	leaseToken = strings.TrimSpace(leaseToken)
	lastError = truncateFeishuCardDeliveryError(lastError)
	now = normalizedWorkflowTime(now)
	availableAt = normalizedWorkflowTime(availableAt)
	if id == "" || accountID == "" || leaseToken == "" {
		return fmt.Errorf("feishu card delivery id, account, and lease token are required")
	}
	if state != FeishuCardDeliveryStatePending && state != FeishuCardDeliveryStateDelivered && state != FeishuCardDeliveryStateDead {
		return fmt.Errorf("unsupported feishu card delivery update state %q", state)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.db.Exec(
		`UPDATE feishu_card_deliveries
		 SET state=?, available_at_ms=?, lease_token='', lease_expires_at_ms=0,
		 last_error=?, delivered_at_ms=?, updated_at_ms=?
		 WHERE id=? AND account_id=? AND state=? AND lease_token=?
		 AND NOT EXISTS (
		  SELECT 1 FROM feishu_card_deliveries newer
		  WHERE newer.account_id=feishu_card_deliveries.account_id
		    AND newer.request_id=feishu_card_deliveries.request_id
		    AND newer.revision>feishu_card_deliveries.revision
		 )`,
		state,
		availableAt.UnixMilli(),
		lastError,
		optionalTimeMillis(deliveredAt),
		now.UnixMilli(),
		id,
		accountID,
		FeishuCardDeliveryStateProcessing,
		leaseToken,
	)
	if err != nil {
		return fmt.Errorf("update claimed feishu card delivery: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect claimed feishu card delivery update: %w", err)
	}
	if count == 1 {
		return nil
	}
	superseded, supersedeErr := s.db.Exec(
		`UPDATE feishu_card_deliveries
		 SET state=?, lease_token='', lease_expires_at_ms=0, updated_at_ms=?
		 WHERE id=? AND account_id=? AND state=? AND lease_token=?
		 AND EXISTS (
		  SELECT 1 FROM feishu_card_deliveries newer
		  WHERE newer.account_id=feishu_card_deliveries.account_id
		    AND newer.request_id=feishu_card_deliveries.request_id
		    AND newer.revision>feishu_card_deliveries.revision
		 )`,
		FeishuCardDeliveryStateSuperseded,
		now.UnixMilli(),
		id,
		accountID,
		FeishuCardDeliveryStateProcessing,
		leaseToken,
	)
	if supersedeErr != nil {
		return fmt.Errorf("supersede stale claimed feishu card delivery: %w", supersedeErr)
	}
	if supersededCount, countErr := superseded.RowsAffected(); countErr != nil {
		return fmt.Errorf("inspect stale claimed feishu card delivery supersede: %w", countErr)
	} else if supersededCount == 1 {
		return ErrFeishuCardDeliveryResolved
	}
	delivery, loadErr := s.GetFeishuCardDelivery(id, accountID)
	if loadErr != nil {
		return loadErr
	}
	if delivery.State != FeishuCardDeliveryStateProcessing {
		return ErrFeishuCardDeliveryResolved
	}
	return ErrFeishuCardDeliveryLeaseLost
}

func (s *Store) GetFeishuCardDelivery(id, accountID string) (FeishuCardDelivery, error) {
	if err := s.requireFeishuDocsStore(); err != nil {
		return FeishuCardDelivery{}, err
	}
	return feishuCardDeliveryByID(s.db, strings.TrimSpace(id), strings.TrimSpace(accountID))
}

func (s *Store) GetFeishuCardDeliveryByKey(accountID, requestID, purpose string, revision int64) (FeishuCardDelivery, error) {
	if err := s.requireFeishuDocsStore(); err != nil {
		return FeishuCardDelivery{}, err
	}
	return scanFeishuCardDelivery(s.db.QueryRow(
		feishuCardDeliverySelect+` WHERE account_id=? AND request_id=? AND purpose=? AND revision=?`,
		strings.TrimSpace(accountID), strings.TrimSpace(requestID), strings.TrimSpace(purpose), revision,
	))
}

func prepareFeishuCardDelivery(delivery FeishuCardDelivery) (FeishuCardDelivery, error) {
	delivery.ID = strings.TrimSpace(delivery.ID)
	delivery.AccountID = strings.TrimSpace(delivery.AccountID)
	delivery.RequestID = strings.TrimSpace(delivery.RequestID)
	delivery.Purpose = strings.TrimSpace(delivery.Purpose)
	delivery.CardMessageID = strings.TrimSpace(delivery.CardMessageID)
	delivery.CreatedAt = normalizedWorkflowTime(delivery.CreatedAt)
	delivery.UpdatedAt = delivery.CreatedAt
	if delivery.AvailableAt.IsZero() {
		delivery.AvailableAt = delivery.CreatedAt
	} else {
		delivery.AvailableAt = normalizedWorkflowTime(delivery.AvailableAt)
	}
	if delivery.ExpiresAt.IsZero() {
		return FeishuCardDelivery{}, fmt.Errorf("feishu card delivery expiry is required")
	}
	delivery.ExpiresAt = normalizedWorkflowTime(delivery.ExpiresAt)
	if delivery.ID == "" {
		value, err := generateID()
		if err != nil {
			return FeishuCardDelivery{}, fmt.Errorf("generate feishu card delivery id: %w", err)
		}
		delivery.ID = "card_" + value
	}
	if delivery.AccountID == "" || delivery.RequestID == "" || delivery.Purpose == "" || delivery.Revision <= 0 || delivery.CardMessageID == "" {
		return FeishuCardDelivery{}, fmt.Errorf("feishu card delivery account, request, purpose, positive revision, and card message are required")
	}
	if !delivery.ExpiresAt.After(delivery.CreatedAt) {
		return FeishuCardDelivery{}, fmt.Errorf("feishu card delivery expiry must be after creation")
	}
	delivery.State = FeishuCardDeliveryStatePending
	delivery.Attempts = 0
	delivery.LeaseToken = ""
	delivery.LeaseExpiresAt = time.Time{}
	delivery.LastError = ""
	delivery.DeliveredAt = time.Time{}
	return delivery, nil
}

func enqueueFeishuCardDelivery(tx *sql.Tx, delivery FeishuCardDelivery) (FeishuCardDelivery, error) {
	if tx == nil {
		return FeishuCardDelivery{}, fmt.Errorf("feishu card delivery transaction is required")
	}
	if _, err := tx.Exec(
		`UPDATE feishu_card_deliveries
		 SET state=?, lease_token='', lease_expires_at_ms=0, updated_at_ms=?
		 WHERE account_id=? AND request_id=? AND revision<?
		 AND (state=? OR (state=? AND lease_expires_at_ms<=?))`,
		FeishuCardDeliveryStateSuperseded,
		delivery.CreatedAt.UnixMilli(),
		delivery.AccountID,
		delivery.RequestID,
		delivery.Revision,
		FeishuCardDeliveryStatePending,
		FeishuCardDeliveryStateProcessing,
		delivery.CreatedAt.UnixMilli(),
	); err != nil {
		return FeishuCardDelivery{}, fmt.Errorf("supersede older feishu card deliveries: %w", err)
	}
	var newer int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM feishu_card_deliveries
		 WHERE account_id=? AND request_id=? AND revision>?`,
		delivery.AccountID, delivery.RequestID, delivery.Revision,
	).Scan(&newer); err != nil {
		return FeishuCardDelivery{}, fmt.Errorf("inspect newer feishu card delivery: %w", err)
	}
	if newer > 0 {
		delivery.State = FeishuCardDeliveryStateSuperseded
	}
	_, err := tx.Exec(
		`INSERT INTO feishu_card_deliveries (
		 id, account_id, request_id, purpose, revision, card_message_id, state,
		 attempts, available_at_ms, lease_token, lease_expires_at_ms, last_error,
		 expires_at_ms, created_at_ms, updated_at_ms, delivered_at_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?, '', 0, '', ?, ?, ?, 0)`,
		delivery.ID,
		delivery.AccountID,
		delivery.RequestID,
		delivery.Purpose,
		delivery.Revision,
		delivery.CardMessageID,
		delivery.State,
		delivery.AvailableAt.UnixMilli(),
		delivery.ExpiresAt.UnixMilli(),
		delivery.CreatedAt.UnixMilli(),
		delivery.UpdatedAt.UnixMilli(),
	)
	if err == nil {
		return delivery, nil
	}
	if !strings.Contains(strings.ToLower(err.Error()), "unique constraint failed") {
		return FeishuCardDelivery{}, fmt.Errorf("enqueue feishu card delivery: %w", err)
	}
	existing, loadErr := scanFeishuCardDelivery(tx.QueryRow(
		feishuCardDeliverySelect+` WHERE account_id=? AND request_id=? AND purpose=? AND revision=?`,
		delivery.AccountID, delivery.RequestID, delivery.Purpose, delivery.Revision,
	))
	if loadErr != nil {
		return FeishuCardDelivery{}, loadErr
	}
	if existing.CardMessageID != delivery.CardMessageID {
		return existing, ErrFeishuCardDeliveryConflict
	}
	return existing, nil
}

func enqueueToolApprovalTerminalCardDelivery(tx *sql.Tx, approval ToolApproval, now time.Time) error {
	if strings.TrimSpace(approval.CardMessageID) == "" {
		return nil
	}
	delivery, err := prepareFeishuCardDelivery(FeishuCardDelivery{
		AccountID:     approval.AccountID,
		RequestID:     approval.ID,
		Purpose:       FeishuCardDeliveryPurposeToolApprovalTerminal,
		Revision:      FeishuCardDeliveryRevisionTerminal,
		CardMessageID: approval.CardMessageID,
		CreatedAt:     now,
		ExpiresAt:     now.Add(feishuTerminalCardDeliveryTTL),
	})
	if err != nil {
		return err
	}
	_, err = enqueueFeishuCardDelivery(tx, delivery)
	return err
}

func enqueueResourceAccessCardDelivery(tx *sql.Tx, request FeishuResourceAccessRequest, purpose string, revision int64, expiresAt, now time.Time) error {
	if strings.TrimSpace(request.CardMessageID) == "" {
		return nil
	}
	delivery, err := prepareFeishuCardDelivery(FeishuCardDelivery{
		AccountID:     request.AccountID,
		RequestID:     request.ID,
		Purpose:       purpose,
		Revision:      revision,
		CardMessageID: request.CardMessageID,
		CreatedAt:     now,
		ExpiresAt:     expiresAt,
	})
	if err != nil {
		return err
	}
	_, err = enqueueFeishuCardDelivery(tx, delivery)
	return err
}

func enqueueResourceAccessTerminalCardDelivery(tx *sql.Tx, request FeishuResourceAccessRequest, now time.Time) error {
	return enqueueResourceAccessCardDelivery(
		tx,
		request,
		FeishuCardDeliveryPurposeResourceTerminal,
		FeishuCardDeliveryRevisionTerminal,
		now.Add(feishuTerminalCardDeliveryTTL),
		now,
	)
}

type feishuCardDeliveryQueryer interface {
	QueryRow(query string, args ...any) *sql.Row
}

func feishuCardDeliveryByID(queryer feishuCardDeliveryQueryer, id, accountID string) (FeishuCardDelivery, error) {
	return scanFeishuCardDelivery(queryer.QueryRow(
		feishuCardDeliverySelect+` WHERE id=? AND account_id=?`, id, accountID,
	))
}

type feishuCardDeliveryScanner interface {
	Scan(dest ...any) error
}

func scanFeishuCardDelivery(scanner feishuCardDeliveryScanner) (FeishuCardDelivery, error) {
	var delivery FeishuCardDelivery
	var availableAtMS, leaseExpiresAtMS, expiresAtMS, createdAtMS, updatedAtMS, deliveredAtMS int64
	if err := scanner.Scan(
		&delivery.ID,
		&delivery.AccountID,
		&delivery.RequestID,
		&delivery.Purpose,
		&delivery.Revision,
		&delivery.CardMessageID,
		&delivery.State,
		&delivery.Attempts,
		&availableAtMS,
		&delivery.LeaseToken,
		&leaseExpiresAtMS,
		&delivery.LastError,
		&expiresAtMS,
		&createdAtMS,
		&updatedAtMS,
		&deliveredAtMS,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return FeishuCardDelivery{}, ErrFeishuCardDeliveryNotFound
		}
		return FeishuCardDelivery{}, fmt.Errorf("scan feishu card delivery: %w", err)
	}
	delivery.AvailableAt = time.UnixMilli(availableAtMS).UTC()
	delivery.ExpiresAt = time.UnixMilli(expiresAtMS).UTC()
	delivery.CreatedAt = time.UnixMilli(createdAtMS).UTC()
	delivery.UpdatedAt = time.UnixMilli(updatedAtMS).UTC()
	if leaseExpiresAtMS > 0 {
		delivery.LeaseExpiresAt = time.UnixMilli(leaseExpiresAtMS).UTC()
	}
	if deliveredAtMS > 0 {
		delivery.DeliveredAt = time.UnixMilli(deliveredAtMS).UTC()
	}
	return delivery, nil
}

func feishuCardDeliveryTerminal(state string) bool {
	switch state {
	case FeishuCardDeliveryStateDelivered, FeishuCardDeliveryStateDead, FeishuCardDeliveryStateSuperseded:
		return true
	default:
		return false
	}
}

func truncateFeishuCardDeliveryError(value string) string {
	value = strings.TrimSpace(value)
	const maxRunes = 512
	runes := []rune(value)
	if len(runes) <= maxRunes {
		return value
	}
	return string(runes[:maxRunes])
}
