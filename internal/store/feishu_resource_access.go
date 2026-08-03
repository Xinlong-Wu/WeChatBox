package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	// ErrFeishuBotResourceNotFound is returned when a resource is not known to be Bot-owned.
	ErrFeishuBotResourceNotFound = errors.New("feishu bot resource not found")
	// ErrFeishuResourceAccessNotFound is returned when a resource access request is unknown.
	ErrFeishuResourceAccessNotFound = errors.New("feishu resource access request not found")
	// ErrFeishuResourceAccessForbidden is returned when a different user responds to a request.
	ErrFeishuResourceAccessForbidden = errors.New("feishu resource access actor does not match")
	// ErrFeishuResourceAccessContextMismatch is returned for a callback from another card or chat.
	ErrFeishuResourceAccessContextMismatch = errors.New("feishu resource access callback context does not match")
	// ErrFeishuResourceAccessOAuthStateMismatch is returned when a card submission carries another OAuth request's state.
	ErrFeishuResourceAccessOAuthStateMismatch = errors.New("feishu resource access oauth state does not match")
	// ErrFeishuResourceAccessExpired is returned when a pending request has expired.
	ErrFeishuResourceAccessExpired = errors.New("feishu resource access request expired")
	// ErrFeishuResourceAccessResolved is returned after a request leaves its actionable state.
	ErrFeishuResourceAccessResolved = errors.New("feishu resource access request already resolved")
	// ErrFeishuResourceAccessConsumed is returned when a granted request already authorized an operation.
	ErrFeishuResourceAccessConsumed = errors.New("feishu resource access request already consumed")
	// ErrFeishuResourceGrantNotFound is returned when no chat-scoped grant is recorded.
	ErrFeishuResourceGrantNotFound = errors.New("feishu resource grant not found")
)

const (
	FeishuResourcePermissionRead  = "read"
	FeishuResourcePermissionWrite = "write"

	FeishuResourceAccessStatePending   = WorkflowRequestStatePending
	FeishuResourceAccessStateExecuting = WorkflowRequestStateExecuting
	FeishuResourceAccessStateDenied    = WorkflowRequestStateDenied
	FeishuResourceAccessStateSucceeded = WorkflowRequestStateSucceeded
	FeishuResourceAccessStateFailed    = WorkflowRequestStateFailed
	FeishuResourceAccessStateExpired   = WorkflowRequestStateExpired

	FeishuResourceGrantSourceBotOwner      = "bot_owner"
	FeishuResourceGrantSourceExistingGrant = "existing_grant"
	FeishuResourceGrantSourceNewlyGranted  = "newly_granted"

	FeishuResourceGrantStateActive  = "active"
	FeishuResourceGrantStateRevoked = "revoked"
	FeishuResourceGrantStateExpired = "expired"

	FeishuResourceGrantModeOnce = "once"
	FeishuResourceGrantModeAll  = "all"

	FeishuResourceGrantActorTypeOpenID = "open_id"
	FeishuResourceGrantActorTypeUserID = "user_id"
)

// FeishuBotResource records one folder or file created and owned by the Bot account.
type FeishuBotResource struct {
	AccountID       string
	ResourceType    string
	ResourceToken   string
	ParentToken     string
	Name            string
	URL             string
	SourceRequestID string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// FeishuResourceAccessRequest is one globally identified access verification workflow.
// OAuth state is stored only as a hash and cleared as soon as the callback atomically
// claims the request. PKCEVerifier is retained only to identify requests created by
// older versions and is cleared by the same claim.
type FeishuResourceAccessRequest struct {
	ID                  string
	AccountID           string
	ActorOpenID         string
	ActorUserID         string
	ChatID              string
	SourceMessageID     string
	ResourceType        string
	ResourceToken       string
	ResourceURL         string
	Permission          string
	Reason              string
	SubjectType         string
	SubjectID           string
	GrantSource         string
	VerifiedPermission  string
	CardMessageID       string
	OAuthStateHash      string
	PKCEVerifier        string
	State               string
	ConsumedByRequestID string
	ConsumedAt          time.Time
	CreatedAt           time.Time
	ExpiresAt           time.Time
	UpdatedAt           time.Time
}

// FeishuResourceAccessMatch contains trusted callback identity and card context.
type FeishuResourceAccessMatch struct {
	ActorOpenID   string
	ActorUserID   string
	ChatID        string
	CardMessageID string
}

// FeishuResourceGrant records the permission last verified for one exact Bot account,
// chat, resource type, and resource token.
type FeishuResourceGrant struct {
	AccountID       string
	ActorType       string
	ActorID         string
	ChatID          string
	ResourceType    string
	ResourceToken   string
	Permission      string
	GrantMode       string
	SourceRequestID string
	State           string
	ExpiresAt       time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// SaveFeishuBotResource creates or refreshes Bot ownership metadata.
func (s *Store) SaveFeishuBotResource(resource FeishuBotResource) (FeishuBotResource, error) {
	if err := s.requireFeishuDocsStore(); err != nil {
		return FeishuBotResource{}, err
	}
	resource = normalizeFeishuBotResource(resource)
	if err := validateFeishuBotResource(resource); err != nil {
		return FeishuBotResource{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(
		`INSERT INTO feishu_bot_resources (
			account_id, resource_type, resource_token, parent_token, name, url,
			source_request_id, created_at_ms, updated_at_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(account_id, resource_type, resource_token) DO UPDATE SET
			parent_token=excluded.parent_token,
			name=CASE WHEN excluded.name='' THEN feishu_bot_resources.name ELSE excluded.name END,
			url=CASE WHEN excluded.url='' THEN feishu_bot_resources.url ELSE excluded.url END,
			source_request_id=CASE
				WHEN excluded.source_request_id='' THEN feishu_bot_resources.source_request_id
				ELSE excluded.source_request_id
			END,
			updated_at_ms=excluded.updated_at_ms`,
		resource.AccountID,
		resource.ResourceType,
		resource.ResourceToken,
		resource.ParentToken,
		resource.Name,
		resource.URL,
		resource.SourceRequestID,
		resource.CreatedAt.UnixMilli(),
		resource.UpdatedAt.UnixMilli(),
	)
	if err != nil {
		return FeishuBotResource{}, fmt.Errorf("save feishu bot resource: %w", err)
	}
	return resource, nil
}

// GetFeishuBotResource returns ownership metadata for one exact account and resource.
func (s *Store) GetFeishuBotResource(accountID, resourceType, resourceToken string) (FeishuBotResource, error) {
	if err := s.requireFeishuDocsStore(); err != nil {
		return FeishuBotResource{}, err
	}
	return scanFeishuBotResource(s.db.QueryRow(
		`SELECT account_id, resource_type, resource_token, parent_token, name, url,
		 source_request_id, created_at_ms, updated_at_ms
		 FROM feishu_bot_resources
		 WHERE account_id=? AND resource_type=? AND resource_token=?`,
		strings.TrimSpace(accountID), strings.TrimSpace(resourceType), strings.TrimSpace(resourceToken),
	))
}

// CreateFeishuResourceAccessRequest atomically creates a global workflow root
// and its pending resource access record.
func (s *Store) CreateFeishuResourceAccessRequest(request FeishuResourceAccessRequest) (FeishuResourceAccessRequest, error) {
	if err := s.requireFeishuDocsStore(); err != nil {
		return FeishuResourceAccessRequest{}, err
	}
	request = normalizeFeishuResourceAccessRequest(request)
	workflow, err := prepareWorkflowRequest(WorkflowRequest{
		ID:        request.ID,
		AccountID: request.AccountID,
		Kind:      WorkflowRequestKindFeishuResourceAccess,
		State:     WorkflowRequestStatePending,
		CreatedAt: request.CreatedAt,
	})
	if err != nil {
		return FeishuResourceAccessRequest{}, err
	}
	request.ID = workflow.ID
	request.State = FeishuResourceAccessStatePending
	request.UpdatedAt = request.CreatedAt
	if err := validateNewFeishuResourceAccessRequest(request); err != nil {
		return FeishuResourceAccessRequest{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return FeishuResourceAccessRequest{}, fmt.Errorf("begin create feishu resource access request: %w", err)
	}
	defer tx.Rollback()
	if err := insertWorkflowRequest(tx, workflow); err != nil {
		return FeishuResourceAccessRequest{}, err
	}
	_, err = tx.Exec(
		`INSERT INTO feishu_resource_access_requests (
			id, account_id, actor_open_id, actor_user_id, chat_id, source_message_id,
			resource_type, resource_token, resource_url, permission, reason,
			subject_type, subject_id, grant_source, verified_permission,
			card_message_id, oauth_state_hash, pkce_verifier, state,
			created_at_ms, expires_at_ms, updated_at_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', '', '', '', '', ?, ?, ?, ?)`,
		request.ID,
		request.AccountID,
		request.ActorOpenID,
		request.ActorUserID,
		request.ChatID,
		request.SourceMessageID,
		request.ResourceType,
		request.ResourceToken,
		request.ResourceURL,
		request.Permission,
		request.Reason,
		request.SubjectType,
		request.SubjectID,
		request.State,
		request.CreatedAt.UnixMilli(),
		request.ExpiresAt.UnixMilli(),
		request.UpdatedAt.UnixMilli(),
	)
	if err != nil {
		return FeishuResourceAccessRequest{}, fmt.Errorf("create feishu resource access request: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return FeishuResourceAccessRequest{}, fmt.Errorf("commit feishu resource access request: %w", err)
	}
	return request, nil
}

// PrepareFeishuResourceAccessOAuth binds the one-time OAuth state. verifier is kept
// for compatibility with requests created by older PKCE-enabled versions.
func (s *Store) PrepareFeishuResourceAccessOAuth(id, accountID, stateHash, verifier, subjectType, subjectID string, now time.Time) error {
	id = strings.TrimSpace(id)
	accountID = strings.TrimSpace(accountID)
	stateHash = strings.TrimSpace(stateHash)
	verifier = strings.TrimSpace(verifier)
	subjectType = strings.TrimSpace(subjectType)
	subjectID = strings.TrimSpace(subjectID)
	if id == "" || accountID == "" || stateHash == "" || subjectType == "" || subjectID == "" {
		return fmt.Errorf("feishu resource access id, account, oauth state, and subject are required")
	}
	now = normalizedWorkflowTime(now)
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.db.Exec(
		`UPDATE feishu_resource_access_requests
		 SET oauth_state_hash=?, pkce_verifier=?, subject_type=?, subject_id=?, updated_at_ms=?
		 WHERE id=? AND account_id=? AND state=? AND oauth_state_hash='' AND pkce_verifier=''`,
		stateHash, verifier, subjectType, subjectID, now.UnixMilli(),
		id, accountID, FeishuResourceAccessStatePending,
	)
	if err != nil {
		return fmt.Errorf("prepare feishu resource access oauth: %w", err)
	}
	return requireOneFeishuResourceAccessRow(result)
}

// SetFeishuResourceAccessCardMessageID binds the pending request to the exact card message.
func (s *Store) SetFeishuResourceAccessCardMessageID(id, accountID, messageID string, now time.Time) error {
	id = strings.TrimSpace(id)
	accountID = strings.TrimSpace(accountID)
	messageID = strings.TrimSpace(messageID)
	if id == "" || accountID == "" || messageID == "" {
		return fmt.Errorf("feishu resource access id, account, and card message id are required")
	}
	now = normalizedWorkflowTime(now)
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.db.Exec(
		`UPDATE feishu_resource_access_requests
		 SET card_message_id=?, updated_at_ms=?
		 WHERE id=? AND account_id=? AND state=? AND card_message_id=''`,
		messageID, now.UnixMilli(), id, accountID, FeishuResourceAccessStatePending,
	)
	if err != nil {
		return fmt.Errorf("bind feishu resource access card: %w", err)
	}
	return requireOneFeishuResourceAccessRow(result)
}

// GetFeishuResourceAccessRequest returns one request by its global ID and account.
func (s *Store) GetFeishuResourceAccessRequest(id, accountID string) (FeishuResourceAccessRequest, error) {
	if err := s.requireFeishuDocsStore(); err != nil {
		return FeishuResourceAccessRequest{}, err
	}
	return scanFeishuResourceAccessRequest(s.db.QueryRow(
		feishuResourceAccessSelect+` WHERE id=? AND account_id=?`,
		strings.TrimSpace(id), strings.TrimSpace(accountID),
	))
}

// ConsumeFeishuResourceAccessRequest atomically binds one still-valid granted
// access request to the concrete workflow that is about to call a create API.
func (s *Store) ConsumeFeishuResourceAccessRequest(id, accountID, consumedByRequestID string, now time.Time) error {
	if err := s.requireFeishuDocsStore(); err != nil {
		return err
	}
	id = strings.TrimSpace(id)
	accountID = strings.TrimSpace(accountID)
	consumedByRequestID = strings.TrimSpace(consumedByRequestID)
	if id == "" || accountID == "" || consumedByRequestID == "" {
		return fmt.Errorf("feishu resource access id, account, and consuming request are required")
	}
	now = normalizedWorkflowTime(now)
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.db.Exec(
		`UPDATE feishu_resource_access_requests
		 SET consumed_by_request_id=?, consumed_at_ms=?, updated_at_ms=?
		 WHERE id=? AND account_id=? AND state=? AND expires_at_ms>? AND consumed_by_request_id=''`,
		consumedByRequestID,
		now.UnixMilli(),
		now.UnixMilli(),
		id,
		accountID,
		FeishuResourceAccessStateSucceeded,
		now.UnixMilli(),
	)
	if err != nil {
		return fmt.Errorf("consume feishu resource access request: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect feishu resource access consumption: %w", err)
	}
	if count == 1 {
		return nil
	}
	request, err := scanFeishuResourceAccessRequest(s.db.QueryRow(
		feishuResourceAccessSelect+` WHERE id=? AND account_id=?`, id, accountID,
	))
	if err != nil {
		return err
	}
	if request.State != FeishuResourceAccessStateSucceeded {
		return ErrFeishuResourceAccessResolved
	}
	if !request.ExpiresAt.After(now) {
		return ErrFeishuResourceAccessExpired
	}
	if request.ConsumedByRequestID != "" {
		return ErrFeishuResourceAccessConsumed
	}
	return fmt.Errorf("consume feishu resource access request: no row updated")
}

// ClaimFeishuResourceAccessOAuth atomically consumes a valid OAuth state. A legacy
// PKCE verifier is returned to the caller for compatibility detection, while both
// stored one-time values are cleared.
func (s *Store) ClaimFeishuResourceAccessOAuth(stateHash, accountID string, now time.Time) (FeishuResourceAccessRequest, error) {
	if err := s.requireFeishuDocsStore(); err != nil {
		return FeishuResourceAccessRequest{}, err
	}
	stateHash = strings.TrimSpace(stateHash)
	accountID = strings.TrimSpace(accountID)
	if stateHash == "" || accountID == "" {
		return FeishuResourceAccessRequest{}, fmt.Errorf("feishu resource oauth state and account are required")
	}
	now = normalizedWorkflowTime(now)
	return s.claimFeishuResourceAccessOAuthRequest(now, func(tx *sql.Tx) (FeishuResourceAccessRequest, error) {
		return scanFeishuResourceAccessRequest(tx.QueryRow(
			feishuResourceAccessSelect+` WHERE oauth_state_hash=? AND account_id=?`, stateHash, accountID,
		))
	}, nil)
}

// ClaimFeishuResourceAccessOAuthFromCard atomically consumes a pending OAuth
// request from the exact requester, chat, and card. stateHash is optional only
// for a raw authorization-code submission from that bound card.
func (s *Store) ClaimFeishuResourceAccessOAuthFromCard(id, accountID, stateHash string, match FeishuResourceAccessMatch, now time.Time) (FeishuResourceAccessRequest, error) {
	if err := s.requireFeishuDocsStore(); err != nil {
		return FeishuResourceAccessRequest{}, err
	}
	id = strings.TrimSpace(id)
	accountID = strings.TrimSpace(accountID)
	stateHash = strings.TrimSpace(stateHash)
	match = normalizeFeishuResourceAccessMatch(match)
	if id == "" || accountID == "" || (match.ActorOpenID == "" && match.ActorUserID == "") || match.ChatID == "" || match.CardMessageID == "" {
		return FeishuResourceAccessRequest{}, fmt.Errorf("feishu resource oauth request, account, actor, chat, and card are required")
	}
	now = normalizedWorkflowTime(now)
	return s.claimFeishuResourceAccessOAuthRequest(now, func(tx *sql.Tx) (FeishuResourceAccessRequest, error) {
		return feishuResourceAccessByID(tx, id, accountID)
	}, func(request FeishuResourceAccessRequest) error {
		if !feishuResourceAccessActorMatches(request, match) {
			return ErrFeishuResourceAccessForbidden
		}
		if request.ChatID != match.ChatID || request.CardMessageID == "" || request.CardMessageID != match.CardMessageID {
			return ErrFeishuResourceAccessContextMismatch
		}
		if stateHash != "" && request.OAuthStateHash != stateHash {
			return ErrFeishuResourceAccessOAuthStateMismatch
		}
		return nil
	})
}

func (s *Store) claimFeishuResourceAccessOAuthRequest(
	now time.Time,
	load func(*sql.Tx) (FeishuResourceAccessRequest, error),
	validate func(FeishuResourceAccessRequest) error,
) (FeishuResourceAccessRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return FeishuResourceAccessRequest{}, fmt.Errorf("begin claim feishu resource oauth: %w", err)
	}
	defer tx.Rollback()
	request, err := load(tx)
	if err != nil {
		return FeishuResourceAccessRequest{}, err
	}
	if request.State != FeishuResourceAccessStatePending {
		return request, ErrFeishuResourceAccessResolved
	}
	if !now.Before(request.ExpiresAt) {
		if err := updateFeishuResourceAccessTerminal(tx, request.ID, request.AccountID, FeishuResourceAccessStateExpired, "", "", now); err != nil {
			return FeishuResourceAccessRequest{}, err
		}
		if err := tx.Commit(); err != nil {
			return FeishuResourceAccessRequest{}, fmt.Errorf("commit expired feishu resource oauth: %w", err)
		}
		request.State = FeishuResourceAccessStateExpired
		request.OAuthStateHash = ""
		request.PKCEVerifier = ""
		request.UpdatedAt = now
		return request, ErrFeishuResourceAccessExpired
	}
	if request.OAuthStateHash == "" {
		return request, ErrFeishuResourceAccessResolved
	}
	if validate != nil {
		if err := validate(request); err != nil {
			return request, err
		}
	}
	result, err := tx.Exec(
		`UPDATE feishu_resource_access_requests
		 SET state=?, oauth_state_hash='', pkce_verifier='', updated_at_ms=?
		 WHERE id=? AND account_id=? AND state=? AND oauth_state_hash<>''`,
		FeishuResourceAccessStateExecuting, now.UnixMilli(), request.ID, request.AccountID, FeishuResourceAccessStatePending,
	)
	if err != nil {
		return FeishuResourceAccessRequest{}, fmt.Errorf("claim feishu resource oauth: %w", err)
	}
	if err := requireOneFeishuResourceAccessRow(result); err != nil {
		return FeishuResourceAccessRequest{}, err
	}
	if err := updateWorkflowRequestState(tx, request.ID, request.AccountID, WorkflowRequestStateExecuting, now); err != nil {
		return FeishuResourceAccessRequest{}, err
	}
	if err := tx.Commit(); err != nil {
		return FeishuResourceAccessRequest{}, fmt.Errorf("commit claimed feishu resource oauth: %w", err)
	}
	request.State = FeishuResourceAccessStateExecuting
	request.OAuthStateHash = ""
	request.UpdatedAt = now
	return request, nil
}

// DenyFeishuResourceAccessRequest consumes a pending request from its exact user/card context.
func (s *Store) DenyFeishuResourceAccessRequest(id, accountID string, match FeishuResourceAccessMatch, now time.Time) (FeishuResourceAccessRequest, error) {
	if err := s.requireFeishuDocsStore(); err != nil {
		return FeishuResourceAccessRequest{}, err
	}
	id = strings.TrimSpace(id)
	accountID = strings.TrimSpace(accountID)
	match = normalizeFeishuResourceAccessMatch(match)
	if id == "" || accountID == "" {
		return FeishuResourceAccessRequest{}, fmt.Errorf("feishu resource access id and account are required")
	}
	now = normalizedWorkflowTime(now)
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return FeishuResourceAccessRequest{}, fmt.Errorf("begin deny feishu resource access: %w", err)
	}
	defer tx.Rollback()
	request, err := feishuResourceAccessByID(tx, id, accountID)
	if err != nil {
		return FeishuResourceAccessRequest{}, err
	}
	if request.State != FeishuResourceAccessStatePending {
		return request, ErrFeishuResourceAccessResolved
	}
	if !now.Before(request.ExpiresAt) {
		if err := updateFeishuResourceAccessTerminal(tx, request.ID, request.AccountID, FeishuResourceAccessStateExpired, "", "", now); err != nil {
			return FeishuResourceAccessRequest{}, err
		}
		if err := tx.Commit(); err != nil {
			return FeishuResourceAccessRequest{}, fmt.Errorf("commit expired feishu resource access: %w", err)
		}
		request.State = FeishuResourceAccessStateExpired
		request.PKCEVerifier = ""
		request.OAuthStateHash = ""
		request.UpdatedAt = now
		return request, ErrFeishuResourceAccessExpired
	}
	if !feishuResourceAccessActorMatches(request, match) {
		return request, ErrFeishuResourceAccessForbidden
	}
	if request.ChatID != match.ChatID || request.CardMessageID == "" || request.CardMessageID != match.CardMessageID {
		return request, ErrFeishuResourceAccessContextMismatch
	}
	if err := updateFeishuResourceAccessTerminal(tx, request.ID, request.AccountID, FeishuResourceAccessStateDenied, "", "", now); err != nil {
		return FeishuResourceAccessRequest{}, err
	}
	if err := tx.Commit(); err != nil {
		return FeishuResourceAccessRequest{}, fmt.Errorf("commit denied feishu resource access: %w", err)
	}
	request.State = FeishuResourceAccessStateDenied
	request.PKCEVerifier = ""
	request.OAuthStateHash = ""
	request.UpdatedAt = now
	return request, nil
}

// CompleteFeishuResourceAccessRequest marks a request as verified and
// optionally upserts the Feishu-side capability and chat-scoped LingoBridge
// grant in the same transaction.
func (s *Store) CompleteFeishuResourceAccessRequest(
	id, accountID, source, verifiedPermission string,
	capability *FeishuResourceCapability,
	grant *FeishuResourceGrant,
	now time.Time,
) error {
	if err := s.requireFeishuDocsStore(); err != nil {
		return err
	}
	id = strings.TrimSpace(id)
	accountID = strings.TrimSpace(accountID)
	source = strings.TrimSpace(source)
	verifiedPermission = strings.TrimSpace(verifiedPermission)
	if id == "" || accountID == "" || !validFeishuResourceGrantSource(source) || !validFeishuResourcePermission(verifiedPermission) {
		return fmt.Errorf("feishu resource access id, account, source, and verified permission are required")
	}
	now = normalizedWorkflowTime(now)
	var normalizedCapability FeishuResourceCapability
	var normalizedGrant FeishuResourceGrant
	subjectType := ""
	subjectID := ""
	if capability != nil {
		normalizedCapability = normalizeFeishuResourceCapability(*capability)
		if err := validateFeishuResourceCapability(normalizedCapability); err != nil {
			return err
		}
		if normalizedCapability.AccountID != accountID || normalizedCapability.SourceRequestID != id {
			return fmt.Errorf("feishu resource capability account and source request must match the access request")
		}
		if !FeishuResourcePermissionSatisfies(normalizedCapability.Permission, verifiedPermission) {
			return fmt.Errorf("feishu resource capability does not satisfy the verified permission")
		}
		subjectType = normalizedCapability.SubjectType
		subjectID = normalizedCapability.SubjectID
	}
	if grant != nil {
		normalizedGrant = normalizeFeishuResourceGrant(*grant)
		if err := validateFeishuResourceGrant(normalizedGrant); err != nil {
			return err
		}
		if normalizedGrant.AccountID != accountID || normalizedGrant.SourceRequestID != id {
			return fmt.Errorf("feishu resource grant account and source request must match the access request")
		}
		if !FeishuResourcePermissionSatisfies(normalizedGrant.Permission, verifiedPermission) {
			return fmt.Errorf("feishu resource grant does not satisfy the verified permission")
		}
	}
	if capability != nil && grant != nil {
		if normalizedCapability.ResourceType != normalizedGrant.ResourceType ||
			normalizedCapability.ResourceToken != normalizedGrant.ResourceToken {
			return fmt.Errorf("feishu resource capability and grant must describe the same resource")
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin complete feishu resource access: %w", err)
	}
	defer tx.Rollback()
	request, err := feishuResourceAccessByID(tx, id, accountID)
	if err != nil {
		return err
	}
	if capability != nil {
		if normalizedCapability.ResourceType != request.ResourceType || normalizedCapability.ResourceToken != request.ResourceToken {
			return fmt.Errorf("feishu resource capability does not match the access request resource")
		}
		if request.SubjectType != "" && (normalizedCapability.SubjectType != request.SubjectType || normalizedCapability.SubjectID != request.SubjectID) {
			return fmt.Errorf("feishu resource capability does not match the access request subject")
		}
	}
	if grant != nil {
		actorType, actorID, ok := feishuResourceAccessGrantActor(request)
		if !ok || normalizedGrant.ActorType != actorType || normalizedGrant.ActorID != actorID {
			return fmt.Errorf("feishu resource grant does not match the access request actor")
		}
		if normalizedGrant.ChatID != request.ChatID || normalizedGrant.ResourceType != request.ResourceType || normalizedGrant.ResourceToken != request.ResourceToken {
			return fmt.Errorf("feishu resource grant does not match the access request scope")
		}
	}
	result, err := tx.Exec(
		`UPDATE feishu_resource_access_requests
		 SET state=?, grant_source=?, verified_permission=?,
		 subject_type=CASE WHEN ?='' THEN subject_type ELSE ? END,
		 subject_id=CASE WHEN ?='' THEN subject_id ELSE ? END,
		 oauth_state_hash='', pkce_verifier='', updated_at_ms=?
		 WHERE id=? AND account_id=? AND state IN (?, ?)`,
		FeishuResourceAccessStateSucceeded, source, verifiedPermission,
		subjectType, subjectType, subjectID, subjectID, now.UnixMilli(),
		id, accountID, FeishuResourceAccessStatePending, FeishuResourceAccessStateExecuting,
	)
	if err != nil {
		return fmt.Errorf("complete feishu resource access: %w", err)
	}
	if err := requireOneFeishuResourceAccessRow(result); err != nil {
		return err
	}
	if capability != nil {
		if err := upsertFeishuResourceCapability(tx, normalizedCapability); err != nil {
			return err
		}
	}
	if grant != nil {
		if err := upsertFeishuResourceGrant(tx, normalizedGrant); err != nil {
			return err
		}
	}
	if err := updateWorkflowRequestState(tx, id, accountID, WorkflowRequestStateSucceeded, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit completed feishu resource access: %w", err)
	}
	return nil
}

// FailFeishuResourceAccessRequest closes a pending or executing request and clears one-time secrets.
func (s *Store) FailFeishuResourceAccessRequest(id, accountID string, now time.Time) error {
	if err := s.requireFeishuDocsStore(); err != nil {
		return err
	}
	id = strings.TrimSpace(id)
	accountID = strings.TrimSpace(accountID)
	if id == "" || accountID == "" {
		return fmt.Errorf("feishu resource access id and account are required")
	}
	now = normalizedWorkflowTime(now)
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin fail feishu resource access: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.Exec(
		`UPDATE feishu_resource_access_requests
		 SET state=?, oauth_state_hash='', pkce_verifier='', updated_at_ms=?
		 WHERE id=? AND account_id=? AND state IN (?, ?)`,
		FeishuResourceAccessStateFailed, now.UnixMilli(), id, accountID,
		FeishuResourceAccessStatePending, FeishuResourceAccessStateExecuting,
	)
	if err != nil {
		return fmt.Errorf("fail feishu resource access: %w", err)
	}
	if err := requireOneFeishuResourceAccessRow(result); err != nil {
		return err
	}
	if err := updateWorkflowRequestState(tx, id, accountID, WorkflowRequestStateFailed, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit failed feishu resource access: %w", err)
	}
	return nil
}

// ExpireFeishuResourceAccessRequests expires stale pending OAuth requests.
func (s *Store) ExpireFeishuResourceAccessRequests(accountID string, now time.Time) (int64, error) {
	if err := s.requireFeishuDocsStore(); err != nil {
		return 0, err
	}
	accountID = strings.TrimSpace(accountID)
	now = normalizedWorkflowTime(now)
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin expire feishu resource access requests: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.Exec(
		`UPDATE feishu_resource_access_requests
		 SET state=?, oauth_state_hash='', pkce_verifier='', updated_at_ms=?
		 WHERE account_id=? AND state=? AND expires_at_ms<=?`,
		FeishuResourceAccessStateExpired, now.UnixMilli(), accountID,
		FeishuResourceAccessStatePending, now.UnixMilli(),
	)
	if err != nil {
		return 0, fmt.Errorf("expire feishu resource access requests: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("inspect expired feishu resource access requests: %w", err)
	}
	if _, err := tx.Exec(
		`UPDATE workflow_requests SET state=?, updated_at_ms=?
		 WHERE account_id=? AND kind=? AND state=?
		 AND id IN (SELECT id FROM feishu_resource_access_requests WHERE account_id=? AND state=? AND updated_at_ms=?)`,
		WorkflowRequestStateExpired, now.UnixMilli(), accountID, WorkflowRequestKindFeishuResourceAccess,
		WorkflowRequestStatePending, accountID, FeishuResourceAccessStateExpired, now.UnixMilli(),
	); err != nil {
		return 0, fmt.Errorf("expire feishu resource access workflows: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit expired feishu resource access requests: %w", err)
	}
	return count, nil
}

// FailExecutingFeishuResourceAccessRequests closes callbacks interrupted by a restart.
func (s *Store) FailExecutingFeishuResourceAccessRequests(accountID string, now time.Time) (int64, error) {
	if err := s.requireFeishuDocsStore(); err != nil {
		return 0, err
	}
	accountID = strings.TrimSpace(accountID)
	now = normalizedWorkflowTime(now)
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin fail executing feishu resource access requests: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.Exec(
		`UPDATE feishu_resource_access_requests
		 SET state=?, oauth_state_hash='', pkce_verifier='', updated_at_ms=?
		 WHERE account_id=? AND state=?`,
		FeishuResourceAccessStateFailed, now.UnixMilli(), accountID, FeishuResourceAccessStateExecuting,
	)
	if err != nil {
		return 0, fmt.Errorf("fail executing feishu resource access requests: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("inspect failed feishu resource access requests: %w", err)
	}
	if _, err := tx.Exec(
		`UPDATE workflow_requests SET state=?, updated_at_ms=?
		 WHERE account_id=? AND kind=? AND state=?`,
		WorkflowRequestStateFailed, now.UnixMilli(), accountID,
		WorkflowRequestKindFeishuResourceAccess, WorkflowRequestStateExecuting,
	); err != nil {
		return 0, fmt.Errorf("fail executing feishu resource access workflows: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit failed executing feishu resource access requests: %w", err)
	}
	return count, nil
}

// UpsertFeishuResourceGrant creates, renews, or upgrades one exact chat-scoped grant.
func (s *Store) UpsertFeishuResourceGrant(grant FeishuResourceGrant) (FeishuResourceGrant, error) {
	if err := s.requireFeishuDocsStore(); err != nil {
		return FeishuResourceGrant{}, err
	}
	grant = normalizeFeishuResourceGrant(grant)
	if err := validateFeishuResourceGrant(grant); err != nil {
		return FeishuResourceGrant{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := upsertFeishuResourceGrant(s.db, grant); err != nil {
		return FeishuResourceGrant{}, err
	}
	return grant, nil
}

// ActiveFeishuResourceGrant returns an active exact-chat grant satisfying the requested permission.
func (s *Store) ActiveFeishuResourceGrant(accountID, actorType, actorID, chatID, resourceType, resourceToken, permission string, now time.Time) (FeishuResourceGrant, bool, error) {
	if err := s.requireFeishuDocsStore(); err != nil {
		return FeishuResourceGrant{}, false, err
	}
	permission = strings.TrimSpace(permission)
	if !validFeishuResourcePermission(permission) {
		return FeishuResourceGrant{}, false, fmt.Errorf("valid feishu resource permission is required")
	}
	actorType = strings.TrimSpace(actorType)
	actorID = strings.TrimSpace(actorID)
	if !validFeishuResourceGrantActor(actorType, actorID) {
		return FeishuResourceGrant{}, false, fmt.Errorf("valid feishu resource grant actor is required")
	}
	now = normalizedWorkflowTime(now)
	grant, err := scanFeishuResourceGrant(s.db.QueryRow(
		`SELECT account_id, actor_type, actor_id, chat_id, resource_type, resource_token,
		 permission, grant_mode, source_request_id, state, expires_at_ms,
		 created_at_ms, updated_at_ms
		 FROM feishu_resource_grants
		 WHERE account_id=? AND actor_type=? AND actor_id=? AND chat_id=?
		 AND resource_type=? AND resource_token=? AND state=?
		 AND (permission=? OR (?='read' AND permission='write'))
		 AND (grant_mode='all' OR expires_at_ms>?)
		 ORDER BY CASE WHEN grant_mode='all' THEN 1 ELSE 0 END DESC,
		          CASE WHEN permission=? THEN 1 ELSE 0 END DESC
		 LIMIT 1`,
		strings.TrimSpace(accountID),
		actorType,
		actorID,
		strings.TrimSpace(chatID),
		strings.TrimSpace(resourceType),
		strings.TrimSpace(resourceToken),
		FeishuResourceGrantStateActive,
		permission,
		permission,
		now.UnixMilli(),
		permission,
	))
	if errors.Is(err, ErrFeishuResourceGrantNotFound) {
		return FeishuResourceGrant{}, false, nil
	}
	if err != nil {
		return FeishuResourceGrant{}, false, err
	}
	if !FeishuResourcePermissionSatisfies(grant.Permission, permission) {
		return FeishuResourceGrant{}, false, nil
	}
	return grant, true, nil
}

// ExpireFeishuResourceGrants marks elapsed once grants as expired. Permanent
// all grants have no local expiry and are not changed.
func (s *Store) ExpireFeishuResourceGrants(accountID string, now time.Time) (int64, error) {
	if err := s.requireFeishuDocsStore(); err != nil {
		return 0, err
	}
	accountID = strings.TrimSpace(accountID)
	now = normalizedWorkflowTime(now)
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.db.Exec(
		`UPDATE feishu_resource_grants SET state=?, updated_at_ms=?
		 WHERE account_id=? AND state=? AND grant_mode=? AND expires_at_ms<=?`,
		FeishuResourceGrantStateExpired,
		now.UnixMilli(),
		accountID,
		FeishuResourceGrantStateActive,
		FeishuResourceGrantModeOnce,
		now.UnixMilli(),
	)
	if err != nil {
		return 0, fmt.Errorf("expire feishu resource grants: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("inspect expired feishu resource grants: %w", err)
	}
	return count, nil
}

// RevokeFeishuResourceGrant marks an exact chat-scoped grant unusable after a failed live check.
func (s *Store) RevokeFeishuResourceGrant(accountID, chatID, resourceType, resourceToken string, now time.Time) error {
	if err := s.requireFeishuDocsStore(); err != nil {
		return err
	}
	now = normalizedWorkflowTime(now)
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.db.Exec(
		`UPDATE feishu_resource_grants SET state=?, updated_at_ms=?
		 WHERE account_id=? AND chat_id=? AND resource_type=? AND resource_token=?`,
		FeishuResourceGrantStateRevoked, now.UnixMilli(),
		strings.TrimSpace(accountID), strings.TrimSpace(chatID), strings.TrimSpace(resourceType), strings.TrimSpace(resourceToken),
	)
	if err != nil {
		return fmt.Errorf("revoke feishu resource grant: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect revoked feishu resource grant: %w", err)
	}
	if count == 0 {
		return ErrFeishuResourceGrantNotFound
	}
	return nil
}

// FeishuResourcePermissionSatisfies reports whether granted covers requested.
func FeishuResourcePermissionSatisfies(granted, requested string) bool {
	granted = strings.TrimSpace(granted)
	requested = strings.TrimSpace(requested)
	return granted == FeishuResourcePermissionWrite || granted == requested
}

const feishuResourceAccessSelect = `SELECT id, account_id, actor_open_id, actor_user_id,
 chat_id, source_message_id, resource_type, resource_token, resource_url,
 permission, reason, subject_type, subject_id, grant_source, verified_permission,
 card_message_id, oauth_state_hash, pkce_verifier, state,
 consumed_by_request_id, consumed_at_ms, created_at_ms, expires_at_ms, updated_at_ms
 FROM feishu_resource_access_requests`

type feishuResourceAccessScanner interface {
	Scan(dest ...any) error
}

func scanFeishuBotResource(row feishuResourceAccessScanner) (FeishuBotResource, error) {
	var resource FeishuBotResource
	var createdAtMS, updatedAtMS int64
	if err := row.Scan(
		&resource.AccountID,
		&resource.ResourceType,
		&resource.ResourceToken,
		&resource.ParentToken,
		&resource.Name,
		&resource.URL,
		&resource.SourceRequestID,
		&createdAtMS,
		&updatedAtMS,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return FeishuBotResource{}, ErrFeishuBotResourceNotFound
		}
		return FeishuBotResource{}, fmt.Errorf("get feishu bot resource: %w", err)
	}
	resource.CreatedAt = time.UnixMilli(createdAtMS).UTC()
	resource.UpdatedAt = time.UnixMilli(updatedAtMS).UTC()
	return resource, nil
}

func scanFeishuResourceAccessRequest(row feishuResourceAccessScanner) (FeishuResourceAccessRequest, error) {
	var request FeishuResourceAccessRequest
	var consumedAtMS, createdAtMS, expiresAtMS, updatedAtMS int64
	if err := row.Scan(
		&request.ID,
		&request.AccountID,
		&request.ActorOpenID,
		&request.ActorUserID,
		&request.ChatID,
		&request.SourceMessageID,
		&request.ResourceType,
		&request.ResourceToken,
		&request.ResourceURL,
		&request.Permission,
		&request.Reason,
		&request.SubjectType,
		&request.SubjectID,
		&request.GrantSource,
		&request.VerifiedPermission,
		&request.CardMessageID,
		&request.OAuthStateHash,
		&request.PKCEVerifier,
		&request.State,
		&request.ConsumedByRequestID,
		&consumedAtMS,
		&createdAtMS,
		&expiresAtMS,
		&updatedAtMS,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return FeishuResourceAccessRequest{}, ErrFeishuResourceAccessNotFound
		}
		return FeishuResourceAccessRequest{}, fmt.Errorf("get feishu resource access request: %w", err)
	}
	request.CreatedAt = time.UnixMilli(createdAtMS).UTC()
	request.ExpiresAt = time.UnixMilli(expiresAtMS).UTC()
	request.UpdatedAt = time.UnixMilli(updatedAtMS).UTC()
	if consumedAtMS > 0 {
		request.ConsumedAt = time.UnixMilli(consumedAtMS).UTC()
	}
	return request, nil
}

func scanFeishuResourceGrant(row feishuResourceAccessScanner) (FeishuResourceGrant, error) {
	var grant FeishuResourceGrant
	var expiresAtMS, createdAtMS, updatedAtMS int64
	if err := row.Scan(
		&grant.AccountID,
		&grant.ActorType,
		&grant.ActorID,
		&grant.ChatID,
		&grant.ResourceType,
		&grant.ResourceToken,
		&grant.Permission,
		&grant.GrantMode,
		&grant.SourceRequestID,
		&grant.State,
		&expiresAtMS,
		&createdAtMS,
		&updatedAtMS,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return FeishuResourceGrant{}, ErrFeishuResourceGrantNotFound
		}
		return FeishuResourceGrant{}, fmt.Errorf("get feishu resource grant: %w", err)
	}
	grant.ExpiresAt = timeFromOptionalMillis(expiresAtMS)
	grant.CreatedAt = time.UnixMilli(createdAtMS).UTC()
	grant.UpdatedAt = time.UnixMilli(updatedAtMS).UTC()
	return grant, nil
}

func normalizeFeishuBotResource(resource FeishuBotResource) FeishuBotResource {
	resource.AccountID = strings.TrimSpace(resource.AccountID)
	resource.ResourceType = strings.TrimSpace(resource.ResourceType)
	resource.ResourceToken = strings.TrimSpace(resource.ResourceToken)
	resource.ParentToken = strings.TrimSpace(resource.ParentToken)
	resource.Name = strings.TrimSpace(resource.Name)
	resource.URL = strings.TrimSpace(resource.URL)
	resource.SourceRequestID = strings.TrimSpace(resource.SourceRequestID)
	resource.CreatedAt = normalizedWorkflowTime(resource.CreatedAt)
	resource.UpdatedAt = resource.CreatedAt
	return resource
}

func validateFeishuBotResource(resource FeishuBotResource) error {
	if resource.AccountID == "" || resource.ResourceType == "" || resource.ResourceToken == "" {
		return fmt.Errorf("feishu bot resource account, type, and token are required")
	}
	return nil
}

func normalizeFeishuResourceAccessRequest(request FeishuResourceAccessRequest) FeishuResourceAccessRequest {
	request.ID = strings.TrimSpace(request.ID)
	request.AccountID = strings.TrimSpace(request.AccountID)
	request.ActorOpenID = strings.TrimSpace(request.ActorOpenID)
	request.ActorUserID = strings.TrimSpace(request.ActorUserID)
	request.ChatID = strings.TrimSpace(request.ChatID)
	request.SourceMessageID = strings.TrimSpace(request.SourceMessageID)
	request.ResourceType = strings.TrimSpace(request.ResourceType)
	request.ResourceToken = strings.TrimSpace(request.ResourceToken)
	request.ResourceURL = strings.TrimSpace(request.ResourceURL)
	request.Permission = strings.TrimSpace(request.Permission)
	request.Reason = strings.TrimSpace(request.Reason)
	request.SubjectType = strings.TrimSpace(request.SubjectType)
	request.SubjectID = strings.TrimSpace(request.SubjectID)
	request.CreatedAt = normalizedWorkflowTime(request.CreatedAt)
	request.ExpiresAt = request.ExpiresAt.UTC()
	return request
}

func validateNewFeishuResourceAccessRequest(request FeishuResourceAccessRequest) error {
	if request.AccountID == "" || request.ChatID == "" || request.ResourceType == "" || request.ResourceToken == "" || !validFeishuResourcePermission(request.Permission) {
		return fmt.Errorf("feishu resource access account, chat, resource, and valid permission are required")
	}
	if request.ActorOpenID == "" && request.ActorUserID == "" {
		return fmt.Errorf("feishu resource access requesting user is required")
	}
	if !request.ExpiresAt.After(request.CreatedAt) {
		return fmt.Errorf("feishu resource access expires_at must be after created_at")
	}
	return nil
}

func normalizeFeishuResourceAccessMatch(match FeishuResourceAccessMatch) FeishuResourceAccessMatch {
	match.ActorOpenID = strings.TrimSpace(match.ActorOpenID)
	match.ActorUserID = strings.TrimSpace(match.ActorUserID)
	match.ChatID = strings.TrimSpace(match.ChatID)
	match.CardMessageID = strings.TrimSpace(match.CardMessageID)
	return match
}

func normalizeFeishuResourceGrant(grant FeishuResourceGrant) FeishuResourceGrant {
	grant.AccountID = strings.TrimSpace(grant.AccountID)
	grant.ActorType = strings.TrimSpace(grant.ActorType)
	grant.ActorID = strings.TrimSpace(grant.ActorID)
	grant.ChatID = strings.TrimSpace(grant.ChatID)
	grant.ResourceType = strings.TrimSpace(grant.ResourceType)
	grant.ResourceToken = strings.TrimSpace(grant.ResourceToken)
	grant.Permission = strings.TrimSpace(grant.Permission)
	grant.GrantMode = strings.TrimSpace(grant.GrantMode)
	grant.SourceRequestID = strings.TrimSpace(grant.SourceRequestID)
	grant.State = strings.TrimSpace(grant.State)
	if grant.State == "" {
		grant.State = FeishuResourceGrantStateActive
	}
	grant.ExpiresAt = optionalUTC(grant.ExpiresAt)
	grant.CreatedAt = normalizedWorkflowTime(grant.CreatedAt)
	if grant.UpdatedAt.IsZero() {
		grant.UpdatedAt = grant.CreatedAt
	} else {
		grant.UpdatedAt = grant.UpdatedAt.UTC()
	}
	return grant
}

func validateFeishuResourceGrant(grant FeishuResourceGrant) error {
	if grant.AccountID == "" || !validFeishuResourceGrantActor(grant.ActorType, grant.ActorID) ||
		grant.ChatID == "" || grant.ResourceType == "" || grant.ResourceToken == "" ||
		grant.SourceRequestID == "" || !validFeishuResourcePermission(grant.Permission) {
		return fmt.Errorf("feishu resource grant account, actor, chat, resource, permission, and source request are required")
	}
	if grant.State != FeishuResourceGrantStateActive && grant.State != FeishuResourceGrantStateRevoked && grant.State != FeishuResourceGrantStateExpired {
		return fmt.Errorf("unsupported feishu resource grant state %q", grant.State)
	}
	if grant.GrantMode != FeishuResourceGrantModeOnce && grant.GrantMode != FeishuResourceGrantModeAll {
		return fmt.Errorf("unsupported feishu resource grant mode %q", grant.GrantMode)
	}
	if grant.GrantMode == FeishuResourceGrantModeOnce && !grant.ExpiresAt.After(grant.CreatedAt) {
		return fmt.Errorf("once feishu resource grant expires_at must be after created_at")
	}
	if grant.GrantMode == FeishuResourceGrantModeAll && !grant.ExpiresAt.IsZero() {
		return fmt.Errorf("all feishu resource grant must not have expires_at")
	}
	return nil
}

func validFeishuResourceGrantActor(actorType, actorID string) bool {
	if strings.TrimSpace(actorID) == "" {
		return false
	}
	return actorType == FeishuResourceGrantActorTypeOpenID || actorType == FeishuResourceGrantActorTypeUserID
}

func validFeishuResourcePermission(permission string) bool {
	return permission == FeishuResourcePermissionRead || permission == FeishuResourcePermissionWrite
}

func validFeishuResourceGrantSource(source string) bool {
	return source == FeishuResourceGrantSourceBotOwner || source == FeishuResourceGrantSourceExistingGrant || source == FeishuResourceGrantSourceNewlyGranted
}

func validFeishuResourceAccessTerminalState(state string) bool {
	return state == FeishuResourceAccessStateDenied || state == FeishuResourceAccessStateFailed || state == FeishuResourceAccessStateExpired
}

func feishuResourceAccessActorMatches(request FeishuResourceAccessRequest, match FeishuResourceAccessMatch) bool {
	if request.ActorOpenID != "" {
		return request.ActorOpenID == match.ActorOpenID
	}
	return request.ActorUserID != "" && request.ActorUserID == match.ActorUserID
}

func feishuResourceAccessGrantActor(request FeishuResourceAccessRequest) (string, string, bool) {
	if request.ActorOpenID != "" {
		return FeishuResourceGrantActorTypeOpenID, request.ActorOpenID, true
	}
	if request.ActorUserID != "" {
		return FeishuResourceGrantActorTypeUserID, request.ActorUserID, true
	}
	return "", "", false
}

type feishuResourceAccessQueryer interface {
	QueryRow(query string, args ...any) *sql.Row
}

func feishuResourceAccessByID(queryer feishuResourceAccessQueryer, id, accountID string) (FeishuResourceAccessRequest, error) {
	return scanFeishuResourceAccessRequest(queryer.QueryRow(
		feishuResourceAccessSelect+` WHERE id=? AND account_id=?`, id, accountID,
	))
}

func updateFeishuResourceAccessTerminal(tx *sql.Tx, id, accountID, state, source, permission string, now time.Time) error {
	if !validFeishuResourceAccessTerminalState(state) {
		return fmt.Errorf("unsupported terminal feishu resource access state %q", state)
	}
	result, err := tx.Exec(
		`UPDATE feishu_resource_access_requests
		 SET state=?, grant_source=?, verified_permission=?, oauth_state_hash='', pkce_verifier='', updated_at_ms=?
		 WHERE id=? AND account_id=? AND state=?`,
		state, source, permission, now.UnixMilli(), id, accountID, FeishuResourceAccessStatePending,
	)
	if err != nil {
		return fmt.Errorf("update terminal feishu resource access: %w", err)
	}
	if err := requireOneFeishuResourceAccessRow(result); err != nil {
		return err
	}
	return updateWorkflowRequestState(tx, id, accountID, state, now)
}

type feishuResourceGrantExecer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func upsertFeishuResourceGrant(execer feishuResourceGrantExecer, grant FeishuResourceGrant) error {
	_, err := execer.Exec(
		`INSERT INTO feishu_resource_grants (
			account_id, actor_type, actor_id, chat_id, resource_type, resource_token,
			permission, grant_mode, source_request_id, state, expires_at_ms,
			created_at_ms, updated_at_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(account_id, actor_type, actor_id, chat_id, resource_type, resource_token, permission) DO UPDATE SET
			grant_mode=CASE
				WHEN (feishu_resource_grants.state='active' AND feishu_resource_grants.grant_mode='all')
					OR excluded.grant_mode='all' THEN 'all'
				ELSE 'once'
			END,
			source_request_id=excluded.source_request_id,
			state=excluded.state,
			expires_at_ms=CASE
				WHEN (feishu_resource_grants.state='active' AND feishu_resource_grants.grant_mode='all')
					OR excluded.grant_mode='all' THEN 0
				WHEN feishu_resource_grants.state='active' AND feishu_resource_grants.expires_at_ms>excluded.expires_at_ms
					THEN feishu_resource_grants.expires_at_ms
				ELSE excluded.expires_at_ms
			END,
			updated_at_ms=excluded.updated_at_ms`,
		grant.AccountID,
		grant.ActorType,
		grant.ActorID,
		grant.ChatID,
		grant.ResourceType,
		grant.ResourceToken,
		grant.Permission,
		grant.GrantMode,
		grant.SourceRequestID,
		grant.State,
		optionalTimeMillis(grant.ExpiresAt),
		grant.CreatedAt.UnixMilli(),
		grant.UpdatedAt.UnixMilli(),
	)
	if err != nil {
		return fmt.Errorf("upsert feishu resource grant: %w", err)
	}
	return nil
}

func requireOneFeishuResourceAccessRow(result sql.Result) error {
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect feishu resource access update: %w", err)
	}
	if count != 1 {
		return ErrFeishuResourceAccessResolved
	}
	return nil
}
