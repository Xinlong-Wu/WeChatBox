package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrFeishuDocxAppendOperationNotFound = errors.New("feishu docx append operation not found")
	ErrFeishuDocxAppendOperationConflict = errors.New("feishu docx append operation conflicts with the persisted request")
	ErrFeishuDocxAppendOperationNotReady = errors.New("feishu docx append operation is not ready for this transition")
)

const (
	FeishuDocxAppendOperationStatePrepared       = "prepared"
	FeishuDocxAppendOperationStateRemoteStarted  = "remote_started"
	FeishuDocxAppendOperationStateSucceeded      = "succeeded"
	FeishuDocxAppendOperationStateOutcomeUnknown = "outcome_unknown"
	FeishuDocxAppendOperationStateFailed         = "failed"
)

const feishuDocxAppendOperationSelect = `SELECT
 request_id, account_id, chat_id, actor_open_id, actor_user_id,
 document_token, client_token, insertion_index, payload_hash, envelope_hash,
 envelope_ciphertext, state, remote_call_started_at_ms, remote_result_at_ms,
 execution_owner_id, execution_token, execution_lease_expires_at_ms,
 last_error_category, created_at_ms, updated_at_ms
 FROM feishu_docx_append_operations`

// feishuDocxAppendWorkflowStateExpression keeps every direct Docs workflow
// write subordinate to the current append ledger in the same SQLite
// statement. The final placeholder is the caller's fallback for legacy
// requests that do not have an append ledger. Tool-approval and other workflow
// kinds also retain that fallback because their terminal semantics are owned by
// their own state machines.
const feishuDocxAppendWorkflowStateExpression = `CASE
 WHEN workflow_requests.kind IN (?, ?) AND EXISTS (
  SELECT 1 FROM feishu_docx_append_operations AS append
  WHERE append.request_id=workflow_requests.id
    AND append.account_id=workflow_requests.account_id
    AND append.state=?
 ) THEN ?
 WHEN workflow_requests.kind=? AND EXISTS (
  SELECT 1 FROM feishu_docx_append_operations AS append
  WHERE append.request_id=workflow_requests.id
    AND append.account_id=workflow_requests.account_id
    AND append.state=?
 ) THEN ?
 WHEN workflow_requests.kind IN (?, ?) AND EXISTS (
  SELECT 1 FROM feishu_docx_append_operations AS append
  WHERE append.request_id=workflow_requests.id
    AND append.account_id=workflow_requests.account_id
 ) THEN ?
 ELSE ?
 END`

// FeishuDocxAppendOperation freezes one non-idempotent append request before
// its first remote mutation. EnvelopeCiphertext contains the exact protected
// request body while recovery remains possible and is cleared at a terminal
// state.
type FeishuDocxAppendOperation struct {
	RequestID           string
	AccountID           string
	ChatID              string
	ActorOpenID         string
	ActorUserID         string
	DocumentToken       string
	ClientToken         string
	InsertionIndex      int
	PayloadHash         string
	EnvelopeHash        string
	EnvelopeCiphertext  string
	State               string
	RemoteCallStartedAt time.Time
	RemoteResultAt      time.Time
	ExecutionOwnerID    string
	ExecutionToken      string
	ExecutionLeaseUntil time.Time
	LastErrorCategory   string
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// PrepareFeishuDocxAppendOperation persists the exact protected request before
// any append mutation. Concurrent equivalent prepares reuse the first frozen
// envelope; a different logical payload for the same request fails closed.
func (s *Store) PrepareFeishuDocxAppendOperation(operation FeishuDocxAppendOperation) (FeishuDocxAppendOperation, bool, error) {
	if err := s.requireFeishuDocsStore(); err != nil {
		return FeishuDocxAppendOperation{}, false, err
	}
	operation = normalizeFeishuDocxAppendOperation(operation)
	if err := validatePreparedFeishuDocxAppendOperation(operation); err != nil {
		return FeishuDocxAppendOperation{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return FeishuDocxAppendOperation{}, false, fmt.Errorf("begin prepare feishu docx append operation: %w", err)
	}
	defer tx.Rollback()
	workflow, err := scanWorkflowRequest(tx.QueryRow(
		`SELECT id, account_id, kind, state, created_at_ms, updated_at_ms
		 FROM workflow_requests WHERE id=? AND account_id=?`,
		operation.RequestID,
		operation.AccountID,
	))
	if err != nil {
		return FeishuDocxAppendOperation{}, false, fmt.Errorf("load feishu docx append workflow: %w", err)
	}
	if !workflowKindSupportsFeishuDocxAppend(workflow.Kind) {
		return FeishuDocxAppendOperation{}, false, fmt.Errorf("%w: workflow kind %q cannot own docx append", ErrFeishuDocxAppendOperationConflict, workflow.Kind)
	}
	_, err = tx.Exec(
		`INSERT INTO feishu_docx_append_operations (
		 request_id, account_id, chat_id, actor_open_id, actor_user_id,
		 document_token, client_token, insertion_index, payload_hash, envelope_hash,
		 envelope_ciphertext, state, remote_call_started_at_ms, remote_result_at_ms,
		 last_error_category, created_at_ms, updated_at_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 0, '', ?, ?)`,
		operation.RequestID,
		operation.AccountID,
		operation.ChatID,
		operation.ActorOpenID,
		operation.ActorUserID,
		operation.DocumentToken,
		operation.ClientToken,
		operation.InsertionIndex,
		operation.PayloadHash,
		operation.EnvelopeHash,
		operation.EnvelopeCiphertext,
		FeishuDocxAppendOperationStatePrepared,
		operation.CreatedAt.UnixMilli(),
		operation.UpdatedAt.UnixMilli(),
	)
	if err != nil {
		if !strings.Contains(strings.ToLower(err.Error()), "unique constraint failed") {
			return FeishuDocxAppendOperation{}, false, fmt.Errorf("prepare feishu docx append operation: %w", err)
		}
		existing, loadErr := feishuDocxAppendOperationByID(tx, operation.RequestID, operation.AccountID)
		if loadErr != nil {
			return FeishuDocxAppendOperation{}, false, loadErr
		}
		if !sameFeishuDocxAppendRequest(existing, operation) {
			return FeishuDocxAppendOperation{}, false, ErrFeishuDocxAppendOperationConflict
		}
		return existing, false, nil
	}
	if err := tx.Commit(); err != nil {
		return FeishuDocxAppendOperation{}, false, fmt.Errorf("commit prepared feishu docx append operation: %w", err)
	}
	operation.State = FeishuDocxAppendOperationStatePrepared
	return operation, true, nil
}

// StartFeishuDocxAppendOperation atomically grants one caller permission for
// the first remote append call.
func (s *Store) StartFeishuDocxAppendOperation(requestID, accountID, executionOwnerID, executionToken string, now time.Time, leaseDuration time.Duration) (FeishuDocxAppendOperation, bool, error) {
	if err := s.requireFeishuDocsStore(); err != nil {
		return FeishuDocxAppendOperation{}, false, err
	}
	requestID = strings.TrimSpace(requestID)
	accountID = strings.TrimSpace(accountID)
	executionOwnerID = strings.TrimSpace(executionOwnerID)
	executionToken = strings.TrimSpace(executionToken)
	now = normalizedWorkflowTime(now)
	if requestID == "" || accountID == "" || executionOwnerID == "" || executionToken == "" || leaseDuration <= 0 {
		return FeishuDocxAppendOperation{}, false, fmt.Errorf("feishu docx append request, account, execution owner, token, and positive lease are required")
	}
	leaseExpiresAt := now.Add(leaseDuration)
	s.mu.Lock()
	result, err := s.db.Exec(
		`UPDATE feishu_docx_append_operations
		 SET state=?, remote_call_started_at_ms=?, execution_owner_id=?, execution_token=?,
		 execution_lease_expires_at_ms=?, last_error_category='', updated_at_ms=?
		 WHERE request_id=? AND account_id=? AND state=?
		 AND EXISTS (
		  SELECT 1 FROM feishu_account_runtime_leases AS runtime
		  WHERE runtime.account_id=feishu_docx_append_operations.account_id
		  AND runtime.owner_id=? AND runtime.lease_expires_at_ms>?
		 )`,
		FeishuDocxAppendOperationStateRemoteStarted,
		now.UnixMilli(),
		executionOwnerID,
		executionToken,
		leaseExpiresAt.UnixMilli(),
		now.UnixMilli(),
		requestID,
		accountID,
		FeishuDocxAppendOperationStatePrepared,
		executionOwnerID,
		now.UnixMilli(),
	)
	s.mu.Unlock()
	if err != nil {
		return FeishuDocxAppendOperation{}, false, fmt.Errorf("start feishu docx append operation: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return FeishuDocxAppendOperation{}, false, fmt.Errorf("inspect feishu docx append start: %w", err)
	}
	operation, err := s.GetFeishuDocxAppendOperation(requestID, accountID)
	if err != nil {
		return FeishuDocxAppendOperation{}, false, err
	}
	if count == 1 && (operation.ExecutionOwnerID != executionOwnerID || operation.ExecutionToken != executionToken) {
		return operation, false, ErrFeishuDocxAppendOperationNotReady
	}
	return operation, count == 1, nil
}

// ClaimFeishuDocxAppendOperationRecovery grants one recovery caller a bounded
// execution lease. Taking over remote_started first promotes it to
// outcome_unknown because the previous owner may already have reached Feishu.
func (s *Store) ClaimFeishuDocxAppendOperationRecovery(requestID, accountID, executionOwnerID, executionToken string, now time.Time, leaseDuration time.Duration) (FeishuDocxAppendOperation, bool, error) {
	if err := s.requireFeishuDocsStore(); err != nil {
		return FeishuDocxAppendOperation{}, false, err
	}
	requestID = strings.TrimSpace(requestID)
	accountID = strings.TrimSpace(accountID)
	executionOwnerID = strings.TrimSpace(executionOwnerID)
	executionToken = strings.TrimSpace(executionToken)
	now = normalizedWorkflowTime(now)
	if requestID == "" || accountID == "" || executionOwnerID == "" || executionToken == "" || leaseDuration <= 0 {
		return FeishuDocxAppendOperation{}, false, fmt.Errorf("feishu docx append recovery request, account, execution owner, token, and positive lease are required")
	}
	leaseExpiresAt := now.Add(leaseDuration)
	s.mu.Lock()
	result, err := s.db.Exec(
		`UPDATE feishu_docx_append_operations
		 SET state=?, execution_owner_id=?, execution_token=?, execution_lease_expires_at_ms=?,
		 last_error_category=CASE WHEN state=? AND last_error_category='' THEN 'interrupted_append_attempt' ELSE last_error_category END,
		 updated_at_ms=?
		 WHERE request_id=? AND account_id=? AND state IN (?, ?)
		 AND (execution_token='' OR execution_owner_id<>? OR execution_lease_expires_at_ms<=?)
		 AND EXISTS (
		  SELECT 1 FROM feishu_account_runtime_leases AS runtime
		  WHERE runtime.account_id=feishu_docx_append_operations.account_id
		  AND runtime.owner_id=? AND runtime.lease_expires_at_ms>?
		 )`,
		FeishuDocxAppendOperationStateOutcomeUnknown,
		executionOwnerID,
		executionToken,
		leaseExpiresAt.UnixMilli(),
		FeishuDocxAppendOperationStateRemoteStarted,
		now.UnixMilli(),
		requestID,
		accountID,
		FeishuDocxAppendOperationStateRemoteStarted,
		FeishuDocxAppendOperationStateOutcomeUnknown,
		executionOwnerID,
		now.UnixMilli(),
		executionOwnerID,
		now.UnixMilli(),
	)
	s.mu.Unlock()
	if err != nil {
		return FeishuDocxAppendOperation{}, false, fmt.Errorf("claim feishu docx append recovery: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return FeishuDocxAppendOperation{}, false, fmt.Errorf("inspect feishu docx append recovery claim: %w", err)
	}
	operation, err := s.GetFeishuDocxAppendOperation(requestID, accountID)
	if err != nil {
		return FeishuDocxAppendOperation{}, false, err
	}
	if count == 1 && (operation.ExecutionOwnerID != executionOwnerID || operation.ExecutionToken != executionToken) {
		return operation, false, ErrFeishuDocxAppendOperationNotReady
	}
	return operation, count == 1, nil
}

func (s *Store) MarkFeishuDocxAppendOperationSucceeded(requestID, accountID, executionToken string, now time.Time) (FeishuDocxAppendOperation, error) {
	return s.completeFeishuDocxAppendOperation(requestID, accountID, executionToken, FeishuDocxAppendOperationStateSucceeded, "", now)
}

func (s *Store) MarkFeishuDocxAppendOperationFailed(requestID, accountID, executionToken, category string, now time.Time) (FeishuDocxAppendOperation, error) {
	return s.completeFeishuDocxAppendOperation(requestID, accountID, executionToken, FeishuDocxAppendOperationStateFailed, category, now)
}

func (s *Store) completeFeishuDocxAppendOperation(requestID, accountID, executionToken, state, category string, now time.Time) (FeishuDocxAppendOperation, error) {
	if err := s.requireFeishuDocsStore(); err != nil {
		return FeishuDocxAppendOperation{}, err
	}
	requestID = strings.TrimSpace(requestID)
	accountID = strings.TrimSpace(accountID)
	executionToken = strings.TrimSpace(executionToken)
	category = truncateFeishuDocxAppendCategory(category)
	now = normalizedWorkflowTime(now)
	if requestID == "" || accountID == "" || executionToken == "" || (state != FeishuDocxAppendOperationStateSucceeded && state != FeishuDocxAppendOperationStateFailed) {
		return FeishuDocxAppendOperation{}, fmt.Errorf("feishu docx append request, account, execution token, and terminal state are required")
	}
	statePredicate := `state=?`
	stateArgs := []any{FeishuDocxAppendOperationStateRemoteStarted}
	if state == FeishuDocxAppendOperationStateSucceeded {
		statePredicate = `state IN (?, ?)`
		stateArgs = []any{FeishuDocxAppendOperationStateRemoteStarted, FeishuDocxAppendOperationStateOutcomeUnknown}
	}
	query := `UPDATE feishu_docx_append_operations
		 SET state=?, envelope_ciphertext='', remote_result_at_ms=?, execution_owner_id='',
		 execution_token='', execution_lease_expires_at_ms=0, last_error_category=?, updated_at_ms=?
		 WHERE request_id=? AND account_id=? AND execution_token=? AND ` + statePredicate
	args := []any{state, now.UnixMilli(), category, now.UnixMilli(), requestID, accountID, executionToken}
	args = append(args, stateArgs...)
	s.mu.Lock()
	result, err := s.db.Exec(query, args...)
	s.mu.Unlock()
	if err != nil {
		return FeishuDocxAppendOperation{}, fmt.Errorf("complete feishu docx append operation: %w", err)
	}
	operation, loadErr := s.GetFeishuDocxAppendOperation(requestID, accountID)
	if loadErr != nil {
		return FeishuDocxAppendOperation{}, loadErr
	}
	count, countErr := result.RowsAffected()
	if countErr != nil {
		return FeishuDocxAppendOperation{}, fmt.Errorf("inspect feishu docx append completion: %w", countErr)
	}
	if count == 0 && operation.State != state {
		return operation, ErrFeishuDocxAppendOperationNotReady
	}
	return operation, nil
}

func (s *Store) MarkFeishuDocxAppendOperationOutcomeUnknown(requestID, accountID, executionToken, category string, now time.Time) (FeishuDocxAppendOperation, error) {
	if err := s.requireFeishuDocsStore(); err != nil {
		return FeishuDocxAppendOperation{}, err
	}
	requestID = strings.TrimSpace(requestID)
	accountID = strings.TrimSpace(accountID)
	executionToken = strings.TrimSpace(executionToken)
	category = truncateFeishuDocxAppendCategory(category)
	now = normalizedWorkflowTime(now)
	if requestID == "" || accountID == "" || executionToken == "" {
		return FeishuDocxAppendOperation{}, fmt.Errorf("feishu docx append request, account, and execution token are required")
	}
	s.mu.Lock()
	result, err := s.db.Exec(
		`UPDATE feishu_docx_append_operations
		 SET state=?, execution_owner_id='', execution_token='', execution_lease_expires_at_ms=0,
		 last_error_category=?, updated_at_ms=?
		 WHERE request_id=? AND account_id=? AND execution_token=? AND state IN (?, ?)`,
		FeishuDocxAppendOperationStateOutcomeUnknown,
		category,
		now.UnixMilli(),
		requestID,
		accountID,
		executionToken,
		FeishuDocxAppendOperationStateRemoteStarted,
		FeishuDocxAppendOperationStateOutcomeUnknown,
	)
	s.mu.Unlock()
	if err != nil {
		return FeishuDocxAppendOperation{}, fmt.Errorf("mark feishu docx append outcome unknown: %w", err)
	}
	operation, loadErr := s.GetFeishuDocxAppendOperation(requestID, accountID)
	if loadErr != nil {
		return FeishuDocxAppendOperation{}, loadErr
	}
	count, countErr := result.RowsAffected()
	if countErr != nil {
		return FeishuDocxAppendOperation{}, fmt.Errorf("inspect feishu docx append outcome unknown: %w", countErr)
	}
	if count == 0 && !(operation.State == FeishuDocxAppendOperationStateOutcomeUnknown && operation.ExecutionToken == "") {
		return operation, ErrFeishuDocxAppendOperationNotReady
	}
	return operation, nil
}

func (s *Store) GetFeishuDocxAppendOperation(requestID, accountID string) (FeishuDocxAppendOperation, error) {
	if err := s.requireFeishuDocsStore(); err != nil {
		return FeishuDocxAppendOperation{}, err
	}
	return scanFeishuDocxAppendOperation(s.db.QueryRow(
		feishuDocxAppendOperationSelect+` WHERE request_id=? AND account_id=?`,
		strings.TrimSpace(requestID),
		strings.TrimSpace(accountID),
	))
}

func (s *Store) ListRecoverableFeishuDocxAppendOperations(accountID string, limit int) ([]FeishuDocxAppendOperation, error) {
	return s.ListRecoverableFeishuDocxAppendOperationsAfter(accountID, time.Time{}, "", limit)
}

func (s *Store) ListRecoverableFeishuDocxAppendOperationsAfter(accountID string, afterCreatedAt time.Time, afterRequestID string, limit int) ([]FeishuDocxAppendOperation, error) {
	if err := s.requireFeishuDocsStore(); err != nil {
		return nil, err
	}
	accountID = strings.TrimSpace(accountID)
	afterRequestID = strings.TrimSpace(afterRequestID)
	if accountID == "" {
		return nil, fmt.Errorf("feishu docx append account is required")
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	afterCreatedAtMS := int64(0)
	if !afterCreatedAt.IsZero() {
		afterCreatedAtMS = afterCreatedAt.UTC().UnixMilli()
	}
	rows, err := s.db.Query(
		feishuDocxAppendOperationSelect+` WHERE account_id=? AND (
		  state IN (?, ?, ?)
		  OR EXISTS (
		    SELECT 1 FROM workflow_requests AS workflow
		    WHERE workflow.id=feishu_docx_append_operations.request_id
		    AND workflow.account_id=feishu_docx_append_operations.account_id
		    AND (
		     (feishu_docx_append_operations.state=? AND workflow.kind IN (?, ?) AND workflow.state<>?)
		     OR (feishu_docx_append_operations.state=? AND workflow.kind=? AND workflow.state<>?)
		     OR (feishu_docx_append_operations.state=? AND workflow.kind=? AND workflow.state<>?)
		    )
		   )
		 )
		 AND (created_at_ms>? OR (created_at_ms=? AND request_id>?))
		 ORDER BY created_at_ms, request_id LIMIT ?`,
		accountID,
		FeishuDocxAppendOperationStatePrepared,
		FeishuDocxAppendOperationStateRemoteStarted,
		FeishuDocxAppendOperationStateOutcomeUnknown,
		FeishuDocxAppendOperationStateSucceeded,
		WorkflowRequestKindFeishuDocsAppend,
		WorkflowRequestKindFeishuDocsCreate,
		WorkflowRequestStateSucceeded,
		FeishuDocxAppendOperationStateFailed,
		WorkflowRequestKindFeishuDocsAppend,
		WorkflowRequestStateFailed,
		FeishuDocxAppendOperationStateFailed,
		WorkflowRequestKindFeishuDocsCreate,
		WorkflowRequestStatePartial,
		afterCreatedAtMS,
		afterCreatedAtMS,
		afterRequestID,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list recoverable feishu docx append operations: %w", err)
	}
	defer rows.Close()
	var operations []FeishuDocxAppendOperation
	for rows.Next() {
		operation, scanErr := scanFeishuDocxAppendOperation(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		operations = append(operations, operation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate recoverable feishu docx append operations: %w", err)
	}
	return operations, nil
}

// ReconcileFeishuDocxAppendWorkflowState atomically derives a direct append or
// document-create workflow state from the current append ledger. Keeping the
// decision and workflow write in one SQLite statement prevents a stale
// recovery reader from downgrading a concurrently completed append.
//
// Workflows without an append ledger and tool-approval workflows are returned
// unchanged with reconciled=false. Their legacy/no-ledger interpretation and
// terminal ownership remain with the higher-level create/approval services.
func (s *Store) ReconcileFeishuDocxAppendWorkflowState(requestID, accountID string, now time.Time) (WorkflowRequest, bool, error) {
	if err := s.requireFeishuDocsStore(); err != nil {
		return WorkflowRequest{}, false, err
	}
	requestID = strings.TrimSpace(requestID)
	accountID = strings.TrimSpace(accountID)
	now = normalizedWorkflowTime(now)
	if requestID == "" || accountID == "" {
		return WorkflowRequest{}, false, fmt.Errorf("feishu docx append workflow request and account are required")
	}
	s.mu.Lock()
	query := `UPDATE workflow_requests
		 SET state=` + feishuDocxAppendWorkflowStateExpression + `,
		  updated_at_ms=?
		 WHERE id=? AND account_id=? AND kind IN (?, ?)
		   AND EXISTS (
		    SELECT 1 FROM feishu_docx_append_operations AS append
		    WHERE append.request_id=workflow_requests.id
		      AND append.account_id=workflow_requests.account_id
		   )
		 RETURNING id, account_id, kind, state, created_at_ms, updated_at_ms`
	args := feishuDocxAppendWorkflowStateArgs(WorkflowRequestStatePartial)
	args = append(args,
		now.UnixMilli(),
		requestID,
		accountID,
		WorkflowRequestKindFeishuDocsAppend,
		WorkflowRequestKindFeishuDocsCreate,
	)
	workflow, err := scanWorkflowRequest(s.db.QueryRow(query, args...))
	s.mu.Unlock()
	if err == nil {
		return workflow, true, nil
	}
	if !errors.Is(err, ErrWorkflowRequestNotFound) {
		return WorkflowRequest{}, false, fmt.Errorf("reconcile feishu docx append workflow: %w", err)
	}
	workflow, err = s.GetWorkflowRequest(requestID, accountID)
	if err != nil {
		return WorkflowRequest{}, false, err
	}
	return workflow, false, nil
}

func feishuDocxAppendWorkflowStateArgs(fallbackState string) []any {
	return []any{
		WorkflowRequestKindFeishuDocsAppend,
		WorkflowRequestKindFeishuDocsCreate,
		FeishuDocxAppendOperationStateSucceeded,
		WorkflowRequestStateSucceeded,
		WorkflowRequestKindFeishuDocsAppend,
		FeishuDocxAppendOperationStateFailed,
		WorkflowRequestStateFailed,
		WorkflowRequestKindFeishuDocsAppend,
		WorkflowRequestKindFeishuDocsCreate,
		WorkflowRequestStatePartial,
		fallbackState,
	}
}

type feishuDocxAppendOperationQueryer interface {
	QueryRow(query string, args ...any) *sql.Row
}

func feishuDocxAppendOperationByID(queryer feishuDocxAppendOperationQueryer, requestID, accountID string) (FeishuDocxAppendOperation, error) {
	return scanFeishuDocxAppendOperation(queryer.QueryRow(
		feishuDocxAppendOperationSelect+` WHERE request_id=? AND account_id=?`,
		requestID,
		accountID,
	))
}

type feishuDocxAppendOperationScanner interface {
	Scan(dest ...any) error
}

func scanFeishuDocxAppendOperation(row feishuDocxAppendOperationScanner) (FeishuDocxAppendOperation, error) {
	var operation FeishuDocxAppendOperation
	var remoteStartedMS, remoteResultMS, executionLeaseMS, createdAtMS, updatedAtMS int64
	if err := row.Scan(
		&operation.RequestID,
		&operation.AccountID,
		&operation.ChatID,
		&operation.ActorOpenID,
		&operation.ActorUserID,
		&operation.DocumentToken,
		&operation.ClientToken,
		&operation.InsertionIndex,
		&operation.PayloadHash,
		&operation.EnvelopeHash,
		&operation.EnvelopeCiphertext,
		&operation.State,
		&remoteStartedMS,
		&remoteResultMS,
		&operation.ExecutionOwnerID,
		&operation.ExecutionToken,
		&executionLeaseMS,
		&operation.LastErrorCategory,
		&createdAtMS,
		&updatedAtMS,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return FeishuDocxAppendOperation{}, ErrFeishuDocxAppendOperationNotFound
		}
		return FeishuDocxAppendOperation{}, fmt.Errorf("get feishu docx append operation: %w", err)
	}
	operation.RemoteCallStartedAt = feishuRemoteOperationTime(remoteStartedMS)
	operation.RemoteResultAt = feishuRemoteOperationTime(remoteResultMS)
	operation.ExecutionLeaseUntil = feishuRemoteOperationTime(executionLeaseMS)
	operation.CreatedAt = time.UnixMilli(createdAtMS).UTC()
	operation.UpdatedAt = time.UnixMilli(updatedAtMS).UTC()
	return operation, nil
}

func normalizeFeishuDocxAppendOperation(operation FeishuDocxAppendOperation) FeishuDocxAppendOperation {
	operation.RequestID = strings.TrimSpace(operation.RequestID)
	operation.AccountID = strings.TrimSpace(operation.AccountID)
	operation.ChatID = strings.TrimSpace(operation.ChatID)
	operation.ActorOpenID = strings.TrimSpace(operation.ActorOpenID)
	operation.ActorUserID = strings.TrimSpace(operation.ActorUserID)
	operation.DocumentToken = strings.TrimSpace(operation.DocumentToken)
	operation.ClientToken = strings.TrimSpace(operation.ClientToken)
	operation.PayloadHash = strings.TrimSpace(operation.PayloadHash)
	operation.EnvelopeHash = strings.TrimSpace(operation.EnvelopeHash)
	operation.EnvelopeCiphertext = strings.TrimSpace(operation.EnvelopeCiphertext)
	operation.CreatedAt = normalizedWorkflowTime(operation.CreatedAt)
	operation.UpdatedAt = operation.CreatedAt
	operation.State = FeishuDocxAppendOperationStatePrepared
	operation.RemoteCallStartedAt = time.Time{}
	operation.RemoteResultAt = time.Time{}
	operation.ExecutionOwnerID = ""
	operation.ExecutionToken = ""
	operation.ExecutionLeaseUntil = time.Time{}
	operation.LastErrorCategory = ""
	return operation
}

func validatePreparedFeishuDocxAppendOperation(operation FeishuDocxAppendOperation) error {
	if operation.RequestID == "" || operation.AccountID == "" || operation.ChatID == "" ||
		(operation.ActorOpenID == "" && operation.ActorUserID == "") || operation.DocumentToken == "" ||
		operation.ClientToken == "" || operation.InsertionIndex < 0 || operation.PayloadHash == "" ||
		operation.EnvelopeHash == "" || operation.EnvelopeCiphertext == "" {
		return fmt.Errorf("feishu docx append request, account, trusted scope, document, frozen request, and protected envelope are required")
	}
	return nil
}

func workflowKindSupportsFeishuDocxAppend(kind string) bool {
	switch strings.TrimSpace(kind) {
	case WorkflowRequestKindFeishuDocsAppend,
		WorkflowRequestKindFeishuDocsCreate,
		WorkflowRequestKindToolApproval:
		return true
	default:
		return false
	}
}

func sameFeishuDocxAppendRequest(left, right FeishuDocxAppendOperation) bool {
	return left.RequestID == right.RequestID &&
		left.AccountID == right.AccountID &&
		left.ChatID == right.ChatID &&
		left.ActorOpenID == right.ActorOpenID &&
		left.ActorUserID == right.ActorUserID &&
		left.DocumentToken == right.DocumentToken &&
		left.ClientToken == right.ClientToken &&
		left.PayloadHash == right.PayloadHash
}

func truncateFeishuDocxAppendCategory(category string) string {
	category = strings.TrimSpace(category)
	if len(category) <= 128 {
		return category
	}
	return category[:128]
}
