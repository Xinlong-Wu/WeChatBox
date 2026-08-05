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
	AccountID     string
	ToolName      string
	ActionKey     string
	ResourceType  string
	ResourceToken string
	ActorType     string
	ActorID       string
	ChatID        string
}

// ToolApprovalGrant permanently authorizes repeated calls within one exact
// tool/action/resource scope until the row is explicitly removed.
type ToolApprovalGrant struct {
	ToolApprovalGrantScope
	SourceRequestID string
	CreatedAt       time.Time
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
			account_id, tool_name, action_key, resource_type, resource_token,
			actor_type, actor_id, chat_id, source_request_id, created_at_ms, updated_at_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(account_id, actor_type, actor_id, chat_id, tool_name, action_key, resource_type, resource_token) DO UPDATE SET
			source_request_id=excluded.source_request_id,
			created_at_ms=excluded.created_at_ms,
			updated_at_ms=excluded.updated_at_ms`,
		grant.AccountID,
		grant.ToolName,
		grant.ActionKey,
		grant.ResourceType,
		grant.ResourceToken,
		grant.ActorType,
		grant.ActorID,
		grant.ChatID,
		grant.SourceRequestID,
		grant.CreatedAt.UnixMilli(),
		grant.UpdatedAt.UnixMilli(),
	)
	if err != nil {
		return ToolApprovalGrant{}, fmt.Errorf("upsert tool approval grant: %w", err)
	}
	return grant, nil
}

// FindToolApprovalGrant returns one permanent grant for an exact scope.
func (s *Store) FindToolApprovalGrant(scope ToolApprovalGrantScope) (ToolApprovalGrant, bool, error) {
	scope = normalizeToolApprovalGrantScope(scope)
	if err := validateToolApprovalGrantScope(scope); err != nil {
		return ToolApprovalGrant{}, false, err
	}

	grant, err := scanToolApprovalGrant(s.db.QueryRow(
		`SELECT account_id, tool_name, action_key, resource_type, resource_token,
		 actor_type, actor_id, chat_id, source_request_id, created_at_ms, updated_at_ms
		 FROM tool_approval_grants
		 WHERE account_id=? AND actor_type=? AND actor_id=? AND chat_id=?
		 AND tool_name=? AND action_key=? AND resource_type=? AND resource_token=?`,
		scope.AccountID,
		scope.ActorType,
		scope.ActorID,
		scope.ChatID,
		scope.ToolName,
		scope.ActionKey,
		scope.ResourceType,
		scope.ResourceToken,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return ToolApprovalGrant{}, false, nil
	}
	if err != nil {
		return ToolApprovalGrant{}, false, fmt.Errorf("get tool approval grant: %w", err)
	}
	return grant, true, nil
}

type toolApprovalGrantScanner interface {
	Scan(dest ...any) error
}

func scanToolApprovalGrant(row toolApprovalGrantScanner) (ToolApprovalGrant, error) {
	var grant ToolApprovalGrant
	var createdAtMS, updatedAtMS int64
	if err := row.Scan(
		&grant.AccountID,
		&grant.ToolName,
		&grant.ActionKey,
		&grant.ResourceType,
		&grant.ResourceToken,
		&grant.ActorType,
		&grant.ActorID,
		&grant.ChatID,
		&grant.SourceRequestID,
		&createdAtMS,
		&updatedAtMS,
	); err != nil {
		return ToolApprovalGrant{}, err
	}
	grant.CreatedAt = time.UnixMilli(createdAtMS).UTC()
	grant.UpdatedAt = time.UnixMilli(updatedAtMS).UTC()
	return grant, nil
}

func normalizeToolApprovalGrant(grant ToolApprovalGrant) ToolApprovalGrant {
	grant.ToolApprovalGrantScope = normalizeToolApprovalGrantScope(grant.ToolApprovalGrantScope)
	grant.SourceRequestID = strings.TrimSpace(grant.SourceRequestID)
	grant.CreatedAt = normalizedApprovalTime(grant.CreatedAt)
	return grant
}

func normalizeToolApprovalGrantScope(scope ToolApprovalGrantScope) ToolApprovalGrantScope {
	scope.AccountID = strings.TrimSpace(scope.AccountID)
	scope.ToolName = strings.TrimSpace(scope.ToolName)
	scope.ActionKey = strings.TrimSpace(scope.ActionKey)
	scope.ResourceType = strings.ToLower(strings.TrimSpace(scope.ResourceType))
	scope.ResourceToken = strings.TrimSpace(scope.ResourceToken)
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
	return nil
}

func validateToolApprovalGrantScope(scope ToolApprovalGrantScope) error {
	if scope.AccountID == "" || scope.ToolName == "" || scope.ActionKey == "" || scope.ResourceType == "" ||
		scope.ResourceToken == "" || scope.ActorID == "" || scope.ChatID == "" {
		return fmt.Errorf("tool approval grant account_id, tool_name, action_key, resource, actor_id, and chat_id are required")
	}
	if scope.ActorType != ToolApprovalActorTypeOpenID && scope.ActorType != ToolApprovalActorTypeUserID {
		return fmt.Errorf("unsupported tool approval grant actor_type %q", scope.ActorType)
	}
	return nil
}
