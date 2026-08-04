package store

import (
	"errors"
	"fmt"
	"sync"
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

func TestFeishuOAuthNewAuthorizationWinsConcurrentStagedRefreshAcrossStores(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	primary, err := Open(PlatformFeishu)
	if err != nil {
		t.Fatalf("open primary store: %v", err)
	}
	t.Cleanup(func() {
		if err := primary.Close(); err != nil {
			t.Errorf("close primary store: %v", err)
		}
	})
	secondary, err := Open(PlatformFeishu)
	if err != nil {
		t.Fatalf("open secondary store: %v", err)
	}
	t.Cleanup(func() {
		if err := secondary.Close(); err != nil {
			t.Errorf("close secondary store: %v", err)
		}
	})

	now := time.Date(2026, time.August, 4, 8, 30, 0, 0, time.UTC)
	credential := saveFeishuOAuthRefreshCredentialForTest(t, primary, "feishu:cli_concurrent", now)
	attempt, owner, err := primary.PrepareFeishuOAuthRefreshAttempt(
		credential.ID, credential.AccountID, credential.Version, "lease-owner", now, time.Minute,
	)
	if err != nil || !owner {
		t.Fatalf("PrepareFeishuOAuthRefreshAttempt = %#v owner=%t err=%v", attempt, owner, err)
	}
	staged, err := primary.StageFeishuOAuthRefreshResponse(attempt.ID, attempt.AccountID, attempt.LeaseToken, FeishuOAuthRefreshStage{
		AccessTokenCiphertext:  "v1.staged-access",
		AccessTokenExpiresAt:   now.Add(2 * time.Hour),
		RefreshTokenCiphertext: "v1.staged-refresh",
		RefreshTokenExpiresAt:  now.Add(60 * 24 * time.Hour),
		Scopes:                 credential.Scopes,
	}, now.Add(time.Second))
	if err != nil {
		t.Fatalf("StageFeishuOAuthRefreshResponse returned error: %v", err)
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	applyErr := make(chan error, 1)
	saveErr := make(chan error, 1)
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, _, err := primary.ApplyFeishuOAuthRefreshAttempt(staged.ID, staged.AccountID, FeishuOAuthRefreshCredentialUpdate{
			AccessTokenCiphertext:  "v1.refreshed-access",
			AccessTokenExpiresAt:   staged.AccessTokenExpiresAt,
			RefreshTokenCiphertext: "v1.refreshed-refresh",
			RefreshTokenExpiresAt:  staged.RefreshTokenExpiresAt,
			Scopes:                 staged.Scopes,
		}, now.Add(2*time.Second))
		applyErr <- err
	}()
	go func() {
		defer wg.Done()
		<-start
		_, err := secondary.SaveFeishuUserOAuthCredential(FeishuUserOAuthCredential{
			AccountID:              credential.AccountID,
			ActorOpenID:            credential.ActorOpenID,
			ActorUserID:            credential.ActorUserID,
			AccessTokenCiphertext:  "v1.new-authorized-access",
			AccessTokenExpiresAt:   now.Add(3 * time.Hour),
			RefreshTokenCiphertext: "v1.new-authorized-refresh",
			RefreshTokenExpiresAt:  now.Add(90 * 24 * time.Hour),
			Scopes:                 credential.Scopes,
			AuthorizedAt:           now.Add(2 * time.Second),
			ReauthorizeAt:          now.Add(365 * 24 * time.Hour),
			Status:                 FeishuUserOAuthCredentialStatusActive,
			CreatedAt:              now.Add(2 * time.Second),
			UpdatedAt:              now.Add(2 * time.Second),
		})
		saveErr <- err
	}()
	close(start)
	wg.Wait()
	if err := <-applyErr; err != nil {
		t.Fatalf("concurrent ApplyFeishuOAuthRefreshAttempt returned error: %v", err)
	}
	if err := <-saveErr; err != nil {
		t.Fatalf("concurrent SaveFeishuUserOAuthCredential returned error: %v", err)
	}

	latest, err := primary.GetFeishuUserOAuthCredentialByID(credential.ID, credential.AccountID)
	if err != nil || latest.Status != FeishuUserOAuthCredentialStatusActive || latest.AccessTokenCiphertext != "v1.new-authorized-access" || latest.Version < 2 {
		t.Fatalf("winning OAuth credential = %#v err=%v", latest, err)
	}
	resolved, err := primary.GetFeishuOAuthRefreshAttempt(attempt.ID, attempt.AccountID)
	if err != nil || resolved.State != FeishuOAuthRefreshAttemptStateCompleted {
		t.Fatalf("resolved refresh attempt = %#v err=%v", resolved, err)
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

func TestFeishuOAuthRefreshAttemptOwnerCanMarkAmbiguousBeforeLeaseExpiry(t *testing.T) {
	st := openFeishuDocsTestStore(t)
	now := time.Date(2026, time.August, 4, 10, 30, 0, 0, time.UTC)
	credential := saveFeishuOAuthRefreshCredentialForTest(t, st, "feishu:cli_test", now)
	attempt, _, err := st.PrepareFeishuOAuthRefreshAttempt(
		credential.ID, credential.AccountID, credential.Version, "lease-owner", now, time.Minute,
	)
	if err != nil {
		t.Fatalf("prepare refresh attempt: %v", err)
	}
	if _, _, err := st.MarkOwnedFeishuOAuthRefreshAttemptAmbiguous(
		attempt.ID, attempt.AccountID, "wrong-owner", now.Add(time.Second),
	); !errors.Is(err, ErrFeishuOAuthRefreshAttemptLeaseLost) {
		t.Fatalf("wrong lease ambiguous error = %v, want lease lost", err)
	}

	marked, ambiguous, err := st.MarkOwnedFeishuOAuthRefreshAttemptAmbiguous(
		attempt.ID, attempt.AccountID, attempt.LeaseToken, now.Add(2*time.Second),
	)
	if err != nil {
		t.Fatalf("MarkOwnedFeishuOAuthRefreshAttemptAmbiguous returned error: %v", err)
	}
	if marked.Status != FeishuUserOAuthCredentialStatusReauthRequired || marked.Version != credential.Version+1 ||
		marked.AccessTokenCiphertext != "" || marked.RefreshTokenCiphertext != "" {
		t.Fatalf("owned ambiguous credential = %#v", marked)
	}
	if ambiguous.State != FeishuOAuthRefreshAttemptStateAmbiguous || ambiguous.ErrorCategory != FeishuOAuthRefreshErrorAmbiguousOutcome ||
		ambiguous.LeaseToken != "" || ambiguous.AccessTokenCiphertext != "" || ambiguous.RefreshTokenCiphertext != "" {
		t.Fatalf("owned ambiguous attempt = %#v", ambiguous)
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

func TestDeleteTerminalFeishuOAuthRefreshAttemptsIsScopedSafeAndBatched(t *testing.T) {
	st := openFeishuDocsTestStore(t)
	now := time.Date(2026, time.August, 4, 13, 0, 0, 0, time.UTC)
	cutoff := now.Add(-30 * 24 * time.Hour)
	for _, attempt := range []FeishuOAuthRefreshAttempt{
		{ID: "refresh_old_completed", AccountID: "feishu:cli_test", State: FeishuOAuthRefreshAttemptStateCompleted, UpdatedAt: cutoff.Add(-3 * time.Hour)},
		{ID: "refresh_old_ambiguous", AccountID: "feishu:cli_test", State: FeishuOAuthRefreshAttemptStateAmbiguous, UpdatedAt: cutoff.Add(-2 * time.Hour)},
		{ID: "refresh_old_failed", AccountID: "feishu:cli_test", State: FeishuOAuthRefreshAttemptStateFailed, UpdatedAt: cutoff.Add(-time.Hour)},
		{ID: "refresh_new_terminal", AccountID: "feishu:cli_test", State: FeishuOAuthRefreshAttemptStateCompleted, UpdatedAt: cutoff},
		{ID: "refresh_old_prepared", AccountID: "feishu:cli_test", State: FeishuOAuthRefreshAttemptStatePrepared, LeaseToken: "lease", UpdatedAt: cutoff.Add(-time.Hour)},
		{ID: "refresh_old_staged", AccountID: "feishu:cli_test", State: FeishuOAuthRefreshAttemptStateResponseStaged, AccessTokenCiphertext: "v1.access", RefreshTokenCiphertext: "v1.refresh", UpdatedAt: cutoff.Add(-time.Hour)},
		{ID: "refresh_old_unsafe_access", AccountID: "feishu:cli_test", State: FeishuOAuthRefreshAttemptStateFailed, AccessTokenCiphertext: "v1.access", UpdatedAt: cutoff.Add(-time.Hour)},
		{ID: "refresh_old_unsafe_refresh", AccountID: "feishu:cli_test", State: FeishuOAuthRefreshAttemptStateCompleted, RefreshTokenCiphertext: "v1.refresh", UpdatedAt: cutoff.Add(-time.Hour)},
		{ID: "refresh_other_account", AccountID: "feishu:cli_other", State: FeishuOAuthRefreshAttemptStateCompleted, UpdatedAt: cutoff.Add(-time.Hour)},
	} {
		insertFeishuOAuthRefreshAttemptForCleanupTest(t, st, attempt, now.Add(-60*24*time.Hour))
	}

	unsafe, err := st.CountUnsafeTerminalFeishuOAuthRefreshAttempts("feishu:cli_test", cutoff)
	if err != nil || unsafe != 2 {
		t.Fatalf("unsafe terminal attempts = %d err=%v, want 2", unsafe, err)
	}
	deleted, err := st.DeleteTerminalFeishuOAuthRefreshAttempts("feishu:cli_test", cutoff, 2)
	if err != nil || deleted != 2 {
		t.Fatalf("first cleanup deleted=%d err=%v, want 2", deleted, err)
	}
	deleted, err = st.DeleteTerminalFeishuOAuthRefreshAttempts("feishu:cli_test", cutoff, 2)
	if err != nil || deleted != 1 {
		t.Fatalf("second cleanup deleted=%d err=%v, want 1", deleted, err)
	}
	deleted, err = st.DeleteTerminalFeishuOAuthRefreshAttempts("feishu:cli_test", cutoff, 2)
	if err != nil || deleted != 0 {
		t.Fatalf("third cleanup deleted=%d err=%v, want 0", deleted, err)
	}

	for _, id := range []string{
		"refresh_new_terminal", "refresh_old_prepared", "refresh_old_staged",
		"refresh_old_unsafe_access", "refresh_old_unsafe_refresh",
	} {
		if _, err := st.GetFeishuOAuthRefreshAttempt(id, "feishu:cli_test"); err != nil {
			t.Fatalf("retained attempt %s: %v", id, err)
		}
	}
	if _, err := st.GetFeishuOAuthRefreshAttempt("refresh_other_account", "feishu:cli_other"); err != nil {
		t.Fatalf("other-account attempt was removed: %v", err)
	}
	for _, id := range []string{"refresh_old_completed", "refresh_old_ambiguous", "refresh_old_failed"} {
		if _, err := st.GetFeishuOAuthRefreshAttempt(id, "feishu:cli_test"); !errors.Is(err, ErrFeishuOAuthRefreshAttemptNotFound) {
			t.Fatalf("deleted attempt %s error = %v, want not found", id, err)
		}
	}
}

func TestDeleteTerminalFeishuOAuthRefreshAttemptsRejectsInvalidArguments(t *testing.T) {
	st := openFeishuDocsTestStore(t)
	cutoff := time.Date(2026, time.August, 4, 13, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		accountID string
		cutoff    time.Time
		limit     int
	}{
		{cutoff: cutoff, limit: 1},
		{accountID: "feishu:cli_test", limit: 1},
		{accountID: "feishu:cli_test", cutoff: cutoff},
	} {
		if _, err := st.DeleteTerminalFeishuOAuthRefreshAttempts(tc.accountID, tc.cutoff, tc.limit); err == nil {
			t.Fatalf("DeleteTerminalFeishuOAuthRefreshAttempts(%q, %s, %d) returned nil error", tc.accountID, tc.cutoff, tc.limit)
		}
	}
}

func TestDeleteTerminalFeishuOAuthRefreshAttemptsConcurrentAcrossStores(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	primary, err := Open(PlatformFeishu)
	if err != nil {
		t.Fatalf("open primary store: %v", err)
	}
	t.Cleanup(func() {
		if err := primary.Close(); err != nil {
			t.Errorf("close primary store: %v", err)
		}
	})
	secondary, err := Open(PlatformFeishu)
	if err != nil {
		t.Fatalf("open secondary store: %v", err)
	}
	t.Cleanup(func() {
		if err := secondary.Close(); err != nil {
			t.Errorf("close secondary store: %v", err)
		}
	})

	now := time.Date(2026, time.August, 4, 14, 0, 0, 0, time.UTC)
	cutoff := now.Add(-30 * 24 * time.Hour)
	const attempts = 50
	for i := 0; i < attempts; i++ {
		insertFeishuOAuthRefreshAttemptForCleanupTest(t, primary, FeishuOAuthRefreshAttempt{
			ID:        fmt.Sprintf("refresh_concurrent_%02d", i),
			AccountID: "feishu:cli_test",
			State:     FeishuOAuthRefreshAttemptStateCompleted,
			UpdatedAt: cutoff.Add(-time.Duration(i+1) * time.Minute),
		}, now.Add(-60*24*time.Hour))
	}

	start := make(chan struct{})
	results := make(chan int64, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, st := range []*Store{primary, secondary} {
		wg.Add(1)
		go func(st *Store) {
			defer wg.Done()
			<-start
			deleted, err := st.DeleteTerminalFeishuOAuthRefreshAttempts("feishu:cli_test", cutoff, attempts)
			results <- deleted
			errs <- err
		}(st)
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent cleanup returned error: %v", err)
		}
	}
	var deleted int64
	for count := range results {
		deleted += count
	}
	if deleted != attempts {
		t.Fatalf("concurrent cleanup deleted=%d, want %d", deleted, attempts)
	}
	remaining, err := primary.DeleteTerminalFeishuOAuthRefreshAttempts("feishu:cli_test", cutoff, attempts)
	if err != nil || remaining != 0 {
		t.Fatalf("remaining cleanup deleted=%d err=%v, want 0", remaining, err)
	}
}

func insertFeishuOAuthRefreshAttemptForCleanupTest(t *testing.T, st *Store, attempt FeishuOAuthRefreshAttempt, createdAt time.Time) {
	t.Helper()
	if attempt.UpdatedAt.IsZero() {
		attempt.UpdatedAt = createdAt
	}
	_, err := st.db.Exec(
		`INSERT INTO feishu_oauth_refresh_attempts (
		 attempt_id, credential_id, account_id, actor_open_id, actor_user_id,
		 expected_version, state, lease_token, lease_expires_at_ms,
		 access_token_ciphertext, access_token_expires_at_ms,
		 refresh_token_ciphertext, refresh_token_expires_at_ms,
		 scopes, error_category, created_at_ms, updated_at_ms
		) VALUES (?, ?, ?, '', '', 1, ?, ?, 0, ?, 0, ?, 0, '', '', ?, ?)`,
		attempt.ID,
		"credential_"+attempt.ID,
		attempt.AccountID,
		attempt.State,
		attempt.LeaseToken,
		attempt.AccessTokenCiphertext,
		attempt.RefreshTokenCiphertext,
		createdAt.UnixMilli(),
		attempt.UpdatedAt.UnixMilli(),
	)
	if err != nil {
		t.Fatalf("insert cleanup attempt %s: %v", attempt.ID, err)
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
