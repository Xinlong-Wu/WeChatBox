package store

import (
	"errors"
	"testing"
	"time"
)

func TestFeishuOAuthRefreshAttemptLifecycle(t *testing.T) {
	st := openFeishuDocsTestStore(t)
	now := time.Date(2026, time.August, 4, 8, 0, 0, 0, time.UTC)
	credential := saveFeishuOAuthRefreshCredentialForTest(t, st, "feishu:cli_test", now)

	attempt, created, err := st.PrepareFeishuOAuthRefreshAttempt(
		credential.ID,
		credential.AccountID,
		credential.Version,
		"lease-owner",
		now,
		time.Minute,
	)
	if err != nil || !created {
		t.Fatalf("PrepareFeishuOAuthRefreshAttempt = %#v created=%t err=%v", attempt, created, err)
	}
	if attempt.State != FeishuOAuthRefreshAttemptStatePrepared || attempt.ActorOpenID != credential.ActorOpenID ||
		attempt.ActorUserID != credential.ActorUserID || attempt.ExpectedVersion != credential.Version {
		t.Fatalf("prepared attempt = %#v", attempt)
	}
	existing, created, err := st.PrepareFeishuOAuthRefreshAttempt(
		credential.ID,
		credential.AccountID,
		credential.Version,
		"second-owner",
		now,
		time.Minute,
	)
	if err != nil || created || existing.ID != attempt.ID || existing.LeaseToken != "lease-owner" {
		t.Fatalf("second prepare = %#v created=%t err=%v", existing, created, err)
	}

	stage := FeishuOAuthRefreshStage{
		AccessTokenCiphertext:  "v1.staged-access-ciphertext",
		AccessTokenExpiresAt:   now.Add(2 * time.Hour),
		RefreshTokenCiphertext: "v1.staged-refresh-ciphertext",
		RefreshTokenExpiresAt:  now.Add(30 * 24 * time.Hour),
		Scopes:                 "offline_access auth:user.id:read",
	}
	if _, err := st.StageFeishuOAuthRefreshResponse(attempt.ID, attempt.AccountID, "wrong-owner", stage, now.Add(time.Second)); !errors.Is(err, ErrFeishuOAuthRefreshAttemptLeaseLost) {
		t.Fatalf("wrong lease stage error = %v, want lease lost", err)
	}
	staged, err := st.StageFeishuOAuthRefreshResponse(attempt.ID, attempt.AccountID, attempt.LeaseToken, stage, now.Add(2*time.Second))
	if err != nil {
		t.Fatalf("StageFeishuOAuthRefreshResponse returned error: %v", err)
	}
	if staged.State != FeishuOAuthRefreshAttemptStateResponseStaged || staged.LeaseToken != "" ||
		staged.AccessTokenCiphertext != stage.AccessTokenCiphertext || staged.RefreshTokenCiphertext != stage.RefreshTokenCiphertext {
		t.Fatalf("staged attempt = %#v", staged)
	}

	rotated, completed, err := st.ApplyFeishuOAuthRefreshAttempt(attempt.ID, attempt.AccountID, FeishuOAuthRefreshCredentialUpdate{
		AccessTokenCiphertext:  "v1.final-access-ciphertext",
		AccessTokenExpiresAt:   stage.AccessTokenExpiresAt,
		RefreshTokenCiphertext: "v1.final-refresh-ciphertext",
		RefreshTokenExpiresAt:  stage.RefreshTokenExpiresAt,
		Scopes:                 stage.Scopes,
	}, now.Add(3*time.Second))
	if err != nil {
		t.Fatalf("ApplyFeishuOAuthRefreshAttempt returned error: %v", err)
	}
	if rotated.Version != credential.Version+1 || rotated.AccessTokenCiphertext != "v1.final-access-ciphertext" ||
		rotated.RefreshTokenCiphertext != "v1.final-refresh-ciphertext" || completed.State != FeishuOAuthRefreshAttemptStateCompleted {
		t.Fatalf("rotated=%#v completed=%#v", rotated, completed)
	}
	persisted, err := st.GetFeishuOAuthRefreshAttempt(attempt.ID, attempt.AccountID)
	if err != nil {
		t.Fatalf("GetFeishuOAuthRefreshAttempt returned error: %v", err)
	}
	if persisted.State != FeishuOAuthRefreshAttemptStateCompleted || persisted.LeaseToken != "" ||
		persisted.AccessTokenCiphertext != "" || persisted.RefreshTokenCiphertext != "" || persisted.Scopes != "" {
		t.Fatalf("persisted terminal attempt retained sensitive data: %#v", persisted)
	}
	if _, err := st.ActiveFeishuOAuthRefreshAttempt(credential.ID, credential.AccountID); !errors.Is(err, ErrFeishuOAuthRefreshAttemptNotFound) {
		t.Fatalf("active attempt after completion error = %v, want not found", err)
	}
}

func TestFeishuOAuthRefreshAttemptDoesNotOverwriteAdvancedCredential(t *testing.T) {
	st := openFeishuDocsTestStore(t)
	now := time.Date(2026, time.August, 4, 9, 0, 0, 0, time.UTC)
	credential := saveFeishuOAuthRefreshCredentialForTest(t, st, "feishu:cli_test", now)
	attempt, _, err := st.PrepareFeishuOAuthRefreshAttempt(
		credential.ID, credential.AccountID, credential.Version, "lease-owner", now, time.Minute,
	)
	if err != nil {
		t.Fatalf("prepare refresh attempt: %v", err)
	}
	stage := FeishuOAuthRefreshStage{
		AccessTokenCiphertext:  "v1.staged-access",
		AccessTokenExpiresAt:   now.Add(2 * time.Hour),
		RefreshTokenCiphertext: "v1.staged-refresh",
		RefreshTokenExpiresAt:  now.Add(30 * 24 * time.Hour),
		Scopes:                 credential.Scopes,
	}
	if _, err := st.StageFeishuOAuthRefreshResponse(attempt.ID, attempt.AccountID, attempt.LeaseToken, stage, now.Add(time.Second)); err != nil {
		t.Fatalf("stage refresh response: %v", err)
	}

	newer := credential
	newer.AccessTokenCiphertext = "v1.newer-access"
	newer.AccessTokenExpiresAt = now.Add(4 * time.Hour)
	newer.RefreshTokenCiphertext = "v1.newer-refresh"
	newer.RefreshTokenExpiresAt = now.Add(60 * 24 * time.Hour)
	newer.LastRefreshedAt = now.Add(2 * time.Second)
	newer.UpdatedAt = now.Add(2 * time.Second)
	newer, err = st.RotateFeishuUserOAuthCredential(newer, credential.Version)
	if err != nil {
		t.Fatalf("advance credential: %v", err)
	}

	got, completed, err := st.ApplyFeishuOAuthRefreshAttempt(attempt.ID, attempt.AccountID, FeishuOAuthRefreshCredentialUpdate{
		AccessTokenCiphertext:  "v1.stale-final-access",
		AccessTokenExpiresAt:   stage.AccessTokenExpiresAt,
		RefreshTokenCiphertext: "v1.stale-final-refresh",
		RefreshTokenExpiresAt:  stage.RefreshTokenExpiresAt,
		Scopes:                 stage.Scopes,
	}, now.Add(3*time.Second))
	if err != nil {
		t.Fatalf("ApplyFeishuOAuthRefreshAttempt returned error: %v", err)
	}
	if got.Version != newer.Version || got.AccessTokenCiphertext != newer.AccessTokenCiphertext ||
		got.RefreshTokenCiphertext != newer.RefreshTokenCiphertext || completed.State != FeishuOAuthRefreshAttemptStateCompleted {
		t.Fatalf("stale attempt overwrote credential: got=%#v newer=%#v attempt=%#v", got, newer, completed)
	}
}

func TestFeishuOAuthRefreshAttemptExpiredPreparedBecomesAmbiguous(t *testing.T) {
	st := openFeishuDocsTestStore(t)
	now := time.Date(2026, time.August, 4, 10, 0, 0, 0, time.UTC)
	credential := saveFeishuOAuthRefreshCredentialForTest(t, st, "feishu:cli_test", now)
	attempt, _, err := st.PrepareFeishuOAuthRefreshAttempt(
		credential.ID, credential.AccountID, credential.Version, "lease-owner", now, time.Minute,
	)
	if err != nil {
		t.Fatalf("prepare refresh attempt: %v", err)
	}
	if _, _, err := st.MarkFeishuOAuthRefreshAttemptAmbiguous(attempt.ID, attempt.AccountID, now.Add(30*time.Second)); !errors.Is(err, ErrFeishuOAuthRefreshAttemptLeaseLost) {
		t.Fatalf("early ambiguous error = %v, want lease lost", err)
	}

	marked, ambiguous, err := st.MarkFeishuOAuthRefreshAttemptAmbiguous(attempt.ID, attempt.AccountID, now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("MarkFeishuOAuthRefreshAttemptAmbiguous returned error: %v", err)
	}
	if marked.Status != FeishuUserOAuthCredentialStatusReauthRequired || marked.Version != credential.Version+1 ||
		marked.AccessTokenCiphertext != "" || marked.RefreshTokenCiphertext != "" {
		t.Fatalf("ambiguous credential = %#v", marked)
	}
	if ambiguous.State != FeishuOAuthRefreshAttemptStateAmbiguous || ambiguous.ErrorCategory != FeishuOAuthRefreshErrorAmbiguousOutcome ||
		ambiguous.LeaseToken != "" || ambiguous.AccessTokenCiphertext != "" || ambiguous.RefreshTokenCiphertext != "" {
		t.Fatalf("ambiguous attempt = %#v", ambiguous)
	}
}

func TestFeishuOAuthRefreshAttemptFailureCanRequireReauthorization(t *testing.T) {
	st := openFeishuDocsTestStore(t)
	now := time.Date(2026, time.August, 4, 11, 0, 0, 0, time.UTC)
	credential := saveFeishuOAuthRefreshCredentialForTest(t, st, "feishu:cli_test", now)
	attempt, _, err := st.PrepareFeishuOAuthRefreshAttempt(
		credential.ID, credential.AccountID, credential.Version, "lease-owner", now, time.Minute,
	)
	if err != nil {
		t.Fatalf("prepare refresh attempt: %v", err)
	}
	if _, _, err := st.FailFeishuOAuthRefreshAttempt(attempt.ID, attempt.AccountID, "wrong-owner", "invalid_grant", true, now.Add(time.Second)); !errors.Is(err, ErrFeishuOAuthRefreshAttemptLeaseLost) {
		t.Fatalf("wrong lease failure error = %v, want lease lost", err)
	}
	marked, failed, err := st.FailFeishuOAuthRefreshAttempt(
		attempt.ID, attempt.AccountID, attempt.LeaseToken, "invalid_grant", true, now.Add(2*time.Second),
	)
	if err != nil {
		t.Fatalf("FailFeishuOAuthRefreshAttempt returned error: %v", err)
	}
	if marked.Status != FeishuUserOAuthCredentialStatusReauthRequired || marked.Version != credential.Version+1 ||
		failed.State != FeishuOAuthRefreshAttemptStateFailed || failed.ErrorCategory != "invalid_grant" {
		t.Fatalf("marked=%#v failed=%#v", marked, failed)
	}
}

func TestListRecoverableFeishuOAuthRefreshAttempts(t *testing.T) {
	st := openFeishuDocsTestStore(t)
	now := time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC)

	expiredCredential := saveFeishuOAuthRefreshCredentialForTest(t, st, "feishu:expired", now)
	expired, _, err := st.PrepareFeishuOAuthRefreshAttempt(
		expiredCredential.ID, expiredCredential.AccountID, expiredCredential.Version, "expired-lease", now, time.Minute,
	)
	if err != nil {
		t.Fatalf("prepare expired attempt: %v", err)
	}
	liveCredential := saveFeishuOAuthRefreshCredentialForTest(t, st, "feishu:live", now)
	if _, _, err := st.PrepareFeishuOAuthRefreshAttempt(
		liveCredential.ID, liveCredential.AccountID, liveCredential.Version, "live-lease", now, 10*time.Minute,
	); err != nil {
		t.Fatalf("prepare live attempt: %v", err)
	}
	stagedCredential := saveFeishuOAuthRefreshCredentialForTest(t, st, "feishu:staged", now)
	staged, _, err := st.PrepareFeishuOAuthRefreshAttempt(
		stagedCredential.ID, stagedCredential.AccountID, stagedCredential.Version, "staged-lease", now, 10*time.Minute,
	)
	if err != nil {
		t.Fatalf("prepare staged attempt: %v", err)
	}
	if _, err := st.StageFeishuOAuthRefreshResponse(staged.ID, staged.AccountID, staged.LeaseToken, FeishuOAuthRefreshStage{
		AccessTokenCiphertext:  "v1.staged-access",
		AccessTokenExpiresAt:   now.Add(2 * time.Hour),
		RefreshTokenCiphertext: "v1.staged-refresh",
		RefreshTokenExpiresAt:  now.Add(30 * 24 * time.Hour),
		Scopes:                 stagedCredential.Scopes,
	}, now.Add(time.Second)); err != nil {
		t.Fatalf("stage response: %v", err)
	}

	for _, tc := range []struct {
		accountID string
		wantID    string
	}{
		{accountID: expired.AccountID, wantID: expired.ID},
		{accountID: liveCredential.AccountID, wantID: ""},
		{accountID: staged.AccountID, wantID: staged.ID},
	} {
		attempts, err := st.ListRecoverableFeishuOAuthRefreshAttempts(tc.accountID, now.Add(2*time.Minute), 10)
		if err != nil {
			t.Fatalf("ListRecoverableFeishuOAuthRefreshAttempts(%s): %v", tc.accountID, err)
		}
		if tc.wantID == "" {
			if len(attempts) != 0 {
				t.Fatalf("recoverable attempts for %s = %#v, want none", tc.accountID, attempts)
			}
			continue
		}
		if len(attempts) != 1 || attempts[0].ID != tc.wantID {
			t.Fatalf("recoverable attempts for %s = %#v, want %s", tc.accountID, attempts, tc.wantID)
		}
	}
}

func saveFeishuOAuthRefreshCredentialForTest(t *testing.T, st *Store, accountID string, now time.Time) FeishuUserOAuthCredential {
	t.Helper()
	credential, err := st.SaveFeishuUserOAuthCredential(FeishuUserOAuthCredential{
		AccountID:              accountID,
		ActorOpenID:            "ou_requester",
		ActorUserID:            "u_requester",
		AccessTokenCiphertext:  "v1.access-" + accountID,
		AccessTokenExpiresAt:   now.Add(time.Minute),
		RefreshTokenCiphertext: "v1.refresh-" + accountID,
		RefreshTokenExpiresAt:  now.Add(30 * 24 * time.Hour),
		Scopes:                 "auth:user.id:read offline_access",
		AuthorizedAt:           now,
		ReauthorizeAt:          now.Add(365 * 24 * time.Hour),
		Status:                 FeishuUserOAuthCredentialStatusActive,
		CreatedAt:              now,
		UpdatedAt:              now,
	})
	if err != nil {
		t.Fatalf("SaveFeishuUserOAuthCredential returned error: %v", err)
	}
	return credential
}
