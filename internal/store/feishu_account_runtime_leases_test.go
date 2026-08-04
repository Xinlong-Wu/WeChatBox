package store

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestFeishuAccountRuntimeLeaseRejectsLiveOwnerAndAllowsExpiredTakeover(t *testing.T) {
	first, second := openSharedFeishuRuntimeLeaseStores(t)
	now := time.Date(2026, time.August, 4, 10, 0, 0, 0, time.UTC)
	ttl := 30 * time.Second

	initial, err := first.AcquireFeishuAccountRuntimeLease("feishu:cli_test", "owner_a", now, ttl)
	if err != nil {
		t.Fatalf("initial AcquireFeishuAccountRuntimeLease returned error: %v", err)
	}
	if initial.OwnerID != "owner_a" || !initial.AcquiredAt.Equal(now) || !initial.LeaseExpiresAt.Equal(now.Add(ttl)) {
		t.Fatalf("initial lease = %#v", initial)
	}

	blocked, err := second.AcquireFeishuAccountRuntimeLease("feishu:cli_test", "owner_b", now.Add(10*time.Second), ttl)
	if !errors.Is(err, ErrFeishuAccountRuntimeLeaseHeld) || blocked.OwnerID != "owner_a" {
		t.Fatalf("live competing acquire lease = %#v err=%v, want owner_a held", blocked, err)
	}

	renewed, err := first.RenewFeishuAccountRuntimeLease("feishu:cli_test", "owner_a", now.Add(20*time.Second), ttl)
	if err != nil {
		t.Fatalf("RenewFeishuAccountRuntimeLease returned error: %v", err)
	}
	if !renewed.AcquiredAt.Equal(now) || !renewed.HeartbeatAt.Equal(now.Add(20*time.Second)) || !renewed.LeaseExpiresAt.Equal(now.Add(50*time.Second)) {
		t.Fatalf("renewed lease = %#v", renewed)
	}

	blocked, err = second.AcquireFeishuAccountRuntimeLease("feishu:cli_test", "owner_b", now.Add(40*time.Second), ttl)
	if !errors.Is(err, ErrFeishuAccountRuntimeLeaseHeld) || blocked.OwnerID != "owner_a" {
		t.Fatalf("acquire before renewed expiry lease = %#v err=%v, want owner_a held", blocked, err)
	}

	takenOver, err := second.AcquireFeishuAccountRuntimeLease("feishu:cli_test", "owner_b", now.Add(51*time.Second), ttl)
	if err != nil {
		t.Fatalf("expired takeover returned error: %v", err)
	}
	if takenOver.OwnerID != "owner_b" || !takenOver.AcquiredAt.Equal(now.Add(51*time.Second)) || !takenOver.LeaseExpiresAt.Equal(now.Add(81*time.Second)) {
		t.Fatalf("taken-over lease = %#v", takenOver)
	}

	if _, err := first.RenewFeishuAccountRuntimeLease("feishu:cli_test", "owner_a", now.Add(52*time.Second), ttl); !errors.Is(err, ErrFeishuAccountRuntimeLeaseLost) {
		t.Fatalf("stale owner renew error = %v, want ErrFeishuAccountRuntimeLeaseLost", err)
	}
	if err := first.ReleaseFeishuAccountRuntimeLease("feishu:cli_test", "owner_a"); !errors.Is(err, ErrFeishuAccountRuntimeLeaseLost) {
		t.Fatalf("stale owner release error = %v, want ErrFeishuAccountRuntimeLeaseLost", err)
	}
	current, err := first.GetFeishuAccountRuntimeLease("feishu:cli_test")
	if err != nil || current.OwnerID != "owner_b" {
		t.Fatalf("lease after stale release = %#v err=%v, want owner_b", current, err)
	}

	if err := second.ReleaseFeishuAccountRuntimeLease("feishu:cli_test", "owner_b"); err != nil {
		t.Fatalf("current owner release returned error: %v", err)
	}
	if _, err := first.GetFeishuAccountRuntimeLease("feishu:cli_test"); !errors.Is(err, ErrFeishuAccountRuntimeLeaseNotFound) {
		t.Fatalf("GetFeishuAccountRuntimeLease after release error = %v, want not found", err)
	}
}

func TestFeishuAccountRuntimeLeaseSameOwnerAcquireIsIdempotent(t *testing.T) {
	st, _ := openSharedFeishuRuntimeLeaseStores(t)
	now := time.Date(2026, time.August, 4, 10, 0, 0, 0, time.UTC)
	first, err := st.AcquireFeishuAccountRuntimeLease("feishu:cli_test", "owner_a", now, 30*time.Second)
	if err != nil {
		t.Fatalf("first acquire returned error: %v", err)
	}
	second, err := st.AcquireFeishuAccountRuntimeLease("feishu:cli_test", "owner_a", now.Add(5*time.Second), time.Minute)
	if err != nil {
		t.Fatalf("idempotent acquire returned error: %v", err)
	}
	if !second.AcquiredAt.Equal(first.AcquiredAt) || !second.HeartbeatAt.Equal(now.Add(5*time.Second)) || !second.LeaseExpiresAt.Equal(now.Add(65*time.Second)) {
		t.Fatalf("idempotently acquired lease = %#v, first = %#v", second, first)
	}
}

func TestFeishuAccountRuntimeLeaseRenewRejectsOwnerChangedBeforeReadback(t *testing.T) {
	st, _ := openSharedFeishuRuntimeLeaseStores(t)
	now := time.Date(2026, time.August, 4, 10, 30, 0, 0, time.UTC)
	if _, err := st.AcquireFeishuAccountRuntimeLease("feishu:cli_test", "owner_a", now, time.Minute); err != nil {
		t.Fatalf("acquire runtime lease: %v", err)
	}
	if _, err := st.db.Exec(`CREATE TEMP TRIGGER replace_feishu_runtime_lease_after_renew
		AFTER UPDATE OF heartbeat_at_ms ON feishu_account_runtime_leases
		WHEN NEW.account_id='feishu:cli_test' AND NEW.owner_id='owner_a'
		BEGIN
			UPDATE feishu_account_runtime_leases
			SET owner_id='owner_b', acquired_at_ms=NEW.heartbeat_at_ms,
			    heartbeat_at_ms=NEW.heartbeat_at_ms, lease_expires_at_ms=NEW.lease_expires_at_ms
			WHERE account_id=NEW.account_id;
		END`); err != nil {
		t.Fatalf("create takeover trigger: %v", err)
	}

	renewed, err := st.RenewFeishuAccountRuntimeLease(
		"feishu:cli_test",
		"owner_a",
		now.Add(10*time.Second),
		time.Minute,
	)
	if !errors.Is(err, ErrFeishuAccountRuntimeLeaseLost) {
		t.Fatalf("renewed lease = %#v err=%v, want ErrFeishuAccountRuntimeLeaseLost", renewed, err)
	}
	current, loadErr := st.GetFeishuAccountRuntimeLease("feishu:cli_test")
	if loadErr != nil || current.OwnerID != "owner_b" {
		t.Fatalf("replacement lease = %#v err=%v, want owner_b", current, loadErr)
	}
}

func TestFeishuAccountRuntimeLeaseConcurrentAcquireHasSingleOwner(t *testing.T) {
	first, second := openSharedFeishuRuntimeLeaseStores(t)
	now := time.Date(2026, time.August, 4, 10, 0, 0, 0, time.UTC)
	start := make(chan struct{})
	type outcome struct {
		owner string
		err   error
	}
	outcomes := make(chan outcome, 2)
	var wg sync.WaitGroup
	for _, candidate := range []struct {
		store *Store
		owner string
	}{
		{store: first, owner: "owner_a"},
		{store: second, owner: "owner_b"},
	} {
		wg.Add(1)
		go func(candidateStore *Store, owner string) {
			defer wg.Done()
			<-start
			_, err := candidateStore.AcquireFeishuAccountRuntimeLease("feishu:cli_test", owner, now, 30*time.Second)
			outcomes <- outcome{owner: owner, err: err}
		}(candidate.store, candidate.owner)
	}
	close(start)
	wg.Wait()
	close(outcomes)

	winners := 0
	held := 0
	for result := range outcomes {
		switch {
		case result.err == nil:
			winners++
		case errors.Is(result.err, ErrFeishuAccountRuntimeLeaseHeld):
			held++
		default:
			t.Fatalf("owner %s acquire error = %v", result.owner, result.err)
		}
	}
	if winners != 1 || held != 1 {
		t.Fatalf("concurrent acquire winners=%d held=%d, want 1/1", winners, held)
	}
}

func openSharedFeishuRuntimeLeaseStores(t *testing.T) (*Store, *Store) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	first, err := Open(PlatformFeishu)
	if err != nil {
		t.Fatalf("open first shared Feishu store: %v", err)
	}
	t.Cleanup(func() {
		if err := first.Close(); err != nil {
			t.Errorf("close first shared Feishu store: %v", err)
		}
	})
	second, err := Open(PlatformFeishu)
	if err != nil {
		t.Fatalf("open second shared Feishu store: %v", err)
	}
	t.Cleanup(func() {
		if err := second.Close(); err != nil {
			t.Errorf("close second shared Feishu store: %v", err)
		}
	})
	return first, second
}
