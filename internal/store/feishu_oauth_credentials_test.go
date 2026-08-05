package store

import (
	"errors"
	"testing"
	"time"
)

func TestFeishuUserOAuthCredentialLifecycleAndVersionCAS(t *testing.T) {
	st := openFeishuDocsTestStore(t)
	now := time.Date(2026, time.August, 3, 12, 0, 0, 0, time.UTC)
	credential, err := st.SaveFeishuUserOAuthCredential(FeishuUserOAuthCredential{
		AccountID:              "feishu:cli_test",
		ActorOpenID:            "ou_requester",
		ActorUserID:            "u_requester",
		AccessTokenCiphertext:  "v1.access-ciphertext",
		AccessTokenExpiresAt:   now.Add(2 * time.Hour),
		RefreshTokenCiphertext: "v1.refresh-ciphertext",
		RefreshTokenExpiresAt:  now.Add(30 * 24 * time.Hour),
		Scopes:                 "offline_access auth:user.id:read",
		AuthorizedAt:           now,
		ReauthorizeAt:          now.Add(365 * 24 * time.Hour),
		Status:                 FeishuUserOAuthCredentialStatusActive,
		CreatedAt:              now,
		UpdatedAt:              now,
	})
	if err != nil {
		t.Fatalf("SaveFeishuUserOAuthCredential returned error: %v", err)
	}
	if credential.ID == "" || credential.Version != 1 {
		t.Fatalf("saved credential = %#v", credential)
	}
	byOpenID, err := st.GetFeishuUserOAuthCredential(credential.AccountID, credential.ActorOpenID, "")
	if err != nil || byOpenID.ID != credential.ID {
		t.Fatalf("credential by open_id = %#v err=%v", byOpenID, err)
	}
	byUserID, err := st.GetFeishuUserOAuthCredential(credential.AccountID, "", credential.ActorUserID)
	if err != nil || byUserID.ID != credential.ID {
		t.Fatalf("credential by user_id = %#v err=%v", byUserID, err)
	}

	canonical, err := st.SaveFeishuUserOAuthCredential(FeishuUserOAuthCredential{
		AccountID:              credential.AccountID,
		ActorUserID:            credential.ActorUserID,
		AccessTokenCiphertext:  "v1.new-access-ciphertext",
		AccessTokenExpiresAt:   now.Add(3 * time.Hour),
		RefreshTokenCiphertext: "v1.new-refresh-ciphertext",
		RefreshTokenExpiresAt:  now.Add(60 * 24 * time.Hour),
		Scopes:                 "auth:user.id:read offline_access",
		AuthorizedAt:           now.Add(time.Hour),
		ReauthorizeAt:          now.Add(366 * 24 * time.Hour),
		Status:                 FeishuUserOAuthCredentialStatusActive,
		CreatedAt:              now.Add(time.Hour),
		UpdatedAt:              now.Add(time.Hour),
	})
	if !errors.Is(err, ErrFeishuUserOAuthIdentityChanged) || canonical.ID != credential.ID || canonical.ActorOpenID != credential.ActorOpenID {
		t.Fatalf("partial identity replacement = %#v err=%v", canonical, err)
	}
	reauthorized, err := st.SaveFeishuUserOAuthCredential(FeishuUserOAuthCredential{
		AccountID:              credential.AccountID,
		ActorOpenID:            credential.ActorOpenID,
		ActorUserID:            credential.ActorUserID,
		AccessTokenCiphertext:  "v1.new-access-ciphertext",
		AccessTokenExpiresAt:   now.Add(3 * time.Hour),
		RefreshTokenCiphertext: "v1.new-refresh-ciphertext",
		RefreshTokenExpiresAt:  now.Add(60 * 24 * time.Hour),
		Scopes:                 "auth:user.id:read offline_access",
		AuthorizedAt:           now.Add(time.Hour),
		ReauthorizeAt:          now.Add(366 * 24 * time.Hour),
		Status:                 FeishuUserOAuthCredentialStatusActive,
		CreatedAt:              now.Add(time.Hour),
		UpdatedAt:              now.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("replace OAuth credential with canonical identity returned error: %v", err)
	}
	if reauthorized.ID != credential.ID || reauthorized.Version != 2 || reauthorized.ActorOpenID != credential.ActorOpenID || !reauthorized.CreatedAt.Equal(now) {
		t.Fatalf("reauthorized credential = %#v", reauthorized)
	}

	rotated := reauthorized
	rotated.AccessTokenCiphertext = "v1.rotated-access-ciphertext"
	rotated.AccessTokenExpiresAt = now.Add(4 * time.Hour)
	rotated.RefreshTokenCiphertext = "v1.rotated-refresh-ciphertext"
	rotated.RefreshTokenExpiresAt = now.Add(90 * 24 * time.Hour)
	rotated.LastRefreshedAt = now.Add(2 * time.Hour)
	rotated.UpdatedAt = now.Add(2 * time.Hour)
	if _, err := st.RotateFeishuUserOAuthCredential(rotated, 1); !errors.Is(err, ErrFeishuUserOAuthCredentialConflict) {
		t.Fatalf("stale rotation error = %v, want conflict", err)
	}
	rotated, err = st.RotateFeishuUserOAuthCredential(rotated, reauthorized.Version)
	if err != nil || rotated.Version != 3 || !rotated.LastRefreshedAt.Equal(now.Add(2*time.Hour)) {
		t.Fatalf("rotated credential = %#v err=%v", rotated, err)
	}

	reauthRequired, err := st.MarkFeishuUserOAuthCredentialReauthRequired(rotated.ID, rotated.AccountID, rotated.Version, now.Add(3*time.Hour))
	if err != nil {
		t.Fatalf("MarkFeishuUserOAuthCredentialReauthRequired returned error: %v", err)
	}
	if reauthRequired.Status != FeishuUserOAuthCredentialStatusReauthRequired || reauthRequired.Version != 4 ||
		reauthRequired.AccessTokenCiphertext != "" || reauthRequired.RefreshTokenCiphertext != "" ||
		!reauthRequired.AccessTokenExpiresAt.IsZero() || !reauthRequired.RefreshTokenExpiresAt.IsZero() {
		t.Fatalf("reauthorization-required credential = %#v", reauthRequired)
	}
}

func TestFeishuUserOAuthCredentialRejectsMergedDifferentIdentities(t *testing.T) {
	st := openFeishuDocsTestStore(t)
	now := time.Date(2026, time.August, 3, 13, 0, 0, 0, time.UTC)
	save := func(openID, userID string) FeishuUserOAuthCredential {
		t.Helper()
		credential, err := st.SaveFeishuUserOAuthCredential(FeishuUserOAuthCredential{
			AccountID:             "feishu:cli_test",
			ActorOpenID:           openID,
			ActorUserID:           userID,
			AccessTokenCiphertext: "v1.access-" + openID,
			AccessTokenExpiresAt:  now.Add(time.Hour),
			AuthorizedAt:          now,
			ReauthorizeAt:         now.Add(365 * 24 * time.Hour),
			Status:                FeishuUserOAuthCredentialStatusActive,
			CreatedAt:             now,
		})
		if err != nil {
			t.Fatalf("save OAuth credential: %v", err)
		}
		return credential
	}
	first := save("ou_one", "u_one")
	second := save("ou_two", "u_two")
	if first.ID == second.ID {
		t.Fatalf("separate identities shared credential ID %q", first.ID)
	}
	_, err := st.SaveFeishuUserOAuthCredential(FeishuUserOAuthCredential{
		AccountID:             "feishu:cli_test",
		ActorOpenID:           "ou_one",
		ActorUserID:           "u_two",
		AccessTokenCiphertext: "v1.merged-access",
		AccessTokenExpiresAt:  now.Add(time.Hour),
		AuthorizedAt:          now,
		ReauthorizeAt:         now.Add(365 * 24 * time.Hour),
		Status:                FeishuUserOAuthCredentialStatusActive,
		CreatedAt:             now,
	})
	if !errors.Is(err, ErrFeishuUserOAuthIdentityConflict) {
		t.Fatalf("merged identity error = %v, want identity conflict", err)
	}
}
