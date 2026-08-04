package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/larksuite/oapi-sdk-go/v3/core/accesstoken"

	"lingobridge/internal/logging"
	"lingobridge/internal/store"
)

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
		w.WriteHeader(http.StatusServiceUnavailable)
		writeResourceAccessJSON(t, w, map[string]any{
			"error":             "temporarily_unavailable",
			"error_description": "try again later",
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
