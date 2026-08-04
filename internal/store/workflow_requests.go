package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	// ErrWorkflowRequestNotFound is returned when a root request ID is unknown.
	ErrWorkflowRequestNotFound = errors.New("workflow request not found")
	// ErrWorkflowRequestExists is returned when a root request ID is already in use.
	ErrWorkflowRequestExists = errors.New("workflow request already exists")
)

const (
	WorkflowRequestKindToolApproval         = "tool_approval"
	WorkflowRequestKindFeishuFolderCreate   = "feishu_docs_folder_create"
	WorkflowRequestKindFeishuDocsCreate     = "feishu_docs_create"
	WorkflowRequestKindFeishuDocsAppend     = "feishu_docs_append"
	WorkflowRequestKindFeishuResourceAccess = "feishu_docs_resource_access"

	WorkflowRequestStatePending   = "pending"
	WorkflowRequestStateExecuting = "executing"
	WorkflowRequestStateDenied    = "denied"
	WorkflowRequestStateSucceeded = "succeeded"
	WorkflowRequestStatePartial   = "partial"
	WorkflowRequestStateFailed    = "failed"
	WorkflowRequestStateExpired   = "expired"
)

// WorkflowRequest is the globally unique root of one user-visible workflow.
// Approval, execution, retries, card updates, and result notifications all use
// this same ID.
type WorkflowRequest struct {
	ID        string
	AccountID string
	Kind      string
	State     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// WorkflowCardReference identifies the original Feishu card attached to a
// user-visible workflow request. CardMessageID is empty for workflows that do
// not have a card or whose card was never bound successfully.
type WorkflowCardReference struct {
	RequestID     string
	AccountID     string
	Kind          string
	CardMessageID string
}

// CreateWorkflowRequest allocates and persists one globally unique root request.
func (s *Store) CreateWorkflowRequest(request WorkflowRequest) (WorkflowRequest, error) {
	request, err := prepareWorkflowRequest(request)
	if err != nil {
		return WorkflowRequest{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := insertWorkflowRequest(s.db, request); err != nil {
		return WorkflowRequest{}, err
	}
	return request, nil
}

// UpdateWorkflowRequestState updates the lifecycle state of one root request.
func (s *Store) UpdateWorkflowRequestState(id, accountID, state string, now time.Time) error {
	id = strings.TrimSpace(id)
	accountID = strings.TrimSpace(accountID)
	state = strings.TrimSpace(state)
	if id == "" || accountID == "" || !validWorkflowRequestState(state) {
		return fmt.Errorf("workflow request id, account_id, and valid state are required")
	}
	now = normalizedWorkflowTime(now)
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.db.Exec(
		`UPDATE workflow_requests SET state=?, updated_at_ms=? WHERE id=? AND account_id=?`,
		state, now.UnixMilli(), id, accountID,
	)
	if err != nil {
		return fmt.Errorf("update workflow request: %w", err)
	}
	return requireOneWorkflowRequestRow(result)
}

// GetWorkflowRequest returns one root request for execution, diagnostics, and tests.
func (s *Store) GetWorkflowRequest(id, accountID string) (WorkflowRequest, error) {
	return scanWorkflowRequest(s.db.QueryRow(
		`SELECT id, account_id, kind, state, created_at_ms, updated_at_ms
		 FROM workflow_requests WHERE id=? AND account_id=?`,
		strings.TrimSpace(id), strings.TrimSpace(accountID),
	))
}

// GetWorkflowCardReference resolves the original card message for either a
// tool approval or a Feishu resource-access workflow without exposing the
// workflow-specific persistence model to asynchronous workers.
func (s *Store) GetWorkflowCardReference(id, accountID string) (WorkflowCardReference, error) {
	id = strings.TrimSpace(id)
	accountID = strings.TrimSpace(accountID)
	var reference WorkflowCardReference
	err := s.db.QueryRow(
		`SELECT workflow.id, workflow.account_id, workflow.kind,
		 COALESCE(approval.card_message_id, access.card_message_id, '')
		 FROM workflow_requests AS workflow
		 LEFT JOIN tool_approvals AS approval
		   ON approval.id=workflow.id AND approval.account_id=workflow.account_id
		 LEFT JOIN feishu_resource_access_requests AS access
		   ON access.id=workflow.id AND access.account_id=workflow.account_id
		 WHERE workflow.id=? AND workflow.account_id=?`,
		id,
		accountID,
	).Scan(&reference.RequestID, &reference.AccountID, &reference.Kind, &reference.CardMessageID)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkflowCardReference{}, ErrWorkflowRequestNotFound
	}
	if err != nil {
		return WorkflowCardReference{}, fmt.Errorf("get workflow card reference: %w", err)
	}
	reference.CardMessageID = strings.TrimSpace(reference.CardMessageID)
	return reference, nil
}

func prepareWorkflowRequest(request WorkflowRequest) (WorkflowRequest, error) {
	request.ID = strings.TrimSpace(request.ID)
	request.AccountID = strings.TrimSpace(request.AccountID)
	request.Kind = strings.TrimSpace(request.Kind)
	request.State = strings.TrimSpace(request.State)
	request.CreatedAt = normalizedWorkflowTime(request.CreatedAt)
	request.UpdatedAt = request.CreatedAt
	if request.ID == "" {
		id, err := generateID()
		if err != nil {
			return WorkflowRequest{}, err
		}
		request.ID = "req_" + id
	}
	if request.AccountID == "" || request.Kind == "" || !validWorkflowRequestState(request.State) {
		return WorkflowRequest{}, fmt.Errorf("workflow request id, account_id, kind, and valid state are required")
	}
	return request, nil
}

type workflowRequestExecer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func insertWorkflowRequest(execer workflowRequestExecer, request WorkflowRequest) error {
	_, err := execer.Exec(
		`INSERT INTO workflow_requests (id, account_id, kind, state, created_at_ms, updated_at_ms)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		request.ID,
		request.AccountID,
		request.Kind,
		request.State,
		request.CreatedAt.UnixMilli(),
		request.UpdatedAt.UnixMilli(),
	)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique constraint failed") {
			return fmt.Errorf("%w: %s", ErrWorkflowRequestExists, request.ID)
		}
		return fmt.Errorf("create workflow request: %w", err)
	}
	return nil
}

func updateWorkflowRequestState(execer workflowRequestExecer, id, accountID, state string, now time.Time) error {
	result, err := execer.Exec(
		`UPDATE workflow_requests SET state=?, updated_at_ms=? WHERE id=? AND account_id=?`,
		state, now.UnixMilli(), id, accountID,
	)
	if err != nil {
		return fmt.Errorf("update workflow request: %w", err)
	}
	return requireOneWorkflowRequestRow(result)
}

type workflowRequestScanner interface {
	Scan(dest ...any) error
}

func scanWorkflowRequest(row workflowRequestScanner) (WorkflowRequest, error) {
	var request WorkflowRequest
	var createdAtMS, updatedAtMS int64
	if err := row.Scan(&request.ID, &request.AccountID, &request.Kind, &request.State, &createdAtMS, &updatedAtMS); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return WorkflowRequest{}, ErrWorkflowRequestNotFound
		}
		return WorkflowRequest{}, fmt.Errorf("get workflow request: %w", err)
	}
	request.CreatedAt = time.UnixMilli(createdAtMS).UTC()
	request.UpdatedAt = time.UnixMilli(updatedAtMS).UTC()
	return request, nil
}

func validWorkflowRequestState(state string) bool {
	switch state {
	case WorkflowRequestStatePending,
		WorkflowRequestStateExecuting,
		WorkflowRequestStateDenied,
		WorkflowRequestStateSucceeded,
		WorkflowRequestStatePartial,
		WorkflowRequestStateFailed,
		WorkflowRequestStateExpired:
		return true
	default:
		return false
	}
}

func normalizedWorkflowTime(value time.Time) time.Time {
	if value.IsZero() {
		return time.Now().UTC()
	}
	return value.UTC()
}

func requireOneWorkflowRequestRow(result sql.Result) error {
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect workflow request update: %w", err)
	}
	if count != 1 {
		return ErrWorkflowRequestNotFound
	}
	return nil
}
