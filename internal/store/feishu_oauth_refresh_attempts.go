package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrFeishuOAuthRefreshAttemptNotFound  = errors.New("feishu oauth refresh attempt not found")
	ErrFeishuOAuthRefreshAttemptConflict  = errors.New("feishu oauth refresh attempt state conflict")
	ErrFeishuOAuthRefreshAttemptLeaseLost = errors.New("feishu oauth refresh attempt lease does not match")
)

const (
	FeishuOAuthRefreshAttemptStatePrepared       = "prepared"
	FeishuOAuthRefreshAttemptStateResponseStaged = "response_staged"
	FeishuOAuthRefreshAttemptStateCompleted      = "completed"
	FeishuOAuthRefreshAttemptStateAmbiguous      = "ambiguous"
	FeishuOAuthRefreshAttemptStateFailed         = "failed"

	FeishuOAuthRefreshErrorAmbiguousOutcome = "ambiguous_remote_outcome"

	maxFeishuOAuthRefreshErrorCategoryRunes = 128
)

// FeishuOAuthRefreshAttempt is the durable state of one refresh-token
// rotation. Staged token fields must contain authenticated ciphertext, never
// plaintext OAuth values.
type FeishuOAuthRefreshAttempt struct {
	ID                     string
	CredentialID           string
	AccountID              string
	ActorOpenID            string
	ActorUserID            string
	ExpectedVersion        int64
	State                  string
	LeaseToken             string
	LeaseExpiresAt         time.Time
	AccessTokenCiphertext  string
	AccessTokenExpiresAt   time.Time
	RefreshTokenCiphertext string
	RefreshTokenExpiresAt  time.Time
	Scopes                 string
	ErrorCategory          string
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

// FeishuOAuthRefreshStage contains the encrypted response that must be made
// durable before the final credential CAS can run.
type FeishuOAuthRefreshStage struct {
	AccessTokenCiphertext  string
	AccessTokenExpiresAt   time.Time
	RefreshTokenCiphertext string
	RefreshTokenExpiresAt  time.Time
	Scopes                 string
}

// FeishuOAuthRefreshCredentialUpdate contains ciphertext re-encrypted for the
// final credential associated data. Its metadata must match the staged
// response exactly.
type FeishuOAuthRefreshCredentialUpdate struct {
	AccessTokenCiphertext  string
	AccessTokenExpiresAt   time.Time
	RefreshTokenCiphertext string
	RefreshTokenExpiresAt  time.Time
	Scopes                 string
}

const feishuOAuthRefreshAttemptSelect = `SELECT attempt_id, credential_id, account_id,
 actor_open_id, actor_user_id, expected_version, state,
 lease_token, lease_expires_at_ms,
 access_token_ciphertext, access_token_expires_at_ms,
 refresh_token_ciphertext, refresh_token_expires_at_ms,
 scopes, error_category, created_at_ms, updated_at_ms
 FROM feishu_oauth_refresh_attempts`

// PrepareFeishuOAuthRefreshAttempt creates and leases one prepared attempt, or
// returns the already-active attempt when another process owns the refresh.
func (s *Store) PrepareFeishuOAuthRefreshAttempt(
	credentialID, accountID string,
	expectedVersion int64,
	leaseToken string,
	now time.Time,
	leaseDuration time.Duration,
) (FeishuOAuthRefreshAttempt, bool, error) {
	if err := s.requireFeishuDocsStore(); err != nil {
		return FeishuOAuthRefreshAttempt{}, false, err
	}
	credentialID = strings.TrimSpace(credentialID)
	accountID = strings.TrimSpace(accountID)
	leaseToken = strings.TrimSpace(leaseToken)
	now = normalizedWorkflowTime(now)
	if credentialID == "" || accountID == "" || expectedVersion <= 0 || leaseToken == "" || leaseDuration <= 0 {
		return FeishuOAuthRefreshAttempt{}, false, fmt.Errorf("feishu oauth refresh credential, account, version, lease, and duration are required")
	}
	attemptIDValue, err := generateID()
	if err != nil {
		return FeishuOAuthRefreshAttempt{}, false, err
	}
	attemptID := "refresh_" + attemptIDValue

	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := beginFeishuOAuthCredentialTx(s.db)
	if err != nil {
		return FeishuOAuthRefreshAttempt{}, false, fmt.Errorf("begin prepare feishu oauth refresh attempt: %w", err)
	}
	defer tx.Rollback()
	credential, err := scanFeishuUserOAuthCredential(tx.QueryRow(
		feishuUserOAuthCredentialSelect+` WHERE id=? AND account_id=?`, credentialID, accountID,
	))
	if err != nil {
		return FeishuOAuthRefreshAttempt{}, false, err
	}
	if credential.Version != expectedVersion || credential.Status != FeishuUserOAuthCredentialStatusActive {
		return FeishuOAuthRefreshAttempt{}, false, ErrFeishuUserOAuthCredentialConflict
	}
	existing, existingErr := feishuOAuthRefreshAttemptByCredential(tx, credentialID, accountID)
	if existingErr == nil {
		return existing, false, nil
	}
	if !errors.Is(existingErr, ErrFeishuOAuthRefreshAttemptNotFound) {
		return FeishuOAuthRefreshAttempt{}, false, existingErr
	}
	attempt := FeishuOAuthRefreshAttempt{
		ID:              attemptID,
		CredentialID:    credential.ID,
		AccountID:       credential.AccountID,
		ActorOpenID:     credential.ActorOpenID,
		ActorUserID:     credential.ActorUserID,
		ExpectedVersion: credential.Version,
		State:           FeishuOAuthRefreshAttemptStatePrepared,
		LeaseToken:      leaseToken,
		LeaseExpiresAt:  now.Add(leaseDuration),
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	_, err = tx.Exec(
		`INSERT INTO feishu_oauth_refresh_attempts (
		 attempt_id, credential_id, account_id, actor_open_id, actor_user_id,
		 expected_version, state, lease_token, lease_expires_at_ms,
		 access_token_ciphertext, access_token_expires_at_ms,
		 refresh_token_ciphertext, refresh_token_expires_at_ms,
		 scopes, error_category, created_at_ms, updated_at_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, '', 0, '', 0, '', '', ?, ?)`,
		attempt.ID,
		attempt.CredentialID,
		attempt.AccountID,
		attempt.ActorOpenID,
		attempt.ActorUserID,
		attempt.ExpectedVersion,
		attempt.State,
		attempt.LeaseToken,
		attempt.LeaseExpiresAt.UnixMilli(),
		attempt.CreatedAt.UnixMilli(),
		attempt.UpdatedAt.UnixMilli(),
	)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique constraint failed") {
			_ = tx.Rollback()
			existing, loadErr := feishuOAuthRefreshAttemptByCredential(s.db, credentialID, accountID)
			if loadErr != nil {
				return FeishuOAuthRefreshAttempt{}, false, loadErr
			}
			return existing, false, nil
		}
		return FeishuOAuthRefreshAttempt{}, false, fmt.Errorf("prepare feishu oauth refresh attempt: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return FeishuOAuthRefreshAttempt{}, false, fmt.Errorf("commit prepared feishu oauth refresh attempt: %w", err)
	}
	return attempt, true, nil
}

// StageFeishuOAuthRefreshResponse persists the encrypted successful response
// before any final credential fields are replaced.
func (s *Store) StageFeishuOAuthRefreshResponse(
	attemptID, accountID, leaseToken string,
	stage FeishuOAuthRefreshStage,
	now time.Time,
) (FeishuOAuthRefreshAttempt, error) {
	if err := s.requireFeishuDocsStore(); err != nil {
		return FeishuOAuthRefreshAttempt{}, err
	}
	attemptID = strings.TrimSpace(attemptID)
	accountID = strings.TrimSpace(accountID)
	leaseToken = strings.TrimSpace(leaseToken)
	stage = normalizeFeishuOAuthRefreshStage(stage)
	now = normalizedWorkflowTime(now)
	if attemptID == "" || accountID == "" || leaseToken == "" || !validFeishuOAuthRefreshStage(stage, now) {
		return FeishuOAuthRefreshAttempt{}, fmt.Errorf("valid feishu oauth refresh attempt, lease, and encrypted staged response are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := beginFeishuOAuthCredentialTx(s.db)
	if err != nil {
		return FeishuOAuthRefreshAttempt{}, fmt.Errorf("begin stage feishu oauth refresh response: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.Exec(
		`UPDATE feishu_oauth_refresh_attempts SET
		 state=?, lease_token='', lease_expires_at_ms=0,
		 access_token_ciphertext=?, access_token_expires_at_ms=?,
		 refresh_token_ciphertext=?, refresh_token_expires_at_ms=?,
		 scopes=?, error_category='', updated_at_ms=?
		 WHERE attempt_id=? AND account_id=? AND state=? AND lease_token=?`,
		FeishuOAuthRefreshAttemptStateResponseStaged,
		stage.AccessTokenCiphertext,
		stage.AccessTokenExpiresAt.UnixMilli(),
		stage.RefreshTokenCiphertext,
		stage.RefreshTokenExpiresAt.UnixMilli(),
		stage.Scopes,
		now.UnixMilli(),
		attemptID,
		accountID,
		FeishuOAuthRefreshAttemptStatePrepared,
		leaseToken,
	)
	if err != nil {
		return FeishuOAuthRefreshAttempt{}, fmt.Errorf("stage feishu oauth refresh response: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return FeishuOAuthRefreshAttempt{}, fmt.Errorf("inspect staged feishu oauth refresh response: %w", err)
	}
	if count != 1 {
		attempt, loadErr := feishuOAuthRefreshAttemptByID(tx, attemptID, accountID)
		if loadErr != nil {
			return FeishuOAuthRefreshAttempt{}, loadErr
		}
		if attempt.State != FeishuOAuthRefreshAttemptStatePrepared {
			return attempt, ErrFeishuOAuthRefreshAttemptConflict
		}
		return attempt, ErrFeishuOAuthRefreshAttemptLeaseLost
	}
	attempt, err := feishuOAuthRefreshAttemptByID(tx, attemptID, accountID)
	if err != nil {
		return FeishuOAuthRefreshAttempt{}, err
	}
	if err := tx.Commit(); err != nil {
		return FeishuOAuthRefreshAttempt{}, fmt.Errorf("commit staged feishu oauth refresh response: %w", err)
	}
	return attempt, nil
}

// ApplyFeishuOAuthRefreshAttempt atomically CAS-rotates the final credential
// and clears staged ciphertext. If another valid credential update already
// advanced the version, the attempt is completed as superseded.
func (s *Store) ApplyFeishuOAuthRefreshAttempt(
	attemptID, accountID string,
	update FeishuOAuthRefreshCredentialUpdate,
	now time.Time,
) (FeishuUserOAuthCredential, FeishuOAuthRefreshAttempt, error) {
	if err := s.requireFeishuDocsStore(); err != nil {
		return FeishuUserOAuthCredential{}, FeishuOAuthRefreshAttempt{}, err
	}
	attemptID = strings.TrimSpace(attemptID)
	accountID = strings.TrimSpace(accountID)
	update = normalizeFeishuOAuthRefreshCredentialUpdate(update)
	now = normalizedWorkflowTime(now)
	if attemptID == "" || accountID == "" {
		return FeishuUserOAuthCredential{}, FeishuOAuthRefreshAttempt{}, fmt.Errorf("feishu oauth refresh attempt and account are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := beginFeishuOAuthCredentialTx(s.db)
	if err != nil {
		return FeishuUserOAuthCredential{}, FeishuOAuthRefreshAttempt{}, fmt.Errorf("begin apply feishu oauth refresh attempt: %w", err)
	}
	defer tx.Rollback()
	attempt, err := feishuOAuthRefreshAttemptByID(tx, attemptID, accountID)
	if err != nil {
		return FeishuUserOAuthCredential{}, FeishuOAuthRefreshAttempt{}, err
	}
	credential, err := scanFeishuUserOAuthCredential(tx.QueryRow(
		feishuUserOAuthCredentialSelect+` WHERE id=? AND account_id=?`, attempt.CredentialID, attempt.AccountID,
	))
	if err != nil {
		return FeishuUserOAuthCredential{}, FeishuOAuthRefreshAttempt{}, err
	}
	if attempt.State == FeishuOAuthRefreshAttemptStateCompleted {
		return credential, attempt, nil
	}
	if attempt.State != FeishuOAuthRefreshAttemptStateResponseStaged {
		return credential, attempt, ErrFeishuOAuthRefreshAttemptConflict
	}
	if credential.Version > attempt.ExpectedVersion {
		if err := completeFeishuOAuthRefreshAttemptTx(tx, attempt, FeishuOAuthRefreshAttemptStateCompleted, "", now); err != nil {
			return FeishuUserOAuthCredential{}, FeishuOAuthRefreshAttempt{}, err
		}
		if err := tx.Commit(); err != nil {
			return FeishuUserOAuthCredential{}, FeishuOAuthRefreshAttempt{}, fmt.Errorf("commit superseded feishu oauth refresh attempt: %w", err)
		}
		attempt = clearedTerminalFeishuOAuthRefreshAttempt(attempt, FeishuOAuthRefreshAttemptStateCompleted, "", now)
		return credential, attempt, nil
	}
	if credential.Version != attempt.ExpectedVersion {
		return credential, attempt, ErrFeishuUserOAuthCredentialConflict
	}
	if !validFeishuOAuthRefreshCredentialUpdate(update, attempt) {
		return credential, attempt, fmt.Errorf("final feishu oauth refresh credential update does not match staged metadata")
	}
	result, err := tx.Exec(
		`UPDATE feishu_user_oauth_credentials SET
		 access_token_ciphertext=?, access_token_expires_at_ms=?,
		 refresh_token_ciphertext=?, refresh_token_expires_at_ms=?,
		 scopes=?, last_refreshed_at_ms=?, status=?, version=version+1, updated_at_ms=?
		 WHERE id=? AND account_id=? AND version=?`,
		update.AccessTokenCiphertext,
		update.AccessTokenExpiresAt.UnixMilli(),
		update.RefreshTokenCiphertext,
		update.RefreshTokenExpiresAt.UnixMilli(),
		update.Scopes,
		now.UnixMilli(),
		FeishuUserOAuthCredentialStatusActive,
		now.UnixMilli(),
		credential.ID,
		credential.AccountID,
		attempt.ExpectedVersion,
	)
	if err != nil {
		return FeishuUserOAuthCredential{}, FeishuOAuthRefreshAttempt{}, fmt.Errorf("apply staged feishu oauth refresh credential: %w", err)
	}
	if err := requireOneFeishuUserOAuthCredentialRow(result); err != nil {
		return FeishuUserOAuthCredential{}, FeishuOAuthRefreshAttempt{}, err
	}
	if err := completeFeishuOAuthRefreshAttemptTx(tx, attempt, FeishuOAuthRefreshAttemptStateCompleted, "", now); err != nil {
		return FeishuUserOAuthCredential{}, FeishuOAuthRefreshAttempt{}, err
	}
	credential, err = scanFeishuUserOAuthCredential(tx.QueryRow(
		feishuUserOAuthCredentialSelect+` WHERE id=? AND account_id=?`, credential.ID, credential.AccountID,
	))
	if err != nil {
		return FeishuUserOAuthCredential{}, FeishuOAuthRefreshAttempt{}, err
	}
	if err := tx.Commit(); err != nil {
		return FeishuUserOAuthCredential{}, FeishuOAuthRefreshAttempt{}, fmt.Errorf("commit applied feishu oauth refresh attempt: %w", err)
	}
	attempt = clearedTerminalFeishuOAuthRefreshAttempt(attempt, FeishuOAuthRefreshAttemptStateCompleted, "", now)
	return credential, attempt, nil
}

// FailFeishuOAuthRefreshAttempt closes the leased prepared attempt. Terminal
// refresh failures can mark the credential reauthorization-required in the
// same transaction.
func (s *Store) FailFeishuOAuthRefreshAttempt(
	attemptID, accountID, leaseToken, errorCategory string,
	requireReauthorization bool,
	now time.Time,
) (FeishuUserOAuthCredential, FeishuOAuthRefreshAttempt, error) {
	if err := s.requireFeishuDocsStore(); err != nil {
		return FeishuUserOAuthCredential{}, FeishuOAuthRefreshAttempt{}, err
	}
	attemptID = strings.TrimSpace(attemptID)
	accountID = strings.TrimSpace(accountID)
	leaseToken = strings.TrimSpace(leaseToken)
	errorCategory = truncateOAuthRefreshErrorCategory(errorCategory)
	now = normalizedWorkflowTime(now)
	if attemptID == "" || accountID == "" || leaseToken == "" {
		return FeishuUserOAuthCredential{}, FeishuOAuthRefreshAttempt{}, fmt.Errorf("feishu oauth refresh attempt, account, and lease are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := beginFeishuOAuthCredentialTx(s.db)
	if err != nil {
		return FeishuUserOAuthCredential{}, FeishuOAuthRefreshAttempt{}, fmt.Errorf("begin fail feishu oauth refresh attempt: %w", err)
	}
	defer tx.Rollback()
	attempt, err := feishuOAuthRefreshAttemptByID(tx, attemptID, accountID)
	if err != nil {
		return FeishuUserOAuthCredential{}, FeishuOAuthRefreshAttempt{}, err
	}
	if attempt.State != FeishuOAuthRefreshAttemptStatePrepared {
		return FeishuUserOAuthCredential{}, attempt, ErrFeishuOAuthRefreshAttemptConflict
	}
	if attempt.LeaseToken != leaseToken {
		return FeishuUserOAuthCredential{}, attempt, ErrFeishuOAuthRefreshAttemptLeaseLost
	}
	credential, err := scanFeishuUserOAuthCredential(tx.QueryRow(
		feishuUserOAuthCredentialSelect+` WHERE id=? AND account_id=?`, attempt.CredentialID, attempt.AccountID,
	))
	if err != nil {
		return FeishuUserOAuthCredential{}, FeishuOAuthRefreshAttempt{}, err
	}
	terminalState := FeishuOAuthRefreshAttemptStateFailed
	if credential.Version > attempt.ExpectedVersion {
		terminalState = FeishuOAuthRefreshAttemptStateCompleted
		errorCategory = ""
	} else if credential.Version < attempt.ExpectedVersion {
		return credential, attempt, ErrFeishuUserOAuthCredentialConflict
	} else if requireReauthorization {
		credential, err = markFeishuOAuthCredentialReauthTx(tx, credential, now)
		if err != nil {
			return FeishuUserOAuthCredential{}, FeishuOAuthRefreshAttempt{}, err
		}
	}
	if err := completeFeishuOAuthRefreshAttemptTx(tx, attempt, terminalState, errorCategory, now); err != nil {
		return FeishuUserOAuthCredential{}, FeishuOAuthRefreshAttempt{}, err
	}
	if err := tx.Commit(); err != nil {
		return FeishuUserOAuthCredential{}, FeishuOAuthRefreshAttempt{}, fmt.Errorf("commit failed feishu oauth refresh attempt: %w", err)
	}
	attempt = clearedTerminalFeishuOAuthRefreshAttempt(attempt, terminalState, errorCategory, now)
	return credential, attempt, nil
}

// MarkFeishuOAuthRefreshAttemptAmbiguous fails closed after a prepared lease
// expires with an unknown remote outcome.
func (s *Store) MarkFeishuOAuthRefreshAttemptAmbiguous(attemptID, accountID string, now time.Time) (FeishuUserOAuthCredential, FeishuOAuthRefreshAttempt, error) {
	if err := s.requireFeishuDocsStore(); err != nil {
		return FeishuUserOAuthCredential{}, FeishuOAuthRefreshAttempt{}, err
	}
	attemptID = strings.TrimSpace(attemptID)
	accountID = strings.TrimSpace(accountID)
	now = normalizedWorkflowTime(now)
	if attemptID == "" || accountID == "" {
		return FeishuUserOAuthCredential{}, FeishuOAuthRefreshAttempt{}, fmt.Errorf("feishu oauth refresh attempt and account are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := beginFeishuOAuthCredentialTx(s.db)
	if err != nil {
		return FeishuUserOAuthCredential{}, FeishuOAuthRefreshAttempt{}, fmt.Errorf("begin ambiguous feishu oauth refresh attempt: %w", err)
	}
	defer tx.Rollback()
	attempt, err := feishuOAuthRefreshAttemptByID(tx, attemptID, accountID)
	if err != nil {
		return FeishuUserOAuthCredential{}, FeishuOAuthRefreshAttempt{}, err
	}
	if attempt.State != FeishuOAuthRefreshAttemptStatePrepared {
		return FeishuUserOAuthCredential{}, attempt, ErrFeishuOAuthRefreshAttemptConflict
	}
	if attempt.LeaseExpiresAt.After(now) {
		return FeishuUserOAuthCredential{}, attempt, ErrFeishuOAuthRefreshAttemptLeaseLost
	}
	credential, terminalState, errorCategory, err := resolveAmbiguousFeishuOAuthRefreshAttemptTx(tx, attempt, now)
	if err != nil {
		return FeishuUserOAuthCredential{}, FeishuOAuthRefreshAttempt{}, err
	}
	if err := tx.Commit(); err != nil {
		return FeishuUserOAuthCredential{}, FeishuOAuthRefreshAttempt{}, fmt.Errorf("commit ambiguous feishu oauth refresh attempt: %w", err)
	}
	attempt = clearedTerminalFeishuOAuthRefreshAttempt(attempt, terminalState, errorCategory, now)
	return credential, attempt, nil
}

// MarkOwnedFeishuOAuthRefreshAttemptAmbiguous fails closed immediately when
// the current lease owner cannot determine whether Feishu consumed the
// one-time refresh token. Unlike startup recovery, it does not wait for lease
// expiry because the owner has already observed the ambiguous transport
// outcome.
func (s *Store) MarkOwnedFeishuOAuthRefreshAttemptAmbiguous(
	attemptID, accountID, leaseToken string,
	now time.Time,
) (FeishuUserOAuthCredential, FeishuOAuthRefreshAttempt, error) {
	if err := s.requireFeishuDocsStore(); err != nil {
		return FeishuUserOAuthCredential{}, FeishuOAuthRefreshAttempt{}, err
	}
	attemptID = strings.TrimSpace(attemptID)
	accountID = strings.TrimSpace(accountID)
	leaseToken = strings.TrimSpace(leaseToken)
	now = normalizedWorkflowTime(now)
	if attemptID == "" || accountID == "" || leaseToken == "" {
		return FeishuUserOAuthCredential{}, FeishuOAuthRefreshAttempt{}, fmt.Errorf("feishu oauth refresh attempt, account, and lease are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := beginFeishuOAuthCredentialTx(s.db)
	if err != nil {
		return FeishuUserOAuthCredential{}, FeishuOAuthRefreshAttempt{}, fmt.Errorf("begin owned ambiguous feishu oauth refresh attempt: %w", err)
	}
	defer tx.Rollback()
	attempt, err := feishuOAuthRefreshAttemptByID(tx, attemptID, accountID)
	if err != nil {
		return FeishuUserOAuthCredential{}, FeishuOAuthRefreshAttempt{}, err
	}
	if attempt.State != FeishuOAuthRefreshAttemptStatePrepared {
		return FeishuUserOAuthCredential{}, attempt, ErrFeishuOAuthRefreshAttemptConflict
	}
	if attempt.LeaseToken != leaseToken {
		return FeishuUserOAuthCredential{}, attempt, ErrFeishuOAuthRefreshAttemptLeaseLost
	}
	credential, terminalState, errorCategory, err := resolveAmbiguousFeishuOAuthRefreshAttemptTx(tx, attempt, now)
	if err != nil {
		return FeishuUserOAuthCredential{}, FeishuOAuthRefreshAttempt{}, err
	}
	if err := tx.Commit(); err != nil {
		return FeishuUserOAuthCredential{}, FeishuOAuthRefreshAttempt{}, fmt.Errorf("commit owned ambiguous feishu oauth refresh attempt: %w", err)
	}
	attempt = clearedTerminalFeishuOAuthRefreshAttempt(attempt, terminalState, errorCategory, now)
	return credential, attempt, nil
}

// InvalidateFeishuOAuthRefreshAttempt fails closed when a durable staged
// response cannot be decrypted or safely applied. If a newer credential has
// already superseded the attempt, the attempt is completed without changing
// that credential.
func (s *Store) InvalidateFeishuOAuthRefreshAttempt(
	attemptID, accountID, errorCategory string,
	now time.Time,
) (FeishuUserOAuthCredential, FeishuOAuthRefreshAttempt, error) {
	if err := s.requireFeishuDocsStore(); err != nil {
		return FeishuUserOAuthCredential{}, FeishuOAuthRefreshAttempt{}, err
	}
	attemptID = strings.TrimSpace(attemptID)
	accountID = strings.TrimSpace(accountID)
	errorCategory = truncateOAuthRefreshErrorCategory(errorCategory)
	now = normalizedWorkflowTime(now)
	if attemptID == "" || accountID == "" {
		return FeishuUserOAuthCredential{}, FeishuOAuthRefreshAttempt{}, fmt.Errorf("feishu oauth refresh attempt and account are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := beginFeishuOAuthCredentialTx(s.db)
	if err != nil {
		return FeishuUserOAuthCredential{}, FeishuOAuthRefreshAttempt{}, fmt.Errorf("begin invalidate feishu oauth refresh attempt: %w", err)
	}
	defer tx.Rollback()
	attempt, err := feishuOAuthRefreshAttemptByID(tx, attemptID, accountID)
	if err != nil {
		return FeishuUserOAuthCredential{}, FeishuOAuthRefreshAttempt{}, err
	}
	if attempt.State != FeishuOAuthRefreshAttemptStatePrepared && attempt.State != FeishuOAuthRefreshAttemptStateResponseStaged {
		return FeishuUserOAuthCredential{}, attempt, ErrFeishuOAuthRefreshAttemptConflict
	}
	credential, err := scanFeishuUserOAuthCredential(tx.QueryRow(
		feishuUserOAuthCredentialSelect+` WHERE id=? AND account_id=?`, attempt.CredentialID, attempt.AccountID,
	))
	if err != nil {
		return FeishuUserOAuthCredential{}, FeishuOAuthRefreshAttempt{}, err
	}
	terminalState := FeishuOAuthRefreshAttemptStateFailed
	if credential.Version > attempt.ExpectedVersion {
		terminalState = FeishuOAuthRefreshAttemptStateCompleted
		errorCategory = ""
	} else if credential.Version == attempt.ExpectedVersion {
		credential, err = markFeishuOAuthCredentialReauthTx(tx, credential, now)
		if err != nil {
			return FeishuUserOAuthCredential{}, FeishuOAuthRefreshAttempt{}, err
		}
	} else {
		return credential, attempt, ErrFeishuUserOAuthCredentialConflict
	}
	if err := completeFeishuOAuthRefreshAttemptTx(tx, attempt, terminalState, errorCategory, now); err != nil {
		return FeishuUserOAuthCredential{}, FeishuOAuthRefreshAttempt{}, err
	}
	if err := tx.Commit(); err != nil {
		return FeishuUserOAuthCredential{}, FeishuOAuthRefreshAttempt{}, fmt.Errorf("commit invalidated feishu oauth refresh attempt: %w", err)
	}
	attempt = clearedTerminalFeishuOAuthRefreshAttempt(attempt, terminalState, errorCategory, now)
	return credential, attempt, nil
}

func (s *Store) GetFeishuOAuthRefreshAttempt(attemptID, accountID string) (FeishuOAuthRefreshAttempt, error) {
	if err := s.requireFeishuDocsStore(); err != nil {
		return FeishuOAuthRefreshAttempt{}, err
	}
	return feishuOAuthRefreshAttemptByID(s.db, strings.TrimSpace(attemptID), strings.TrimSpace(accountID))
}

func (s *Store) ActiveFeishuOAuthRefreshAttempt(credentialID, accountID string) (FeishuOAuthRefreshAttempt, error) {
	if err := s.requireFeishuDocsStore(); err != nil {
		return FeishuOAuthRefreshAttempt{}, err
	}
	return feishuOAuthRefreshAttemptByCredential(s.db, strings.TrimSpace(credentialID), strings.TrimSpace(accountID))
}

func (s *Store) ListRecoverableFeishuOAuthRefreshAttempts(accountID string, now time.Time, limit int) ([]FeishuOAuthRefreshAttempt, error) {
	if err := s.requireFeishuDocsStore(); err != nil {
		return nil, err
	}
	accountID = strings.TrimSpace(accountID)
	now = normalizedWorkflowTime(now)
	if accountID == "" || limit <= 0 {
		return nil, fmt.Errorf("feishu oauth refresh account and positive limit are required")
	}
	if limit > 1000 {
		limit = 1000
	}
	rows, err := s.db.Query(
		feishuOAuthRefreshAttemptSelect+`
		 WHERE account_id=? AND (
		  state=? OR (state=? AND lease_expires_at_ms<=?)
		 )
		 ORDER BY updated_at_ms ASC, attempt_id ASC LIMIT ?`,
		accountID,
		FeishuOAuthRefreshAttemptStateResponseStaged,
		FeishuOAuthRefreshAttemptStatePrepared,
		now.UnixMilli(),
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list recoverable feishu oauth refresh attempts: %w", err)
	}
	defer rows.Close()
	attempts := make([]FeishuOAuthRefreshAttempt, 0)
	for rows.Next() {
		attempt, scanErr := scanFeishuOAuthRefreshAttempt(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		attempts = append(attempts, attempt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate recoverable feishu oauth refresh attempts: %w", err)
	}
	return attempts, nil
}

// DeleteTerminalFeishuOAuthRefreshAttempts removes one bounded batch of old,
// sanitized terminal attempts for one Feishu account. Rows that still contain
// staged token ciphertext are deliberately retained for diagnosis.
func (s *Store) DeleteTerminalFeishuOAuthRefreshAttempts(accountID string, completedBefore time.Time, limit int) (int64, error) {
	if err := s.requireFeishuDocsStore(); err != nil {
		return 0, err
	}
	accountID = strings.TrimSpace(accountID)
	if accountID == "" || completedBefore.IsZero() || limit <= 0 {
		return 0, fmt.Errorf("feishu oauth refresh account, completion cutoff, and positive limit are required")
	}
	completedBefore = completedBefore.UTC()
	if limit > 1000 {
		limit = 1000
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.db.Exec(
		`DELETE FROM feishu_oauth_refresh_attempts
		 WHERE attempt_id IN (
		  SELECT attempt_id FROM feishu_oauth_refresh_attempts
		  WHERE account_id=? AND state IN (?, ?, ?) AND updated_at_ms<?
		   AND access_token_ciphertext='' AND refresh_token_ciphertext=''
		  ORDER BY updated_at_ms ASC, attempt_id ASC
		  LIMIT ?
		 )`,
		accountID,
		FeishuOAuthRefreshAttemptStateCompleted,
		FeishuOAuthRefreshAttemptStateAmbiguous,
		FeishuOAuthRefreshAttemptStateFailed,
		completedBefore.UnixMilli(),
		limit,
	)
	if err != nil {
		return 0, fmt.Errorf("delete terminal feishu oauth refresh attempts: %w", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("inspect terminal feishu oauth refresh attempt cleanup: %w", err)
	}
	return deleted, nil
}

// CountUnsafeTerminalFeishuOAuthRefreshAttempts reports old terminal rows that
// cannot be retention-cleaned because they still contain token ciphertext.
func (s *Store) CountUnsafeTerminalFeishuOAuthRefreshAttempts(accountID string, completedBefore time.Time) (int64, error) {
	if err := s.requireFeishuDocsStore(); err != nil {
		return 0, err
	}
	accountID = strings.TrimSpace(accountID)
	if accountID == "" || completedBefore.IsZero() {
		return 0, fmt.Errorf("feishu oauth refresh account and completion cutoff are required")
	}
	var count int64
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM feishu_oauth_refresh_attempts
		 WHERE account_id=? AND state IN (?, ?, ?) AND updated_at_ms<?
		  AND (access_token_ciphertext<>'' OR refresh_token_ciphertext<>'')`,
		accountID,
		FeishuOAuthRefreshAttemptStateCompleted,
		FeishuOAuthRefreshAttemptStateAmbiguous,
		FeishuOAuthRefreshAttemptStateFailed,
		completedBefore.UTC().UnixMilli(),
	).Scan(&count); err != nil {
		return 0, fmt.Errorf("count unsafe terminal feishu oauth refresh attempts: %w", err)
	}
	return count, nil
}

func feishuOAuthRefreshAttemptByID(queryer interface {
	QueryRow(query string, args ...any) *sql.Row
}, attemptID, accountID string) (FeishuOAuthRefreshAttempt, error) {
	return scanFeishuOAuthRefreshAttempt(queryer.QueryRow(
		feishuOAuthRefreshAttemptSelect+` WHERE attempt_id=? AND account_id=?`, attemptID, accountID,
	))
}

func feishuOAuthRefreshAttemptByCredential(queryer interface {
	QueryRow(query string, args ...any) *sql.Row
}, credentialID, accountID string) (FeishuOAuthRefreshAttempt, error) {
	return scanFeishuOAuthRefreshAttempt(queryer.QueryRow(
		feishuOAuthRefreshAttemptSelect+` WHERE credential_id=? AND account_id=? AND state IN (?, ?) ORDER BY created_at_ms DESC LIMIT 1`,
		credentialID,
		accountID,
		FeishuOAuthRefreshAttemptStatePrepared,
		FeishuOAuthRefreshAttemptStateResponseStaged,
	))
}

type feishuOAuthRefreshAttemptScanner interface {
	Scan(dest ...any) error
}

func scanFeishuOAuthRefreshAttempt(row feishuOAuthRefreshAttemptScanner) (FeishuOAuthRefreshAttempt, error) {
	var attempt FeishuOAuthRefreshAttempt
	var leaseExpiresAtMS, accessExpiresAtMS, refreshExpiresAtMS, createdAtMS, updatedAtMS int64
	err := row.Scan(
		&attempt.ID,
		&attempt.CredentialID,
		&attempt.AccountID,
		&attempt.ActorOpenID,
		&attempt.ActorUserID,
		&attempt.ExpectedVersion,
		&attempt.State,
		&attempt.LeaseToken,
		&leaseExpiresAtMS,
		&attempt.AccessTokenCiphertext,
		&accessExpiresAtMS,
		&attempt.RefreshTokenCiphertext,
		&refreshExpiresAtMS,
		&attempt.Scopes,
		&attempt.ErrorCategory,
		&createdAtMS,
		&updatedAtMS,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return FeishuOAuthRefreshAttempt{}, ErrFeishuOAuthRefreshAttemptNotFound
	}
	if err != nil {
		return FeishuOAuthRefreshAttempt{}, fmt.Errorf("get feishu oauth refresh attempt: %w", err)
	}
	attempt.LeaseExpiresAt = timeFromOptionalMillis(leaseExpiresAtMS)
	attempt.AccessTokenExpiresAt = timeFromOptionalMillis(accessExpiresAtMS)
	attempt.RefreshTokenExpiresAt = timeFromOptionalMillis(refreshExpiresAtMS)
	attempt.CreatedAt = time.UnixMilli(createdAtMS).UTC()
	attempt.UpdatedAt = time.UnixMilli(updatedAtMS).UTC()
	return attempt, nil
}

func normalizeFeishuOAuthRefreshStage(stage FeishuOAuthRefreshStage) FeishuOAuthRefreshStage {
	stage.AccessTokenCiphertext = strings.TrimSpace(stage.AccessTokenCiphertext)
	stage.AccessTokenExpiresAt = optionalUTC(stage.AccessTokenExpiresAt)
	stage.RefreshTokenCiphertext = strings.TrimSpace(stage.RefreshTokenCiphertext)
	stage.RefreshTokenExpiresAt = optionalUTC(stage.RefreshTokenExpiresAt)
	stage.Scopes = strings.Join(strings.Fields(stage.Scopes), " ")
	return stage
}

func validFeishuOAuthRefreshStage(stage FeishuOAuthRefreshStage, now time.Time) bool {
	return stage.AccessTokenCiphertext != "" && stage.AccessTokenExpiresAt.After(now) &&
		stage.RefreshTokenCiphertext != "" && stage.RefreshTokenExpiresAt.After(now) && stage.Scopes != ""
}

func normalizeFeishuOAuthRefreshCredentialUpdate(update FeishuOAuthRefreshCredentialUpdate) FeishuOAuthRefreshCredentialUpdate {
	update.AccessTokenCiphertext = strings.TrimSpace(update.AccessTokenCiphertext)
	update.AccessTokenExpiresAt = optionalUTC(update.AccessTokenExpiresAt)
	update.RefreshTokenCiphertext = strings.TrimSpace(update.RefreshTokenCiphertext)
	update.RefreshTokenExpiresAt = optionalUTC(update.RefreshTokenExpiresAt)
	update.Scopes = strings.Join(strings.Fields(update.Scopes), " ")
	return update
}

func validFeishuOAuthRefreshCredentialUpdate(update FeishuOAuthRefreshCredentialUpdate, attempt FeishuOAuthRefreshAttempt) bool {
	return update.AccessTokenCiphertext != "" && update.RefreshTokenCiphertext != "" &&
		update.AccessTokenExpiresAt.Equal(attempt.AccessTokenExpiresAt) &&
		update.RefreshTokenExpiresAt.Equal(attempt.RefreshTokenExpiresAt) &&
		update.Scopes == strings.Join(strings.Fields(attempt.Scopes), " ")
}

func completeFeishuOAuthRefreshAttemptTx(tx feishuOAuthRefreshExecutor, attempt FeishuOAuthRefreshAttempt, state, errorCategory string, now time.Time) error {
	result, err := tx.Exec(
		`UPDATE feishu_oauth_refresh_attempts SET
		 state=?, lease_token='', lease_expires_at_ms=0,
		 access_token_ciphertext='', access_token_expires_at_ms=0,
		 refresh_token_ciphertext='', refresh_token_expires_at_ms=0,
		 scopes='', error_category=?, updated_at_ms=?
		 WHERE attempt_id=? AND account_id=? AND state IN (?, ?)`,
		state,
		errorCategory,
		now.UnixMilli(),
		attempt.ID,
		attempt.AccountID,
		FeishuOAuthRefreshAttemptStatePrepared,
		FeishuOAuthRefreshAttemptStateResponseStaged,
	)
	if err != nil {
		return fmt.Errorf("complete feishu oauth refresh attempt: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect feishu oauth refresh attempt completion: %w", err)
	}
	if count != 1 {
		return ErrFeishuOAuthRefreshAttemptConflict
	}
	return nil
}

func markFeishuOAuthCredentialReauthTx(tx feishuOAuthRefreshExecutor, credential FeishuUserOAuthCredential, now time.Time) (FeishuUserOAuthCredential, error) {
	result, err := tx.Exec(
		`UPDATE feishu_user_oauth_credentials SET
		 access_token_ciphertext='', access_token_expires_at_ms=0,
		 refresh_token_ciphertext='', refresh_token_expires_at_ms=0,
		 status=?, version=version+1, updated_at_ms=?
		 WHERE id=? AND account_id=? AND version=?`,
		FeishuUserOAuthCredentialStatusReauthRequired,
		now.UnixMilli(),
		credential.ID,
		credential.AccountID,
		credential.Version,
	)
	if err != nil {
		return FeishuUserOAuthCredential{}, fmt.Errorf("mark refresh credential reauthorization required: %w", err)
	}
	if err := requireOneFeishuUserOAuthCredentialRow(result); err != nil {
		return FeishuUserOAuthCredential{}, err
	}
	return scanFeishuUserOAuthCredential(tx.QueryRow(
		feishuUserOAuthCredentialSelect+` WHERE id=? AND account_id=?`, credential.ID, credential.AccountID,
	))
}

func resolveAmbiguousFeishuOAuthRefreshAttemptTx(
	tx feishuOAuthRefreshExecutor,
	attempt FeishuOAuthRefreshAttempt,
	now time.Time,
) (FeishuUserOAuthCredential, string, string, error) {
	credential, err := scanFeishuUserOAuthCredential(tx.QueryRow(
		feishuUserOAuthCredentialSelect+` WHERE id=? AND account_id=?`, attempt.CredentialID, attempt.AccountID,
	))
	if err != nil {
		return FeishuUserOAuthCredential{}, "", "", err
	}
	terminalState := FeishuOAuthRefreshAttemptStateAmbiguous
	errorCategory := FeishuOAuthRefreshErrorAmbiguousOutcome
	if credential.Version > attempt.ExpectedVersion {
		terminalState = FeishuOAuthRefreshAttemptStateCompleted
		errorCategory = ""
	} else if credential.Version == attempt.ExpectedVersion {
		credential, err = markFeishuOAuthCredentialReauthTx(tx, credential, now)
		if err != nil {
			return FeishuUserOAuthCredential{}, "", "", err
		}
	} else {
		return credential, "", "", ErrFeishuUserOAuthCredentialConflict
	}
	if err := completeFeishuOAuthRefreshAttemptTx(tx, attempt, terminalState, errorCategory, now); err != nil {
		return FeishuUserOAuthCredential{}, "", "", err
	}
	return credential, terminalState, errorCategory, nil
}

func clearedTerminalFeishuOAuthRefreshAttempt(attempt FeishuOAuthRefreshAttempt, state, errorCategory string, now time.Time) FeishuOAuthRefreshAttempt {
	attempt.State = state
	attempt.LeaseToken = ""
	attempt.LeaseExpiresAt = time.Time{}
	attempt.AccessTokenCiphertext = ""
	attempt.AccessTokenExpiresAt = time.Time{}
	attempt.RefreshTokenCiphertext = ""
	attempt.RefreshTokenExpiresAt = time.Time{}
	attempt.Scopes = ""
	attempt.ErrorCategory = errorCategory
	attempt.UpdatedAt = now
	return attempt
}

func truncateOAuthRefreshErrorCategory(value string) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) <= maxFeishuOAuthRefreshErrorCategoryRunes {
		return value
	}
	return string(runes[:maxFeishuOAuthRefreshErrorCategoryRunes])
}

type feishuOAuthRefreshExecutor interface {
	Exec(query string, args ...any) (sql.Result, error)
	QueryRow(query string, args ...any) *sql.Row
}

// feishuOAuthCredentialTx uses BEGIN IMMEDIATE so separate LingoBridge
// processes queue before reading credential or refresh state instead of
// failing later with SQLITE_BUSY_SNAPSHOT after a concurrent writer commits.
type feishuOAuthCredentialTx struct {
	conn *sql.Conn
	done bool
}

func beginFeishuOAuthCredentialTx(db *sql.DB) (*feishuOAuthCredentialTx, error) {
	if db == nil {
		return nil, fmt.Errorf("feishu oauth refresh database is required")
	}
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA busy_timeout=5000`); err != nil {
		conn.Close()
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		conn.Close()
		return nil, err
	}
	return &feishuOAuthCredentialTx{conn: conn}, nil
}

func (tx *feishuOAuthCredentialTx) Exec(query string, args ...any) (sql.Result, error) {
	return tx.conn.ExecContext(context.Background(), query, args...)
}

func (tx *feishuOAuthCredentialTx) QueryRow(query string, args ...any) *sql.Row {
	return tx.conn.QueryRowContext(context.Background(), query, args...)
}

func (tx *feishuOAuthCredentialTx) Commit() error {
	if tx == nil || tx.conn == nil || tx.done {
		return nil
	}
	_, err := tx.conn.ExecContext(context.Background(), `COMMIT`)
	if err != nil {
		_, _ = tx.conn.ExecContext(context.Background(), `ROLLBACK`)
	}
	tx.done = true
	_ = tx.conn.Close()
	if err != nil {
		return err
	}
	return nil
}

func (tx *feishuOAuthCredentialTx) Rollback() error {
	if tx == nil || tx.conn == nil || tx.done {
		return nil
	}
	_, err := tx.conn.ExecContext(context.Background(), `ROLLBACK`)
	tx.done = true
	_ = tx.conn.Close()
	if err != nil {
		return err
	}
	return nil
}
