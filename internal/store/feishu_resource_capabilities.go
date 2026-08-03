package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

var ErrFeishuResourceCapabilityNotFound = errors.New("feishu resource capability not found")

const (
	FeishuResourceCapabilityStateActive  = "active"
	FeishuResourceCapabilityStateRevoked = "revoked"
)

// FeishuResourceCapability records the permission Feishu currently reports
// for one exact collaborator subject. It does not represent whether a user has
// authorized LingoBridge to use that capability in a particular chat.
type FeishuResourceCapability struct {
	AccountID         string
	ResourceType      string
	ResourceToken     string
	SubjectType       string
	SubjectID         string
	Permission        string
	SourceActorOpenID string
	SourceActorUserID string
	SourceRequestID   string
	State             string
	CreatedAt         time.Time
	VerifiedAt        time.Time
	UpdatedAt         time.Time
}

// UpsertFeishuResourceCapability creates, refreshes, or upgrades one exact
// Feishu-side collaborator capability without downgrading an active write
// capability to read.
func (s *Store) UpsertFeishuResourceCapability(capability FeishuResourceCapability) (FeishuResourceCapability, error) {
	if err := s.requireFeishuDocsStore(); err != nil {
		return FeishuResourceCapability{}, err
	}
	capability = normalizeFeishuResourceCapability(capability)
	if err := validateFeishuResourceCapability(capability); err != nil {
		return FeishuResourceCapability{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := upsertFeishuResourceCapability(s.db, capability); err != nil {
		return FeishuResourceCapability{}, err
	}
	return capability, nil
}

// ActiveFeishuResourceCapability returns an active exact-subject capability
// satisfying the requested permission.
func (s *Store) ActiveFeishuResourceCapability(accountID, resourceType, resourceToken, subjectType, subjectID, permission string) (FeishuResourceCapability, bool, error) {
	if err := s.requireFeishuDocsStore(); err != nil {
		return FeishuResourceCapability{}, false, err
	}
	permission = strings.TrimSpace(permission)
	if !validFeishuResourcePermission(permission) {
		return FeishuResourceCapability{}, false, fmt.Errorf("valid feishu resource permission is required")
	}
	capability, err := scanFeishuResourceCapability(s.db.QueryRow(
		`SELECT account_id, resource_type, resource_token, subject_type, subject_id,
		 permission, source_actor_open_id, source_actor_user_id, source_request_id,
		 state, created_at_ms, verified_at_ms, updated_at_ms
		 FROM feishu_resource_capabilities
		 WHERE account_id=? AND resource_type=? AND resource_token=?
		 AND subject_type=? AND subject_id=? AND state=?`,
		strings.TrimSpace(accountID),
		strings.TrimSpace(resourceType),
		strings.TrimSpace(resourceToken),
		strings.TrimSpace(subjectType),
		strings.TrimSpace(subjectID),
		FeishuResourceCapabilityStateActive,
	))
	if errors.Is(err, ErrFeishuResourceCapabilityNotFound) {
		return FeishuResourceCapability{}, false, nil
	}
	if err != nil {
		return FeishuResourceCapability{}, false, err
	}
	if !FeishuResourcePermissionSatisfies(capability.Permission, permission) {
		return FeishuResourceCapability{}, false, nil
	}
	return capability, true, nil
}

// RevokeFeishuResourceCapability marks one exact collaborator capability as
// unavailable after a failed Feishu live check.
func (s *Store) RevokeFeishuResourceCapability(accountID, resourceType, resourceToken, subjectType, subjectID string, now time.Time) error {
	if err := s.requireFeishuDocsStore(); err != nil {
		return err
	}
	now = normalizedWorkflowTime(now)
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.db.Exec(
		`UPDATE feishu_resource_capabilities SET state=?, updated_at_ms=?
		 WHERE account_id=? AND resource_type=? AND resource_token=? AND subject_type=? AND subject_id=?`,
		FeishuResourceCapabilityStateRevoked,
		now.UnixMilli(),
		strings.TrimSpace(accountID),
		strings.TrimSpace(resourceType),
		strings.TrimSpace(resourceToken),
		strings.TrimSpace(subjectType),
		strings.TrimSpace(subjectID),
	)
	if err != nil {
		return fmt.Errorf("revoke feishu resource capability: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect revoked feishu resource capability: %w", err)
	}
	if count == 0 {
		return ErrFeishuResourceCapabilityNotFound
	}
	return nil
}

type feishuResourceCapabilityScanner interface {
	Scan(dest ...any) error
}

func scanFeishuResourceCapability(row feishuResourceCapabilityScanner) (FeishuResourceCapability, error) {
	var capability FeishuResourceCapability
	var createdAtMS, verifiedAtMS, updatedAtMS int64
	if err := row.Scan(
		&capability.AccountID,
		&capability.ResourceType,
		&capability.ResourceToken,
		&capability.SubjectType,
		&capability.SubjectID,
		&capability.Permission,
		&capability.SourceActorOpenID,
		&capability.SourceActorUserID,
		&capability.SourceRequestID,
		&capability.State,
		&createdAtMS,
		&verifiedAtMS,
		&updatedAtMS,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return FeishuResourceCapability{}, ErrFeishuResourceCapabilityNotFound
		}
		return FeishuResourceCapability{}, fmt.Errorf("get feishu resource capability: %w", err)
	}
	capability.CreatedAt = time.UnixMilli(createdAtMS).UTC()
	capability.VerifiedAt = time.UnixMilli(verifiedAtMS).UTC()
	capability.UpdatedAt = time.UnixMilli(updatedAtMS).UTC()
	return capability, nil
}

func normalizeFeishuResourceCapability(capability FeishuResourceCapability) FeishuResourceCapability {
	capability.AccountID = strings.TrimSpace(capability.AccountID)
	capability.ResourceType = strings.TrimSpace(capability.ResourceType)
	capability.ResourceToken = strings.TrimSpace(capability.ResourceToken)
	capability.SubjectType = strings.TrimSpace(capability.SubjectType)
	capability.SubjectID = strings.TrimSpace(capability.SubjectID)
	capability.Permission = strings.TrimSpace(capability.Permission)
	capability.SourceActorOpenID = strings.TrimSpace(capability.SourceActorOpenID)
	capability.SourceActorUserID = strings.TrimSpace(capability.SourceActorUserID)
	capability.SourceRequestID = strings.TrimSpace(capability.SourceRequestID)
	capability.State = strings.TrimSpace(capability.State)
	if capability.State == "" {
		capability.State = FeishuResourceCapabilityStateActive
	}
	capability.CreatedAt = normalizedWorkflowTime(capability.CreatedAt)
	capability.VerifiedAt = normalizedWorkflowTime(capability.VerifiedAt)
	capability.UpdatedAt = capability.VerifiedAt
	return capability
}

func validateFeishuResourceCapability(capability FeishuResourceCapability) error {
	if capability.AccountID == "" || capability.ResourceType == "" || capability.ResourceToken == "" ||
		capability.SubjectType == "" || capability.SubjectID == "" || capability.SourceRequestID == "" ||
		!validFeishuResourcePermission(capability.Permission) {
		return fmt.Errorf("feishu resource capability account, resource, permission, subject, and source request are required")
	}
	if capability.State != FeishuResourceCapabilityStateActive && capability.State != FeishuResourceCapabilityStateRevoked {
		return fmt.Errorf("unsupported feishu resource capability state %q", capability.State)
	}
	return nil
}

type feishuResourceCapabilityExecer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func upsertFeishuResourceCapability(execer feishuResourceCapabilityExecer, capability FeishuResourceCapability) error {
	_, err := execer.Exec(
		`INSERT INTO feishu_resource_capabilities (
			account_id, resource_type, resource_token, subject_type, subject_id,
			permission, source_actor_open_id, source_actor_user_id, source_request_id,
			state, created_at_ms, verified_at_ms, updated_at_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(account_id, resource_type, resource_token, subject_type, subject_id) DO UPDATE SET
			permission=CASE
				WHEN (feishu_resource_capabilities.state='active' AND feishu_resource_capabilities.permission='write')
					OR excluded.permission='write' THEN 'write'
				ELSE 'read'
			END,
			source_actor_open_id=CASE
				WHEN excluded.source_actor_open_id='' THEN feishu_resource_capabilities.source_actor_open_id
				ELSE excluded.source_actor_open_id
			END,
			source_actor_user_id=CASE
				WHEN excluded.source_actor_user_id='' THEN feishu_resource_capabilities.source_actor_user_id
				ELSE excluded.source_actor_user_id
			END,
			source_request_id=excluded.source_request_id,
			state=excluded.state,
			verified_at_ms=excluded.verified_at_ms,
			updated_at_ms=excluded.updated_at_ms`,
		capability.AccountID,
		capability.ResourceType,
		capability.ResourceToken,
		capability.SubjectType,
		capability.SubjectID,
		capability.Permission,
		capability.SourceActorOpenID,
		capability.SourceActorUserID,
		capability.SourceRequestID,
		capability.State,
		capability.CreatedAt.UnixMilli(),
		capability.VerifiedAt.UnixMilli(),
		capability.UpdatedAt.UnixMilli(),
	)
	if err != nil {
		return fmt.Errorf("upsert feishu resource capability: %w", err)
	}
	return nil
}
