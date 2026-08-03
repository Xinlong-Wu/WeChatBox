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
	for range 2 {
		go func() {
			<-start
			token, err := manager.feishuUserAccessToken(context.Background(), "ou_requester", "u_requester")
			results <- token
			errs <- err
		}()
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
	logText := logs.String()
	for _, secret := range []string{"access-token-old", "access-token-new", "refresh-token-old", "refresh-token-new"} {
		if strings.Contains(logText, secret) {
			t.Fatalf("OAuth refresh logs leaked %q:\n%s", secret, logText)
		}
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
