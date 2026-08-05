package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrFeishuUserOAuthCredentialNotFound = errors.New("feishu user oauth credential not found")
	ErrFeishuUserOAuthCredentialConflict = errors.New("feishu user oauth credential version conflict")
	ErrFeishuUserOAuthIdentityConflict   = errors.New("feishu user oauth identities belong to different credentials")
	ErrFeishuUserOAuthIdentityChanged    = errors.New("feishu user oauth credential identity changed")
)

const (
	FeishuUserOAuthCredentialStatusActive         = "active"
	FeishuUserOAuthCredentialStatusReauthRequired = "reauth_required"
)

// FeishuUserOAuthCredential stores only encrypted OAuth tokens. Plaintext
// access and refresh tokens must be encrypted before crossing this API.
type FeishuUserOAuthCredential struct {
	ID                     string
	AccountID              string
	ActorOpenID            string
	ActorUserID            string
	AccessTokenCiphertext  string
	AccessTokenExpiresAt   time.Time
	RefreshTokenCiphertext string
	RefreshTokenExpiresAt  time.Time
	Scopes                 string
	AuthorizedAt           time.Time
	LastRefreshedAt        time.Time
	ReauthorizeAt          time.Time
	Status                 string
	Version                int64
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

const feishuUserOAuthCredentialSelect = `SELECT id, account_id, actor_open_id, actor_user_id,
	access_token_ciphertext, access_token_expires_at_ms,
	refresh_token_ciphertext, refresh_token_expires_at_ms,
	scopes, authorized_at_ms, last_refreshed_at_ms, reauthorize_at_ms,
	status, version, created_at_ms, updated_at_ms
	FROM feishu_user_oauth_credentials`

// SaveFeishuUserOAuthCredential inserts a newly authorized credential or
// replaces the credential already associated with either verified user ID.
func (s *Store) SaveFeishuUserOAuthCredential(credential FeishuUserOAuthCredential) (FeishuUserOAuthCredential, error) {
	if err := s.requireFeishuDocsStore(); err != nil {
		return FeishuUserOAuthCredential{}, err
	}
	credential = normalizeFeishuUserOAuthCredential(credential)
	if err := validateFeishuUserOAuthCredential(credential); err != nil {
		return FeishuUserOAuthCredential{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := beginFeishuOAuthCredentialTx(s.db)
	if err != nil {
		return FeishuUserOAuthCredential{}, fmt.Errorf("begin save feishu user oauth credential: %w", err)
	}
	defer tx.Rollback()

	existing, found, err := findFeishuUserOAuthCredential(tx, credential.AccountID, credential.ActorOpenID, credential.ActorUserID)
	if err != nil {
		return FeishuUserOAuthCredential{}, err
	}
	if found {
		incomingIdentityType, incomingIdentityID := feishuUserOAuthCredentialIdentityKey(credential)
		existingIdentityType, existingIdentityID := feishuUserOAuthCredentialIdentityKey(existing)
		if incomingIdentityType != existingIdentityType || incomingIdentityID != existingIdentityID {
			// Ciphertext is authenticated against the identity supplied by the
			// caller. Do not silently add a higher-priority alias after encryption;
			// return the canonical stored identity so the caller can re-encrypt.
			return existing, ErrFeishuUserOAuthIdentityChanged
		}
		credential.ID = existing.ID
		credential.CreatedAt = existing.CreatedAt
		credential.Version = existing.Version + 1
		if credential.ActorOpenID == "" {
			credential.ActorOpenID = existing.ActorOpenID
		}
		if credential.ActorUserID == "" {
			credential.ActorUserID = existing.ActorUserID
		}
		result, updateErr := tx.Exec(
			`UPDATE feishu_user_oauth_credentials SET
			 actor_open_id=?, actor_user_id=?,
			 access_token_ciphertext=?, access_token_expires_at_ms=?,
			 refresh_token_ciphertext=?, refresh_token_expires_at_ms=?,
			 scopes=?, authorized_at_ms=?, last_refreshed_at_ms=?, reauthorize_at_ms=?,
			 status=?, version=?, updated_at_ms=?
			 WHERE id=? AND account_id=? AND version=?`,
			credential.ActorOpenID,
			credential.ActorUserID,
			credential.AccessTokenCiphertext,
			credential.AccessTokenExpiresAt.UnixMilli(),
			credential.RefreshTokenCiphertext,
			optionalTimeMillis(credential.RefreshTokenExpiresAt),
			credential.Scopes,
			credential.AuthorizedAt.UnixMilli(),
			optionalTimeMillis(credential.LastRefreshedAt),
			credential.ReauthorizeAt.UnixMilli(),
			credential.Status,
			credential.Version,
			credential.UpdatedAt.UnixMilli(),
			credential.ID,
			credential.AccountID,
			existing.Version,
		)
		if updateErr != nil {
			return FeishuUserOAuthCredential{}, fmt.Errorf("replace feishu user oauth credential: %w", updateErr)
		}
		if err := requireOneFeishuUserOAuthCredentialRow(result); err != nil {
			return FeishuUserOAuthCredential{}, err
		}
	} else {
		credential.ID, err = generateID()
		if err != nil {
			return FeishuUserOAuthCredential{}, err
		}
		credential.Version = 1
		_, err = tx.Exec(
			`INSERT INTO feishu_user_oauth_credentials (
			 id, account_id, actor_open_id, actor_user_id,
			 access_token_ciphertext, access_token_expires_at_ms,
			 refresh_token_ciphertext, refresh_token_expires_at_ms,
			 scopes, authorized_at_ms, last_refreshed_at_ms, reauthorize_at_ms,
			 status, version, created_at_ms, updated_at_ms
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			credential.ID,
			credential.AccountID,
			credential.ActorOpenID,
			credential.ActorUserID,
			credential.AccessTokenCiphertext,
			credential.AccessTokenExpiresAt.UnixMilli(),
			credential.RefreshTokenCiphertext,
			optionalTimeMillis(credential.RefreshTokenExpiresAt),
			credential.Scopes,
			credential.AuthorizedAt.UnixMilli(),
			optionalTimeMillis(credential.LastRefreshedAt),
			credential.ReauthorizeAt.UnixMilli(),
			credential.Status,
			credential.Version,
			credential.CreatedAt.UnixMilli(),
			credential.UpdatedAt.UnixMilli(),
		)
		if err != nil {
			return FeishuUserOAuthCredential{}, fmt.Errorf("insert feishu user oauth credential: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return FeishuUserOAuthCredential{}, fmt.Errorf("commit feishu user oauth credential: %w", err)
	}
	return credential, nil
}

func feishuUserOAuthCredentialIdentityKey(credential FeishuUserOAuthCredential) (string, string) {
	if credential.ActorOpenID != "" {
		return "open_id", credential.ActorOpenID
	}
	return "user_id", credential.ActorUserID
}

// GetFeishuUserOAuthCredential returns the credential matching either trusted
// actor ID. If both IDs resolve to different rows, it fails closed.
func (s *Store) GetFeishuUserOAuthCredential(accountID, actorOpenID, actorUserID string) (FeishuUserOAuthCredential, error) {
	if err := s.requireFeishuDocsStore(); err != nil {
		return FeishuUserOAuthCredential{}, err
	}
	credential, found, err := findFeishuUserOAuthCredential(s.db, strings.TrimSpace(accountID), strings.TrimSpace(actorOpenID), strings.TrimSpace(actorUserID))
	if err != nil {
		return FeishuUserOAuthCredential{}, err
	}
	if !found {
		return FeishuUserOAuthCredential{}, ErrFeishuUserOAuthCredentialNotFound
	}
	return credential, nil
}

// GetFeishuUserOAuthCredentialByID loads one exact credential for durable
// refresh recovery without resolving mutable actor aliases.
func (s *Store) GetFeishuUserOAuthCredentialByID(id, accountID string) (FeishuUserOAuthCredential, error) {
	if err := s.requireFeishuDocsStore(); err != nil {
		return FeishuUserOAuthCredential{}, err
	}
	return scanFeishuUserOAuthCredential(s.db.QueryRow(
		feishuUserOAuthCredentialSelect+` WHERE id=? AND account_id=?`,
		strings.TrimSpace(id),
		strings.TrimSpace(accountID),
	))
}

// RotateFeishuUserOAuthCredential atomically replaces both one-time token
// values only when the caller still owns the loaded version.
func (s *Store) RotateFeishuUserOAuthCredential(credential FeishuUserOAuthCredential, expectedVersion int64) (FeishuUserOAuthCredential, error) {
	if err := s.requireFeishuDocsStore(); err != nil {
		return FeishuUserOAuthCredential{}, err
	}
	credential = normalizeFeishuUserOAuthCredential(credential)
	if err := validateFeishuUserOAuthCredential(credential); err != nil {
		return FeishuUserOAuthCredential{}, err
	}
	if credential.ID == "" || expectedVersion <= 0 {
		return FeishuUserOAuthCredential{}, fmt.Errorf("feishu user oauth credential id and expected version are required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	credential.Version = expectedVersion + 1
	result, err := s.db.Exec(
		`UPDATE feishu_user_oauth_credentials SET
		 actor_open_id=?, actor_user_id=?,
		 access_token_ciphertext=?, access_token_expires_at_ms=?,
		 refresh_token_ciphertext=?, refresh_token_expires_at_ms=?,
		 scopes=?, authorized_at_ms=?, last_refreshed_at_ms=?, reauthorize_at_ms=?,
		 status=?, version=?, updated_at_ms=?
		 WHERE id=? AND account_id=? AND version=?`,
		credential.ActorOpenID,
		credential.ActorUserID,
		credential.AccessTokenCiphertext,
		credential.AccessTokenExpiresAt.UnixMilli(),
		credential.RefreshTokenCiphertext,
		optionalTimeMillis(credential.RefreshTokenExpiresAt),
		credential.Scopes,
		credential.AuthorizedAt.UnixMilli(),
		optionalTimeMillis(credential.LastRefreshedAt),
		credential.ReauthorizeAt.UnixMilli(),
		credential.Status,
		credential.Version,
		credential.UpdatedAt.UnixMilli(),
		credential.ID,
		credential.AccountID,
		expectedVersion,
	)
	if err != nil {
		return FeishuUserOAuthCredential{}, fmt.Errorf("rotate feishu user oauth credential: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return FeishuUserOAuthCredential{}, fmt.Errorf("inspect feishu user oauth credential rotation: %w", err)
	}
	if count != 1 {
		return FeishuUserOAuthCredential{}, ErrFeishuUserOAuthCredentialConflict
	}
	return credential, nil
}

// MarkFeishuUserOAuthCredentialReauthRequired clears unusable ciphertext and
// prevents further refresh attempts until a new authorization succeeds.
func (s *Store) MarkFeishuUserOAuthCredentialReauthRequired(id, accountID string, expectedVersion int64, now time.Time) (FeishuUserOAuthCredential, error) {
	if err := s.requireFeishuDocsStore(); err != nil {
		return FeishuUserOAuthCredential{}, err
	}
	id = strings.TrimSpace(id)
	accountID = strings.TrimSpace(accountID)
	now = normalizedWorkflowTime(now)
	if id == "" || accountID == "" || expectedVersion <= 0 {
		return FeishuUserOAuthCredential{}, fmt.Errorf("feishu user oauth credential id, account, and expected version are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.db.Exec(
		`UPDATE feishu_user_oauth_credentials SET
		 access_token_ciphertext='', access_token_expires_at_ms=0,
		 refresh_token_ciphertext='', refresh_token_expires_at_ms=0,
		 status=?, version=version+1, updated_at_ms=?
		 WHERE id=? AND account_id=? AND version=?`,
		FeishuUserOAuthCredentialStatusReauthRequired,
		now.UnixMilli(),
		id,
		accountID,
		expectedVersion,
	)
	if err != nil {
		return FeishuUserOAuthCredential{}, fmt.Errorf("mark feishu user oauth credential reauthorization required: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return FeishuUserOAuthCredential{}, fmt.Errorf("inspect feishu user oauth credential reauthorization update: %w", err)
	}
	if count != 1 {
		return FeishuUserOAuthCredential{}, ErrFeishuUserOAuthCredentialConflict
	}
	return scanFeishuUserOAuthCredential(s.db.QueryRow(feishuUserOAuthCredentialSelect+` WHERE id=? AND account_id=?`, id, accountID))
}

type feishuUserOAuthCredentialQueryer interface {
	QueryRow(query string, args ...any) *sql.Row
}

func findFeishuUserOAuthCredential(queryer feishuUserOAuthCredentialQueryer, accountID, actorOpenID, actorUserID string) (FeishuUserOAuthCredential, bool, error) {
	if accountID == "" || (actorOpenID == "" && actorUserID == "") {
		return FeishuUserOAuthCredential{}, false, fmt.Errorf("feishu user oauth account and actor identity are required")
	}
	var byOpenID, byUserID FeishuUserOAuthCredential
	openFound := false
	userFound := false
	if actorOpenID != "" {
		var err error
		byOpenID, err = scanFeishuUserOAuthCredential(queryer.QueryRow(
			feishuUserOAuthCredentialSelect+` WHERE account_id=? AND actor_open_id=?`, accountID, actorOpenID,
		))
		if err == nil {
			openFound = true
		} else if !errors.Is(err, ErrFeishuUserOAuthCredentialNotFound) {
			return FeishuUserOAuthCredential{}, false, err
		}
	}
	if actorUserID != "" {
		var err error
		byUserID, err = scanFeishuUserOAuthCredential(queryer.QueryRow(
			feishuUserOAuthCredentialSelect+` WHERE account_id=? AND actor_user_id=?`, accountID, actorUserID,
		))
		if err == nil {
			userFound = true
		} else if !errors.Is(err, ErrFeishuUserOAuthCredentialNotFound) {
			return FeishuUserOAuthCredential{}, false, err
		}
	}
	if openFound && userFound && byOpenID.ID != byUserID.ID {
		return FeishuUserOAuthCredential{}, false, ErrFeishuUserOAuthIdentityConflict
	}
	if openFound {
		return byOpenID, true, nil
	}
	if userFound {
		return byUserID, true, nil
	}
	return FeishuUserOAuthCredential{}, false, nil
}

type feishuUserOAuthCredentialScanner interface {
	Scan(dest ...any) error
}

func scanFeishuUserOAuthCredential(row feishuUserOAuthCredentialScanner) (FeishuUserOAuthCredential, error) {
	var credential FeishuUserOAuthCredential
	var accessExpiresAtMS, refreshExpiresAtMS int64
	var authorizedAtMS, lastRefreshedAtMS, reauthorizeAtMS int64
	var createdAtMS, updatedAtMS int64
	err := row.Scan(
		&credential.ID,
		&credential.AccountID,
		&credential.ActorOpenID,
		&credential.ActorUserID,
		&credential.AccessTokenCiphertext,
		&accessExpiresAtMS,
		&credential.RefreshTokenCiphertext,
		&refreshExpiresAtMS,
		&credential.Scopes,
		&authorizedAtMS,
		&lastRefreshedAtMS,
		&reauthorizeAtMS,
		&credential.Status,
		&credential.Version,
		&createdAtMS,
		&updatedAtMS,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return FeishuUserOAuthCredential{}, ErrFeishuUserOAuthCredentialNotFound
		}
		return FeishuUserOAuthCredential{}, fmt.Errorf("scan feishu user oauth credential: %w", err)
	}
	credential.AccessTokenExpiresAt = timeFromOptionalMillis(accessExpiresAtMS)
	credential.RefreshTokenExpiresAt = timeFromOptionalMillis(refreshExpiresAtMS)
	credential.AuthorizedAt = timeFromOptionalMillis(authorizedAtMS)
	credential.LastRefreshedAt = timeFromOptionalMillis(lastRefreshedAtMS)
	credential.ReauthorizeAt = timeFromOptionalMillis(reauthorizeAtMS)
	credential.CreatedAt = timeFromOptionalMillis(createdAtMS)
	credential.UpdatedAt = timeFromOptionalMillis(updatedAtMS)
	return credential, nil
}

func normalizeFeishuUserOAuthCredential(credential FeishuUserOAuthCredential) FeishuUserOAuthCredential {
	credential.ID = strings.TrimSpace(credential.ID)
	credential.AccountID = strings.TrimSpace(credential.AccountID)
	credential.ActorOpenID = strings.TrimSpace(credential.ActorOpenID)
	credential.ActorUserID = strings.TrimSpace(credential.ActorUserID)
	credential.AccessTokenCiphertext = strings.TrimSpace(credential.AccessTokenCiphertext)
	credential.RefreshTokenCiphertext = strings.TrimSpace(credential.RefreshTokenCiphertext)
	credential.Scopes = strings.Join(strings.Fields(credential.Scopes), " ")
	credential.Status = strings.TrimSpace(credential.Status)
	credential.AccessTokenExpiresAt = credential.AccessTokenExpiresAt.UTC()
	credential.RefreshTokenExpiresAt = optionalUTC(credential.RefreshTokenExpiresAt)
	credential.AuthorizedAt = credential.AuthorizedAt.UTC()
	credential.LastRefreshedAt = optionalUTC(credential.LastRefreshedAt)
	credential.ReauthorizeAt = credential.ReauthorizeAt.UTC()
	credential.CreatedAt = normalizedWorkflowTime(credential.CreatedAt)
	if credential.UpdatedAt.IsZero() {
		credential.UpdatedAt = credential.CreatedAt
	} else {
		credential.UpdatedAt = credential.UpdatedAt.UTC()
	}
	return credential
}

func validateFeishuUserOAuthCredential(credential FeishuUserOAuthCredential) error {
	if credential.AccountID == "" || (credential.ActorOpenID == "" && credential.ActorUserID == "") {
		return fmt.Errorf("feishu user oauth account and actor identity are required")
	}
	if credential.Status != FeishuUserOAuthCredentialStatusActive && credential.Status != FeishuUserOAuthCredentialStatusReauthRequired {
		return fmt.Errorf("unsupported feishu user oauth credential status %q", credential.Status)
	}
	if credential.Status == FeishuUserOAuthCredentialStatusActive {
		if credential.AccessTokenCiphertext == "" || credential.AccessTokenExpiresAt.IsZero() {
			return fmt.Errorf("active feishu user oauth access token ciphertext and expiry are required")
		}
		if credential.RefreshTokenCiphertext == "" && !credential.RefreshTokenExpiresAt.IsZero() {
			return fmt.Errorf("feishu user oauth refresh token expiry requires ciphertext")
		}
		if credential.RefreshTokenCiphertext != "" && credential.RefreshTokenExpiresAt.IsZero() {
			return fmt.Errorf("feishu user oauth refresh token ciphertext requires expiry")
		}
	}
	if credential.AuthorizedAt.IsZero() || credential.ReauthorizeAt.IsZero() || credential.ReauthorizeAt.Before(credential.AuthorizedAt) {
		return fmt.Errorf("feishu user oauth authorization and mandatory reauthorization times are required")
	}
	return nil
}

func requireOneFeishuUserOAuthCredentialRow(result sql.Result) error {
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect feishu user oauth credential update: %w", err)
	}
	if count != 1 {
		return ErrFeishuUserOAuthCredentialConflict
	}
	return nil
}

func optionalTimeMillis(value time.Time) int64 {
	if value.IsZero() {
		return 0
	}
	return value.UTC().UnixMilli()
}

func timeFromOptionalMillis(value int64) time.Time {
	if value <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(value).UTC()
}

func optionalUTC(value time.Time) time.Time {
	if value.IsZero() {
		return time.Time{}
	}
	return value.UTC()
}
