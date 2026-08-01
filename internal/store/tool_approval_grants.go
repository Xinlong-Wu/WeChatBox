package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	ToolApprovalActorTypeOpenID = "open_id"
	ToolApprovalActorTypeUserID = "user_id"
)

// ToolApprovalGrantScope identifies one exact reusable authorization boundary.
type ToolApprovalGrantScope struct {
	AccountID string
	ToolName  string
	ActorType string
	ActorID   string
	ChatID    string
}

// ToolApprovalGrant authorizes repeated calls within one scope until ExpiresAt.
type ToolApprovalGrant struct {
	ToolApprovalGrantScope
	SourceRequestID string
	CreatedAt       time.Time
	ExpiresAt       time.Time
	UpdatedAt       time.Time
}

// UpsertToolApprovalGrant creates or renews one exact reusable authorization scope.
func (s *Store) UpsertToolApprovalGrant(grant ToolApprovalGrant) (ToolApprovalGrant, error) {
	grant = normalizeToolApprovalGrant(grant)
	if err := validateToolApprovalGrant(grant); err != nil {
		return ToolApprovalGrant{}, err
	}
	grant.UpdatedAt = grant.CreatedAt

	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(
		`INSERT INTO tool_approval_grants (
			account_id, tool_name, actor_type, actor_id, chat_id,
			source_request_id, created_at_ms, expires_at_ms, updated_at_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(account_id, tool_name, actor_type, actor_id, chat_id) DO UPDATE SET
			source_request_id=excluded.source_request_id,
			created_at_ms=excluded.created_at_ms,
			expires_at_ms=excluded.expires_at_ms,
			updated_at_ms=excluded.updated_at_ms`,
		grant.AccountID,
		grant.ToolName,
		grant.ActorType,
		grant.ActorID,
		grant.ChatID,
		grant.SourceRequestID,
		grant.CreatedAt.UnixMilli(),
		grant.ExpiresAt.UnixMilli(),
		grant.UpdatedAt.UnixMilli(),
	)
	if err != nil {
		return ToolApprovalGrant{}, fmt.Errorf("upsert tool approval grant: %w", err)
	}
	return grant, nil
}

// ActiveToolApprovalGrant returns an unexpired grant for one exact scope.
// An expired matching row is removed lazily before lookup.
func (s *Store) ActiveToolApprovalGrant(scope ToolApprovalGrantScope, now time.Time) (ToolApprovalGrant, bool, error) {
	scope = normalizeToolApprovalGrantScope(scope)
	if err := validateToolApprovalGrantScope(scope); err != nil {
		return ToolApprovalGrant{}, false, err
	}
	now = normalizedApprovalTime(now)

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.Exec(
		`DELETE FROM tool_approval_grants
		 WHERE account_id=? AND tool_name=? AND actor_type=? AND actor_id=? AND chat_id=?
		 AND expires_at_ms<=?`,
		scope.AccountID,
		scope.ToolName,
		scope.ActorType,
		scope.ActorID,
		scope.ChatID,
		now.UnixMilli(),
	); err != nil {
		return ToolApprovalGrant{}, false, fmt.Errorf("expire tool approval grant: %w", err)
	}
	grant, err := scanToolApprovalGrant(s.db.QueryRow(
		`SELECT account_id, tool_name, actor_type, actor_id, chat_id,
		 source_request_id, created_at_ms, expires_at_ms, updated_at_ms
		 FROM tool_approval_grants
		 WHERE account_id=? AND tool_name=? AND actor_type=? AND actor_id=? AND chat_id=?
		 AND expires_at_ms>?`,
		scope.AccountID,
		scope.ToolName,
		scope.ActorType,
		scope.ActorID,
		scope.ChatID,
		now.UnixMilli(),
	))
	if errors.Is(err, sql.ErrNoRows) {
		return ToolApprovalGrant{}, false, nil
	}
	if err != nil {
		return ToolApprovalGrant{}, false, fmt.Errorf("get active tool approval grant: %w", err)
	}
	return grant, true, nil
}

type toolApprovalGrantScanner interface {
	Scan(dest ...any) error
}

func scanToolApprovalGrant(row toolApprovalGrantScanner) (ToolApprovalGrant, error) {
	var grant ToolApprovalGrant
	var createdAtMS, expiresAtMS, updatedAtMS int64
	if err := row.Scan(
		&grant.AccountID,
		&grant.ToolName,
		&grant.ActorType,
		&grant.ActorID,
		&grant.ChatID,
		&grant.SourceRequestID,
		&createdAtMS,
		&expiresAtMS,
		&updatedAtMS,
	); err != nil {
		return ToolApprovalGrant{}, err
	}
	grant.CreatedAt = time.UnixMilli(createdAtMS).UTC()
	grant.ExpiresAt = time.UnixMilli(expiresAtMS).UTC()
	grant.UpdatedAt = time.UnixMilli(updatedAtMS).UTC()
	return grant, nil
}

func normalizeToolApprovalGrant(grant ToolApprovalGrant) ToolApprovalGrant {
	grant.ToolApprovalGrantScope = normalizeToolApprovalGrantScope(grant.ToolApprovalGrantScope)
	grant.SourceRequestID = strings.TrimSpace(grant.SourceRequestID)
	grant.CreatedAt = normalizedApprovalTime(grant.CreatedAt)
	grant.ExpiresAt = grant.ExpiresAt.UTC()
	return grant
}

func normalizeToolApprovalGrantScope(scope ToolApprovalGrantScope) ToolApprovalGrantScope {
	scope.AccountID = strings.TrimSpace(scope.AccountID)
	scope.ToolName = strings.TrimSpace(scope.ToolName)
	scope.ActorType = strings.TrimSpace(scope.ActorType)
	scope.ActorID = strings.TrimSpace(scope.ActorID)
	scope.ChatID = strings.TrimSpace(scope.ChatID)
	return scope
}

func validateToolApprovalGrant(grant ToolApprovalGrant) error {
	if err := validateToolApprovalGrantScope(grant.ToolApprovalGrantScope); err != nil {
		return err
	}
	if grant.SourceRequestID == "" {
		return fmt.Errorf("tool approval grant source_request_id is required")
	}
	if !grant.ExpiresAt.After(grant.CreatedAt) {
		return fmt.Errorf("tool approval grant expires_at must be after created_at")
	}
	return nil
}

func validateToolApprovalGrantScope(scope ToolApprovalGrantScope) error {
	if scope.AccountID == "" || scope.ToolName == "" || scope.ActorID == "" || scope.ChatID == "" {
		return fmt.Errorf("tool approval grant account_id, tool_name, actor_id, and chat_id are required")
	}
	if scope.ActorType != ToolApprovalActorTypeOpenID && scope.ActorType != ToolApprovalActorTypeUserID {
		return fmt.Errorf("unsupported tool approval grant actor_type %q", scope.ActorType)
	}
	return nil
}
