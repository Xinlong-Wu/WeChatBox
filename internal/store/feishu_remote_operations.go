package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrFeishuRemoteOperationNotFound = errors.New("feishu remote operation not found")
	ErrFeishuRemoteOperationConflict = errors.New("feishu remote operation conflicts with the persisted request")
	ErrFeishuRemoteOperationNotReady = errors.New("feishu remote operation is not ready for this transition")
)

const (
	FeishuRemoteOperationKindDocumentCreate = WorkflowRequestKindFeishuDocsCreate
	FeishuRemoteOperationKindFolderCreate   = WorkflowRequestKindFeishuFolderCreate

	FeishuRemoteOperationStatePrepared          = "prepared"
	FeishuRemoteOperationStateRemoteStarted     = "remote_started"
	FeishuRemoteOperationStateRemoteSucceeded   = "remote_succeeded"
	FeishuRemoteOperationStatePersisted         = "persisted"
	FeishuRemoteOperationStateReconcileRequired = "reconcile_required"
	FeishuRemoteOperationStateOutcomeUnknown    = "outcome_unknown"
	FeishuRemoteOperationStateFailed            = "failed"
)

const feishuRemoteOperationSelect = `SELECT
 request_id, account_id, operation_kind, chat_id, actor_open_id, actor_user_id,
 parent_resource_type, parent_resource_token, binding_parent_token,
 requested_name, payload_hash, set_default, share_member_type, share_member_id,
 initial_content_requested, state, remote_resource_type, remote_resource_token,
 remote_url, remote_call_started_at_ms, remote_result_at_ms, last_error_category,
 created_at_ms, updated_at_ms
 FROM feishu_remote_operations`

// FeishuRemoteOperation is the durable local ledger for one non-idempotent
// Feishu create call. It stores only trusted routing and recovery metadata;
// document content is represented by PayloadHash and is never persisted here.
type FeishuRemoteOperation struct {
	RequestID               string
	AccountID               string
	OperationKind           string
	ChatID                  string
	ActorOpenID             string
	ActorUserID             string
	ParentResourceType      string
	ParentResourceToken     string
	BindingParentToken      string
	RequestedName           string
	PayloadHash             string
	SetDefault              bool
	ShareMemberType         string
	ShareMemberID           string
	InitialContentRequested bool
	State                   string
	RemoteResourceType      string
	RemoteResourceToken     string
	RemoteURL               string
	RemoteCallStartedAt     time.Time
	RemoteResultAt          time.Time
	LastErrorCategory       string
	CreatedAt               time.Time
	UpdatedAt               time.Time
}

// PrepareFeishuRemoteOperation creates the durable boundary that must exist
// before a Feishu create API can be called. Repeating the same immutable
// payload is idempotent; changing it for the same request ID fails closed.
func (s *Store) PrepareFeishuRemoteOperation(operation FeishuRemoteOperation) (FeishuRemoteOperation, error) {
	if err := s.requireFeishuDocsStore(); err != nil {
		return FeishuRemoteOperation{}, err
	}
	operation = normalizeFeishuRemoteOperation(operation)
	if err := validatePreparedFeishuRemoteOperation(operation); err != nil {
		return FeishuRemoteOperation{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return FeishuRemoteOperation{}, fmt.Errorf("begin prepare feishu remote operation: %w", err)
	}
	defer tx.Rollback()
	workflow, err := scanWorkflowRequest(tx.QueryRow(
		`SELECT id, account_id, kind, state, created_at_ms, updated_at_ms
		 FROM workflow_requests WHERE id=? AND account_id=?`,
		operation.RequestID,
		operation.AccountID,
	))
	if err != nil {
		return FeishuRemoteOperation{}, fmt.Errorf("load feishu remote operation workflow: %w", err)
	}
	if !workflowKindSupportsFeishuRemoteOperation(workflow.Kind, operation.OperationKind) {
		return FeishuRemoteOperation{}, fmt.Errorf("%w: workflow kind %q cannot own operation %q", ErrFeishuRemoteOperationConflict, workflow.Kind, operation.OperationKind)
	}
	_, err = tx.Exec(
		`INSERT INTO feishu_remote_operations (
		 request_id, account_id, operation_kind, chat_id, actor_open_id, actor_user_id,
		 parent_resource_type, parent_resource_token, binding_parent_token,
		 requested_name, payload_hash, set_default, share_member_type, share_member_id,
		 initial_content_requested, state, remote_resource_type, remote_resource_token,
		 remote_url, remote_call_started_at_ms, remote_result_at_ms, last_error_category,
		 created_at_ms, updated_at_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', '', 0, 0, '', ?, ?)`,
		operation.RequestID,
		operation.AccountID,
		operation.OperationKind,
		operation.ChatID,
		operation.ActorOpenID,
		operation.ActorUserID,
		operation.ParentResourceType,
		operation.ParentResourceToken,
		operation.BindingParentToken,
		operation.RequestedName,
		operation.PayloadHash,
		boolToInt(operation.SetDefault),
		operation.ShareMemberType,
		operation.ShareMemberID,
		boolToInt(operation.InitialContentRequested),
		FeishuRemoteOperationStatePrepared,
		operation.RemoteResourceType,
		operation.CreatedAt.UnixMilli(),
		operation.UpdatedAt.UnixMilli(),
	)
	if err != nil {
		if !strings.Contains(strings.ToLower(err.Error()), "unique constraint failed") {
			return FeishuRemoteOperation{}, fmt.Errorf("prepare feishu remote operation: %w", err)
		}
		existing, loadErr := feishuRemoteOperationByID(tx, operation.RequestID, operation.AccountID)
		if loadErr != nil {
			return FeishuRemoteOperation{}, loadErr
		}
		if !sameFeishuRemoteOperationRequest(existing, operation) {
			return FeishuRemoteOperation{}, ErrFeishuRemoteOperationConflict
		}
		return existing, nil
	}
	if err := tx.Commit(); err != nil {
		return FeishuRemoteOperation{}, fmt.Errorf("commit prepared feishu remote operation: %w", err)
	}
	operation.State = FeishuRemoteOperationStatePrepared
	return operation, nil
}

// StartFeishuRemoteOperation atomically grants exactly one caller permission
// to perform the first remote create call. No later state is startable.
func (s *Store) StartFeishuRemoteOperation(requestID, accountID string, now time.Time) (FeishuRemoteOperation, bool, error) {
	if err := s.requireFeishuDocsStore(); err != nil {
		return FeishuRemoteOperation{}, false, err
	}
	requestID = strings.TrimSpace(requestID)
	accountID = strings.TrimSpace(accountID)
	now = normalizedWorkflowTime(now)
	if requestID == "" || accountID == "" {
		return FeishuRemoteOperation{}, false, fmt.Errorf("feishu remote operation request and account are required")
	}
	s.mu.Lock()
	result, err := s.db.Exec(
		`UPDATE feishu_remote_operations
		 SET state=?, remote_call_started_at_ms=?, last_error_category='', updated_at_ms=?
		 WHERE request_id=? AND account_id=? AND state=?`,
		FeishuRemoteOperationStateRemoteStarted,
		now.UnixMilli(),
		now.UnixMilli(),
		requestID,
		accountID,
		FeishuRemoteOperationStatePrepared,
	)
	s.mu.Unlock()
	if err != nil {
		return FeishuRemoteOperation{}, false, fmt.Errorf("start feishu remote operation: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return FeishuRemoteOperation{}, false, fmt.Errorf("inspect feishu remote operation start: %w", err)
	}
	operation, err := s.GetFeishuRemoteOperation(requestID, accountID)
	if err != nil {
		return FeishuRemoteOperation{}, false, err
	}
	return operation, count == 1, nil
}

func (s *Store) MarkFeishuRemoteOperationReconcileRequired(requestID, accountID, category string, now time.Time) (FeishuRemoteOperation, error) {
	return s.transitionFeishuRemoteOperationUncertain(
		requestID,
		accountID,
		FeishuRemoteOperationStateReconcileRequired,
		category,
		now,
	)
}

func (s *Store) MarkFeishuRemoteOperationOutcomeUnknown(requestID, accountID, category string, now time.Time) (FeishuRemoteOperation, error) {
	return s.transitionFeishuRemoteOperationUncertain(
		requestID,
		accountID,
		FeishuRemoteOperationStateOutcomeUnknown,
		category,
		now,
	)
}

func (s *Store) transitionFeishuRemoteOperationUncertain(requestID, accountID, state, category string, now time.Time) (FeishuRemoteOperation, error) {
	if err := s.requireFeishuDocsStore(); err != nil {
		return FeishuRemoteOperation{}, err
	}
	requestID = strings.TrimSpace(requestID)
	accountID = strings.TrimSpace(accountID)
	category = truncateFeishuRemoteOperationCategory(category)
	now = normalizedWorkflowTime(now)
	if requestID == "" || accountID == "" || (state != FeishuRemoteOperationStateReconcileRequired && state != FeishuRemoteOperationStateOutcomeUnknown) {
		return FeishuRemoteOperation{}, fmt.Errorf("feishu remote operation request, account, and uncertain state are required")
	}
	s.mu.Lock()
	result, err := s.db.Exec(
		`UPDATE feishu_remote_operations
		 SET state=?, last_error_category=?, updated_at_ms=?
		 WHERE request_id=? AND account_id=? AND state IN (?, ?, ?)`,
		state,
		category,
		now.UnixMilli(),
		requestID,
		accountID,
		FeishuRemoteOperationStateRemoteStarted,
		FeishuRemoteOperationStateReconcileRequired,
		FeishuRemoteOperationStateOutcomeUnknown,
	)
	s.mu.Unlock()
	if err != nil {
		return FeishuRemoteOperation{}, fmt.Errorf("mark feishu remote operation %s: %w", state, err)
	}
	operation, loadErr := s.GetFeishuRemoteOperation(requestID, accountID)
	if loadErr != nil {
		return FeishuRemoteOperation{}, loadErr
	}
	count, countErr := result.RowsAffected()
	if countErr != nil {
		return FeishuRemoteOperation{}, fmt.Errorf("inspect feishu remote operation %s: %w", state, countErr)
	}
	if count == 0 && operation.State != FeishuRemoteOperationStateRemoteSucceeded && operation.State != FeishuRemoteOperationStatePersisted {
		return operation, ErrFeishuRemoteOperationNotReady
	}
	return operation, nil
}

func (s *Store) RecordFeishuRemoteOperationSuccess(requestID, accountID, resourceType, resourceToken, remoteURL string, now time.Time) (FeishuRemoteOperation, error) {
	if err := s.requireFeishuDocsStore(); err != nil {
		return FeishuRemoteOperation{}, err
	}
	requestID = strings.TrimSpace(requestID)
	accountID = strings.TrimSpace(accountID)
	resourceType = strings.TrimSpace(resourceType)
	resourceToken = strings.TrimSpace(resourceToken)
	remoteURL = strings.TrimSpace(remoteURL)
	now = normalizedWorkflowTime(now)
	if requestID == "" || accountID == "" || resourceType == "" || resourceToken == "" {
		return FeishuRemoteOperation{}, fmt.Errorf("feishu remote operation request, account, resource type, and resource token are required")
	}
	s.mu.Lock()
	result, err := s.db.Exec(
		`UPDATE feishu_remote_operations
		 SET state=?, remote_resource_type=?, remote_resource_token=?, remote_url=?,
		 remote_result_at_ms=?, last_error_category='', updated_at_ms=?
		 WHERE request_id=? AND account_id=? AND remote_resource_type=?
		 AND state IN (?, ?, ?)
		 AND NOT EXISTS (
			 SELECT 1 FROM feishu_remote_operations AS claimed
			 WHERE claimed.account_id=?
			 AND claimed.remote_resource_type=?
			 AND claimed.remote_resource_token=?
			 AND claimed.request_id<>?
		 )`,
		FeishuRemoteOperationStateRemoteSucceeded,
		resourceType,
		resourceToken,
		remoteURL,
		now.UnixMilli(),
		now.UnixMilli(),
		requestID,
		accountID,
		resourceType,
		FeishuRemoteOperationStateRemoteStarted,
		FeishuRemoteOperationStateReconcileRequired,
		FeishuRemoteOperationStateOutcomeUnknown,
		accountID,
		resourceType,
		resourceToken,
		requestID,
	)
	s.mu.Unlock()
	if err != nil {
		return FeishuRemoteOperation{}, fmt.Errorf("record feishu remote operation success: %w", err)
	}
	operation, loadErr := s.GetFeishuRemoteOperation(requestID, accountID)
	if loadErr != nil {
		return FeishuRemoteOperation{}, loadErr
	}
	count, countErr := result.RowsAffected()
	if countErr != nil {
		return FeishuRemoteOperation{}, fmt.Errorf("inspect feishu remote operation success: %w", countErr)
	}
	if count == 1 {
		return operation, nil
	}
	if (operation.State == FeishuRemoteOperationStateRemoteSucceeded || operation.State == FeishuRemoteOperationStatePersisted) &&
		operation.RemoteResourceType == resourceType && operation.RemoteResourceToken == resourceToken {
		return operation, nil
	}
	if operation.RemoteResourceToken != "" && operation.RemoteResourceToken != resourceToken {
		return operation, ErrFeishuRemoteOperationConflict
	}
	claimed, claimErr := s.feishuRemoteResourceClaimedByAnotherRequest(accountID, requestID, resourceType, resourceToken)
	if claimErr != nil {
		return operation, claimErr
	}
	if claimed {
		return operation, ErrFeishuRemoteOperationConflict
	}
	return operation, ErrFeishuRemoteOperationNotReady
}

func (s *Store) feishuRemoteResourceClaimedByAnotherRequest(accountID, requestID, resourceType, resourceToken string) (bool, error) {
	var claimed int
	if err := s.db.QueryRow(
		`SELECT EXISTS (
			 SELECT 1 FROM feishu_remote_operations
			 WHERE account_id=? AND remote_resource_type=? AND remote_resource_token=? AND request_id<>?
		 )`,
		accountID,
		resourceType,
		resourceToken,
		requestID,
	).Scan(&claimed); err != nil {
		return false, fmt.Errorf("inspect feishu remote operation resource claim: %w", err)
	}
	return claimed != 0, nil
}

// HasCompetingFeishuRemoteCreate reports whether another uncertain create call
// could have produced the same reconciliation candidate. The caller supplies
// the inclusive remote-call start window derived from the candidate timestamp.
func (s *Store) HasCompetingFeishuRemoteCreate(
	requestID, accountID, parentResourceType, parentResourceToken, requestedName, remoteResourceType string,
	startedAfter, startedBefore time.Time,
) (bool, error) {
	if err := s.requireFeishuDocsStore(); err != nil {
		return false, err
	}
	requestID = strings.TrimSpace(requestID)
	accountID = strings.TrimSpace(accountID)
	parentResourceType = strings.TrimSpace(parentResourceType)
	parentResourceToken = strings.TrimSpace(parentResourceToken)
	requestedName = strings.TrimSpace(requestedName)
	remoteResourceType = strings.TrimSpace(remoteResourceType)
	startedAfter = normalizedWorkflowTime(startedAfter)
	startedBefore = normalizedWorkflowTime(startedBefore)
	if requestID == "" || accountID == "" || parentResourceType == "" || parentResourceToken == "" ||
		requestedName == "" || remoteResourceType == "" || startedBefore.Before(startedAfter) {
		return false, fmt.Errorf("competing feishu remote create scope and valid start window are required")
	}
	var competing int
	if err := s.db.QueryRow(
		`SELECT EXISTS (
			 SELECT 1 FROM feishu_remote_operations
			 WHERE account_id=? AND request_id<>?
			   AND parent_resource_type=? AND parent_resource_token=?
			   AND requested_name=? AND remote_resource_type=?
			   AND remote_call_started_at_ms BETWEEN ? AND ?
			   AND state IN (?, ?, ?)
		 )`,
		accountID,
		requestID,
		parentResourceType,
		parentResourceToken,
		requestedName,
		remoteResourceType,
		startedAfter.UnixMilli(),
		startedBefore.UnixMilli(),
		FeishuRemoteOperationStateRemoteStarted,
		FeishuRemoteOperationStateReconcileRequired,
		FeishuRemoteOperationStateOutcomeUnknown,
	).Scan(&competing); err != nil {
		return false, fmt.Errorf("inspect competing feishu remote create: %w", err)
	}
	return competing != 0, nil
}

func (s *Store) MarkFeishuRemoteOperationPersisted(requestID, accountID string, now time.Time) (FeishuRemoteOperation, error) {
	if err := s.requireFeishuDocsStore(); err != nil {
		return FeishuRemoteOperation{}, err
	}
	requestID = strings.TrimSpace(requestID)
	accountID = strings.TrimSpace(accountID)
	now = normalizedWorkflowTime(now)
	if requestID == "" || accountID == "" {
		return FeishuRemoteOperation{}, fmt.Errorf("feishu remote operation request and account are required")
	}
	s.mu.Lock()
	result, err := s.db.Exec(
		`UPDATE feishu_remote_operations SET state=?, last_error_category='', updated_at_ms=?
		 WHERE request_id=? AND account_id=? AND state=?`,
		FeishuRemoteOperationStatePersisted,
		now.UnixMilli(),
		requestID,
		accountID,
		FeishuRemoteOperationStateRemoteSucceeded,
	)
	s.mu.Unlock()
	if err != nil {
		return FeishuRemoteOperation{}, fmt.Errorf("mark feishu remote operation persisted: %w", err)
	}
	operation, loadErr := s.GetFeishuRemoteOperation(requestID, accountID)
	if loadErr != nil {
		return FeishuRemoteOperation{}, loadErr
	}
	count, countErr := result.RowsAffected()
	if countErr != nil {
		return FeishuRemoteOperation{}, fmt.Errorf("inspect persisted feishu remote operation: %w", countErr)
	}
	if count == 1 || operation.State == FeishuRemoteOperationStatePersisted {
		return operation, nil
	}
	return operation, ErrFeishuRemoteOperationNotReady
}

func (s *Store) FailFeishuRemoteOperation(requestID, accountID, category string, now time.Time) (FeishuRemoteOperation, error) {
	if err := s.requireFeishuDocsStore(); err != nil {
		return FeishuRemoteOperation{}, err
	}
	requestID = strings.TrimSpace(requestID)
	accountID = strings.TrimSpace(accountID)
	category = truncateFeishuRemoteOperationCategory(category)
	now = normalizedWorkflowTime(now)
	if requestID == "" || accountID == "" {
		return FeishuRemoteOperation{}, fmt.Errorf("feishu remote operation request and account are required")
	}
	s.mu.Lock()
	result, err := s.db.Exec(
		`UPDATE feishu_remote_operations SET state=?, last_error_category=?, updated_at_ms=?
		 WHERE request_id=? AND account_id=? AND state IN (?, ?)`,
		FeishuRemoteOperationStateFailed,
		category,
		now.UnixMilli(),
		requestID,
		accountID,
		FeishuRemoteOperationStatePrepared,
		FeishuRemoteOperationStateRemoteStarted,
	)
	s.mu.Unlock()
	if err != nil {
		return FeishuRemoteOperation{}, fmt.Errorf("fail feishu remote operation: %w", err)
	}
	operation, loadErr := s.GetFeishuRemoteOperation(requestID, accountID)
	if loadErr != nil {
		return FeishuRemoteOperation{}, loadErr
	}
	count, countErr := result.RowsAffected()
	if countErr != nil {
		return FeishuRemoteOperation{}, fmt.Errorf("inspect failed feishu remote operation: %w", countErr)
	}
	if count == 1 || operation.State == FeishuRemoteOperationStateFailed {
		return operation, nil
	}
	return operation, ErrFeishuRemoteOperationNotReady
}

func (s *Store) GetFeishuRemoteOperation(requestID, accountID string) (FeishuRemoteOperation, error) {
	if err := s.requireFeishuDocsStore(); err != nil {
		return FeishuRemoteOperation{}, err
	}
	return scanFeishuRemoteOperation(s.db.QueryRow(
		feishuRemoteOperationSelect+` WHERE request_id=? AND account_id=?`,
		strings.TrimSpace(requestID),
		strings.TrimSpace(accountID),
	))
}

type feishuRemoteOperationQueryer interface {
	QueryRow(query string, args ...any) *sql.Row
}

func feishuRemoteOperationByID(queryer feishuRemoteOperationQueryer, requestID, accountID string) (FeishuRemoteOperation, error) {
	return scanFeishuRemoteOperation(queryer.QueryRow(
		feishuRemoteOperationSelect+` WHERE request_id=? AND account_id=?`,
		requestID,
		accountID,
	))
}

type feishuRemoteOperationScanner interface {
	Scan(dest ...any) error
}

func scanFeishuRemoteOperation(row feishuRemoteOperationScanner) (FeishuRemoteOperation, error) {
	var operation FeishuRemoteOperation
	var setDefault, initialContent int
	var remoteStartedMS, remoteResultMS, createdAtMS, updatedAtMS int64
	if err := row.Scan(
		&operation.RequestID,
		&operation.AccountID,
		&operation.OperationKind,
		&operation.ChatID,
		&operation.ActorOpenID,
		&operation.ActorUserID,
		&operation.ParentResourceType,
		&operation.ParentResourceToken,
		&operation.BindingParentToken,
		&operation.RequestedName,
		&operation.PayloadHash,
		&setDefault,
		&operation.ShareMemberType,
		&operation.ShareMemberID,
		&initialContent,
		&operation.State,
		&operation.RemoteResourceType,
		&operation.RemoteResourceToken,
		&operation.RemoteURL,
		&remoteStartedMS,
		&remoteResultMS,
		&operation.LastErrorCategory,
		&createdAtMS,
		&updatedAtMS,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return FeishuRemoteOperation{}, ErrFeishuRemoteOperationNotFound
		}
		return FeishuRemoteOperation{}, fmt.Errorf("get feishu remote operation: %w", err)
	}
	operation.SetDefault = setDefault != 0
	operation.InitialContentRequested = initialContent != 0
	operation.RemoteCallStartedAt = feishuRemoteOperationTime(remoteStartedMS)
	operation.RemoteResultAt = feishuRemoteOperationTime(remoteResultMS)
	operation.CreatedAt = time.UnixMilli(createdAtMS).UTC()
	operation.UpdatedAt = time.UnixMilli(updatedAtMS).UTC()
	return operation, nil
}

func normalizeFeishuRemoteOperation(operation FeishuRemoteOperation) FeishuRemoteOperation {
	operation.RequestID = strings.TrimSpace(operation.RequestID)
	operation.AccountID = strings.TrimSpace(operation.AccountID)
	operation.OperationKind = strings.TrimSpace(operation.OperationKind)
	operation.ChatID = strings.TrimSpace(operation.ChatID)
	operation.ActorOpenID = strings.TrimSpace(operation.ActorOpenID)
	operation.ActorUserID = strings.TrimSpace(operation.ActorUserID)
	operation.ParentResourceType = strings.TrimSpace(operation.ParentResourceType)
	operation.ParentResourceToken = strings.TrimSpace(operation.ParentResourceToken)
	operation.BindingParentToken = strings.TrimSpace(operation.BindingParentToken)
	operation.RequestedName = strings.TrimSpace(operation.RequestedName)
	operation.PayloadHash = strings.TrimSpace(operation.PayloadHash)
	operation.ShareMemberType = strings.TrimSpace(operation.ShareMemberType)
	operation.ShareMemberID = strings.TrimSpace(operation.ShareMemberID)
	operation.RemoteResourceType = strings.TrimSpace(operation.RemoteResourceType)
	operation.CreatedAt = normalizedWorkflowTime(operation.CreatedAt)
	operation.UpdatedAt = operation.CreatedAt
	operation.State = FeishuRemoteOperationStatePrepared
	operation.RemoteResourceToken = ""
	operation.RemoteURL = ""
	operation.RemoteCallStartedAt = time.Time{}
	operation.RemoteResultAt = time.Time{}
	operation.LastErrorCategory = ""
	return operation
}

func validatePreparedFeishuRemoteOperation(operation FeishuRemoteOperation) error {
	if operation.RequestID == "" || operation.AccountID == "" || operation.ChatID == "" ||
		(operation.ActorOpenID == "" && operation.ActorUserID == "") ||
		operation.ParentResourceType == "" || operation.ParentResourceToken == "" ||
		operation.RequestedName == "" || operation.PayloadHash == "" || operation.RemoteResourceType == "" {
		return fmt.Errorf("feishu remote operation request, account, trusted scope, parent, name, payload hash, and remote type are required")
	}
	switch operation.OperationKind {
	case FeishuRemoteOperationKindDocumentCreate:
		if operation.RemoteResourceType != "docx" || operation.BindingParentToken == "" {
			return fmt.Errorf("feishu document remote operation requires docx type and binding parent")
		}
	case FeishuRemoteOperationKindFolderCreate:
		if operation.RemoteResourceType != "folder" || operation.ShareMemberType == "" || operation.ShareMemberID == "" {
			return fmt.Errorf("feishu folder remote operation requires folder type and share target")
		}
	default:
		return fmt.Errorf("unsupported feishu remote operation kind %q", operation.OperationKind)
	}
	return nil
}

func workflowKindSupportsFeishuRemoteOperation(workflowKind, operationKind string) bool {
	if workflowKind == operationKind {
		return true
	}
	return operationKind == FeishuRemoteOperationKindDocumentCreate && workflowKind == WorkflowRequestKindToolApproval
}

func sameFeishuRemoteOperationRequest(left, right FeishuRemoteOperation) bool {
	return left.RequestID == right.RequestID &&
		left.AccountID == right.AccountID &&
		left.OperationKind == right.OperationKind &&
		left.ChatID == right.ChatID &&
		left.ActorOpenID == right.ActorOpenID &&
		left.ActorUserID == right.ActorUserID &&
		left.ParentResourceType == right.ParentResourceType &&
		left.ParentResourceToken == right.ParentResourceToken &&
		left.BindingParentToken == right.BindingParentToken &&
		left.RequestedName == right.RequestedName &&
		left.PayloadHash == right.PayloadHash &&
		left.SetDefault == right.SetDefault &&
		left.ShareMemberType == right.ShareMemberType &&
		left.ShareMemberID == right.ShareMemberID &&
		left.InitialContentRequested == right.InitialContentRequested &&
		left.RemoteResourceType == right.RemoteResourceType
}

func validFeishuRemoteOperationState(state string) bool {
	switch state {
	case FeishuRemoteOperationStatePrepared,
		FeishuRemoteOperationStateRemoteStarted,
		FeishuRemoteOperationStateRemoteSucceeded,
		FeishuRemoteOperationStatePersisted,
		FeishuRemoteOperationStateReconcileRequired,
		FeishuRemoteOperationStateOutcomeUnknown,
		FeishuRemoteOperationStateFailed:
		return true
	default:
		return false
	}
}

func truncateFeishuRemoteOperationCategory(category string) string {
	category = strings.TrimSpace(category)
	if len(category) <= 128 {
		return category
	}
	return category[:128]
}

func feishuRemoteOperationTime(milliseconds int64) time.Time {
	if milliseconds <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(milliseconds).UTC()
}
