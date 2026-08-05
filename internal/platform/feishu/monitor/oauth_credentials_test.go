package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/core/accesstoken"

	"lingobridge/internal/logging"
	"lingobridge/internal/store"
)

type observedRefreshAttemptStore struct {
	*store.Store
	preparedRead chan struct{}
	once         sync.Once
}

func (s *observedRefreshAttemptStore) GetFeishuOAuthRefreshAttempt(attemptID, accountID string) (store.FeishuOAuthRefreshAttempt, error) {
	attempt, err := s.Store.GetFeishuOAuthRefreshAttempt(attemptID, accountID)
	if err == nil && attempt.State == store.FeishuOAuthRefreshAttemptStatePrepared {
		s.once.Do(func() { close(s.preparedRead) })
	}
	return attempt, err
}

type stageOnAmbiguousRefreshStore struct {
	*store.Store
	stage func() error
	once  sync.Once
	err   error
}

func (s *stageOnAmbiguousRefreshStore) MarkFeishuOAuthRefreshAttemptAmbiguous(attemptID, accountID string, now time.Time) (store.FeishuUserOAuthCredential, store.FeishuOAuthRefreshAttempt, error) {
	s.once.Do(func() {
		if s.stage != nil {
			s.err = s.stage()
		}
	})
	if s.err != nil {
		return store.FeishuUserOAuthCredential{}, store.FeishuOAuthRefreshAttempt{}, s.err
	}
	return s.Store.MarkFeishuOAuthRefreshAttemptAmbiguous(attemptID, accountID, now)
}

type authorizeOnRefreshInvalidationStore struct {
	*store.Store
	authorize func() error
	once      sync.Once
	err       error
}

type blockFirstOAuthCredentialSaveStore struct {
	*store.Store
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *blockFirstOAuthCredentialSaveStore) SaveFeishuUserOAuthCredential(credential store.FeishuUserOAuthCredential) (store.FeishuUserOAuthCredential, error) {
	s.once.Do(func() {
		close(s.entered)
		<-s.release
	})
	return s.Store.SaveFeishuUserOAuthCredential(credential)
}

func (s *authorizeOnRefreshInvalidationStore) InvalidateFeishuOAuthRefreshAttempt(attemptID, accountID, errorCategory string, now time.Time) (store.FeishuUserOAuthCredential, store.FeishuOAuthRefreshAttempt, error) {
	s.once.Do(func() {
		if s.authorize != nil {
			s.err = s.authorize()
		}
	})
	if s.err != nil {
		return store.FeishuUserOAuthCredential{}, store.FeishuOAuthRefreshAttempt{}, s.err
	}
	return s.Store.InvalidateFeishuOAuthRefreshAttempt(attemptID, accountID, errorCategory, now)
}

func TestFeishuOAuthCredentialCipherAuthenticatesIdentityAndAccount(t *testing.T) {
	ciphertextCipher, err := newFeishuOAuthCredentialCipher("app-secret", "feishu:cli_test")
	if err != nil {
		t.Fatalf("newFeishuOAuthCredentialCipher returned error: %v", err)
	}
	identity := feishuOAuthIdentity{OpenID: "ou_requester", UserID: "u_requester"}
	ciphertext, err := ciphertextCipher.Encrypt(identity, "access_token", "user-access-token")
	if err != nil {
		t.Fatalf("Encrypt returned error: %v", err)
	}
	if ciphertext == "" || strings.Contains(ciphertext, "user-access-token") {
		t.Fatalf("ciphertext leaked plaintext: %q", ciphertext)
	}
	plaintext, err := ciphertextCipher.Decrypt(identity, "access_token", ciphertext)
	if err != nil || plaintext != "user-access-token" {
		t.Fatalf("Decrypt = %q err=%v", plaintext, err)
	}
	if _, err := ciphertextCipher.Decrypt(feishuOAuthIdentity{OpenID: "ou_other"}, "access_token", ciphertext); err == nil {
		t.Fatal("Decrypt accepted another user identity")
	}
	otherAccountCipher, err := newFeishuOAuthCredentialCipher("app-secret", "feishu:cli_other")
	if err != nil {
		t.Fatalf("new other account cipher: %v", err)
	}
	if _, err := otherAccountCipher.Decrypt(identity, "access_token", ciphertext); err == nil {
		t.Fatal("Decrypt accepted another Bot account")
	}
	tamperedBytes := []byte(ciphertext)
	tamperIndex := len(tamperedBytes) / 2
	if tamperedBytes[tamperIndex] == 'A' {
		tamperedBytes[tamperIndex] = 'B'
	} else {
		tamperedBytes[tamperIndex] = 'A'
	}
	tampered := string(tamperedBytes)
	if _, err := ciphertextCipher.Decrypt(identity, "access_token", tampered); err == nil {
		t.Fatal("Decrypt accepted tampered ciphertext")
	}
}

func TestFeishuOAuthCredentialCipherDecryptsLegacyCiphertextVector(t *testing.T) {
	const legacyCiphertext = "v1.AAECAwQFBgcICQoL9YXWUekCP_2QNRPOm3q4fynKjkKt8MryMXvnsNjkVkqQUXiTLerXaw"
	identity := feishuOAuthIdentity{OpenID: "ou_requester", UserID: "u_requester"}
	cipherValue, err := newFeishuOAuthCredentialCipher("app-secret", "feishu:cli_test")
	if err != nil {
		t.Fatalf("newFeishuOAuthCredentialCipher returned error: %v", err)
	}
	plaintext, err := cipherValue.Decrypt(identity, "access_token", legacyCiphertext)
	if err != nil || plaintext != "legacy-user-access-token" {
		t.Fatalf("Decrypt legacy ciphertext = %q err=%v", plaintext, err)
	}
	if _, err := cipherValue.Decrypt(identity, "refresh_token", legacyCiphertext); err == nil {
		t.Fatal("legacy ciphertext decrypted under a different token field")
	}
	if _, err := cipherValue.Decrypt(feishuOAuthIdentity{OpenID: "ou_other"}, "access_token", legacyCiphertext); err == nil {
		t.Fatal("legacy ciphertext decrypted under a different user identity")
	}
	otherAccountCipher, err := newFeishuOAuthCredentialCipher("app-secret", "feishu:cli_other")
	if err != nil {
		t.Fatalf("new other account cipher: %v", err)
	}
	if _, err := otherAccountCipher.Decrypt(identity, "access_token", legacyCiphertext); err == nil {
		t.Fatal("legacy ciphertext decrypted under a different account")
	}
	otherSecretCipher, err := newFeishuOAuthCredentialCipher("other-app-secret", "feishu:cli_test")
	if err != nil {
		t.Fatalf("new other secret cipher: %v", err)
	}
	if _, err := otherSecretCipher.Decrypt(identity, "access_token", legacyCiphertext); err == nil {
		t.Fatal("legacy ciphertext decrypted with a different application secret")
	}
}

func TestFeishuOAuthCredentialCipherDecryptsLegacyRefreshAttemptVector(t *testing.T) {
	const legacyCiphertext = "v1.DA0ODxAREhMUFRYXR_Oq2nu6twJLmdflLv1m0JN8UbJcHSGWbN0RUtrvnGHMFbOyG_7cG_dtmA"
	attempt := store.FeishuOAuthRefreshAttempt{
		ID:          "refresh_legacy",
		AccountID:   "feishu:cli_test",
		ActorOpenID: "ou_requester",
		ActorUserID: "u_requester",
	}
	cipherValue, err := newFeishuOAuthCredentialCipher("app-secret", attempt.AccountID)
	if err != nil {
		t.Fatalf("newFeishuOAuthCredentialCipher returned error: %v", err)
	}
	plaintext, err := cipherValue.DecryptRefreshAttempt(attempt, "refresh_token", legacyCiphertext)
	if err != nil || plaintext != "legacy-staged-refresh-token" {
		t.Fatalf("DecryptRefreshAttempt legacy ciphertext = %q err=%v", plaintext, err)
	}
	changed := attempt
	changed.ID = "refresh_other"
	if _, err := cipherValue.DecryptRefreshAttempt(changed, "refresh_token", legacyCiphertext); err == nil {
		t.Fatal("legacy refresh ciphertext decrypted under a different attempt")
	}
	if _, err := cipherValue.DecryptRefreshAttempt(attempt, "access_token", legacyCiphertext); err == nil {
		t.Fatal("legacy refresh ciphertext decrypted under a different token field")
	}
}

func TestFeishuOAuthCredentialCipherAuthenticatesRefreshAttemptContext(t *testing.T) {
	cipherValue, err := newFeishuOAuthCredentialCipher("app-secret", "feishu:cli_test")
	if err != nil {
		t.Fatalf("newFeishuOAuthCredentialCipher returned error: %v", err)
	}
	attempt := store.FeishuOAuthRefreshAttempt{
		ID:          "refresh_one",
		AccountID:   "feishu:cli_test",
		ActorOpenID: "ou_requester",
		ActorUserID: "u_requester",
	}
	ciphertext, err := cipherValue.EncryptRefreshAttempt(attempt, "access_token", "staged-access-token")
	if err != nil {
		t.Fatalf("EncryptRefreshAttempt returned error: %v", err)
	}
	if ciphertext == "" || strings.Contains(ciphertext, "staged-access-token") {
		t.Fatalf("refresh attempt ciphertext leaked plaintext: %q", ciphertext)
	}
	plaintext, err := cipherValue.DecryptRefreshAttempt(attempt, "access_token", ciphertext)
	if err != nil || plaintext != "staged-access-token" {
		t.Fatalf("DecryptRefreshAttempt = %q err=%v", plaintext, err)
	}
	for name, changed := range map[string]store.FeishuOAuthRefreshAttempt{
		"attempt": {
			ID: "refresh_two", AccountID: attempt.AccountID,
			ActorOpenID: attempt.ActorOpenID, ActorUserID: attempt.ActorUserID,
		},
		"actor": {
			ID: attempt.ID, AccountID: attempt.AccountID,
			ActorOpenID: "ou_other", ActorUserID: "u_other",
		},
	} {
		if _, err := cipherValue.DecryptRefreshAttempt(changed, "access_token", ciphertext); err == nil {
			t.Fatalf("DecryptRefreshAttempt accepted changed %s context", name)
		}
	}
	if _, err := cipherValue.DecryptRefreshAttempt(attempt, "refresh_token", ciphertext); err == nil {
		t.Fatal("DecryptRefreshAttempt accepted another token field")
	}
	otherAccountCipher, err := newFeishuOAuthCredentialCipher("app-secret", "feishu:cli_other")
	if err != nil {
		t.Fatalf("new other account cipher: %v", err)
	}
	otherAccountAttempt := attempt
	otherAccountAttempt.AccountID = "feishu:cli_other"
	if _, err := otherAccountCipher.DecryptRefreshAttempt(otherAccountAttempt, "access_token", ciphertext); err == nil {
		t.Fatal("DecryptRefreshAttempt accepted another account")
	}
}

func TestPersistFeishuOAuthCredentialCompletesIdentityBeforeEncryptingReplacement(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	manager, st, _ := newTestResourceAccessManager(t, server, resourceAccessOAuthConfig{
		ClientID:    "cli_xxx",
		BaseURL:     server.URL,
		CallbackURL: "https://bridge.example.com/feishu/oauth/callback",
	})
	now := time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC)
	manager.now = func() time.Time { return now }
	if _, err := manager.persistFeishuOAuthCredential(context.Background(), feishuOAuthIdentity{
		OpenID: "ou_requester",
		UserID: "u_requester",
	}, feishuOAuthTokenBundle{
		AccessToken:           "old-access-token",
		AccessTokenExpiresIn:  2 * time.Hour,
		RefreshToken:          "old-refresh-token",
		RefreshTokenExpiresIn: 24 * time.Hour,
		Scopes:                resourceAccessOAuthScope,
	}); err != nil {
		t.Fatalf("persist initial OAuth credential: %v", err)
	}

	if _, err := manager.persistFeishuOAuthCredential(context.Background(), feishuOAuthIdentity{
		UserID: "u_requester",
	}, feishuOAuthTokenBundle{
		AccessToken:           "new-access-token",
		AccessTokenExpiresIn:  2 * time.Hour,
		RefreshToken:          "new-refresh-token",
		RefreshTokenExpiresIn: 24 * time.Hour,
		Scopes:                resourceAccessOAuthScope,
	}); err != nil {
		t.Fatalf("replace OAuth credential through user ID alias: %v", err)
	}

	credential, err := st.GetFeishuUserOAuthCredential(manager.account.ID, "ou_requester", "u_requester")
	if err != nil {
		t.Fatalf("load replaced OAuth credential: %v", err)
	}
	if credential.ActorOpenID != "ou_requester" || credential.ActorUserID != "u_requester" {
		t.Fatalf("replaced OAuth identity = open_id %q user_id %q", credential.ActorOpenID, credential.ActorUserID)
	}
	token, err := manager.feishuUserAccessToken(context.Background(), "ou_requester", "u_requester")
	if err != nil || token != "new-access-token" {
		t.Fatalf("feishuUserAccessToken after alias replacement = %q err=%v", token, err)
	}
}

func TestPersistFeishuOAuthCredentialReencryptsWhenConcurrentAuthorizationAddsCanonicalIdentity(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	manager, st, _ := newTestResourceAccessManager(t, server, resourceAccessOAuthConfig{
		ClientID:    "cli_xxx",
		BaseURL:     server.URL,
		CallbackURL: "https://bridge.example.com/feishu/oauth/callback",
	})
	now := time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC)
	manager.now = func() time.Time { return now }
	blockedStore := &blockFirstOAuthCredentialSaveStore{
		Store:   st,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	partialService := manager.oauthCredentialService()
	partialService.store = blockedStore
	canonicalService := manager.oauthCredentialService()
	canonicalService.store = st

	type persistResult struct {
		credential store.FeishuUserOAuthCredential
		err        error
	}
	partialResult := make(chan persistResult, 1)
	go func() {
		credential, err := partialService.persistFeishuOAuthCredential(context.Background(), feishuOAuthIdentity{
			UserID: "u_requester",
		}, feishuOAuthTokenBundle{
			AccessToken:           "partial-access-token",
			AccessTokenExpiresIn:  2 * time.Hour,
			RefreshToken:          "partial-refresh-token",
			RefreshTokenExpiresIn: 24 * time.Hour,
			Scopes:                resourceAccessOAuthScope,
		})
		partialResult <- persistResult{credential: credential, err: err}
	}()
	select {
	case <-blockedStore.entered:
	case <-time.After(time.Second):
		t.Fatal("partial authorization did not reach credential save")
	}
	if _, err := canonicalService.persistFeishuOAuthCredential(context.Background(), feishuOAuthIdentity{
		OpenID: "ou_requester",
		UserID: "u_requester",
	}, feishuOAuthTokenBundle{
		AccessToken:           "canonical-access-token",
		AccessTokenExpiresIn:  2 * time.Hour,
		RefreshToken:          "canonical-refresh-token",
		RefreshTokenExpiresIn: 24 * time.Hour,
		Scopes:                resourceAccessOAuthScope,
	}); err != nil {
		t.Fatalf("persist concurrent canonical OAuth credential: %v", err)
	}
	close(blockedStore.release)
	select {
	case result := <-partialResult:
		if result.err != nil || result.credential.ActorOpenID != "ou_requester" {
			t.Fatalf("partial authorization result = %#v err=%v", result.credential, result.err)
		}
	case <-time.After(time.Second):
		t.Fatal("partial authorization did not finish")
	}

	token, err := manager.feishuUserAccessToken(context.Background(), "ou_requester", "u_requester")
	if err != nil || token != "partial-access-token" {
		t.Fatalf("feishuUserAccessToken after concurrent identity completion = %q err=%v", token, err)
	}
}

func TestFeishuOAuthRefreshErrorCategoryDoesNotPersistArbitraryRemoteText(t *testing.T) {
	err := &accesstoken.AccessTokenError{ErrorType: "invalid grant token=secret"}
	if category := feishuOAuthRefreshErrorCategory(err); category != "oauth_error" {
		t.Fatalf("unsafe OAuth error category = %q", category)
	}
	err = &accesstoken.AccessTokenError{ErrorType: "invalid_grant"}
	if category := feishuOAuthRefreshErrorCategory(err); category != "oauth_invalid_grant" {
		t.Fatalf("valid OAuth error category = %q", category)
	}
}

func TestFeishuOAuthRefreshRequiresReauthorizationOnlyForInvalidGrant(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "invalid refresh grant",
			err: &accesstoken.AccessTokenError{
				ErrorType: "invalid_grant",
				ApiResp:   &larkcore.ApiResp{StatusCode: http.StatusBadRequest},
			},
			want: true,
		},
		{
			name: "invalid client configuration",
			err: &accesstoken.AccessTokenError{
				ErrorType: "invalid_client",
				ApiResp:   &larkcore.ApiResp{StatusCode: http.StatusUnauthorized},
			},
			want: false,
		},
		{
			name: "permission response without token rejection",
			err: &accesstoken.AccessTokenError{
				ApiResp: &larkcore.ApiResp{StatusCode: http.StatusForbidden},
			},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := feishuOAuthRefreshRequiresReauthorization(tt.err); got != tt.want {
				t.Fatalf("feishuOAuthRefreshRequiresReauthorization(%v) = %t, want %t", tt.err, got, tt.want)
			}
		})
	}
}

func TestFeishuOAuthRefreshOutcomeTreatsServerFailureAsAmbiguous(t *testing.T) {
	err := &accesstoken.AccessTokenError{
		ApiResp: &larkcore.ApiResp{StatusCode: http.StatusBadGateway},
	}
	if !feishuOAuthRefreshOutcomeAmbiguous(err) {
		t.Fatal("OAuth 502 response was treated as a deterministic refresh rejection")
	}
	err = &accesstoken.AccessTokenError{
		ErrorType: "invalid_grant",
		ApiResp:   &larkcore.ApiResp{StatusCode: http.StatusBadRequest},
	}
	if feishuOAuthRefreshOutcomeAmbiguous(err) {
		t.Fatal("OAuth invalid_grant response was treated as an ambiguous refresh outcome")
	}
}

func TestFeishuOAuthRefreshUsesNewAuthorizationThatWinsDuringRemoteResolution(t *testing.T) {
	tests := []struct {
		name      string
		respond   func(*testing.T, http.ResponseWriter)
		wantState string
	}{
		{
			name: "lost response",
			respond: func(t *testing.T, w http.ResponseWriter) {
				t.Helper()
				closeHTTPResponseWithoutReply(t, w)
			},
			wantState: store.FeishuOAuthRefreshAttemptStateCompleted,
		},
		{
			name: "invalid grant response",
			respond: func(t *testing.T, w http.ResponseWriter) {
				t.Helper()
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				if err := json.NewEncoder(w).Encode(map[string]any{
					"error":             "invalid_grant",
					"error_description": "refresh token rejected",
				}); err != nil {
					t.Fatalf("encode refresh rejection: %v", err)
				}
			},
			wantState: store.FeishuOAuthRefreshAttemptStateCompleted,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			refreshStarted := make(chan struct{})
			releaseRefresh := make(chan struct{})
			var startedOnce sync.Once
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/oauth/v3/token" {
					t.Fatalf("unexpected path: %s", r.URL.Path)
				}
				startedOnce.Do(func() { close(refreshStarted) })
				<-releaseRefresh
				tt.respond(t, w)
			}))
			defer server.Close()

			manager, st, _ := newTestResourceAccessManager(t, server, resourceAccessOAuthConfig{
				ClientID:    "cli_xxx",
				BaseURL:     server.URL,
				CallbackURL: "https://bridge.example.com/feishu/oauth/callback",
			})
			now := time.Date(2026, time.August, 4, 12, 20, 0, 0, time.UTC)
			manager.now = func() time.Time { return now }
			oldCredential, err := manager.persistFeishuOAuthCredential(context.Background(), feishuOAuthIdentity{
				OpenID: "ou_requester", UserID: "u_requester",
			}, feishuOAuthTokenBundle{
				AccessToken: "old-access-token", AccessTokenExpiresIn: time.Minute,
				RefreshToken: "old-refresh-token", RefreshTokenExpiresIn: 24 * time.Hour,
				Scopes: resourceAccessOAuthScope,
			})
			if err != nil {
				t.Fatalf("persist old OAuth credential: %v", err)
			}

			type tokenResult struct {
				token string
				err   error
			}
			result := make(chan tokenResult, 1)
			go func() {
				token, tokenErr := manager.feishuUserAccessToken(context.Background(), "ou_requester", "u_requester")
				result <- tokenResult{token: token, err: tokenErr}
			}()
			select {
			case <-refreshStarted:
			case <-time.After(time.Second):
				t.Fatal("refresh request did not start")
			}
			activeAttempt, err := st.ActiveFeishuOAuthRefreshAttempt(oldCredential.ID, oldCredential.AccountID)
			if err != nil || activeAttempt.State != store.FeishuOAuthRefreshAttemptStatePrepared {
				t.Fatalf("active refresh attempt before winning authorization = %#v err=%v", activeAttempt, err)
			}
			newCredential, err := manager.persistFeishuOAuthCredential(context.Background(), feishuOAuthIdentity{
				OpenID: "ou_requester", UserID: "u_requester",
			}, feishuOAuthTokenBundle{
				AccessToken: "new-authorized-access-token", AccessTokenExpiresIn: 2 * time.Hour,
				RefreshToken: "new-authorized-refresh-token", RefreshTokenExpiresIn: 30 * 24 * time.Hour,
				Scopes: resourceAccessOAuthScope,
			})
			if err != nil {
				t.Fatalf("persist winning OAuth credential: %v", err)
			}
			close(releaseRefresh)

			select {
			case got := <-result:
				if got.err != nil || got.token != "new-authorized-access-token" {
					t.Fatalf("feishuUserAccessToken after concurrent authorization = %q err=%v", got.token, got.err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("refresh caller did not finish")
			}
			if newCredential.Version != oldCredential.Version+1 || newCredential.Status != store.FeishuUserOAuthCredentialStatusActive {
				t.Fatalf("winning OAuth credential = %#v", newCredential)
			}
			attempt, err := st.ActiveFeishuOAuthRefreshAttempt(oldCredential.ID, oldCredential.AccountID)
			if !errors.Is(err, store.ErrFeishuOAuthRefreshAttemptNotFound) {
				t.Fatalf("active refresh attempt after winning authorization = %#v err=%v", attempt, err)
			}
			terminal, err := st.GetFeishuOAuthRefreshAttempt(activeAttempt.ID, activeAttempt.AccountID)
			if err != nil || terminal.State != tt.wantState {
				t.Fatalf("terminal refresh attempt = %#v err=%v, want state %s", terminal, err, tt.wantState)
			}
		})
	}
}

func TestFeishuOAuthRefreshWaiterUsesNewCredentialWhenStaleLeaseExpires(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	manager, st, _ := newTestResourceAccessManager(t, server, resourceAccessOAuthConfig{
		ClientID:    "cli_xxx",
		BaseURL:     server.URL,
		CallbackURL: "https://bridge.example.com/feishu/oauth/callback",
	})
	now := time.Date(2026, time.August, 4, 12, 25, 0, 0, time.UTC)
	manager.now = func() time.Time { return now }
	oldCredential, err := manager.persistFeishuOAuthCredential(context.Background(), feishuOAuthIdentity{
		OpenID: "ou_requester", UserID: "u_requester",
	}, feishuOAuthTokenBundle{
		AccessToken: "old-access-token", AccessTokenExpiresIn: time.Minute,
		RefreshToken: "old-refresh-token", RefreshTokenExpiresIn: 24 * time.Hour,
		Scopes: resourceAccessOAuthScope,
	})
	if err != nil {
		t.Fatalf("persist old OAuth credential: %v", err)
	}
	attempt, owner, err := st.PrepareFeishuOAuthRefreshAttempt(
		oldCredential.ID, oldCredential.AccountID, oldCredential.Version, "lost-owner", now, 10*time.Second,
	)
	if err != nil || !owner {
		t.Fatalf("prepare stale refresh attempt = %#v owner=%t err=%v", attempt, owner, err)
	}
	newCredential, err := manager.persistFeishuOAuthCredential(context.Background(), feishuOAuthIdentity{
		OpenID: "ou_requester", UserID: "u_requester",
	}, feishuOAuthTokenBundle{
		AccessToken: "new-authorized-access-token", AccessTokenExpiresIn: 2 * time.Hour,
		RefreshToken: "new-authorized-refresh-token", RefreshTokenExpiresIn: 30 * 24 * time.Hour,
		Scopes: resourceAccessOAuthScope,
	})
	if err != nil {
		t.Fatalf("persist new OAuth credential: %v", err)
	}
	manager.now = func() time.Time { return now.Add(20 * time.Second) }

	token, err := manager.waitForFeishuOAuthRefresh(context.Background(), newCredential, attempt)
	if err != nil || token != "new-authorized-access-token" {
		t.Fatalf("waitForFeishuOAuthRefresh with superseded lease = %q err=%v", token, err)
	}
	resolved, err := st.GetFeishuOAuthRefreshAttempt(attempt.ID, attempt.AccountID)
	if err != nil || resolved.State != store.FeishuOAuthRefreshAttemptStateCompleted {
		t.Fatalf("superseded stale refresh attempt = %#v err=%v", resolved, err)
	}
}

func TestFeishuUserAccessTokenFailsClosedWhenRefreshResponseIsLost(t *testing.T) {
	var refreshCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/v3/token" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		refreshCalls.Add(1)
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("test server does not support connection hijacking")
		}
		conn, _, err := hijacker.Hijack()
		if err != nil {
			t.Fatalf("hijack refresh response: %v", err)
		}
		_ = conn.Close()
	}))
	defer server.Close()

	manager, st, _ := newTestResourceAccessManager(t, server, resourceAccessOAuthConfig{
		ClientID:    "cli_xxx",
		BaseURL:     server.URL,
		CallbackURL: "https://bridge.example.com/feishu/oauth/callback",
	})
	now := time.Date(2026, time.August, 4, 12, 30, 0, 0, time.UTC)
	manager.now = func() time.Time { return now }
	credential, err := manager.persistFeishuOAuthCredential(context.Background(), feishuOAuthIdentity{
		OpenID: "ou_requester",
		UserID: "u_requester",
	}, feishuOAuthTokenBundle{
		AccessToken:           "still-valid-access-token",
		AccessTokenExpiresIn:  time.Minute,
		RefreshToken:          "one-time-refresh-token",
		RefreshTokenExpiresIn: 24 * time.Hour,
		Scopes:                resourceAccessOAuthScope,
	})
	if err != nil {
		t.Fatalf("persistFeishuOAuthCredential returned error: %v", err)
	}

	if _, err := manager.feishuUserAccessToken(context.Background(), "ou_requester", "u_requester"); !errors.Is(err, ErrFeishuUserOAuthReauthorizationNeeded) {
		t.Fatalf("feishuUserAccessToken error = %v, want reauthorization after ambiguous refresh outcome", err)
	}
	marked, err := st.GetFeishuUserOAuthCredentialByID(credential.ID, credential.AccountID)
	if err != nil || marked.Status != store.FeishuUserOAuthCredentialStatusReauthRequired ||
		marked.AccessTokenCiphertext != "" || marked.RefreshTokenCiphertext != "" {
		t.Fatalf("credential after lost refresh response = %#v err=%v", marked, err)
	}
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("refresh calls = %d, want exactly one ambiguous remote request", got)
	}
}

func TestFeishuUserAccessTokenUsesFallbackWhenRefreshCannotConnect(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	manager, st, _ := newTestResourceAccessManager(t, server, resourceAccessOAuthConfig{
		ClientID:    "cli_xxx",
		BaseURL:     server.URL,
		CallbackURL: "https://bridge.example.com/feishu/oauth/callback",
	})
	now := time.Date(2026, time.August, 4, 12, 45, 0, 0, time.UTC)
	manager.now = func() time.Time { return now }
	credential, err := manager.persistFeishuOAuthCredential(context.Background(), feishuOAuthIdentity{
		OpenID: "ou_requester",
		UserID: "u_requester",
	}, feishuOAuthTokenBundle{
		AccessToken:           "still-valid-access-token",
		AccessTokenExpiresIn:  time.Minute,
		RefreshToken:          "unconsumed-refresh-token",
		RefreshTokenExpiresIn: 24 * time.Hour,
		Scopes:                resourceAccessOAuthScope,
	})
	if err != nil {
		server.Close()
		t.Fatalf("persistFeishuOAuthCredential returned error: %v", err)
	}
	server.Close()

	token, err := manager.feishuUserAccessToken(context.Background(), "ou_requester", "u_requester")
	if err != nil || token != "still-valid-access-token" {
		t.Fatalf("feishuUserAccessToken fallback = %q err=%v", token, err)
	}
	unchanged, err := st.GetFeishuUserOAuthCredentialByID(credential.ID, credential.AccountID)
	if err != nil || unchanged.Status != store.FeishuUserOAuthCredentialStatusActive || unchanged.Version != credential.Version {
		t.Fatalf("credential after pre-connect refresh failure = %#v err=%v", unchanged, err)
	}
}

func TestFeishuOAuthRefreshPeerTimeoutDoesNotUseCredentialMarkedAmbiguous(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	manager, st, _ := newTestResourceAccessManager(t, server, resourceAccessOAuthConfig{
		ClientID:    "cli_xxx",
		BaseURL:     server.URL,
		CallbackURL: "https://bridge.example.com/feishu/oauth/callback",
	})
	now := time.Date(2026, time.August, 4, 12, 55, 0, 0, time.UTC)
	manager.now = func() time.Time { return now }
	credential, err := manager.persistFeishuOAuthCredential(context.Background(), feishuOAuthIdentity{
		OpenID: "ou_requester", UserID: "u_requester",
	}, feishuOAuthTokenBundle{
		AccessToken: "still-valid-access-token", AccessTokenExpiresIn: time.Minute,
		RefreshToken: "one-time-refresh-token", RefreshTokenExpiresIn: 24 * time.Hour,
		Scopes: resourceAccessOAuthScope,
	})
	if err != nil {
		t.Fatalf("persistFeishuOAuthCredential returned error: %v", err)
	}
	attempt, owner, err := st.PrepareFeishuOAuthRefreshAttempt(
		credential.ID, credential.AccountID, credential.Version, "lease-owner", now, time.Minute,
	)
	if err != nil || !owner {
		t.Fatalf("PrepareFeishuOAuthRefreshAttempt = %#v owner=%t err=%v", attempt, owner, err)
	}
	observed := &observedRefreshAttemptStore{Store: st, preparedRead: make(chan struct{})}
	manager.store = observed
	manager.refreshPeerWait = 100 * time.Millisecond
	manager.refreshPoll = time.Hour

	tokenResult := make(chan string, 1)
	errorResult := make(chan error, 1)
	go func() {
		token, waitErr := manager.waitForFeishuOAuthRefresh(context.Background(), credential, attempt)
		tokenResult <- token
		errorResult <- waitErr
	}()
	select {
	case <-observed.preparedRead:
	case <-time.After(time.Second):
		t.Fatal("refresh waiter did not read the prepared attempt")
	}
	if _, _, err := st.MarkOwnedFeishuOAuthRefreshAttemptAmbiguous(
		attempt.ID, attempt.AccountID, attempt.LeaseToken, now.Add(time.Second),
	); err != nil {
		t.Fatalf("MarkOwnedFeishuOAuthRefreshAttemptAmbiguous returned error: %v", err)
	}

	select {
	case token := <-tokenResult:
		waitErr := <-errorResult
		if token != "" || !errors.Is(waitErr, ErrFeishuUserOAuthReauthorizationNeeded) {
			t.Fatalf("waitForFeishuOAuthRefresh = %q err=%v, want fail-closed reauthorization", token, waitErr)
		}
	case <-time.After(time.Second):
		t.Fatal("refresh waiter did not finish")
	}
}

func TestFeishuOAuthRefreshPeerTimeoutReloadsAttemptAfterAmbiguousClaimConflict(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	manager, st, _ := newTestResourceAccessManager(t, server, resourceAccessOAuthConfig{
		ClientID:    "cli_xxx",
		BaseURL:     server.URL,
		CallbackURL: "https://bridge.example.com/feishu/oauth/callback",
	})
	now := time.Date(2026, time.August, 4, 12, 58, 0, 0, time.UTC)
	manager.now = func() time.Time { return now }
	credential, err := manager.persistFeishuOAuthCredential(context.Background(), feishuOAuthIdentity{
		OpenID: "ou_requester", UserID: "u_requester",
	}, feishuOAuthTokenBundle{
		AccessToken: "expired-before-peer-timeout", AccessTokenExpiresIn: time.Minute,
		RefreshToken: "one-time-refresh-token", RefreshTokenExpiresIn: 24 * time.Hour,
		Scopes: resourceAccessOAuthScope,
	})
	if err != nil {
		t.Fatalf("persistFeishuOAuthCredential returned error: %v", err)
	}
	attempt, owner, err := st.PrepareFeishuOAuthRefreshAttempt(
		credential.ID, credential.AccountID, credential.Version, "lease-owner", now, time.Minute,
	)
	if err != nil || !owner {
		t.Fatalf("PrepareFeishuOAuthRefreshAttempt = %#v owner=%t err=%v", attempt, owner, err)
	}
	stagedAccess, err := manager.credentialCipher.EncryptRefreshAttempt(attempt, "access_token", "staged-access-token")
	if err != nil {
		t.Fatalf("encrypt staged access token: %v", err)
	}
	stagedRefresh, err := manager.credentialCipher.EncryptRefreshAttempt(attempt, "refresh_token", "staged-refresh-token")
	if err != nil {
		t.Fatalf("encrypt staged refresh token: %v", err)
	}
	manager.now = func() time.Time { return now.Add(2 * time.Minute) }
	manager.store = &stageOnAmbiguousRefreshStore{
		Store: st,
		stage: func() error {
			_, stageErr := st.StageFeishuOAuthRefreshResponse(attempt.ID, attempt.AccountID, attempt.LeaseToken, store.FeishuOAuthRefreshStage{
				AccessTokenCiphertext:  stagedAccess,
				AccessTokenExpiresAt:   now.Add(2 * time.Hour),
				RefreshTokenCiphertext: stagedRefresh,
				RefreshTokenExpiresAt:  now.Add(30 * 24 * time.Hour),
				Scopes:                 resourceAccessOAuthScope,
			}, now.Add(2*time.Minute))
			return stageErr
		},
	}

	token, err := manager.resolveFeishuOAuthRefreshPeerTimeout(context.Background(), credential, attempt)
	if err != nil || token != "staged-access-token" {
		t.Fatalf("resolveFeishuOAuthRefreshPeerTimeout = %q err=%v, want safely staged token", token, err)
	}
	resolved, err := st.GetFeishuOAuthRefreshAttempt(attempt.ID, attempt.AccountID)
	if err != nil || resolved.State != store.FeishuOAuthRefreshAttemptStateCompleted {
		t.Fatalf("resolved refresh attempt = %#v err=%v", resolved, err)
	}
}

func TestFeishuOAuthStagedInvalidationUsesConcurrentNewAuthorization(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	manager, st, _ := newTestResourceAccessManager(t, server, resourceAccessOAuthConfig{
		ClientID:    "cli_xxx",
		BaseURL:     server.URL,
		CallbackURL: "https://bridge.example.com/feishu/oauth/callback",
	})
	now := time.Date(2026, time.August, 4, 12, 59, 0, 0, time.UTC)
	manager.now = func() time.Time { return now }
	oldCredential, err := manager.persistFeishuOAuthCredential(context.Background(), feishuOAuthIdentity{
		OpenID: "ou_requester", UserID: "u_requester",
	}, feishuOAuthTokenBundle{
		AccessToken: "old-access-token", AccessTokenExpiresIn: time.Minute,
		RefreshToken: "old-refresh-token", RefreshTokenExpiresIn: 24 * time.Hour,
		Scopes: resourceAccessOAuthScope,
	})
	if err != nil {
		t.Fatalf("persist old OAuth credential: %v", err)
	}
	attempt, owner, err := st.PrepareFeishuOAuthRefreshAttempt(
		oldCredential.ID, oldCredential.AccountID, oldCredential.Version, "lease-owner", now, time.Minute,
	)
	if err != nil || !owner {
		t.Fatalf("PrepareFeishuOAuthRefreshAttempt = %#v owner=%t err=%v", attempt, owner, err)
	}
	staged, err := st.StageFeishuOAuthRefreshResponse(attempt.ID, attempt.AccountID, attempt.LeaseToken, store.FeishuOAuthRefreshStage{
		AccessTokenCiphertext:  "corrupt-staged-access-token",
		AccessTokenExpiresAt:   now.Add(2 * time.Hour),
		RefreshTokenCiphertext: "corrupt-staged-refresh-token",
		RefreshTokenExpiresAt:  now.Add(30 * 24 * time.Hour),
		Scopes:                 resourceAccessOAuthScope,
	}, now)
	if err != nil {
		t.Fatalf("StageFeishuOAuthRefreshResponse returned error: %v", err)
	}
	manager.store = &authorizeOnRefreshInvalidationStore{
		Store: st,
		authorize: func() error {
			_, authorizeErr := manager.persistFeishuOAuthCredential(context.Background(), feishuOAuthIdentity{
				OpenID: "ou_requester", UserID: "u_requester",
			}, feishuOAuthTokenBundle{
				AccessToken: "new-authorized-access-token", AccessTokenExpiresIn: 2 * time.Hour,
				RefreshToken: "new-authorized-refresh-token", RefreshTokenExpiresIn: 30 * 24 * time.Hour,
				Scopes: resourceAccessOAuthScope,
			})
			return authorizeErr
		},
	}

	token, err := manager.accessTokenFromStagedFeishuOAuthRefresh(context.Background(), staged)
	if err != nil || token != "new-authorized-access-token" {
		t.Fatalf("accessTokenFromStagedFeishuOAuthRefresh = %q err=%v, want concurrent authorization", token, err)
	}
	latest, err := st.GetFeishuUserOAuthCredentialByID(oldCredential.ID, oldCredential.AccountID)
	if err != nil || latest.Status != store.FeishuUserOAuthCredentialStatusActive || latest.Version != oldCredential.Version+1 {
		t.Fatalf("winning OAuth credential = %#v err=%v", latest, err)
	}
	resolved, err := st.GetFeishuOAuthRefreshAttempt(attempt.ID, attempt.AccountID)
	if err != nil || resolved.State != store.FeishuOAuthRefreshAttemptStateCompleted {
		t.Fatalf("superseded invalid staged attempt = %#v err=%v", resolved, err)
	}
}

func TestFeishuUserAccessTokenRefreshesOnceAcrossConcurrentCallers(t *testing.T) {
	logs := captureMonitorLogs(t)
	logging.SetLevel(logging.Debug)
	var mu sync.Mutex
	refreshCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/v3/token" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode refresh request: %v", err)
		}
		if body["grant_type"] != "refresh_token" || body["refresh_token"] != "refresh-token-old" || body["client_secret"] != "secret" {
			t.Fatalf("refresh request body = %#v", body)
		}
		mu.Lock()
		refreshCalls++
		mu.Unlock()
		time.Sleep(100 * time.Millisecond)
		writeResourceAccessJSON(t, w, map[string]any{
			"access_token":             "access-token-new",
			"expires_in":               7200,
			"refresh_token":            "refresh-token-new",
			"refresh_token_expires_in": 2592000,
			"scope":                    resourceAccessOAuthScope,
		})
	}))
	defer server.Close()
	manager, st, _ := newTestResourceAccessManager(t, server, resourceAccessOAuthConfig{
		ClientID:    "cli_xxx",
		BaseURL:     server.URL,
		CallbackURL: "https://bridge.example.com/feishu/oauth/callback",
	})
	now := time.Date(2026, time.August, 3, 15, 0, 0, 0, time.UTC)
	manager.now = func() time.Time { return now }
	peerStore, err := store.Open(store.PlatformFeishu)
	if err != nil {
		t.Fatalf("open peer Feishu store: %v", err)
	}
	t.Cleanup(func() {
		if err := peerStore.Close(); err != nil {
			t.Fatalf("close peer Feishu store: %v", err)
		}
	})
	peerManager := &resourceAccessManager{
		store:            peerStore,
		client:           manager.client,
		cards:            manager.cards,
		account:          manager.account,
		botOpenID:        manager.botOpenID,
		oauth:            manager.oauth,
		runCtx:           manager.runCtx,
		ttl:              manager.ttl,
		now:              manager.now,
		credentialCipher: manager.credentialCipher,
	}
	if _, err := manager.persistFeishuOAuthCredential(context.Background(), feishuOAuthIdentity{
		OpenID: "ou_requester",
		UserID: "u_requester",
	}, feishuOAuthTokenBundle{
		AccessToken:           "access-token-old",
		AccessTokenExpiresIn:  time.Minute,
		RefreshToken:          "refresh-token-old",
		RefreshTokenExpiresIn: 30 * 24 * time.Hour,
		Scopes:                resourceAccessOAuthScope,
	}); err != nil {
		t.Fatalf("persistFeishuOAuthCredential returned error: %v", err)
	}

	start := make(chan struct{})
	results := make(chan string, 2)
	errs := make(chan error, 2)
	for _, tokenManager := range []*resourceAccessManager{manager, peerManager} {
		go func(current *resourceAccessManager) {
			<-start
			token, err := current.feishuUserAccessToken(context.Background(), "ou_requester", "u_requester")
			results <- token
			errs <- err
		}(tokenManager)
	}
	close(start)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("feishuUserAccessToken returned error: %v", err)
		}
		if token := <-results; token != "access-token-new" {
			t.Fatalf("feishuUserAccessToken = %q, want rotated token", token)
		}
	}
	mu.Lock()
	calls := refreshCalls
	mu.Unlock()
	if calls != 1 {
		t.Fatalf("refresh calls = %d, want 1", calls)
	}
	credential, err := st.GetFeishuUserOAuthCredential("feishu:cli_test", "ou_requester", "u_requester")
	if err != nil || credential.Version != 2 || credential.LastRefreshedAt.IsZero() ||
		strings.Contains(credential.AccessTokenCiphertext, "access-token-new") || strings.Contains(credential.RefreshTokenCiphertext, "refresh-token-new") {
		t.Fatalf("rotated encrypted credential = %#v err=%v", credential, err)
	}
	if _, err := st.ActiveFeishuOAuthRefreshAttempt(credential.ID, credential.AccountID); !errors.Is(err, store.ErrFeishuOAuthRefreshAttemptNotFound) {
		t.Fatalf("active refresh attempt error = %v, want not found", err)
	}
	logText := logs.String()
	for _, secret := range []string{"access-token-old", "access-token-new", "refresh-token-old", "refresh-token-new"} {
		if strings.Contains(logText, secret) {
			t.Fatalf("OAuth refresh logs leaked %q:\n%s", secret, logText)
		}
	}
}

func TestFeishuOAuthRefreshRecoveryAppliesStagedResponseWithoutRemoteReplay(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("recovery replayed remote OAuth request: %s", r.URL.Path)
	}))
	defer server.Close()
	manager, st, _ := newTestResourceAccessManager(t, server, resourceAccessOAuthConfig{
		ClientID:    "cli_xxx",
		BaseURL:     server.URL,
		CallbackURL: "https://bridge.example.com/feishu/oauth/callback",
	})
	now := time.Date(2026, time.August, 4, 13, 0, 0, 0, time.UTC)
	manager.now = func() time.Time { return now }
	credential, err := manager.persistFeishuOAuthCredential(context.Background(), feishuOAuthIdentity{
		OpenID: "ou_requester",
		UserID: "u_requester",
	}, feishuOAuthTokenBundle{
		AccessToken:           "access-token-old",
		AccessTokenExpiresIn:  time.Minute,
		RefreshToken:          "refresh-token-old",
		RefreshTokenExpiresIn: 30 * 24 * time.Hour,
		Scopes:                resourceAccessOAuthScope,
	})
	if err != nil {
		t.Fatalf("persistFeishuOAuthCredential returned error: %v", err)
	}
	attempt, _, err := st.PrepareFeishuOAuthRefreshAttempt(
		credential.ID, credential.AccountID, credential.Version, "lease-owner", now, time.Minute,
	)
	if err != nil {
		t.Fatalf("PrepareFeishuOAuthRefreshAttempt returned error: %v", err)
	}
	stagedAccess, err := manager.credentialCipher.EncryptRefreshAttempt(attempt, "access_token", "access-token-new")
	if err != nil {
		t.Fatalf("encrypt staged access token: %v", err)
	}
	stagedRefresh, err := manager.credentialCipher.EncryptRefreshAttempt(attempt, "refresh_token", "refresh-token-new")
	if err != nil {
		t.Fatalf("encrypt staged refresh token: %v", err)
	}
	staged, err := st.StageFeishuOAuthRefreshResponse(attempt.ID, attempt.AccountID, attempt.LeaseToken, store.FeishuOAuthRefreshStage{
		AccessTokenCiphertext:  stagedAccess,
		AccessTokenExpiresAt:   now.Add(2 * time.Hour),
		RefreshTokenCiphertext: stagedRefresh,
		RefreshTokenExpiresAt:  now.Add(30 * 24 * time.Hour),
		Scopes:                 resourceAccessOAuthScope,
	}, now.Add(time.Second))
	if err != nil {
		t.Fatalf("StageFeishuOAuthRefreshResponse returned error: %v", err)
	}
	for _, secret := range []string{"access-token-new", "refresh-token-new"} {
		if strings.Contains(staged.AccessTokenCiphertext, secret) || strings.Contains(staged.RefreshTokenCiphertext, secret) {
			t.Fatalf("staged refresh attempt leaked %q: %#v", secret, staged)
		}
	}

	restartedStore, err := store.Open(store.PlatformFeishu)
	if err != nil {
		t.Fatalf("open restarted Feishu store: %v", err)
	}
	t.Cleanup(func() {
		if err := restartedStore.Close(); err != nil {
			t.Fatalf("close restarted Feishu store: %v", err)
		}
	})
	restartedCipher, err := newFeishuOAuthCredentialCipher("secret", manager.account.ID)
	if err != nil {
		t.Fatalf("initialize restarted credential cipher: %v", err)
	}
	restartedManager := &resourceAccessManager{
		store:            restartedStore,
		client:           manager.client,
		cards:            manager.cards,
		account:          manager.account,
		botOpenID:        manager.botOpenID,
		oauth:            manager.oauth,
		runCtx:           manager.runCtx,
		ttl:              manager.ttl,
		now:              manager.now,
		credentialCipher: restartedCipher,
	}
	if err := restartedManager.recoverFeishuOAuthRefreshAttempts(context.Background()); err != nil {
		t.Fatalf("recoverFeishuOAuthRefreshAttempts returned error: %v", err)
	}
	rotated, err := st.GetFeishuUserOAuthCredentialByID(credential.ID, credential.AccountID)
	if err != nil || rotated.Version != credential.Version+1 ||
		strings.Contains(rotated.AccessTokenCiphertext, "access-token-new") || strings.Contains(rotated.RefreshTokenCiphertext, "refresh-token-new") {
		t.Fatalf("recovered credential = %#v err=%v", rotated, err)
	}
	completed, err := st.GetFeishuOAuthRefreshAttempt(attempt.ID, attempt.AccountID)
	if err != nil || completed.State != store.FeishuOAuthRefreshAttemptStateCompleted ||
		completed.AccessTokenCiphertext != "" || completed.RefreshTokenCiphertext != "" || completed.Scopes != "" {
		t.Fatalf("completed refresh attempt = %#v err=%v", completed, err)
	}
	token, err := restartedManager.feishuUserAccessToken(context.Background(), "ou_requester", "u_requester")
	if err != nil || token != "access-token-new" {
		t.Fatalf("feishuUserAccessToken after recovery = %q err=%v", token, err)
	}
}

func TestFeishuOAuthRefreshRecoveryMarksExpiredPreparedAttemptAmbiguous(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("ambiguous recovery called remote OAuth endpoint: %s", r.URL.Path)
	}))
	defer server.Close()
	manager, st, _ := newTestResourceAccessManager(t, server, resourceAccessOAuthConfig{
		ClientID:    "cli_xxx",
		BaseURL:     server.URL,
		CallbackURL: "https://bridge.example.com/feishu/oauth/callback",
	})
	now := time.Date(2026, time.August, 4, 14, 0, 0, 0, time.UTC)
	manager.now = func() time.Time { return now }
	credential, err := manager.persistFeishuOAuthCredential(context.Background(), feishuOAuthIdentity{
		OpenID: "ou_requester",
		UserID: "u_requester",
	}, feishuOAuthTokenBundle{
		AccessToken:           "access-token-old",
		AccessTokenExpiresIn:  time.Minute,
		RefreshToken:          "refresh-token-old",
		RefreshTokenExpiresIn: 30 * 24 * time.Hour,
		Scopes:                resourceAccessOAuthScope,
	})
	if err != nil {
		t.Fatalf("persistFeishuOAuthCredential returned error: %v", err)
	}
	attempt, _, err := st.PrepareFeishuOAuthRefreshAttempt(
		credential.ID, credential.AccountID, credential.Version, "lost-owner", now, time.Minute,
	)
	if err != nil {
		t.Fatalf("PrepareFeishuOAuthRefreshAttempt returned error: %v", err)
	}
	manager.now = func() time.Time { return now.Add(2 * time.Minute) }
	if err := manager.recoverFeishuOAuthRefreshAttempts(context.Background()); err != nil {
		t.Fatalf("recoverFeishuOAuthRefreshAttempts returned error: %v", err)
	}
	marked, err := st.GetFeishuUserOAuthCredentialByID(credential.ID, credential.AccountID)
	if err != nil || marked.Status != store.FeishuUserOAuthCredentialStatusReauthRequired ||
		marked.AccessTokenCiphertext != "" || marked.RefreshTokenCiphertext != "" {
		t.Fatalf("ambiguous credential = %#v err=%v", marked, err)
	}
	ambiguous, err := st.GetFeishuOAuthRefreshAttempt(attempt.ID, attempt.AccountID)
	if err != nil || ambiguous.State != store.FeishuOAuthRefreshAttemptStateAmbiguous ||
		ambiguous.ErrorCategory != store.FeishuOAuthRefreshErrorAmbiguousOutcome {
		t.Fatalf("ambiguous refresh attempt = %#v err=%v", ambiguous, err)
	}
}

func TestFeishuOAuthRefreshRecoveryProcessesAllBatches(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("batched recovery called remote OAuth endpoint: %s", r.URL.Path)
	}))
	defer server.Close()
	manager, st, _ := newTestResourceAccessManager(t, server, resourceAccessOAuthConfig{
		ClientID:    "cli_xxx",
		BaseURL:     server.URL,
		CallbackURL: "https://bridge.example.com/feishu/oauth/callback",
	})
	now := time.Date(2026, time.August, 4, 14, 30, 0, 0, time.UTC)
	for i := 0; i < feishuOAuthRefreshRecoveryLimit+1; i++ {
		actorOpenID := fmt.Sprintf("ou_recovery_%03d", i)
		credential, err := st.SaveFeishuUserOAuthCredential(store.FeishuUserOAuthCredential{
			AccountID:              manager.account.ID,
			ActorOpenID:            actorOpenID,
			AccessTokenCiphertext:  "v1.access-" + actorOpenID,
			AccessTokenExpiresAt:   now.Add(time.Minute),
			RefreshTokenCiphertext: "v1.refresh-" + actorOpenID,
			RefreshTokenExpiresAt:  now.Add(24 * time.Hour),
			Scopes:                 resourceAccessOAuthScope,
			AuthorizedAt:           now,
			ReauthorizeAt:          now.Add(365 * 24 * time.Hour),
			Status:                 store.FeishuUserOAuthCredentialStatusActive,
			CreatedAt:              now,
			UpdatedAt:              now,
		})
		if err != nil {
			t.Fatalf("save recovery credential %d: %v", i, err)
		}
		if _, _, err := st.PrepareFeishuOAuthRefreshAttempt(
			credential.ID,
			credential.AccountID,
			credential.Version,
			fmt.Sprintf("lease-%03d", i),
			now,
			time.Minute,
		); err != nil {
			t.Fatalf("prepare recovery attempt %d: %v", i, err)
		}
	}
	manager.now = func() time.Time { return now.Add(2 * time.Minute) }

	if err := manager.recoverFeishuOAuthRefreshAttempts(context.Background()); err != nil {
		t.Fatalf("recoverFeishuOAuthRefreshAttempts returned error: %v", err)
	}
	remaining, err := st.ListRecoverableFeishuOAuthRefreshAttempts(manager.account.ID, manager.currentTime(), 10)
	if err != nil {
		t.Fatalf("ListRecoverableFeishuOAuthRefreshAttempts returned error: %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("recoverable attempts after batched recovery = %d, want 0", len(remaining))
	}
}

func TestFeishuOAuthRefreshRecoveryFailsClosedAfterCredentialKeyRotation(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	manager, st, _ := newTestResourceAccessManager(t, server, resourceAccessOAuthConfig{
		ClientID:         "cli_xxx",
		BaseURL:          server.URL,
		CallbackURL:      "https://bridge.example.com/feishu/oauth/callback",
		CredentialSecret: "old-secret",
	})
	now := time.Date(2026, time.August, 4, 15, 0, 0, 0, time.UTC)
	manager.now = func() time.Time { return now }
	credential, err := manager.persistFeishuOAuthCredential(context.Background(), feishuOAuthIdentity{
		OpenID: "ou_requester",
		UserID: "u_requester",
	}, feishuOAuthTokenBundle{
		AccessToken:           "access-token-old",
		AccessTokenExpiresIn:  time.Minute,
		RefreshToken:          "refresh-token-old",
		RefreshTokenExpiresIn: 30 * 24 * time.Hour,
		Scopes:                resourceAccessOAuthScope,
	})
	if err != nil {
		t.Fatalf("persistFeishuOAuthCredential returned error: %v", err)
	}
	attempt, _, err := st.PrepareFeishuOAuthRefreshAttempt(
		credential.ID, credential.AccountID, credential.Version, "lease-owner", now, time.Minute,
	)
	if err != nil {
		t.Fatalf("PrepareFeishuOAuthRefreshAttempt returned error: %v", err)
	}
	stagedAccess, err := manager.credentialCipher.EncryptRefreshAttempt(attempt, "access_token", "access-token-new")
	if err != nil {
		t.Fatalf("encrypt staged access token: %v", err)
	}
	stagedRefresh, err := manager.credentialCipher.EncryptRefreshAttempt(attempt, "refresh_token", "refresh-token-new")
	if err != nil {
		t.Fatalf("encrypt staged refresh token: %v", err)
	}
	if _, err := st.StageFeishuOAuthRefreshResponse(attempt.ID, attempt.AccountID, attempt.LeaseToken, store.FeishuOAuthRefreshStage{
		AccessTokenCiphertext:  stagedAccess,
		AccessTokenExpiresAt:   now.Add(2 * time.Hour),
		RefreshTokenCiphertext: stagedRefresh,
		RefreshTokenExpiresAt:  now.Add(30 * 24 * time.Hour),
		Scopes:                 resourceAccessOAuthScope,
	}, now.Add(time.Second)); err != nil {
		t.Fatalf("StageFeishuOAuthRefreshResponse returned error: %v", err)
	}
	manager.credentialCipher, err = newFeishuOAuthCredentialCipher("new-secret", manager.account.ID)
	if err != nil {
		t.Fatalf("new rotated credential cipher: %v", err)
	}
	if err := manager.recoverFeishuOAuthRefreshAttempts(context.Background()); err != nil {
		t.Fatalf("recoverFeishuOAuthRefreshAttempts returned error: %v", err)
	}
	marked, err := st.GetFeishuUserOAuthCredentialByID(credential.ID, credential.AccountID)
	if err != nil || marked.Status != store.FeishuUserOAuthCredentialStatusReauthRequired {
		t.Fatalf("credential after key rotation recovery = %#v err=%v", marked, err)
	}
	failed, err := st.GetFeishuOAuthRefreshAttempt(attempt.ID, attempt.AccountID)
	if err != nil || failed.State != store.FeishuOAuthRefreshAttemptStateFailed ||
		failed.AccessTokenCiphertext != "" || failed.RefreshTokenCiphertext != "" {
		t.Fatalf("failed refresh attempt after key rotation = %#v err=%v", failed, err)
	}
}

func TestFeishuUserAccessTokenRequiresMandatoryReauthorizationAfter365Days(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("mandatory reauthorization called remote OAuth endpoint: %s", r.URL.Path)
	}))
	defer server.Close()
	manager, st, _ := newTestResourceAccessManager(t, server, resourceAccessOAuthConfig{
		ClientID:    "cli_xxx",
		BaseURL:     server.URL,
		CallbackURL: "https://bridge.example.com/feishu/oauth/callback",
	})
	now := time.Date(2026, time.August, 4, 16, 0, 0, 0, time.UTC)
	manager.now = func() time.Time { return now }
	credential, err := manager.persistFeishuOAuthCredential(context.Background(), feishuOAuthIdentity{
		OpenID: "ou_requester",
		UserID: "u_requester",
	}, feishuOAuthTokenBundle{
		AccessToken:           "long-lived-access-token",
		AccessTokenExpiresIn:  400 * 24 * time.Hour,
		RefreshToken:          "long-lived-refresh-token",
		RefreshTokenExpiresIn: 400 * 24 * time.Hour,
		Scopes:                resourceAccessOAuthScope,
	})
	if err != nil {
		t.Fatalf("persistFeishuOAuthCredential returned error: %v", err)
	}
	manager.now = func() time.Time { return now.Add(feishuOAuthMandatoryReauthorizationTTL) }
	if _, err := manager.feishuUserAccessToken(context.Background(), "ou_requester", "u_requester"); !errors.Is(err, ErrFeishuUserOAuthReauthorizationNeeded) {
		t.Fatalf("feishuUserAccessToken error = %v, want mandatory reauthorization", err)
	}
	marked, err := st.GetFeishuUserOAuthCredentialByID(credential.ID, credential.AccountID)
	if err != nil || marked.Status != store.FeishuUserOAuthCredentialStatusReauthRequired ||
		marked.AccessTokenCiphertext != "" || marked.RefreshTokenCiphertext != "" {
		t.Fatalf("mandatory reauthorization credential = %#v err=%v", marked, err)
	}
}

func TestFeishuUserAccessTokenMarksInvalidRefreshForReauthorization(t *testing.T) {
	var mu sync.Mutex
	refreshCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		refreshCalls++
		mu.Unlock()
		w.WriteHeader(http.StatusBadRequest)
		writeResourceAccessJSON(t, w, map[string]any{
			"code":              20026,
			"error":             "invalid_grant",
			"error_description": "refresh token expired",
		})
	}))
	defer server.Close()
	manager, st, _ := newTestResourceAccessManager(t, server, resourceAccessOAuthConfig{
		ClientID:    "cli_xxx",
		BaseURL:     server.URL,
		CallbackURL: "https://bridge.example.com/feishu/oauth/callback",
	})
	now := time.Date(2026, time.August, 3, 16, 0, 0, 0, time.UTC)
	manager.now = func() time.Time { return now }
	if _, err := manager.persistFeishuOAuthCredential(context.Background(), feishuOAuthIdentity{
		OpenID: "ou_requester",
		UserID: "u_requester",
	}, feishuOAuthTokenBundle{
		AccessToken:           "expired-access-token",
		AccessTokenExpiresIn:  time.Minute,
		RefreshToken:          "expired-refresh-token",
		RefreshTokenExpiresIn: 24 * time.Hour,
		Scopes:                resourceAccessOAuthScope,
	}); err != nil {
		t.Fatalf("persist OAuth credential: %v", err)
	}

	if _, err := manager.feishuUserAccessToken(context.Background(), "ou_requester", "u_requester"); !errors.Is(err, ErrFeishuUserOAuthReauthorizationNeeded) {
		t.Fatalf("first access token error = %v, want reauthorization", err)
	}
	credential, err := st.GetFeishuUserOAuthCredential("feishu:cli_test", "ou_requester", "u_requester")
	if err != nil || credential.Status != store.FeishuUserOAuthCredentialStatusReauthRequired ||
		credential.AccessTokenCiphertext != "" || credential.RefreshTokenCiphertext != "" {
		t.Fatalf("reauthorization credential = %#v err=%v", credential, err)
	}
	if _, err := manager.feishuUserAccessToken(context.Background(), "ou_requester", "u_requester"); !errors.Is(err, ErrFeishuUserOAuthReauthorizationNeeded) {
		t.Fatalf("second access token error = %v, want reauthorization", err)
	}
	mu.Lock()
	calls := refreshCalls
	mu.Unlock()
	if calls != 1 {
		t.Fatalf("refresh calls after terminal failure = %d, want 1", calls)
	}
}

func TestFeishuUserAccessTokenUsesSafeFallbackAfterRecoverableRefreshFailure(t *testing.T) {
	logs := captureMonitorLogs(t)
	logging.SetLevel(logging.Debug)
	var mu sync.Mutex
	refreshCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		refreshCalls++
		mu.Unlock()
		w.WriteHeader(http.StatusTooManyRequests)
		writeResourceAccessJSON(t, w, map[string]any{
			"error":             "rate_limited",
			"error_description": "retry later",
		})
	}))
	defer server.Close()
	manager, st, _ := newTestResourceAccessManager(t, server, resourceAccessOAuthConfig{
		ClientID:    "cli_xxx",
		BaseURL:     server.URL,
		CallbackURL: "https://bridge.example.com/feishu/oauth/callback",
	})
	now := time.Date(2026, time.August, 4, 17, 0, 0, 0, time.UTC)
	manager.now = func() time.Time { return now }
	credential, err := manager.persistFeishuOAuthCredential(context.Background(), feishuOAuthIdentity{
		OpenID: "ou_requester",
		UserID: "u_requester",
	}, feishuOAuthTokenBundle{
		AccessToken:           "still-valid-access-token",
		AccessTokenExpiresIn:  time.Minute,
		RefreshToken:          "refresh-token",
		RefreshTokenExpiresIn: 24 * time.Hour,
		Scopes:                resourceAccessOAuthScope,
	})
	if err != nil {
		t.Fatalf("persist OAuth credential: %v", err)
	}
	token, err := manager.feishuUserAccessToken(context.Background(), "ou_requester", "u_requester")
	if err != nil || token != "still-valid-access-token" {
		t.Fatalf("feishuUserAccessToken fallback = %q err=%v", token, err)
	}
	unchanged, err := st.GetFeishuUserOAuthCredentialByID(credential.ID, credential.AccountID)
	if err != nil || unchanged.Status != store.FeishuUserOAuthCredentialStatusActive || unchanged.Version != credential.Version {
		t.Fatalf("credential after recoverable refresh failure = %#v err=%v", unchanged, err)
	}
	if _, err := st.ActiveFeishuOAuthRefreshAttempt(credential.ID, credential.AccountID); !errors.Is(err, store.ErrFeishuOAuthRefreshAttemptNotFound) {
		t.Fatalf("active attempt after recoverable failure error = %v, want not found", err)
	}
	mu.Lock()
	calls := refreshCalls
	mu.Unlock()
	if calls != 1 {
		t.Fatalf("refresh calls = %d, want 1", calls)
	}
	for _, secret := range []string{"still-valid-access-token", "refresh-token"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("recoverable refresh logs leaked %q:\n%s", secret, logs.String())
		}
	}
}

func TestFeishuUserAccessTokenRequiresReauthorizationWhenStoredScopesAreStale(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected OAuth request: %s", r.URL.Path)
	}))
	defer server.Close()
	manager, st, _ := newTestResourceAccessManager(t, server, resourceAccessOAuthConfig{
		ClientID:    "cli_xxx",
		BaseURL:     server.URL,
		CallbackURL: "https://bridge.example.com/feishu/oauth/callback",
	})
	now := time.Date(2026, time.August, 3, 16, 30, 0, 0, time.UTC)
	manager.now = func() time.Time { return now }
	if _, err := manager.persistFeishuOAuthCredential(context.Background(), feishuOAuthIdentity{
		OpenID: "ou_requester",
		UserID: "u_requester",
	}, feishuOAuthTokenBundle{
		AccessToken:           "access-token",
		AccessTokenExpiresIn:  2 * time.Hour,
		RefreshToken:          "refresh-token",
		RefreshTokenExpiresIn: 24 * time.Hour,
		Scopes:                "auth:user.id:read docs:permission.member:create offline_access",
	}); err != nil {
		t.Fatalf("persist OAuth credential: %v", err)
	}

	if _, err := manager.feishuUserAccessToken(context.Background(), "ou_requester", "u_requester"); !errors.Is(err, ErrFeishuUserOAuthReauthorizationNeeded) {
		t.Fatalf("access token error = %v, want reauthorization for missing update scope", err)
	}
	credential, err := st.GetFeishuUserOAuthCredential(manager.account.ID, "ou_requester", "u_requester")
	if err != nil || credential.Status != store.FeishuUserOAuthCredentialStatusReauthRequired ||
		credential.AccessTokenCiphertext != "" || credential.RefreshTokenCiphertext != "" {
		t.Fatalf("credential after stale scope detection = %#v err=%v", credential, err)
	}
}

func TestFeishuUserAccessTokenRequiresReauthorizationAfterCredentialKeyRotation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected refresh request: %s", r.URL.Path)
	}))
	defer server.Close()
	manager, st, _ := newTestResourceAccessManager(t, server, resourceAccessOAuthConfig{
		ClientID:         "cli_xxx",
		BaseURL:          server.URL,
		CallbackURL:      "https://bridge.example.com/feishu/oauth/callback",
		CredentialSecret: "old-secret",
	})
	now := time.Date(2026, time.August, 3, 17, 0, 0, 0, time.UTC)
	manager.now = func() time.Time { return now }
	if _, err := manager.persistFeishuOAuthCredential(context.Background(), feishuOAuthIdentity{
		OpenID: "ou_requester",
		UserID: "u_requester",
	}, feishuOAuthTokenBundle{
		AccessToken:           "access-token",
		AccessTokenExpiresIn:  2 * time.Hour,
		RefreshToken:          "refresh-token",
		RefreshTokenExpiresIn: 30 * 24 * time.Hour,
		Scopes:                resourceAccessOAuthScope,
	}); err != nil {
		t.Fatalf("persist OAuth credential: %v", err)
	}
	rotatedCipher, err := newFeishuOAuthCredentialCipher("new-secret", manager.account.ID)
	if err != nil {
		t.Fatalf("new rotated credential cipher: %v", err)
	}
	manager.credentialCipher = rotatedCipher

	if _, err := manager.feishuUserAccessToken(context.Background(), "ou_requester", "u_requester"); !errors.Is(err, ErrFeishuUserOAuthReauthorizationNeeded) {
		t.Fatalf("access token error after key rotation = %v, want reauthorization", err)
	}
	credential, err := st.GetFeishuUserOAuthCredential(manager.account.ID, "ou_requester", "u_requester")
	if err != nil || credential.Status != store.FeishuUserOAuthCredentialStatusReauthRequired ||
		credential.AccessTokenCiphertext != "" || credential.RefreshTokenCiphertext != "" {
		t.Fatalf("credential after key rotation = %#v err=%v", credential, err)
	}
}
