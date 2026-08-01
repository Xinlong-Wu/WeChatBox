package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	// ErrToolApprovalNotFound is returned when an approval ID is unknown for an account.
	ErrToolApprovalNotFound = errors.New("tool approval not found")
	// ErrToolApprovalForbidden is returned when someone other than the requesting actor responds.
	ErrToolApprovalForbidden = errors.New("tool approval actor does not match")
	// ErrToolApprovalContextMismatch is returned when a callback is not from the original card and chat.
	ErrToolApprovalContextMismatch = errors.New("tool approval callback context does not match")
	// ErrToolApprovalExpired is returned after a pending approval reaches its expiry time.
	ErrToolApprovalExpired = errors.New("tool approval expired")
	// ErrToolApprovalResolved is returned when an approval has already left the pending state.
	ErrToolApprovalResolved = errors.New("tool approval already resolved")
)

const (
	ToolApprovalStatePending   = "pending"
	ToolApprovalStateExecuting = "executing"
	ToolApprovalStateDenied    = "denied"
	ToolApprovalStateSucceeded = "succeeded"
	ToolApprovalStateFailed    = "failed"
	ToolApprovalStateExpired   = "expired"

	ToolApprovalDecisionApprove = "approve"
	ToolApprovalDecisionDeny    = "deny"
)

// ToolApproval is one durable, single-use human authorization request for a tool call.
type ToolApproval struct {
	ID              string
	AccountID       string
	ToolName        string
	ActorOpenID     string
	ActorUserID     string
	ChatID          string
	SourceMessageID string
	CardMessageID   string
	Payload         string
	State           string
	CreatedAt       time.Time
	ExpiresAt       time.Time
	UpdatedAt       time.Time
}

// ToolApprovalMatch contains trusted Feishu callback identity and card context.
type ToolApprovalMatch struct {
	ActorOpenID   string
	ActorUserID   string
	ChatID        string
	CardMessageID string
}

// CreateToolApproval persists a new pending approval and generates an ID when omitted.
func (s *Store) CreateToolApproval(approval ToolApproval) (ToolApproval, error) {
	approval = normalizeToolApproval(approval)
	if approval.ID == "" {
		id, err := generateID()
		if err != nil {
			return ToolApproval{}, err
		}
		approval.ID = id
	}
	if err := validateNewToolApproval(approval); err != nil {
		return ToolApproval{}, err
	}
	approval.State = ToolApprovalStatePending
	approval.CardMessageID = ""
	approval.UpdatedAt = approval.CreatedAt

	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(
		`INSERT INTO tool_approvals (
			id, account_id, tool_name, actor_open_id, actor_user_id, chat_id,
			source_message_id, card_message_id, payload, state,
			created_at_ms, expires_at_ms, updated_at_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, '', ?, ?, ?, ?, ?)`,
		approval.ID,
		approval.AccountID,
		approval.ToolName,
		approval.ActorOpenID,
		approval.ActorUserID,
		approval.ChatID,
		approval.SourceMessageID,
		approval.Payload,
		approval.State,
		approval.CreatedAt.UnixMilli(),
		approval.ExpiresAt.UnixMilli(),
		approval.UpdatedAt.UnixMilli(),
	)
	if err != nil {
		return ToolApproval{}, fmt.Errorf("create tool approval: %w", err)
	}
	return approval, nil
}

// SetToolApprovalCardMessageID binds a pending approval to the exact card message sent to Feishu.
func (s *Store) SetToolApprovalCardMessageID(id, accountID, messageID string, now time.Time) error {
	id = strings.TrimSpace(id)
	accountID = strings.TrimSpace(accountID)
	messageID = strings.TrimSpace(messageID)
	if id == "" || accountID == "" || messageID == "" {
		return fmt.Errorf("tool approval id, account_id, and card message_id are required")
	}
	now = normalizedApprovalTime(now)

	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.db.Exec(
		`UPDATE tool_approvals
		 SET card_message_id=?, updated_at_ms=?
		 WHERE id=? AND account_id=? AND state=? AND card_message_id=''`,
		messageID, now.UnixMilli(), id, accountID, ToolApprovalStatePending,
	)
	if err != nil {
		return fmt.Errorf("bind tool approval card: %w", err)
	}
	return requireOneToolApprovalRow(result)
}

// DecideToolApproval atomically consumes a pending approval as approved or denied.
func (s *Store) DecideToolApproval(id, accountID, decision string, match ToolApprovalMatch, now time.Time) (ToolApproval, error) {
	id = strings.TrimSpace(id)
	accountID = strings.TrimSpace(accountID)
	decision = strings.TrimSpace(decision)
	match = normalizeToolApprovalMatch(match)
	if id == "" || accountID == "" {
		return ToolApproval{}, fmt.Errorf("tool approval id and account_id are required")
	}
	if decision != ToolApprovalDecisionApprove && decision != ToolApprovalDecisionDeny {
		return ToolApproval{}, fmt.Errorf("unsupported tool approval decision %q", decision)
	}
	now = normalizedApprovalTime(now)

	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return ToolApproval{}, fmt.Errorf("begin tool approval decision: %w", err)
	}
	defer tx.Rollback()

	approval, err := toolApprovalByID(tx, id, accountID)
	if err != nil {
		return ToolApproval{}, err
	}
	if approval.State != ToolApprovalStatePending {
		return approval, ErrToolApprovalResolved
	}
	if !now.Before(approval.ExpiresAt) {
		if _, err := tx.Exec(
			`UPDATE tool_approvals SET state=?, payload='', updated_at_ms=?
			 WHERE id=? AND account_id=? AND state=?`,
			ToolApprovalStateExpired, now.UnixMilli(), id, accountID, ToolApprovalStatePending,
		); err != nil {
			return ToolApproval{}, fmt.Errorf("expire tool approval: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return ToolApproval{}, fmt.Errorf("commit expired tool approval: %w", err)
		}
		approval.State = ToolApprovalStateExpired
		approval.Payload = ""
		approval.UpdatedAt = now
		return approval, ErrToolApprovalExpired
	}
	if !toolApprovalActorMatches(approval, match) {
		return approval, ErrToolApprovalForbidden
	}
	if approval.ChatID != match.ChatID || approval.CardMessageID == "" || approval.CardMessageID != match.CardMessageID {
		return approval, ErrToolApprovalContextMismatch
	}

	nextState := ToolApprovalStateExecuting
	payload := approval.Payload
	if decision == ToolApprovalDecisionDeny {
		nextState = ToolApprovalStateDenied
		payload = ""
	}
	result, err := tx.Exec(
		`UPDATE tool_approvals SET state=?, payload=?, updated_at_ms=?
		 WHERE id=? AND account_id=? AND state=?`,
		nextState, payload, now.UnixMilli(), id, accountID, ToolApprovalStatePending,
	)
	if err != nil {
		return ToolApproval{}, fmt.Errorf("decide tool approval: %w", err)
	}
	if err := requireOneToolApprovalRow(result); err != nil {
		return ToolApproval{}, err
	}
	if err := tx.Commit(); err != nil {
		return ToolApproval{}, fmt.Errorf("commit tool approval decision: %w", err)
	}
	approval.State = nextState
	approval.Payload = payload
	approval.UpdatedAt = now
	return approval, nil
}

// CompleteToolApproval marks an executing request as succeeded or failed and clears its payload.
func (s *Store) CompleteToolApproval(id, accountID, state string, now time.Time) error {
	id = strings.TrimSpace(id)
	accountID = strings.TrimSpace(accountID)
	state = strings.TrimSpace(state)
	if state != ToolApprovalStateSucceeded && state != ToolApprovalStateFailed {
		return fmt.Errorf("unsupported completed tool approval state %q", state)
	}
	now = normalizedApprovalTime(now)

	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.db.Exec(
		`UPDATE tool_approvals SET state=?, payload='', updated_at_ms=?
		 WHERE id=? AND account_id=? AND state=?`,
		state, now.UnixMilli(), id, accountID, ToolApprovalStateExecuting,
	)
	if err != nil {
		return fmt.Errorf("complete tool approval: %w", err)
	}
	return requireOneToolApprovalRow(result)
}

// FailToolApproval closes a pending or executing request and clears its payload.
func (s *Store) FailToolApproval(id, accountID string, now time.Time) error {
	now = normalizedApprovalTime(now)
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.db.Exec(
		`UPDATE tool_approvals SET state=?, payload='', updated_at_ms=?
		 WHERE id=? AND account_id=? AND state IN (?, ?)`,
		ToolApprovalStateFailed,
		now.UnixMilli(),
		strings.TrimSpace(id),
		strings.TrimSpace(accountID),
		ToolApprovalStatePending,
		ToolApprovalStateExecuting,
	)
	if err != nil {
		return fmt.Errorf("fail tool approval: %w", err)
	}
	return requireOneToolApprovalRow(result)
}

// ExpireToolApprovals closes all elapsed pending requests for one account and clears their payloads.
func (s *Store) ExpireToolApprovals(accountID string, now time.Time) (int64, error) {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return 0, fmt.Errorf("tool approval account_id is required")
	}
	now = normalizedApprovalTime(now)
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.db.Exec(
		`UPDATE tool_approvals SET state=?, payload='', updated_at_ms=?
		 WHERE account_id=? AND state=? AND expires_at_ms<=?`,
		ToolApprovalStateExpired,
		now.UnixMilli(),
		accountID,
		ToolApprovalStatePending,
		now.UnixMilli(),
	)
	if err != nil {
		return 0, fmt.Errorf("expire tool approvals: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count expired tool approvals: %w", err)
	}
	return count, nil
}

// FailExecutingToolApprovals closes operations that were interrupted by a process restart.
// They are not retried automatically because the external operation may already have succeeded.
func (s *Store) FailExecutingToolApprovals(accountID string, now time.Time) (int64, error) {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return 0, fmt.Errorf("tool approval account_id is required")
	}
	now = normalizedApprovalTime(now)
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.db.Exec(
		`UPDATE tool_approvals SET state=?, payload='', updated_at_ms=?
		 WHERE account_id=? AND state=?`,
		ToolApprovalStateFailed,
		now.UnixMilli(),
		accountID,
		ToolApprovalStateExecuting,
	)
	if err != nil {
		return 0, fmt.Errorf("fail interrupted tool approvals: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count interrupted tool approvals: %w", err)
	}
	return count, nil
}

// DeleteToolApprovals removes all approval metadata associated with an account.
func (s *Store) DeleteToolApprovals(accountID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM tool_approvals WHERE account_id=?`, strings.TrimSpace(accountID))
	if err != nil {
		return fmt.Errorf("delete tool approvals: %w", err)
	}
	return nil
}

// GetToolApproval returns one approval for diagnostics and tests.
func (s *Store) GetToolApproval(id, accountID string) (ToolApproval, error) {
	return toolApprovalByID(s.db, strings.TrimSpace(id), strings.TrimSpace(accountID))
}

type toolApprovalScanner interface {
	Scan(dest ...any) error
}

func toolApprovalByID(queryer interface {
	QueryRow(query string, args ...any) *sql.Row
}, id, accountID string) (ToolApproval, error) {
	return scanToolApproval(queryer.QueryRow(
		`SELECT id, account_id, tool_name, actor_open_id, actor_user_id, chat_id,
		 source_message_id, card_message_id, payload, state,
		 created_at_ms, expires_at_ms, updated_at_ms
		 FROM tool_approvals WHERE id=? AND account_id=?`,
		id, accountID,
	))
}

func scanToolApproval(row toolApprovalScanner) (ToolApproval, error) {
	var approval ToolApproval
	var createdAtMS, expiresAtMS, updatedAtMS int64
	err := row.Scan(
		&approval.ID,
		&approval.AccountID,
		&approval.ToolName,
		&approval.ActorOpenID,
		&approval.ActorUserID,
		&approval.ChatID,
		&approval.SourceMessageID,
		&approval.CardMessageID,
		&approval.Payload,
		&approval.State,
		&createdAtMS,
		&expiresAtMS,
		&updatedAtMS,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return ToolApproval{}, ErrToolApprovalNotFound
	}
	if err != nil {
		return ToolApproval{}, fmt.Errorf("get tool approval: %w", err)
	}
	approval.CreatedAt = time.UnixMilli(createdAtMS).UTC()
	approval.ExpiresAt = time.UnixMilli(expiresAtMS).UTC()
	approval.UpdatedAt = time.UnixMilli(updatedAtMS).UTC()
	return approval, nil
}

func normalizeToolApproval(approval ToolApproval) ToolApproval {
	approval.ID = strings.TrimSpace(approval.ID)
	approval.AccountID = strings.TrimSpace(approval.AccountID)
	approval.ToolName = strings.TrimSpace(approval.ToolName)
	approval.ActorOpenID = strings.TrimSpace(approval.ActorOpenID)
	approval.ActorUserID = strings.TrimSpace(approval.ActorUserID)
	approval.ChatID = strings.TrimSpace(approval.ChatID)
	approval.SourceMessageID = strings.TrimSpace(approval.SourceMessageID)
	approval.Payload = strings.TrimSpace(approval.Payload)
	approval.CreatedAt = normalizedApprovalTime(approval.CreatedAt)
	approval.ExpiresAt = approval.ExpiresAt.UTC()
	return approval
}

func normalizeToolApprovalMatch(match ToolApprovalMatch) ToolApprovalMatch {
	match.ActorOpenID = strings.TrimSpace(match.ActorOpenID)
	match.ActorUserID = strings.TrimSpace(match.ActorUserID)
	match.ChatID = strings.TrimSpace(match.ChatID)
	match.CardMessageID = strings.TrimSpace(match.CardMessageID)
	return match
}

func validateNewToolApproval(approval ToolApproval) error {
	if approval.ID == "" || approval.AccountID == "" || approval.ToolName == "" || approval.ChatID == "" || approval.Payload == "" {
		return fmt.Errorf("tool approval id, account_id, tool_name, chat_id, and payload are required")
	}
	if approval.ActorOpenID == "" && approval.ActorUserID == "" {
		return fmt.Errorf("tool approval actor identity is required")
	}
	if !approval.ExpiresAt.After(approval.CreatedAt) {
		return fmt.Errorf("tool approval expires_at must be after created_at")
	}
	return nil
}

func toolApprovalActorMatches(approval ToolApproval, match ToolApprovalMatch) bool {
	if approval.ActorOpenID != "" {
		if approval.ActorOpenID != match.ActorOpenID {
			return false
		}
		return approval.ActorUserID == "" || match.ActorUserID == "" || approval.ActorUserID == match.ActorUserID
	}
	return approval.ActorUserID != "" && approval.ActorUserID == match.ActorUserID
}

func normalizedApprovalTime(value time.Time) time.Time {
	if value.IsZero() {
		return time.Now().UTC()
	}
	return value.UTC()
}

func requireOneToolApprovalRow(result sql.Result) error {
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect tool approval update: %w", err)
	}
	if count != 1 {
		return ErrToolApprovalResolved
	}
	return nil
}
