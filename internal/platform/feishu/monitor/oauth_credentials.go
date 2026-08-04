package monitor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/core/accesstoken"
	"github.com/larksuite/oapi-sdk-go/v3/core/accesstoken/refreshtoken"

	feishusecure "lingobridge/internal/platform/feishu/secure"
	"lingobridge/internal/store"
)

const (
	feishuOAuthCredentialCipherVersion     = "v1"
	feishuOAuthCredentialKeyInfo           = "lingobridge/feishu/user-oauth-credentials/v1"
	feishuOAuthRefreshAttemptCipherContext = "refresh_attempt"
	feishuOAuthAccessRefreshSkew           = 5 * time.Minute
	feishuOAuthMinimumFallbackValidity     = 30 * time.Second
	feishuOAuthMandatoryReauthorizationTTL = 365 * 24 * time.Hour
	feishuOAuthRefreshLeaseDuration        = time.Minute
	feishuOAuthRefreshPeerWait             = 3 * time.Second
	feishuOAuthRefreshPollInterval         = 25 * time.Millisecond
	feishuOAuthRefreshRecoveryLimit        = 100
)

var (
	ErrFeishuUserOAuthCredentialUnavailable = errors.New("feishu user oauth credential is unavailable")
	ErrFeishuUserOAuthReauthorizationNeeded = errors.New("feishu user oauth reauthorization is required")
)

type feishuOAuthCredentialCipher struct {
	accountID string
	cipher    *feishusecure.AccountCipher
}

type feishuOAuthIdentity struct {
	OpenID string
	UserID string
}

type feishuOAuthTokenBundle struct {
	AccessToken           string
	AccessTokenExpiresIn  time.Duration
	RefreshToken          string
	RefreshTokenExpiresIn time.Duration
	Scopes                string
}

// oauthCredentialService owns persisted user OAuth credentials and durable
// refresh-token rotation. It intentionally depends only on OAuth storage and
// the Feishu REST client, not cards, resource requests, or callback routing.
type oauthCredentialService struct {
	store            oauthCredentialStore
	client           *lark.Client
	account          store.Account
	credentialCipher *feishuOAuthCredentialCipher
	now              func() time.Time
	refreshPeerWait  time.Duration
	refreshPoll      time.Duration
}

func (s *oauthCredentialService) currentTime() time.Time {
	if s == nil || s.now == nil {
		return time.Now().UTC()
	}
	return s.now().UTC()
}

func (m *resourceAccessManager) oauthCredentialService() *oauthCredentialService {
	if m == nil {
		return nil
	}
	return &oauthCredentialService{
		store:            m.store,
		client:           m.client,
		account:          m.account,
		credentialCipher: m.credentialCipher,
		now:              m.currentTime,
		refreshPeerWait:  m.refreshPeerWait,
		refreshPoll:      m.refreshPoll,
	}
}

func (m *resourceAccessManager) persistFeishuOAuthCredential(ctx context.Context, identity feishuOAuthIdentity, tokens feishuOAuthTokenBundle) (store.FeishuUserOAuthCredential, error) {
	return m.oauthCredentialService().persistFeishuOAuthCredential(ctx, identity, tokens)
}

func (m *resourceAccessManager) feishuUserAccessToken(ctx context.Context, actorOpenID, actorUserID string) (string, error) {
	return m.oauthCredentialService().feishuUserAccessToken(ctx, actorOpenID, actorUserID)
}

func (m *resourceAccessManager) recoverFeishuOAuthRefreshAttempts(ctx context.Context) error {
	return m.oauthCredentialService().recoverFeishuOAuthRefreshAttempts(ctx)
}

func (m *resourceAccessManager) accessTokenFromStagedFeishuOAuthRefresh(ctx context.Context, attempt store.FeishuOAuthRefreshAttempt) (string, error) {
	return m.oauthCredentialService().accessTokenFromStagedFeishuOAuthRefresh(ctx, attempt)
}

func (m *resourceAccessManager) waitForFeishuOAuthRefresh(ctx context.Context, original store.FeishuUserOAuthCredential, attempt store.FeishuOAuthRefreshAttempt) (string, error) {
	return m.oauthCredentialService().waitForFeishuOAuthRefresh(ctx, original, attempt)
}

func (m *resourceAccessManager) resolveFeishuOAuthRefreshPeerTimeout(ctx context.Context, original store.FeishuUserOAuthCredential, attempt store.FeishuOAuthRefreshAttempt) (string, error) {
	return m.oauthCredentialService().resolveFeishuOAuthRefreshPeerTimeout(ctx, original, attempt)
}

func newFeishuOAuthCredentialCipher(secret, accountID string) (*feishuOAuthCredentialCipher, error) {
	secret = strings.TrimSpace(secret)
	accountID = strings.TrimSpace(accountID)
	if secret == "" || accountID == "" {
		return nil, fmt.Errorf("feishu OAuth credential secret and account are required")
	}
	accountCipher, err := feishusecure.NewAccountCipher(secret, accountID, feishuOAuthCredentialKeyInfo)
	if err != nil {
		return nil, fmt.Errorf("create feishu OAuth credential cipher: %w", err)
	}
	return &feishuOAuthCredentialCipher{accountID: accountID, cipher: accountCipher}, nil
}

func (c *feishuOAuthCredentialCipher) Encrypt(identity feishuOAuthIdentity, field, plaintext string) (string, error) {
	return c.encrypt(plaintext, c.additionalData(identity, field))
}

func (c *feishuOAuthCredentialCipher) EncryptRefreshAttempt(attempt store.FeishuOAuthRefreshAttempt, field, plaintext string) (string, error) {
	additionalData, err := c.refreshAttemptAdditionalData(attempt, field)
	if err != nil {
		return "", err
	}
	return c.encrypt(plaintext, additionalData)
}

func (c *feishuOAuthCredentialCipher) encrypt(plaintext string, additionalData []byte) (string, error) {
	if c == nil || c.cipher == nil {
		return "", ErrFeishuUserOAuthCredentialUnavailable
	}
	plaintext = strings.TrimSpace(plaintext)
	if plaintext == "" {
		return "", nil
	}
	ciphertext, err := c.cipher.Encrypt([]byte(plaintext), additionalData)
	if err != nil {
		return "", fmt.Errorf("encrypt feishu OAuth credential: %w", err)
	}
	return ciphertext, nil
}

func (c *feishuOAuthCredentialCipher) Decrypt(identity feishuOAuthIdentity, field, ciphertext string) (string, error) {
	return c.decrypt(ciphertext, c.additionalData(identity, field))
}

func (c *feishuOAuthCredentialCipher) DecryptRefreshAttempt(attempt store.FeishuOAuthRefreshAttempt, field, ciphertext string) (string, error) {
	additionalData, err := c.refreshAttemptAdditionalData(attempt, field)
	if err != nil {
		return "", err
	}
	return c.decrypt(ciphertext, additionalData)
}

func (c *feishuOAuthCredentialCipher) decrypt(ciphertext string, additionalData []byte) (string, error) {
	if c == nil || c.cipher == nil {
		return "", ErrFeishuUserOAuthCredentialUnavailable
	}
	plain, err := c.cipher.Decrypt(ciphertext, additionalData)
	if err != nil {
		return "", fmt.Errorf("decrypt feishu OAuth credential: %w", err)
	}
	return string(plain), nil
}

func (c *feishuOAuthCredentialCipher) refreshAttemptAdditionalData(attempt store.FeishuOAuthRefreshAttempt, field string) ([]byte, error) {
	if c == nil {
		return nil, ErrFeishuUserOAuthCredentialUnavailable
	}
	attempt.ID = strings.TrimSpace(attempt.ID)
	attempt.AccountID = strings.TrimSpace(attempt.AccountID)
	attempt.ActorOpenID = strings.TrimSpace(attempt.ActorOpenID)
	attempt.ActorUserID = strings.TrimSpace(attempt.ActorUserID)
	field = strings.TrimSpace(field)
	if attempt.ID == "" || attempt.AccountID == "" || attempt.AccountID != c.accountID ||
		(attempt.ActorOpenID == "" && attempt.ActorUserID == "") || field == "" {
		return nil, fmt.Errorf("valid feishu OAuth refresh attempt encryption context is required")
	}
	return []byte(strings.Join([]string{
		feishuOAuthCredentialCipherVersion,
		c.accountID,
		feishuOAuthRefreshAttemptCipherContext,
		attempt.ID,
		attempt.ActorOpenID,
		attempt.ActorUserID,
		field,
	}, "\x00")), nil
}

func (c *feishuOAuthCredentialCipher) additionalData(identity feishuOAuthIdentity, field string) []byte {
	identityType, identityID := feishuOAuthIdentityKey(identity)
	return []byte(strings.Join([]string{
		feishuOAuthCredentialCipherVersion,
		c.accountID,
		identityType,
		identityID,
		strings.TrimSpace(field),
	}, "\x00"))
}

func feishuOAuthIdentityKey(identity feishuOAuthIdentity) (string, string) {
	if openID := strings.TrimSpace(identity.OpenID); openID != "" {
		return "open_id", openID
	}
	return "user_id", strings.TrimSpace(identity.UserID)
}

func (m *oauthCredentialService) persistFeishuOAuthCredential(ctx context.Context, identity feishuOAuthIdentity, tokens feishuOAuthTokenBundle) (store.FeishuUserOAuthCredential, error) {
	if m == nil || m.credentialCipher == nil {
		return store.FeishuUserOAuthCredential{}, ErrFeishuUserOAuthCredentialUnavailable
	}
	identity.OpenID = strings.TrimSpace(identity.OpenID)
	identity.UserID = strings.TrimSpace(identity.UserID)
	if identity.OpenID == "" && identity.UserID == "" {
		return store.FeishuUserOAuthCredential{}, fmt.Errorf("verified feishu OAuth identity is required")
	}
	tokens = normalizeFeishuOAuthTokenBundle(tokens)
	if tokens.AccessToken == "" || tokens.AccessTokenExpiresIn <= 0 {
		return store.FeishuUserOAuthCredential{}, fmt.Errorf("feishu OAuth access token and positive expiry are required")
	}
	now := m.currentTime()
	var credential store.FeishuUserOAuthCredential
	for saveAttempt := 0; saveAttempt < 2; saveAttempt++ {
		encrypted, err := m.encryptedFeishuOAuthCredential(identity, tokens, now)
		if err != nil {
			return store.FeishuUserOAuthCredential{}, err
		}
		credential, err = m.store.SaveFeishuUserOAuthCredential(encrypted)
		if err == nil {
			break
		}
		if !errors.Is(err, store.ErrFeishuUserOAuthIdentityChanged) {
			return store.FeishuUserOAuthCredential{}, fmt.Errorf("persist encrypted feishu OAuth credential: %w", err)
		}
		canonical := feishuOAuthIdentity{OpenID: credential.ActorOpenID, UserID: credential.ActorUserID}
		currentType, currentID := feishuOAuthIdentityKey(identity)
		canonicalType, canonicalID := feishuOAuthIdentityKey(canonical)
		if canonicalID == "" || (currentType == canonicalType && currentID == canonicalID) {
			return store.FeishuUserOAuthCredential{}, fmt.Errorf("persist encrypted feishu OAuth credential: %w", err)
		}
		identity = canonical
		feishuLog.Debug(ctx, "re-encrypting feishu user OAuth credential with canonical identity account=%s credential=%s user_ref=%s",
			m.account.ID, shortResourceRef(credential.ID), shortResourceRef(canonicalID))
	}
	if credential.ID == "" {
		return store.FeishuUserOAuthCredential{}, fmt.Errorf("persist encrypted feishu OAuth credential: canonical identity did not stabilize")
	}
	_, actorID := feishuOAuthIdentityKey(feishuOAuthIdentity{OpenID: credential.ActorOpenID, UserID: credential.ActorUserID})
	feishuLog.Info(ctx, "persisted encrypted feishu user OAuth credential account=%s user_ref=%s access_expires_at=%s refresh_present=%t refresh_expires_at=%s scope_count=%d version=%d",
		m.account.ID,
		shortResourceRef(actorID),
		credential.AccessTokenExpiresAt.Format(time.RFC3339),
		credential.RefreshTokenCiphertext != "",
		formatOptionalOAuthTime(credential.RefreshTokenExpiresAt),
		len(strings.Fields(credential.Scopes)),
		credential.Version)
	return credential, nil
}

func (m *oauthCredentialService) encryptedFeishuOAuthCredential(
	identity feishuOAuthIdentity,
	tokens feishuOAuthTokenBundle,
	now time.Time,
) (store.FeishuUserOAuthCredential, error) {
	accessCiphertext, err := m.credentialCipher.Encrypt(identity, "access_token", tokens.AccessToken)
	if err != nil {
		return store.FeishuUserOAuthCredential{}, err
	}
	refreshCiphertext := ""
	refreshExpiresAt := time.Time{}
	if tokens.RefreshToken != "" && tokens.RefreshTokenExpiresIn > 0 {
		refreshCiphertext, err = m.credentialCipher.Encrypt(identity, "refresh_token", tokens.RefreshToken)
		if err != nil {
			return store.FeishuUserOAuthCredential{}, err
		}
		refreshExpiresAt = now.Add(tokens.RefreshTokenExpiresIn)
	}
	return store.FeishuUserOAuthCredential{
		AccountID:              m.account.ID,
		ActorOpenID:            identity.OpenID,
		ActorUserID:            identity.UserID,
		AccessTokenCiphertext:  accessCiphertext,
		AccessTokenExpiresAt:   now.Add(tokens.AccessTokenExpiresIn),
		RefreshTokenCiphertext: refreshCiphertext,
		RefreshTokenExpiresAt:  refreshExpiresAt,
		Scopes:                 tokens.Scopes,
		AuthorizedAt:           now,
		ReauthorizeAt:          now.Add(feishuOAuthMandatoryReauthorizationTTL),
		Status:                 store.FeishuUserOAuthCredentialStatusActive,
		CreatedAt:              now,
		UpdatedAt:              now,
	}, nil
}

// feishuUserAccessToken returns a decrypted usable token and silently rotates
// the one-time refresh token when the access token is near expiry.
func (m *oauthCredentialService) feishuUserAccessToken(ctx context.Context, actorOpenID, actorUserID string) (string, error) {
	if m == nil || m.credentialCipher == nil {
		return "", ErrFeishuUserOAuthCredentialUnavailable
	}

	credential, err := m.store.GetFeishuUserOAuthCredential(m.account.ID, actorOpenID, actorUserID)
	if err != nil {
		if errors.Is(err, store.ErrFeishuUserOAuthCredentialNotFound) {
			return "", ErrFeishuUserOAuthReauthorizationNeeded
		}
		return "", fmt.Errorf("load feishu user OAuth credential: %w", err)
	}
	return m.usableFeishuUserAccessToken(ctx, credential)
}

func (m *oauthCredentialService) usableFeishuUserAccessToken(ctx context.Context, credential store.FeishuUserOAuthCredential) (string, error) {
	now := m.currentTime()
	if credential.Status != store.FeishuUserOAuthCredentialStatusActive {
		return "", ErrFeishuUserOAuthReauthorizationNeeded
	}
	missingScopes := missingOAuthScopes(credential.Scopes, resourceAccessOAuthScope)
	if len(missingScopes) > 0 {
		m.markFeishuOAuthReauthorizationBestEffort(ctx, credential)
		feishuLog.Warn(ctx, "stored feishu user OAuth credential lacks required resource scopes; reauthorization required account=%s credential=%s version=%d missing_scope_count=%d missing_scopes=%s",
			m.account.ID, shortResourceRef(credential.ID), credential.Version, len(missingScopes), strings.Join(missingScopes, ","))
		return "", ErrFeishuUserOAuthReauthorizationNeeded
	}
	if !credential.ReauthorizeAt.After(now) {
		m.markFeishuOAuthReauthorizationBestEffort(ctx, credential)
		return "", ErrFeishuUserOAuthReauthorizationNeeded
	}
	identity := feishuOAuthIdentity{OpenID: credential.ActorOpenID, UserID: credential.ActorUserID}
	if credential.AccessTokenExpiresAt.After(now.Add(feishuOAuthAccessRefreshSkew)) {
		return m.decryptStoredFeishuOAuthToken(ctx, credential, identity, "access_token", credential.AccessTokenCiphertext)
	}
	if credential.RefreshTokenCiphertext == "" || !credential.RefreshTokenExpiresAt.After(now) {
		if credential.AccessTokenExpiresAt.After(now) {
			return m.decryptStoredFeishuOAuthToken(ctx, credential, identity, "access_token", credential.AccessTokenCiphertext)
		}
		m.markFeishuOAuthReauthorizationBestEffort(ctx, credential)
		return "", ErrFeishuUserOAuthReauthorizationNeeded
	}
	return m.refreshFeishuUserAccessToken(ctx, credential)
}

func (m *oauthCredentialService) refreshFeishuUserAccessToken(ctx context.Context, credential store.FeishuUserOAuthCredential) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	leaseToken, err := randomBase64URL(24)
	if err != nil {
		return "", fmt.Errorf("create feishu OAuth refresh lease: %w", err)
	}
	now := m.currentTime()
	attempt, owner, err := m.store.PrepareFeishuOAuthRefreshAttempt(
		credential.ID,
		credential.AccountID,
		credential.Version,
		leaseToken,
		now,
		feishuOAuthRefreshLeaseDuration,
	)
	if err != nil {
		if errors.Is(err, store.ErrFeishuUserOAuthCredentialConflict) {
			if token, handled, latestErr := m.useAdvancedFeishuOAuthCredential(ctx, credential); handled {
				return token, latestErr
			}
		}
		return "", fmt.Errorf("prepare durable feishu OAuth refresh: %w", err)
	}
	if !owner {
		return m.waitForFeishuOAuthRefresh(ctx, credential, attempt)
	}

	identity := feishuOAuthIdentity{OpenID: credential.ActorOpenID, UserID: credential.ActorUserID}
	refreshToken, err := m.credentialCipher.Decrypt(identity, "refresh_token", credential.RefreshTokenCiphertext)
	if err != nil {
		failErr := m.failOwnedFeishuOAuthRefreshAttempt(ctx, attempt, "refresh_token_decrypt_failed", true)
		if token, handled, latestErr := m.useAdvancedFeishuOAuthCredential(ctx, credential); handled {
			return token, latestErr
		}
		if failErr != nil {
			return "", fmt.Errorf("persist feishu OAuth refresh decryption failure: %w", failErr)
		}
		return "", fmt.Errorf("%w: stored refresh token cannot be decrypted", ErrFeishuUserOAuthReauthorizationNeeded)
	}
	feishuLog.Debug(ctx, "refreshing feishu user OAuth credential account=%s credential=%s attempt=%s version=%d",
		m.account.ID, shortResourceRef(credential.ID), shortResourceRef(attempt.ID), credential.Version)
	request := refreshtoken.NewTokenRequestBuilder().RefreshToken(refreshToken).Build()
	resp, err := m.client.AccessToken.Refresh(ctx, request)
	if err != nil {
		if feishuOAuthRefreshOutcomeAmbiguous(err) {
			_, _, markErr := m.store.MarkOwnedFeishuOAuthRefreshAttemptAmbiguous(
				attempt.ID,
				attempt.AccountID,
				attempt.LeaseToken,
				m.currentTime(),
			)
			if token, handled, latestErr := m.useAdvancedFeishuOAuthCredential(ctx, credential); handled {
				return token, latestErr
			}
			if markErr != nil {
				return "", fmt.Errorf("persist ambiguous feishu OAuth refresh outcome: %w", markErr)
			}
			feishuLog.Warn(ctx, "feishu user OAuth refresh outcome is ambiguous; reauthorization required account=%s credential=%s attempt=%s error_type=%T",
				m.account.ID, shortResourceRef(credential.ID), shortResourceRef(attempt.ID), err)
			return "", fmt.Errorf("%w: refresh response outcome could not be verified", ErrFeishuUserOAuthReauthorizationNeeded)
		}
		requiresReauthorization := feishuOAuthRefreshRequiresReauthorization(err)
		failErr := m.failOwnedFeishuOAuthRefreshAttempt(
			ctx,
			attempt,
			feishuOAuthRefreshErrorCategory(err),
			requiresReauthorization,
		)
		if token, handled, latestErr := m.useAdvancedFeishuOAuthCredential(ctx, credential); handled {
			return token, latestErr
		}
		if failErr != nil {
			return "", fmt.Errorf("persist feishu OAuth refresh failure: %w", failErr)
		}
		if requiresReauthorization {
			return "", fmt.Errorf("%w: refresh rejected", ErrFeishuUserOAuthReauthorizationNeeded)
		}
		if token, ok, fallbackErr := m.fallbackFeishuOAuthAccessToken(ctx, credential); ok {
			feishuLog.Warn(ctx, "refresh feishu user OAuth credential failed; using still-valid access token account=%s credential=%s version=%d error_type=%T",
				m.account.ID, shortResourceRef(credential.ID), credential.Version, err)
			return token, fallbackErr
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		return "", fmt.Errorf("%w: refresh request failed", ErrFeishuUserOAuthCredentialUnavailable)
	}
	bundle, err := feishuOAuthTokenBundleFromResponse(resp, credential.Scopes)
	if err != nil || bundle.RefreshToken == "" || bundle.RefreshTokenExpiresIn <= 0 {
		failErr := m.failOwnedFeishuOAuthRefreshAttempt(ctx, attempt, "invalid_refresh_response", true)
		if token, handled, latestErr := m.useAdvancedFeishuOAuthCredential(ctx, credential); handled {
			return token, latestErr
		}
		if failErr != nil {
			return "", fmt.Errorf("persist unusable feishu OAuth refresh response: %w", failErr)
		}
		return "", fmt.Errorf("%w: refresh response was unusable", ErrFeishuUserOAuthReauthorizationNeeded)
	}
	responseAt := m.currentTime()
	accessCiphertext, err := m.credentialCipher.EncryptRefreshAttempt(attempt, "access_token", bundle.AccessToken)
	if err != nil {
		failErr := m.failOwnedFeishuOAuthRefreshAttempt(ctx, attempt, "stage_encryption_failed", true)
		if token, handled, latestErr := m.useAdvancedFeishuOAuthCredential(ctx, credential); handled {
			return token, latestErr
		}
		if failErr != nil {
			return "", fmt.Errorf("persist feishu OAuth refresh staging failure: %w", failErr)
		}
		return "", fmt.Errorf("%w: refreshed credential could not be staged", ErrFeishuUserOAuthReauthorizationNeeded)
	}
	refreshCiphertext, err := m.credentialCipher.EncryptRefreshAttempt(attempt, "refresh_token", bundle.RefreshToken)
	if err != nil {
		failErr := m.failOwnedFeishuOAuthRefreshAttempt(ctx, attempt, "stage_encryption_failed", true)
		if token, handled, latestErr := m.useAdvancedFeishuOAuthCredential(ctx, credential); handled {
			return token, latestErr
		}
		if failErr != nil {
			return "", fmt.Errorf("persist feishu OAuth refresh staging failure: %w", failErr)
		}
		return "", fmt.Errorf("%w: refreshed credential could not be staged", ErrFeishuUserOAuthReauthorizationNeeded)
	}
	staged, err := m.store.StageFeishuOAuthRefreshResponse(
		attempt.ID,
		attempt.AccountID,
		attempt.LeaseToken,
		store.FeishuOAuthRefreshStage{
			AccessTokenCiphertext:  accessCiphertext,
			AccessTokenExpiresAt:   responseAt.Add(bundle.AccessTokenExpiresIn),
			RefreshTokenCiphertext: refreshCiphertext,
			RefreshTokenExpiresAt:  responseAt.Add(bundle.RefreshTokenExpiresIn),
			Scopes:                 bundle.Scopes,
		},
		responseAt,
	)
	if err != nil {
		failErr := m.failOwnedFeishuOAuthRefreshAttempt(ctx, attempt, "stage_persistence_failed", true)
		if errors.Is(err, store.ErrFeishuOAuthRefreshAttemptConflict) && staged.State == store.FeishuOAuthRefreshAttemptStateResponseStaged {
			return m.accessTokenFromStagedFeishuOAuthRefresh(ctx, staged)
		}
		if token, handled, latestErr := m.useAdvancedFeishuOAuthCredential(ctx, credential); handled {
			return token, latestErr
		}
		if errors.Is(failErr, store.ErrFeishuOAuthRefreshAttemptConflict) || errors.Is(failErr, store.ErrFeishuOAuthRefreshAttemptLeaseLost) {
			current, loadErr := m.store.GetFeishuOAuthRefreshAttempt(attempt.ID, attempt.AccountID)
			if loadErr == nil && current.State == store.FeishuOAuthRefreshAttemptStateResponseStaged {
				return m.accessTokenFromStagedFeishuOAuthRefresh(ctx, current)
			}
			if token, handled, latestErr := m.useAdvancedFeishuOAuthCredential(ctx, credential); handled {
				return token, latestErr
			}
		}
		return "", fmt.Errorf("%w: refreshed credential could not be persisted", ErrFeishuUserOAuthReauthorizationNeeded)
	}
	return m.accessTokenFromStagedFeishuOAuthRefresh(ctx, staged)
}

func (m *oauthCredentialService) accessTokenFromStagedFeishuOAuthRefresh(
	ctx context.Context,
	attempt store.FeishuOAuthRefreshAttempt,
) (string, error) {
	credential, err := m.applyStagedFeishuOAuthRefresh(ctx, attempt)
	if err != nil {
		return "", err
	}
	return m.usableFeishuUserAccessToken(ctx, credential)
}

func (m *oauthCredentialService) applyStagedFeishuOAuthRefresh(
	ctx context.Context,
	attempt store.FeishuOAuthRefreshAttempt,
) (store.FeishuUserOAuthCredential, error) {
	credential, err := m.store.GetFeishuUserOAuthCredentialByID(attempt.CredentialID, attempt.AccountID)
	if err != nil {
		return store.FeishuUserOAuthCredential{}, fmt.Errorf("load staged feishu OAuth credential: %w", err)
	}
	if credential.Version > attempt.ExpectedVersion {
		rotated, _, err := m.store.ApplyFeishuOAuthRefreshAttempt(
			attempt.ID,
			attempt.AccountID,
			store.FeishuOAuthRefreshCredentialUpdate{},
			m.currentTime(),
		)
		if err != nil && !errors.Is(err, store.ErrFeishuOAuthRefreshAttemptConflict) {
			return store.FeishuUserOAuthCredential{}, fmt.Errorf("complete superseded feishu OAuth refresh: %w", err)
		}
		if err == nil {
			credential = rotated
		}
		return credential, nil
	}
	if credential.Version != attempt.ExpectedVersion || credential.Status != store.FeishuUserOAuthCredentialStatusActive || m.credentialCipher == nil {
		resolved, invalidateErr := m.invalidateStagedFeishuOAuthRefresh(ctx, attempt, "staged_context_invalid")
		if invalidateErr != nil {
			return store.FeishuUserOAuthCredential{}, fmt.Errorf("invalidate staged feishu OAuth refresh context: %w", invalidateErr)
		}
		if resolved.Version > credential.Version && resolved.Status == store.FeishuUserOAuthCredentialStatusActive {
			return resolved, nil
		}
		return store.FeishuUserOAuthCredential{}, ErrFeishuUserOAuthReauthorizationNeeded
	}
	accessToken, err := m.credentialCipher.DecryptRefreshAttempt(attempt, "access_token", attempt.AccessTokenCiphertext)
	if err != nil {
		resolved, invalidateErr := m.invalidateStagedFeishuOAuthRefresh(ctx, attempt, "staged_access_decrypt_failed")
		if invalidateErr != nil {
			return store.FeishuUserOAuthCredential{}, fmt.Errorf("invalidate unreadable staged feishu OAuth access token: %w", invalidateErr)
		}
		if resolved.Version > credential.Version && resolved.Status == store.FeishuUserOAuthCredentialStatusActive {
			return resolved, nil
		}
		return store.FeishuUserOAuthCredential{}, fmt.Errorf("%w: staged access token cannot be decrypted", ErrFeishuUserOAuthReauthorizationNeeded)
	}
	refreshToken, err := m.credentialCipher.DecryptRefreshAttempt(attempt, "refresh_token", attempt.RefreshTokenCiphertext)
	if err != nil {
		resolved, invalidateErr := m.invalidateStagedFeishuOAuthRefresh(ctx, attempt, "staged_refresh_decrypt_failed")
		if invalidateErr != nil {
			return store.FeishuUserOAuthCredential{}, fmt.Errorf("invalidate unreadable staged feishu OAuth refresh token: %w", invalidateErr)
		}
		if resolved.Version > credential.Version && resolved.Status == store.FeishuUserOAuthCredentialStatusActive {
			return resolved, nil
		}
		return store.FeishuUserOAuthCredential{}, fmt.Errorf("%w: staged refresh token cannot be decrypted", ErrFeishuUserOAuthReauthorizationNeeded)
	}
	identity := feishuOAuthIdentity{OpenID: credential.ActorOpenID, UserID: credential.ActorUserID}
	accessCiphertext, err := m.credentialCipher.Encrypt(identity, "access_token", accessToken)
	if err != nil {
		resolved, invalidateErr := m.invalidateStagedFeishuOAuthRefresh(ctx, attempt, "credential_encryption_failed")
		if invalidateErr != nil {
			return store.FeishuUserOAuthCredential{}, fmt.Errorf("invalidate unencryptable feishu OAuth access token: %w", invalidateErr)
		}
		if resolved.Version > credential.Version && resolved.Status == store.FeishuUserOAuthCredentialStatusActive {
			return resolved, nil
		}
		return store.FeishuUserOAuthCredential{}, fmt.Errorf("%w: refreshed access token cannot be encrypted", ErrFeishuUserOAuthReauthorizationNeeded)
	}
	refreshCiphertext, err := m.credentialCipher.Encrypt(identity, "refresh_token", refreshToken)
	if err != nil {
		resolved, invalidateErr := m.invalidateStagedFeishuOAuthRefresh(ctx, attempt, "credential_encryption_failed")
		if invalidateErr != nil {
			return store.FeishuUserOAuthCredential{}, fmt.Errorf("invalidate unencryptable feishu OAuth refresh token: %w", invalidateErr)
		}
		if resolved.Version > credential.Version && resolved.Status == store.FeishuUserOAuthCredentialStatusActive {
			return resolved, nil
		}
		return store.FeishuUserOAuthCredential{}, fmt.Errorf("%w: refreshed refresh token cannot be encrypted", ErrFeishuUserOAuthReauthorizationNeeded)
	}
	rotated, completed, err := m.store.ApplyFeishuOAuthRefreshAttempt(
		attempt.ID,
		attempt.AccountID,
		store.FeishuOAuthRefreshCredentialUpdate{
			AccessTokenCiphertext:  accessCiphertext,
			AccessTokenExpiresAt:   attempt.AccessTokenExpiresAt,
			RefreshTokenCiphertext: refreshCiphertext,
			RefreshTokenExpiresAt:  attempt.RefreshTokenExpiresAt,
			Scopes:                 attempt.Scopes,
		},
		m.currentTime(),
	)
	if err != nil {
		return store.FeishuUserOAuthCredential{}, fmt.Errorf("apply staged feishu OAuth refresh: %w", err)
	}
	feishuLog.Info(ctx, "completed durable feishu user OAuth refresh account=%s credential=%s attempt=%s access_expires_at=%s refresh_expires_at=%s scope_count=%d version=%d state=%s",
		m.account.ID,
		shortResourceRef(rotated.ID),
		shortResourceRef(attempt.ID),
		rotated.AccessTokenExpiresAt.Format(time.RFC3339),
		formatOptionalOAuthTime(rotated.RefreshTokenExpiresAt),
		len(strings.Fields(rotated.Scopes)),
		rotated.Version,
		completed.State)
	return rotated, nil
}

func (m *oauthCredentialService) waitForFeishuOAuthRefresh(
	ctx context.Context,
	original store.FeishuUserOAuthCredential,
	attempt store.FeishuOAuthRefreshAttempt,
) (string, error) {
	timer := time.NewTimer(m.effectiveFeishuOAuthRefreshPeerWait())
	defer timer.Stop()
	ticker := time.NewTicker(m.effectiveFeishuOAuthRefreshPollInterval())
	defer ticker.Stop()
	for {
		latest, err := m.store.GetFeishuUserOAuthCredentialByID(original.ID, original.AccountID)
		if err != nil {
			return "", fmt.Errorf("reload feishu OAuth credential while waiting for refresh: %w", err)
		}
		if latest.Version > original.Version || latest.Status != store.FeishuUserOAuthCredentialStatusActive {
			return m.usableFeishuUserAccessToken(ctx, latest)
		}
		current, err := m.store.GetFeishuOAuthRefreshAttempt(attempt.ID, attempt.AccountID)
		if err != nil {
			if !errors.Is(err, store.ErrFeishuOAuthRefreshAttemptNotFound) {
				return "", fmt.Errorf("reload feishu OAuth refresh attempt: %w", err)
			}
		} else {
			switch current.State {
			case store.FeishuOAuthRefreshAttemptStateResponseStaged:
				return m.accessTokenFromStagedFeishuOAuthRefresh(ctx, current)
			case store.FeishuOAuthRefreshAttemptStateCompleted:
				return m.usableFeishuUserAccessToken(ctx, latest)
			case store.FeishuOAuthRefreshAttemptStateAmbiguous:
				return "", ErrFeishuUserOAuthReauthorizationNeeded
			case store.FeishuOAuthRefreshAttemptStateFailed:
				if token, ok, fallbackErr := m.fallbackFeishuOAuthAccessToken(ctx, latest); ok {
					return token, fallbackErr
				}
				return "", ErrFeishuUserOAuthCredentialUnavailable
			case store.FeishuOAuthRefreshAttemptStatePrepared:
				if !current.LeaseExpiresAt.After(m.currentTime()) {
					resolvedCredential, _, markErr := m.store.MarkFeishuOAuthRefreshAttemptAmbiguous(current.ID, current.AccountID, m.currentTime())
					if markErr == nil {
						return m.usableFeishuUserAccessToken(ctx, resolvedCredential)
					}
					if !errors.Is(markErr, store.ErrFeishuOAuthRefreshAttemptConflict) && !errors.Is(markErr, store.ErrFeishuOAuthRefreshAttemptLeaseLost) {
						return "", fmt.Errorf("resolve expired feishu OAuth refresh attempt: %w", markErr)
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-timer.C:
			return m.resolveFeishuOAuthRefreshPeerTimeout(ctx, original, attempt)
		case <-ticker.C:
		}
	}
}

func (m *oauthCredentialService) resolveFeishuOAuthRefreshPeerTimeout(
	ctx context.Context,
	original store.FeishuUserOAuthCredential,
	attempt store.FeishuOAuthRefreshAttempt,
) (string, error) {
	// One bounded reload closes the race where the lease owner stages or
	// completes its response after our read but before our ambiguous claim.
	for loadAttempt := 0; loadAttempt < 2; loadAttempt++ {
		latest, err := m.store.GetFeishuUserOAuthCredentialByID(original.ID, original.AccountID)
		if err != nil {
			return "", fmt.Errorf("reload feishu OAuth credential after peer wait: %w", err)
		}
		current, attemptErr := m.store.GetFeishuOAuthRefreshAttempt(attempt.ID, attempt.AccountID)
		if attemptErr != nil && !errors.Is(attemptErr, store.ErrFeishuOAuthRefreshAttemptNotFound) {
			return "", fmt.Errorf("reload feishu OAuth refresh attempt after peer wait: %w", attemptErr)
		}
		if latest.Version > original.Version || latest.Status != store.FeishuUserOAuthCredentialStatusActive {
			return m.usableFeishuUserAccessToken(ctx, latest)
		}
		if attemptErr == nil {
			switch current.State {
			case store.FeishuOAuthRefreshAttemptStateResponseStaged:
				return m.accessTokenFromStagedFeishuOAuthRefresh(ctx, current)
			case store.FeishuOAuthRefreshAttemptStateCompleted:
				return m.usableFeishuUserAccessToken(ctx, latest)
			case store.FeishuOAuthRefreshAttemptStateAmbiguous:
				return "", ErrFeishuUserOAuthReauthorizationNeeded
			case store.FeishuOAuthRefreshAttemptStateFailed:
				if token, ok, fallbackErr := m.fallbackFeishuOAuthAccessToken(ctx, latest); ok {
					return token, fallbackErr
				}
				return "", ErrFeishuUserOAuthCredentialUnavailable
			case store.FeishuOAuthRefreshAttemptStatePrepared:
				if !current.LeaseExpiresAt.After(m.currentTime()) {
					resolvedCredential, _, markErr := m.store.MarkFeishuOAuthRefreshAttemptAmbiguous(current.ID, current.AccountID, m.currentTime())
					if markErr == nil {
						return m.usableFeishuUserAccessToken(ctx, resolvedCredential)
					}
					if errors.Is(markErr, store.ErrFeishuOAuthRefreshAttemptConflict) ||
						errors.Is(markErr, store.ErrFeishuOAuthRefreshAttemptLeaseLost) {
						continue
					}
					return "", fmt.Errorf("resolve expired feishu OAuth refresh attempt after peer wait: %w", markErr)
				}
			}
		}
		if token, ok, fallbackErr := m.fallbackFeishuOAuthAccessToken(ctx, latest); ok {
			feishuLog.Warn(ctx, "feishu OAuth refresh is still owned by another worker; using still-valid access token account=%s credential=%s attempt=%s version=%d",
				m.account.ID, shortResourceRef(original.ID), shortResourceRef(attempt.ID), original.Version)
			return token, fallbackErr
		}
		return "", fmt.Errorf("%w: refresh is already in progress", ErrFeishuUserOAuthCredentialUnavailable)
	}
	return "", fmt.Errorf("%w: refresh state changed repeatedly while resolving peer timeout", ErrFeishuUserOAuthCredentialUnavailable)
}

func (m *oauthCredentialService) effectiveFeishuOAuthRefreshPeerWait() time.Duration {
	if m.refreshPeerWait <= 0 {
		return feishuOAuthRefreshPeerWait
	}
	return m.refreshPeerWait
}

func (m *oauthCredentialService) effectiveFeishuOAuthRefreshPollInterval() time.Duration {
	if m.refreshPoll <= 0 {
		return feishuOAuthRefreshPollInterval
	}
	return m.refreshPoll
}

func (m *oauthCredentialService) fallbackFeishuOAuthAccessToken(
	ctx context.Context,
	credential store.FeishuUserOAuthCredential,
) (string, bool, error) {
	if !credential.AccessTokenExpiresAt.After(m.currentTime().Add(feishuOAuthMinimumFallbackValidity)) {
		return "", false, nil
	}
	token, err := m.decryptStoredFeishuOAuthToken(
		ctx,
		credential,
		feishuOAuthIdentity{OpenID: credential.ActorOpenID, UserID: credential.ActorUserID},
		"access_token",
		credential.AccessTokenCiphertext,
	)
	return token, true, err
}

func (m *oauthCredentialService) useAdvancedFeishuOAuthCredential(
	ctx context.Context,
	original store.FeishuUserOAuthCredential,
) (string, bool, error) {
	latest, err := m.store.GetFeishuUserOAuthCredentialByID(original.ID, original.AccountID)
	if err != nil {
		return "", true, fmt.Errorf("load advanced feishu OAuth credential: %w", err)
	}
	if latest.Version <= original.Version {
		return "", false, nil
	}
	returnToken, tokenErr := m.usableFeishuUserAccessToken(ctx, latest)
	return returnToken, true, tokenErr
}

func (m *oauthCredentialService) failOwnedFeishuOAuthRefreshAttempt(
	ctx context.Context,
	attempt store.FeishuOAuthRefreshAttempt,
	errorCategory string,
	requireReauthorization bool,
) error {
	_, _, err := m.store.FailFeishuOAuthRefreshAttempt(
		attempt.ID,
		attempt.AccountID,
		attempt.LeaseToken,
		errorCategory,
		requireReauthorization,
		m.currentTime(),
	)
	if err != nil && !errors.Is(err, store.ErrFeishuOAuthRefreshAttemptConflict) &&
		!errors.Is(err, store.ErrFeishuOAuthRefreshAttemptLeaseLost) &&
		!errors.Is(err, store.ErrFeishuUserOAuthCredentialConflict) {
		feishuLog.Warn(ctx, "close feishu OAuth refresh attempt failed account=%s credential=%s attempt=%s category=%s error_type=%T",
			m.account.ID, shortResourceRef(attempt.CredentialID), shortResourceRef(attempt.ID), errorCategory, err)
	}
	return err
}

func (m *oauthCredentialService) invalidateStagedFeishuOAuthRefresh(
	ctx context.Context,
	attempt store.FeishuOAuthRefreshAttempt,
	errorCategory string,
) (store.FeishuUserOAuthCredential, error) {
	credential, _, err := m.store.InvalidateFeishuOAuthRefreshAttempt(
		attempt.ID,
		attempt.AccountID,
		errorCategory,
		m.currentTime(),
	)
	if err == nil {
		return credential, nil
	}
	if errors.Is(err, store.ErrFeishuOAuthRefreshAttemptConflict) ||
		errors.Is(err, store.ErrFeishuUserOAuthCredentialConflict) {
		latest, loadErr := m.store.GetFeishuUserOAuthCredentialByID(attempt.CredentialID, attempt.AccountID)
		if loadErr != nil {
			return store.FeishuUserOAuthCredential{}, fmt.Errorf("reload credential after staged refresh invalidation conflict: %w", loadErr)
		}
		return latest, nil
	}
	feishuLog.Warn(ctx, "invalidate staged feishu OAuth refresh failed account=%s credential=%s attempt=%s category=%s error_type=%T",
		m.account.ID, shortResourceRef(attempt.CredentialID), shortResourceRef(attempt.ID), errorCategory, err)
	return store.FeishuUserOAuthCredential{}, err
}

func (m *oauthCredentialService) recoverFeishuOAuthRefreshAttempts(ctx context.Context) error {
	if m == nil || m.store == nil {
		return ErrFeishuUserOAuthCredentialUnavailable
	}
	now := m.currentTime()
	scanned := 0
	completed := 0
	ambiguous := 0
	failed := 0
	for {
		attempts, err := m.store.ListRecoverableFeishuOAuthRefreshAttempts(
			m.account.ID,
			now,
			feishuOAuthRefreshRecoveryLimit,
		)
		if err != nil {
			return fmt.Errorf("list recoverable feishu OAuth refresh attempts: %w", err)
		}
		if len(attempts) == 0 {
			break
		}
		scanned += len(attempts)
		resolvedThisBatch := 0
		for _, attempt := range attempts {
			switch attempt.State {
			case store.FeishuOAuthRefreshAttemptStateResponseStaged:
				_, applyErr := m.applyStagedFeishuOAuthRefresh(ctx, attempt)
				if applyErr != nil {
					if errors.Is(applyErr, ErrFeishuUserOAuthReauthorizationNeeded) {
						failed++
						resolvedThisBatch++
						continue
					}
					if errors.Is(applyErr, store.ErrFeishuOAuthRefreshAttemptConflict) {
						continue
					}
					return fmt.Errorf("recover staged feishu OAuth refresh attempt: %w", applyErr)
				}
				completed++
				resolvedThisBatch++
			case store.FeishuOAuthRefreshAttemptStatePrepared:
				_, resolved, markErr := m.store.MarkFeishuOAuthRefreshAttemptAmbiguous(
					attempt.ID,
					attempt.AccountID,
					now,
				)
				if markErr != nil {
					if errors.Is(markErr, store.ErrFeishuOAuthRefreshAttemptConflict) ||
						errors.Is(markErr, store.ErrFeishuOAuthRefreshAttemptLeaseLost) ||
						errors.Is(markErr, store.ErrFeishuUserOAuthCredentialConflict) {
						continue
					}
					return fmt.Errorf("resolve interrupted feishu OAuth refresh attempt: %w", markErr)
				}
				resolvedThisBatch++
				if resolved.State == store.FeishuOAuthRefreshAttemptStateAmbiguous {
					ambiguous++
				} else if resolved.State == store.FeishuOAuthRefreshAttemptStateCompleted {
					completed++
				}
			}
		}
		if len(attempts) < feishuOAuthRefreshRecoveryLimit {
			break
		}
		if resolvedThisBatch == 0 {
			feishuLog.Warn(ctx, "stopped feishu OAuth refresh recovery without progress account=%s batch_size=%d",
				m.account.ID, len(attempts))
			break
		}
	}
	if completed > 0 || ambiguous > 0 || failed > 0 {
		feishuLog.Info(ctx, "recovered durable feishu OAuth refresh attempts account=%s scanned=%d completed=%d ambiguous=%d failed=%d",
			m.account.ID, scanned, completed, ambiguous, failed)
	}
	return nil
}

func (m *oauthCredentialService) decryptStoredFeishuOAuthToken(
	ctx context.Context,
	credential store.FeishuUserOAuthCredential,
	identity feishuOAuthIdentity,
	field string,
	ciphertext string,
) (string, error) {
	token, err := m.credentialCipher.Decrypt(identity, field, ciphertext)
	if err == nil {
		return token, nil
	}
	m.markFeishuOAuthReauthorizationBestEffort(ctx, credential)
	feishuLog.Warn(ctx, "decrypt stored feishu user OAuth credential failed; reauthorization required account=%s credential=%s version=%d field=%s error_type=%T",
		m.account.ID, shortResourceRef(credential.ID), credential.Version, field, err)
	return "", fmt.Errorf("%w: stored %s cannot be decrypted", ErrFeishuUserOAuthReauthorizationNeeded, field)
}

func (m *oauthCredentialService) markFeishuOAuthReauthorizationBestEffort(ctx context.Context, credential store.FeishuUserOAuthCredential) {
	if credential.ID == "" || credential.Version <= 0 {
		return
	}
	_, err := m.store.MarkFeishuUserOAuthCredentialReauthRequired(
		credential.ID,
		credential.AccountID,
		credential.Version,
		m.currentTime(),
	)
	if err != nil && !errors.Is(err, store.ErrFeishuUserOAuthCredentialConflict) {
		feishuLog.Warn(ctx, "mark feishu user OAuth credential reauthorization required failed account=%s credential=%s version=%d: %v",
			m.account.ID, shortResourceRef(credential.ID), credential.Version, err)
	}
}

func feishuOAuthTokenBundleFromResponse(resp *accesstoken.AccessTokenResp, fallbackScopes string) (feishuOAuthTokenBundle, error) {
	if resp == nil || !resp.Success() || resp.Data == nil {
		return feishuOAuthTokenBundle{}, fmt.Errorf("empty or unsuccessful feishu OAuth token response")
	}
	bundle := feishuOAuthTokenBundle{
		AccessToken:  strings.TrimSpace(deref(resp.Data.AccessToken)),
		RefreshToken: strings.TrimSpace(deref(resp.Data.RefreshToken)),
		Scopes:       strings.TrimSpace(deref(resp.Data.Scope)),
	}
	if resp.Data.ExpiresIn != nil && *resp.Data.ExpiresIn > 0 {
		bundle.AccessTokenExpiresIn = time.Duration(*resp.Data.ExpiresIn) * time.Second
	}
	if resp.Data.RefreshTokenExpiresIn != nil && *resp.Data.RefreshTokenExpiresIn > 0 {
		bundle.RefreshTokenExpiresIn = time.Duration(*resp.Data.RefreshTokenExpiresIn) * time.Second
	}
	if bundle.Scopes == "" {
		bundle.Scopes = fallbackScopes
	}
	bundle = normalizeFeishuOAuthTokenBundle(bundle)
	if bundle.AccessToken == "" || bundle.AccessTokenExpiresIn <= 0 {
		return feishuOAuthTokenBundle{}, fmt.Errorf("feishu OAuth token response has no usable access token")
	}
	return bundle, nil
}

func normalizeFeishuOAuthTokenBundle(bundle feishuOAuthTokenBundle) feishuOAuthTokenBundle {
	bundle.AccessToken = strings.TrimSpace(bundle.AccessToken)
	bundle.RefreshToken = strings.TrimSpace(bundle.RefreshToken)
	bundle.Scopes = canonicalOAuthScopes(bundle.Scopes)
	return bundle
}

func canonicalOAuthScopes(scopes string) string {
	values := strings.Fields(scopes)
	sort.Strings(values)
	unique := values[:0]
	for _, value := range values {
		if len(unique) == 0 || unique[len(unique)-1] != value {
			unique = append(unique, value)
		}
	}
	return strings.Join(unique, " ")
}

func missingOAuthScopes(granted, required string) []string {
	grantedSet := make(map[string]struct{}, len(strings.Fields(granted)))
	for _, scope := range strings.Fields(granted) {
		grantedSet[scope] = struct{}{}
	}
	missing := make([]string, 0)
	for _, scope := range strings.Fields(required) {
		if _, ok := grantedSet[scope]; !ok {
			missing = append(missing, scope)
		}
	}
	sort.Strings(missing)
	return missing
}

func feishuOAuthRefreshRequiresReauthorization(err error) bool {
	var tokenErr *accesstoken.AccessTokenError
	if !errors.As(err, &tokenErr) {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(tokenErr.ErrorType), "invalid_grant")
}

func feishuOAuthRefreshOutcomeAmbiguous(err error) bool {
	if err == nil {
		return false
	}
	var tokenErr *accesstoken.AccessTokenError
	if errors.As(err, &tokenErr) {
		if tokenErr.ApiResp != nil {
			return tokenErr.ApiResp.StatusCode == http.StatusRequestTimeout || tokenErr.ApiResp.StatusCode >= http.StatusInternalServerError
		}
		return tokenErr.Code == 0 && strings.TrimSpace(tokenErr.ErrorType) == ""
	}
	var dialErr *larkcore.DialFailedError
	if errors.As(err, &dialErr) {
		return false
	}
	var illegalParamErr *larkcore.IllegalParamError
	if errors.As(err, &illegalParamErr) {
		return false
	}
	var codeErr *larkcore.CodeError
	return !errors.As(err, &codeErr)
}

func feishuOAuthRefreshErrorCategory(err error) string {
	if errors.Is(err, context.Canceled) {
		return "context_canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline_exceeded"
	}
	var tokenErr *accesstoken.AccessTokenError
	if errors.As(err, &tokenErr) {
		if errorType := strings.ToLower(strings.TrimSpace(tokenErr.ErrorType)); errorType != "" {
			if sanitized := sanitizeFeishuOAuthErrorType(errorType); sanitized != "" {
				return "oauth_" + sanitized
			}
			return "oauth_error"
		}
		if tokenErr.ApiResp != nil {
			return fmt.Sprintf("oauth_http_%d", tokenErr.ApiResp.StatusCode)
		}
		return "oauth_error"
	}
	return "transport_error"
}

func sanitizeFeishuOAuthErrorType(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" || len(value) > 48 {
		return ""
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '_' || char == '-' {
			continue
		}
		return ""
	}
	return value
}

func formatOptionalOAuthTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}
