package monitor

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/larksuite/oapi-sdk-go/v3/core/accesstoken"
	"github.com/larksuite/oapi-sdk-go/v3/core/accesstoken/refreshtoken"

	"lingobridge/internal/store"
)

const (
	feishuOAuthCredentialCipherVersion     = "v1"
	feishuOAuthCredentialKeyInfo           = "lingobridge/feishu/user-oauth-credentials/v1"
	feishuOAuthAccessRefreshSkew           = 5 * time.Minute
	feishuOAuthMinimumFallbackValidity     = 30 * time.Second
	feishuOAuthMandatoryReauthorizationTTL = 365 * 24 * time.Hour
)

var (
	ErrFeishuUserOAuthCredentialUnavailable = errors.New("feishu user oauth credential is unavailable")
	ErrFeishuUserOAuthReauthorizationNeeded = errors.New("feishu user oauth reauthorization is required")
)

type feishuOAuthCredentialCipher struct {
	accountID string
	aead      cipher.AEAD
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

func newFeishuOAuthCredentialCipher(secret, accountID string) (*feishuOAuthCredentialCipher, error) {
	secret = strings.TrimSpace(secret)
	accountID = strings.TrimSpace(accountID)
	if secret == "" || accountID == "" {
		return nil, fmt.Errorf("feishu OAuth credential secret and account are required")
	}
	key, err := hkdf.Key(sha256.New, []byte(secret), []byte(accountID), feishuOAuthCredentialKeyInfo, 32)
	if err != nil {
		return nil, fmt.Errorf("derive feishu OAuth credential key: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create feishu OAuth credential cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create feishu OAuth credential AEAD: %w", err)
	}
	return &feishuOAuthCredentialCipher{accountID: accountID, aead: aead}, nil
}

func (c *feishuOAuthCredentialCipher) Encrypt(identity feishuOAuthIdentity, field, plaintext string) (string, error) {
	if c == nil || c.aead == nil {
		return "", ErrFeishuUserOAuthCredentialUnavailable
	}
	plaintext = strings.TrimSpace(plaintext)
	if plaintext == "" {
		return "", nil
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("generate feishu OAuth credential nonce: %w", err)
	}
	sealed := c.aead.Seal(nil, nonce, []byte(plaintext), c.additionalData(identity, field))
	encoded := append(nonce, sealed...)
	return feishuOAuthCredentialCipherVersion + "." + base64.RawURLEncoding.EncodeToString(encoded), nil
}

func (c *feishuOAuthCredentialCipher) Decrypt(identity feishuOAuthIdentity, field, ciphertext string) (string, error) {
	if c == nil || c.aead == nil {
		return "", ErrFeishuUserOAuthCredentialUnavailable
	}
	version, encoded, ok := strings.Cut(strings.TrimSpace(ciphertext), ".")
	if !ok || version != feishuOAuthCredentialCipherVersion || encoded == "" {
		return "", fmt.Errorf("unsupported feishu OAuth credential ciphertext")
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("decode feishu OAuth credential ciphertext: %w", err)
	}
	if len(raw) <= c.aead.NonceSize() {
		return "", fmt.Errorf("invalid feishu OAuth credential ciphertext")
	}
	nonce := raw[:c.aead.NonceSize()]
	sealed := raw[c.aead.NonceSize():]
	plain, err := c.aead.Open(nil, nonce, sealed, c.additionalData(identity, field))
	if err != nil {
		return "", fmt.Errorf("decrypt feishu OAuth credential: %w", err)
	}
	return string(plain), nil
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

func (m *resourceAccessManager) persistFeishuOAuthCredential(ctx context.Context, identity feishuOAuthIdentity, tokens feishuOAuthTokenBundle) (store.FeishuUserOAuthCredential, error) {
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
	credential, err := m.store.SaveFeishuUserOAuthCredential(store.FeishuUserOAuthCredential{
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
	})
	if err != nil {
		return store.FeishuUserOAuthCredential{}, fmt.Errorf("persist encrypted feishu OAuth credential: %w", err)
	}
	_, actorID := feishuOAuthIdentityKey(identity)
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

// feishuUserAccessToken returns a decrypted usable token and silently rotates
// the one-time refresh token when the access token is near expiry.
func (m *resourceAccessManager) feishuUserAccessToken(ctx context.Context, actorOpenID, actorUserID string) (string, error) {
	if m == nil || m.credentialCipher == nil {
		return "", ErrFeishuUserOAuthCredentialUnavailable
	}
	m.credentialMu.Lock()
	defer m.credentialMu.Unlock()

	credential, err := m.store.GetFeishuUserOAuthCredential(m.account.ID, actorOpenID, actorUserID)
	if err != nil {
		if errors.Is(err, store.ErrFeishuUserOAuthCredentialNotFound) {
			return "", ErrFeishuUserOAuthReauthorizationNeeded
		}
		return "", fmt.Errorf("load feishu user OAuth credential: %w", err)
	}
	return m.usableFeishuUserAccessToken(ctx, credential)
}

func (m *resourceAccessManager) usableFeishuUserAccessToken(ctx context.Context, credential store.FeishuUserOAuthCredential) (string, error) {
	now := m.currentTime()
	if credential.Status != store.FeishuUserOAuthCredentialStatusActive {
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
	refreshToken, err := m.credentialCipher.Decrypt(identity, "refresh_token", credential.RefreshTokenCiphertext)
	if err != nil {
		m.markFeishuOAuthReauthorizationBestEffort(ctx, credential)
		return "", fmt.Errorf("%w: stored refresh token cannot be decrypted", ErrFeishuUserOAuthReauthorizationNeeded)
	}
	request := refreshtoken.NewTokenRequestBuilder().RefreshToken(refreshToken).Build()
	resp, err := m.client.AccessToken.Refresh(ctx, request)
	if err != nil {
		if feishuOAuthRefreshRequiresReauthorization(err) {
			m.markFeishuOAuthReauthorizationBestEffort(ctx, credential)
			return "", fmt.Errorf("%w: refresh rejected", ErrFeishuUserOAuthReauthorizationNeeded)
		}
		if credential.AccessTokenExpiresAt.After(now.Add(feishuOAuthMinimumFallbackValidity)) {
			feishuLog.Warn(ctx, "refresh feishu user OAuth credential failed; using still-valid access token account=%s credential=%s version=%d error_type=%T",
				m.account.ID, shortResourceRef(credential.ID), credential.Version, err)
			return m.credentialCipher.Decrypt(identity, "access_token", credential.AccessTokenCiphertext)
		}
		return "", fmt.Errorf("refresh feishu user OAuth credential: %w", err)
	}
	bundle, err := feishuOAuthTokenBundleFromResponse(resp, credential.Scopes)
	if err != nil || bundle.RefreshToken == "" || bundle.RefreshTokenExpiresIn <= 0 {
		m.markFeishuOAuthReauthorizationBestEffort(ctx, credential)
		if err == nil {
			err = fmt.Errorf("refresh response did not rotate refresh token")
		}
		return "", fmt.Errorf("%w: %v", ErrFeishuUserOAuthReauthorizationNeeded, err)
	}
	accessCiphertext, err := m.credentialCipher.Encrypt(identity, "access_token", bundle.AccessToken)
	if err != nil {
		return "", err
	}
	refreshCiphertext, err := m.credentialCipher.Encrypt(identity, "refresh_token", bundle.RefreshToken)
	if err != nil {
		return "", err
	}
	rotated := credential
	rotated.AccessTokenCiphertext = accessCiphertext
	rotated.AccessTokenExpiresAt = now.Add(bundle.AccessTokenExpiresIn)
	rotated.RefreshTokenCiphertext = refreshCiphertext
	rotated.RefreshTokenExpiresAt = now.Add(bundle.RefreshTokenExpiresIn)
	rotated.Scopes = bundle.Scopes
	rotated.LastRefreshedAt = now
	rotated.Status = store.FeishuUserOAuthCredentialStatusActive
	rotated.UpdatedAt = now
	rotated, err = m.store.RotateFeishuUserOAuthCredential(rotated, credential.Version)
	if err != nil {
		if errors.Is(err, store.ErrFeishuUserOAuthCredentialConflict) {
			latest, loadErr := m.store.GetFeishuUserOAuthCredential(m.account.ID, credential.ActorOpenID, credential.ActorUserID)
			if loadErr == nil && latest.Status == store.FeishuUserOAuthCredentialStatusActive && latest.AccessTokenExpiresAt.After(now) {
				return m.decryptStoredFeishuOAuthToken(
					ctx,
					latest,
					feishuOAuthIdentity{OpenID: latest.ActorOpenID, UserID: latest.ActorUserID},
					"access_token",
					latest.AccessTokenCiphertext,
				)
			}
		}
		return "", fmt.Errorf("persist rotated feishu user OAuth credential: %w", err)
	}
	feishuLog.Info(ctx, "rotated encrypted feishu user OAuth credential account=%s credential=%s access_expires_at=%s refresh_expires_at=%s scope_count=%d version=%d",
		m.account.ID,
		shortResourceRef(rotated.ID),
		rotated.AccessTokenExpiresAt.Format(time.RFC3339),
		formatOptionalOAuthTime(rotated.RefreshTokenExpiresAt),
		len(strings.Fields(rotated.Scopes)),
		rotated.Version)
	return bundle.AccessToken, nil
}

func (m *resourceAccessManager) decryptStoredFeishuOAuthToken(
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

func (m *resourceAccessManager) markFeishuOAuthReauthorizationBestEffort(ctx context.Context, credential store.FeishuUserOAuthCredential) {
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

func feishuOAuthRefreshRequiresReauthorization(err error) bool {
	var tokenErr *accesstoken.AccessTokenError
	if !errors.As(err, &tokenErr) {
		return false
	}
	if tokenErr.ApiResp != nil && tokenErr.ApiResp.StatusCode >= http.StatusBadRequest && tokenErr.ApiResp.StatusCode < http.StatusInternalServerError {
		return true
	}
	return tokenErr.ErrorType == "invalid_grant"
}

func formatOptionalOAuthTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}
