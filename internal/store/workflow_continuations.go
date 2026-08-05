package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	ErrWorkflowResultNotFound        = errors.New("workflow result not found")
	ErrWorkflowResultConflict        = errors.New("workflow result conflicts with existing terminal result")
	ErrWorkflowContinuationNotFound  = errors.New("workflow continuation not found")
	ErrWorkflowContinuationNotReady  = errors.New("workflow continuation is not ready")
	ErrWorkflowContinuationResolved  = errors.New("workflow continuation is already resolved")
	ErrWorkflowContinuationLeaseLost = errors.New("workflow continuation lease does not match")
)

const (
	WorkflowResultStateSucceeded = "succeeded"
	WorkflowResultStateDenied    = "denied"
	WorkflowResultStateExpired   = "expired"
	WorkflowResultStateFailed    = "failed"

	WorkflowContinuationStateWaiting    = "waiting"
	WorkflowContinuationStateReady      = "ready"
	WorkflowContinuationStateProcessing = "processing"
	WorkflowContinuationStateDelivered  = "delivered"
	WorkflowContinuationStateCanceled   = "canceled"
	WorkflowContinuationStateFailed     = "failed"

	maxWorkflowContinuationErrorRunes = 2000
)

// WorkflowResult is the single durable terminal result for one workflow.
// Payload must be sanitized, model-safe JSON and must never contain OAuth
// tokens, authorization codes, callback URLs, or callback tokens.
type WorkflowResult struct {
	RequestID string
	AccountID string
	State     string
	Payload   json.RawMessage
	CreatedAt time.Time
}

// WorkflowContinuation identifies the exact conversation turn to resume after
// an asynchronous workflow reaches a terminal result.
type WorkflowContinuation struct {
	RequestID         string
	AccountID         string
	Platform          string
	UserKey           string
	SessionID         string
	ChatID            string
	ChatIsGroup       bool
	SourceMessageID   string
	ActorOpenID       string
	ActorUserID       string
	OriginRevision    int64
	CommittedRevision int64
	OriginTurnID      string
	ToolCallID        string
	ToolName          string
	State             string
	Attempts          int
	AvailableAt       time.Time
	LeaseToken        string
	LeaseExpiresAt    time.Time
	LastError         string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

const workflowContinuationSelect = `SELECT request_id, account_id, platform, user_key, session_id,
	chat_id, chat_is_group, source_message_id, actor_open_id, actor_user_id,
	origin_revision, committed_revision, origin_turn_id, tool_call_id, tool_name,
	state, attempts, available_at_ms, lease_token, lease_expires_at_ms, last_error,
	created_at_ms, updated_at_ms
	FROM workflow_continuations`

// CreateWorkflowContinuation stores one waiting continuation for an existing
// workflow request. The origin turn is committed separately after its CAS save.
func (s *Store) CreateWorkflowContinuation(continuation WorkflowContinuation) (WorkflowContinuation, error) {
	continuation, err := prepareWorkflowContinuation(continuation)
	if err != nil {
		return WorkflowContinuation{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return WorkflowContinuation{}, fmt.Errorf("begin create workflow continuation: %w", err)
	}
	defer tx.Rollback()
	var requestCount int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM workflow_requests WHERE id=? AND account_id=?`,
		continuation.RequestID, continuation.AccountID,
	).Scan(&requestCount); err != nil {
		return WorkflowContinuation{}, fmt.Errorf("check workflow request for continuation: %w", err)
	}
	if requestCount != 1 {
		return WorkflowContinuation{}, ErrWorkflowRequestNotFound
	}
	_, err = tx.Exec(
		`INSERT INTO workflow_continuations (
			request_id, account_id, platform, user_key, session_id,
			chat_id, chat_is_group, source_message_id, actor_open_id, actor_user_id,
			origin_revision, committed_revision, origin_turn_id, tool_call_id, tool_name,
			state, attempts, available_at_ms, lease_token, lease_expires_at_ms, last_error,
			created_at_ms, updated_at_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, -1, ?, ?, ?, ?, 0, ?, '', 0, '', ?, ?)`,
		continuation.RequestID,
		continuation.AccountID,
		continuation.Platform,
		continuation.UserKey,
		continuation.SessionID,
		continuation.ChatID,
		continuation.ChatIsGroup,
		continuation.SourceMessageID,
		continuation.ActorOpenID,
		continuation.ActorUserID,
		continuation.OriginRevision,
		continuation.OriginTurnID,
		continuation.ToolCallID,
		continuation.ToolName,
		continuation.State,
		continuation.AvailableAt.UnixMilli(),
		continuation.CreatedAt.UnixMilli(),
		continuation.UpdatedAt.UnixMilli(),
	)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique constraint failed") {
			return WorkflowContinuation{}, fmt.Errorf("%w: %s", ErrWorkflowRequestExists, continuation.RequestID)
		}
		return WorkflowContinuation{}, fmt.Errorf("create workflow continuation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return WorkflowContinuation{}, fmt.Errorf("commit workflow continuation: %w", err)
	}
	return continuation, nil
}

// CommitWorkflowContinuation records the revision produced by the origin turn.
// If a terminal result arrived first, the continuation becomes ready atomically.
func (s *Store) CommitWorkflowContinuation(requestID, accountID string, committedRevision int64, now time.Time) (WorkflowContinuation, bool, error) {
	requestID = strings.TrimSpace(requestID)
	accountID = strings.TrimSpace(accountID)
	now = normalizedWorkflowTime(now)
	if requestID == "" || accountID == "" || committedRevision < 0 {
		return WorkflowContinuation{}, false, fmt.Errorf("workflow continuation request, account, and committed revision are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return WorkflowContinuation{}, false, fmt.Errorf("begin commit workflow continuation: %w", err)
	}
	defer tx.Rollback()
	continuation, err := workflowContinuationByID(tx, requestID, accountID)
	if err != nil {
		return WorkflowContinuation{}, false, err
	}
	if committedRevision <= continuation.OriginRevision {
		return WorkflowContinuation{}, false, fmt.Errorf("committed revision %d must be newer than origin revision %d", committedRevision, continuation.OriginRevision)
	}
	if continuation.CommittedRevision >= 0 {
		if continuation.CommittedRevision != committedRevision {
			return continuation, continuation.State == WorkflowContinuationStateReady, fmt.Errorf("workflow continuation committed revision conflict: existing=%d requested=%d", continuation.CommittedRevision, committedRevision)
		}
		return continuation, continuation.State == WorkflowContinuationStateReady, nil
	}
	if workflowContinuationTerminal(continuation.State) {
		return continuation, false, ErrWorkflowContinuationResolved
	}
	resultExists, err := workflowResultExists(tx, requestID, accountID)
	if err != nil {
		return WorkflowContinuation{}, false, err
	}
	nextState := continuation.State
	availableAt := continuation.AvailableAt
	if resultExists && continuation.State == WorkflowContinuationStateWaiting {
		nextState = WorkflowContinuationStateReady
		availableAt = now
	}
	result, err := tx.Exec(
		`UPDATE workflow_continuations
		 SET committed_revision=?, state=?, available_at_ms=?, updated_at_ms=?
		 WHERE request_id=? AND account_id=? AND committed_revision=-1`,
		committedRevision, nextState, availableAt.UnixMilli(), now.UnixMilli(), requestID, accountID,
	)
	if err != nil {
		return WorkflowContinuation{}, false, fmt.Errorf("commit workflow continuation: %w", err)
	}
	if err := requireOneWorkflowContinuationRow(result); err != nil {
		return WorkflowContinuation{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return WorkflowContinuation{}, false, fmt.Errorf("commit workflow continuation transaction: %w", err)
	}
	continuation.CommittedRevision = committedRevision
	continuation.State = nextState
	continuation.AvailableAt = availableAt
	continuation.UpdatedAt = now
	return continuation, nextState == WorkflowContinuationStateReady, nil
}

// StoreWorkflowResult inserts the single terminal result and makes a committed
// waiting continuation ready. Repeating the exact same result is idempotent.
func (s *Store) StoreWorkflowResult(result WorkflowResult) (WorkflowResult, WorkflowContinuation, bool, error) {
	result, err := prepareWorkflowResult(result)
	if err != nil {
		return WorkflowResult{}, WorkflowContinuation{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return WorkflowResult{}, WorkflowContinuation{}, false, fmt.Errorf("begin store workflow result: %w", err)
	}
	defer tx.Rollback()
	stored, continuation, ready, err := storeWorkflowResultTx(tx, result)
	if err != nil {
		return stored, continuation, false, err
	}
	if err := tx.Commit(); err != nil {
		return WorkflowResult{}, WorkflowContinuation{}, false, fmt.Errorf("commit workflow result: %w", err)
	}
	return stored, continuation, ready, nil
}

// storeWorkflowResultTx inserts a terminal result and readies its committed
// continuation inside the caller's transaction. This keeps workflow-specific
// terminal state, the model-visible result, and any terminal outbox atomically
// visible when a higher-level finalizer uses the same transaction.
func storeWorkflowResultTx(tx *sql.Tx, result WorkflowResult) (WorkflowResult, WorkflowContinuation, bool, error) {
	continuation, err := workflowContinuationByID(tx, result.RequestID, result.AccountID)
	if err != nil {
		return WorkflowResult{}, WorkflowContinuation{}, false, err
	}
	_, err = tx.Exec(
		`INSERT INTO workflow_results (request_id, account_id, state, payload, created_at_ms)
		 VALUES (?, ?, ?, ?, ?)`,
		result.RequestID, result.AccountID, result.State, string(result.Payload), result.CreatedAt.UnixMilli(),
	)
	if err != nil {
		if !strings.Contains(strings.ToLower(err.Error()), "unique constraint failed") {
			return WorkflowResult{}, WorkflowContinuation{}, false, fmt.Errorf("store workflow result: %w", err)
		}
		existing, loadErr := workflowResultByID(tx, result.RequestID, result.AccountID)
		if loadErr != nil {
			return WorkflowResult{}, WorkflowContinuation{}, false, loadErr
		}
		if existing.State != result.State || string(existing.Payload) != string(result.Payload) {
			return existing, continuation, false, ErrWorkflowResultConflict
		}
		return existing, continuation, continuation.State == WorkflowContinuationStateReady, nil
	}

	ready := false
	if continuation.State == WorkflowContinuationStateWaiting && continuation.CommittedRevision >= 0 {
		update, updateErr := tx.Exec(
			`UPDATE workflow_continuations
			 SET state=?, available_at_ms=?, updated_at_ms=?
			 WHERE request_id=? AND account_id=? AND state=?`,
			WorkflowContinuationStateReady,
			result.CreatedAt.UnixMilli(),
			result.CreatedAt.UnixMilli(),
			result.RequestID,
			result.AccountID,
			WorkflowContinuationStateWaiting,
		)
		if updateErr != nil {
			return WorkflowResult{}, WorkflowContinuation{}, false, fmt.Errorf("ready workflow continuation: %w", updateErr)
		}
		if err := requireOneWorkflowContinuationRow(update); err != nil {
			return WorkflowResult{}, WorkflowContinuation{}, false, err
		}
		continuation.State = WorkflowContinuationStateReady
		continuation.AvailableAt = result.CreatedAt
		continuation.UpdatedAt = result.CreatedAt
		ready = true
	}
	return result, continuation, ready, nil
}

// ClaimWorkflowContinuation leases one ready continuation. An expired
// processing lease can be reclaimed after a process restart.
func (s *Store) ClaimWorkflowContinuation(requestID, accountID, leaseToken string, now time.Time, leaseDuration time.Duration) (WorkflowContinuation, error) {
	requestID = strings.TrimSpace(requestID)
	accountID = strings.TrimSpace(accountID)
	leaseToken = strings.TrimSpace(leaseToken)
	now = normalizedWorkflowTime(now)
	if requestID == "" || accountID == "" || leaseToken == "" || leaseDuration <= 0 {
		return WorkflowContinuation{}, fmt.Errorf("workflow continuation request, account, lease token, and positive lease duration are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return WorkflowContinuation{}, fmt.Errorf("begin claim workflow continuation: %w", err)
	}
	defer tx.Rollback()
	continuation, err := workflowContinuationByID(tx, requestID, accountID)
	if err != nil {
		return WorkflowContinuation{}, err
	}
	eligible := continuation.State == WorkflowContinuationStateReady && !continuation.AvailableAt.After(now)
	if continuation.State == WorkflowContinuationStateProcessing && !continuation.LeaseExpiresAt.After(now) {
		eligible = true
	}
	if !eligible {
		if workflowContinuationTerminal(continuation.State) {
			return continuation, ErrWorkflowContinuationResolved
		}
		return continuation, ErrWorkflowContinuationNotReady
	}
	leaseExpiresAt := now.Add(leaseDuration)
	update, err := tx.Exec(
		`UPDATE workflow_continuations
		 SET state=?, attempts=attempts+1, lease_token=?, lease_expires_at_ms=?, last_error='', updated_at_ms=?
		 WHERE request_id=? AND account_id=?`,
		WorkflowContinuationStateProcessing,
		leaseToken,
		leaseExpiresAt.UnixMilli(),
		now.UnixMilli(),
		requestID,
		accountID,
	)
	if err != nil {
		return WorkflowContinuation{}, fmt.Errorf("claim workflow continuation: %w", err)
	}
	if err := requireOneWorkflowContinuationRow(update); err != nil {
		return WorkflowContinuation{}, err
	}
	if err := tx.Commit(); err != nil {
		return WorkflowContinuation{}, fmt.Errorf("commit claimed workflow continuation: %w", err)
	}
	continuation.State = WorkflowContinuationStateProcessing
	continuation.Attempts++
	continuation.LeaseToken = leaseToken
	continuation.LeaseExpiresAt = leaseExpiresAt
	continuation.LastError = ""
	continuation.UpdatedAt = now
	return continuation, nil
}

// RetryWorkflowContinuation releases a processing lease and schedules retry.
func (s *Store) RetryWorkflowContinuation(requestID, accountID, leaseToken string, availableAt time.Time, lastError string, now time.Time) error {
	availableAt = normalizedWorkflowTime(availableAt)
	return s.updateLeasedWorkflowContinuation(
		requestID, accountID, leaseToken,
		WorkflowContinuationStateReady, availableAt, lastError, now, false, "",
	)
}

// ReleaseWorkflowContinuation returns a lease interrupted by runtime shutdown
// to ready state without consuming a delivery-attempt budget entry.
func (s *Store) ReleaseWorkflowContinuation(requestID, accountID, leaseToken, lastError string, now time.Time) error {
	now = normalizedWorkflowTime(now)
	return s.updateLeasedWorkflowContinuation(
		requestID, accountID, leaseToken,
		WorkflowContinuationStateReady, now, lastError, now, true, "",
	)
}

// CompleteWorkflowContinuation marks a leased continuation delivered, canceled,
// or failed.
func (s *Store) CompleteWorkflowContinuation(requestID, accountID, leaseToken, state, lastError string, now time.Time) error {
	if state != WorkflowContinuationStateDelivered && state != WorkflowContinuationStateCanceled && state != WorkflowContinuationStateFailed {
		return fmt.Errorf("unsupported workflow continuation completion state %q", state)
	}
	purpose := ""
	switch state {
	case WorkflowContinuationStateCanceled:
		purpose = FeishuCardDeliveryPurposeWorkflowUnavailable
	case WorkflowContinuationStateFailed:
		purpose = FeishuCardDeliveryPurposeWorkflowExhausted
	}
	return s.updateLeasedWorkflowContinuation(
		requestID, accountID, leaseToken,
		state, normalizedWorkflowTime(now), lastError, now, false, purpose,
	)
}

func (s *Store) updateLeasedWorkflowContinuation(requestID, accountID, leaseToken, state string, availableAt time.Time, lastError string, now time.Time, restoreAttempt bool, cardPurpose string) error {
	requestID = strings.TrimSpace(requestID)
	accountID = strings.TrimSpace(accountID)
	leaseToken = strings.TrimSpace(leaseToken)
	lastError = truncateWorkflowContinuationError(lastError)
	now = normalizedWorkflowTime(now)
	if requestID == "" || accountID == "" || leaseToken == "" {
		return fmt.Errorf("workflow continuation request, account, and lease token are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin update leased workflow continuation: %w", err)
	}
	defer tx.Rollback()
	query := `UPDATE workflow_continuations
		 SET state=?, available_at_ms=?, lease_token='', lease_expires_at_ms=0, last_error=?, updated_at_ms=?
		 WHERE request_id=? AND account_id=? AND state=? AND lease_token=?`
	if restoreAttempt {
		query = `UPDATE workflow_continuations
		 SET state=?, available_at_ms=?, lease_token='', lease_expires_at_ms=0,
		 attempts=CASE WHEN attempts>0 THEN attempts-1 ELSE 0 END,
		 last_error=?, updated_at_ms=?
		 WHERE request_id=? AND account_id=? AND state=? AND lease_token=?`
	}
	result, err := tx.Exec(
		query,
		state,
		availableAt.UnixMilli(),
		lastError,
		now.UnixMilli(),
		requestID,
		accountID,
		WorkflowContinuationStateProcessing,
		leaseToken,
	)
	if err != nil {
		return fmt.Errorf("update leased workflow continuation: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect leased workflow continuation update: %w", err)
	}
	if count == 1 {
		if cardPurpose != "" {
			reference, err := workflowCardReferenceByID(tx, requestID, accountID)
			if err != nil {
				return err
			}
			if reference.CardMessageID != "" {
				delivery, err := prepareFeishuCardDelivery(FeishuCardDelivery{
					AccountID:     accountID,
					RequestID:     requestID,
					Purpose:       cardPurpose,
					Revision:      FeishuCardDeliveryRevisionContinuation,
					CardMessageID: reference.CardMessageID,
					CreatedAt:     now,
					ExpiresAt:     now.Add(feishuTerminalCardDeliveryTTL),
				})
				if err != nil {
					return err
				}
				if _, err := enqueueFeishuCardDelivery(tx, delivery); err != nil {
					return fmt.Errorf("enqueue terminal workflow card delivery: %w", err)
				}
			}
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit leased workflow continuation update: %w", err)
		}
		return nil
	}
	continuation, loadErr := workflowContinuationByID(tx, requestID, accountID)
	if loadErr != nil {
		return loadErr
	}
	if continuation.State != WorkflowContinuationStateProcessing {
		return ErrWorkflowContinuationResolved
	}
	return ErrWorkflowContinuationLeaseLost
}

// CancelWorkflowContinuation prevents a waiting or ready workflow from being resumed.
func (s *Store) CancelWorkflowContinuation(requestID, accountID, reason string, now time.Time) error {
	requestID = strings.TrimSpace(requestID)
	accountID = strings.TrimSpace(accountID)
	reason = truncateWorkflowContinuationError(reason)
	now = normalizedWorkflowTime(now)
	if requestID == "" || accountID == "" {
		return fmt.Errorf("workflow continuation request and account are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.db.Exec(
		`UPDATE workflow_continuations
		 SET state=?, lease_token='', lease_expires_at_ms=0, last_error=?, updated_at_ms=?
		 WHERE request_id=? AND account_id=? AND state IN (?, ?)`,
		WorkflowContinuationStateCanceled,
		reason,
		now.UnixMilli(),
		requestID,
		accountID,
		WorkflowContinuationStateWaiting,
		WorkflowContinuationStateReady,
	)
	if err != nil {
		return fmt.Errorf("cancel workflow continuation: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect workflow continuation cancellation: %w", err)
	}
	if count == 1 {
		return nil
	}
	return ErrWorkflowContinuationResolved
}

// ListResumableWorkflowContinuations returns ready work and expired leases.
func (s *Store) ListResumableWorkflowContinuations(accountID string, now time.Time, limit int) ([]WorkflowContinuation, error) {
	accountID = strings.TrimSpace(accountID)
	now = normalizedWorkflowTime(now)
	if accountID == "" {
		return nil, fmt.Errorf("workflow continuation account is required")
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin list resumable workflow continuations: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(
		`UPDATE workflow_continuations
		 SET state=?, available_at_ms=?, updated_at_ms=?
		 WHERE account_id=? AND state=? AND committed_revision>=0
		   AND EXISTS (SELECT 1 FROM workflow_results WHERE request_id=workflow_continuations.request_id AND account_id=workflow_continuations.account_id)`,
		WorkflowContinuationStateReady,
		now.UnixMilli(),
		now.UnixMilli(),
		accountID,
		WorkflowContinuationStateWaiting,
	); err != nil {
		return nil, fmt.Errorf("reconcile ready workflow continuations: %w", err)
	}
	rows, err := tx.Query(
		workflowContinuationSelect+`
		 WHERE account_id=? AND (
			(state=? AND available_at_ms<=?) OR
			(state=? AND lease_expires_at_ms<=?)
		 )
		 ORDER BY available_at_ms, created_at_ms
		 LIMIT ?`,
		accountID,
		WorkflowContinuationStateReady,
		now.UnixMilli(),
		WorkflowContinuationStateProcessing,
		now.UnixMilli(),
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list resumable workflow continuations: %w", err)
	}
	defer rows.Close()
	continuations := []WorkflowContinuation{}
	for rows.Next() {
		continuation, scanErr := scanWorkflowContinuation(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		continuations = append(continuations, continuation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list resumable workflow continuations: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close resumable workflow continuations: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit resumable workflow continuation scan: %w", err)
	}
	return continuations, nil
}

// ListUncommittedWorkflowContinuations returns waiting origins whose post-tool
// conversation revision was not recorded. Callers must verify the saved
// conversation contains the exact tool trace before committing one of them.
func (s *Store) ListUncommittedWorkflowContinuations(accountID string, limit int) ([]WorkflowContinuation, error) {
	return s.ListUncommittedWorkflowContinuationsAfter(accountID, time.Time{}, "", limit)
}

// ListUncommittedWorkflowContinuationsAfter returns one keyset page of waiting
// origin commits. A recovery worker can advance past permanently unverifiable
// old rows instead of repeatedly starving newer recoverable continuations.
func (s *Store) ListUncommittedWorkflowContinuationsAfter(
	accountID string,
	afterCreatedAt time.Time,
	afterRequestID string,
	limit int,
) ([]WorkflowContinuation, error) {
	accountID = strings.TrimSpace(accountID)
	afterRequestID = strings.TrimSpace(afterRequestID)
	if accountID == "" {
		return nil, fmt.Errorf("workflow continuation account is required")
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	afterCreatedAtMS := int64(-1)
	if !afterCreatedAt.IsZero() {
		afterCreatedAtMS = afterCreatedAt.UTC().UnixMilli()
	}
	rows, err := s.db.Query(
		workflowContinuationSelect+`
		 WHERE account_id=? AND state=? AND committed_revision=-1
		 AND (created_at_ms>? OR (created_at_ms=? AND request_id>?))
		 ORDER BY created_at_ms, request_id
		 LIMIT ?`,
		accountID,
		WorkflowContinuationStateWaiting,
		afterCreatedAtMS,
		afterCreatedAtMS,
		afterRequestID,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list uncommitted workflow continuations: %w", err)
	}
	defer rows.Close()
	continuations := []WorkflowContinuation{}
	for rows.Next() {
		continuation, scanErr := scanWorkflowContinuation(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		continuations = append(continuations, continuation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list uncommitted workflow continuations: %w", err)
	}
	return continuations, nil
}

// ListTerminalWorkflowResultGaps returns terminal workflow roots whose waiting
// continuation does not yet have a durable result. Callers use this to repair
// the small crash window between a workflow-specific terminal update and
// StoreWorkflowResult. updatedBefore lets live reconcilers avoid racing a
// terminal callback that is still writing its richer result payload.
func (s *Store) ListTerminalWorkflowResultGaps(accountID, kind string, updatedBefore time.Time, limit int) ([]WorkflowRequest, error) {
	accountID = strings.TrimSpace(accountID)
	kind = strings.TrimSpace(kind)
	updatedBefore = normalizedWorkflowTime(updatedBefore)
	if accountID == "" || kind == "" {
		return nil, fmt.Errorf("workflow result gap account and kind are required")
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.db.Query(
		`SELECT request.id, request.account_id, request.kind, request.state,
			request.created_at_ms, request.updated_at_ms
		 FROM workflow_requests AS request
		 JOIN workflow_continuations AS continuation
		   ON continuation.request_id=request.id AND continuation.account_id=request.account_id
		 LEFT JOIN workflow_results AS result
		   ON result.request_id=request.id AND result.account_id=request.account_id
		 WHERE request.account_id=? AND request.kind=?
		   AND request.state IN (?, ?, ?, ?, ?)
		   AND request.updated_at_ms<=?
		   AND continuation.state=?
		   AND result.request_id IS NULL
		 ORDER BY request.updated_at_ms, request.created_at_ms
		 LIMIT ?`,
		accountID,
		kind,
		WorkflowRequestStateDenied,
		WorkflowRequestStateSucceeded,
		WorkflowRequestStatePartial,
		WorkflowRequestStateFailed,
		WorkflowRequestStateExpired,
		updatedBefore.UnixMilli(),
		WorkflowContinuationStateWaiting,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list terminal workflow result gaps: %w", err)
	}
	defer rows.Close()
	requests := []WorkflowRequest{}
	for rows.Next() {
		request, scanErr := scanWorkflowRequest(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		requests = append(requests, request)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list terminal workflow result gaps: %w", err)
	}
	return requests, nil
}

func (s *Store) GetWorkflowContinuation(requestID, accountID string) (WorkflowContinuation, error) {
	return workflowContinuationByID(s.db, strings.TrimSpace(requestID), strings.TrimSpace(accountID))
}

func (s *Store) GetWorkflowResult(requestID, accountID string) (WorkflowResult, error) {
	return workflowResultByID(s.db, strings.TrimSpace(requestID), strings.TrimSpace(accountID))
}

func prepareWorkflowContinuation(continuation WorkflowContinuation) (WorkflowContinuation, error) {
	continuation.RequestID = strings.TrimSpace(continuation.RequestID)
	continuation.AccountID = strings.TrimSpace(continuation.AccountID)
	continuation.Platform = strings.TrimSpace(continuation.Platform)
	continuation.UserKey = strings.TrimSpace(continuation.UserKey)
	continuation.SessionID = strings.TrimSpace(continuation.SessionID)
	continuation.ChatID = strings.TrimSpace(continuation.ChatID)
	continuation.SourceMessageID = strings.TrimSpace(continuation.SourceMessageID)
	continuation.ActorOpenID = strings.TrimSpace(continuation.ActorOpenID)
	continuation.ActorUserID = strings.TrimSpace(continuation.ActorUserID)
	continuation.OriginTurnID = strings.TrimSpace(continuation.OriginTurnID)
	continuation.ToolCallID = strings.TrimSpace(continuation.ToolCallID)
	continuation.ToolName = strings.TrimSpace(continuation.ToolName)
	continuation.CreatedAt = normalizedWorkflowTime(continuation.CreatedAt)
	continuation.UpdatedAt = continuation.CreatedAt
	if continuation.AvailableAt.IsZero() {
		continuation.AvailableAt = continuation.CreatedAt
	} else {
		continuation.AvailableAt = continuation.AvailableAt.UTC()
	}
	continuation.CommittedRevision = -1
	continuation.State = WorkflowContinuationStateWaiting
	continuation.Attempts = 0
	continuation.LeaseToken = ""
	continuation.LeaseExpiresAt = time.Time{}
	continuation.LastError = ""
	if continuation.RequestID == "" || continuation.AccountID == "" || continuation.Platform == "" ||
		continuation.UserKey == "" || continuation.SessionID == "" || continuation.ChatID == "" || continuation.SourceMessageID == "" ||
		(continuation.ActorOpenID == "" && continuation.ActorUserID == "") || continuation.OriginTurnID == "" ||
		continuation.ToolCallID == "" || continuation.ToolName == "" || continuation.OriginRevision < 0 {
		return WorkflowContinuation{}, fmt.Errorf("workflow continuation request, account, platform, user, session, chat/message, actor, origin revision/turn, tool call, and tool name are required")
	}
	return continuation, nil
}

func prepareWorkflowResult(result WorkflowResult) (WorkflowResult, error) {
	result.RequestID = strings.TrimSpace(result.RequestID)
	result.AccountID = strings.TrimSpace(result.AccountID)
	result.State = strings.TrimSpace(result.State)
	result.CreatedAt = normalizedWorkflowTime(result.CreatedAt)
	if result.RequestID == "" || result.AccountID == "" || !validWorkflowResultState(result.State) {
		return WorkflowResult{}, fmt.Errorf("workflow result request, account, and terminal state are required")
	}
	payload, err := normalizeWorkflowResultPayload(result.Payload)
	if err != nil {
		return WorkflowResult{}, err
	}
	result.Payload = payload
	return result, nil
}

func normalizeWorkflowResultPayload(payload json.RawMessage) (json.RawMessage, error) {
	if len(payload) == 0 {
		return json.RawMessage(`{}`), nil
	}
	var value any
	if err := json.Unmarshal(payload, &value); err != nil {
		return nil, fmt.Errorf("workflow result payload must be valid JSON: %w", err)
	}
	normalized, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("normalize workflow result payload: %w", err)
	}
	return normalized, nil
}

func validWorkflowResultState(state string) bool {
	switch state {
	case WorkflowResultStateSucceeded, WorkflowResultStateDenied, WorkflowResultStateExpired, WorkflowResultStateFailed:
		return true
	default:
		return false
	}
}

func workflowContinuationTerminal(state string) bool {
	switch state {
	case WorkflowContinuationStateDelivered, WorkflowContinuationStateCanceled, WorkflowContinuationStateFailed:
		return true
	default:
		return false
	}
}

func workflowResultExists(queryer interface {
	QueryRow(query string, args ...any) *sql.Row
}, requestID, accountID string) (bool, error) {
	var count int
	if err := queryer.QueryRow(
		`SELECT COUNT(*) FROM workflow_results WHERE request_id=? AND account_id=?`,
		requestID, accountID,
	).Scan(&count); err != nil {
		return false, fmt.Errorf("check workflow result: %w", err)
	}
	return count == 1, nil
}

func workflowContinuationByID(queryer interface {
	QueryRow(query string, args ...any) *sql.Row
}, requestID, accountID string) (WorkflowContinuation, error) {
	return scanWorkflowContinuation(queryer.QueryRow(
		workflowContinuationSelect+` WHERE request_id=? AND account_id=?`,
		requestID, accountID,
	))
}

type workflowContinuationScanner interface {
	Scan(dest ...any) error
}

func scanWorkflowContinuation(row workflowContinuationScanner) (WorkflowContinuation, error) {
	var continuation WorkflowContinuation
	var availableAtMS, leaseExpiresAtMS, createdAtMS, updatedAtMS int64
	err := row.Scan(
		&continuation.RequestID,
		&continuation.AccountID,
		&continuation.Platform,
		&continuation.UserKey,
		&continuation.SessionID,
		&continuation.ChatID,
		&continuation.ChatIsGroup,
		&continuation.SourceMessageID,
		&continuation.ActorOpenID,
		&continuation.ActorUserID,
		&continuation.OriginRevision,
		&continuation.CommittedRevision,
		&continuation.OriginTurnID,
		&continuation.ToolCallID,
		&continuation.ToolName,
		&continuation.State,
		&continuation.Attempts,
		&availableAtMS,
		&continuation.LeaseToken,
		&leaseExpiresAtMS,
		&continuation.LastError,
		&createdAtMS,
		&updatedAtMS,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return WorkflowContinuation{}, ErrWorkflowContinuationNotFound
		}
		return WorkflowContinuation{}, fmt.Errorf("scan workflow continuation: %w", err)
	}
	continuation.AvailableAt = time.UnixMilli(availableAtMS).UTC()
	if leaseExpiresAtMS > 0 {
		continuation.LeaseExpiresAt = time.UnixMilli(leaseExpiresAtMS).UTC()
	}
	continuation.CreatedAt = time.UnixMilli(createdAtMS).UTC()
	continuation.UpdatedAt = time.UnixMilli(updatedAtMS).UTC()
	return continuation, nil
}

func workflowResultByID(queryer interface {
	QueryRow(query string, args ...any) *sql.Row
}, requestID, accountID string) (WorkflowResult, error) {
	var result WorkflowResult
	var payload string
	var createdAtMS int64
	err := queryer.QueryRow(
		`SELECT request_id, account_id, state, payload, created_at_ms
		 FROM workflow_results WHERE request_id=? AND account_id=?`,
		requestID, accountID,
	).Scan(&result.RequestID, &result.AccountID, &result.State, &payload, &createdAtMS)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return WorkflowResult{}, ErrWorkflowResultNotFound
		}
		return WorkflowResult{}, fmt.Errorf("get workflow result: %w", err)
	}
	result.Payload = json.RawMessage(payload)
	result.CreatedAt = time.UnixMilli(createdAtMS).UTC()
	return result, nil
}

func requireOneWorkflowContinuationRow(result sql.Result) error {
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect workflow continuation update: %w", err)
	}
	if count != 1 {
		return ErrWorkflowContinuationNotFound
	}
	return nil
}

func truncateWorkflowContinuationError(value string) string {
	value = strings.TrimSpace(value)
	if utf8.RuneCountInString(value) <= maxWorkflowContinuationErrorRunes {
		return value
	}
	runes := []rune(value)
	return string(runes[:maxWorkflowContinuationErrorRunes])
}

func deleteWorkflowRuntimeData(execer workflowRequestExecer, accountID string, kinds ...string) error {
	if len(kinds) == 0 {
		return nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(kinds)), ",")
	args := make([]any, 0, len(kinds)+2)
	args = append(args, accountID, accountID)
	for _, kind := range kinds {
		args = append(args, kind)
	}
	requestFilter := `request_id IN (
		SELECT id FROM workflow_requests WHERE account_id=? AND kind IN (` + placeholders + `)
	)`
	for _, table := range []string{"workflow_continuations", "workflow_results"} {
		if _, err := execer.Exec(
			`DELETE FROM `+table+` WHERE account_id=? AND `+requestFilter,
			args...,
		); err != nil {
			return fmt.Errorf("delete %s for account workflows: %w", table, err)
		}
	}
	return nil
}
