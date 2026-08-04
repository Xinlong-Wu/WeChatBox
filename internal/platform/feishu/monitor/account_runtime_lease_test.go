package monitor

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"lingobridge/internal/core"
	"lingobridge/internal/store"
)

func TestFeishuAccountRuntimeLeaseHeartbeatRenewsUntilStopped(t *testing.T) {
	st := openFeishuApprovalTestStore(t)
	runtimeLease, err := acquireFeishuAccountRuntimeLease(st, "feishu:cli_test", feishuAccountRuntimeLeaseOptions{
		OwnerID:           "runtime_heartbeat",
		TTL:               200 * time.Millisecond,
		HeartbeatInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("acquireFeishuAccountRuntimeLease returned error: %v", err)
	}
	t.Cleanup(func() {
		if err := runtimeLease.release(); err != nil && !errors.Is(err, store.ErrFeishuAccountRuntimeLeaseLost) {
			t.Errorf("release runtime lease: %v", err)
		}
	})
	initialHeartbeat := runtimeLease.lease.HeartbeatAt
	heartbeat := runtimeLease.startHeartbeat(nil)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		current, loadErr := st.GetFeishuAccountRuntimeLease("feishu:cli_test")
		if loadErr == nil && current.HeartbeatAt.After(initialHeartbeat) {
			if err := heartbeat.stopAndWait(); err != nil {
				t.Fatalf("heartbeat stop returned error: %v", err)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := heartbeat.stopAndWait(); err != nil {
		t.Fatalf("heartbeat stop returned error: %v", err)
	}
	t.Fatal("runtime lease heartbeat was not renewed")
}

func TestAcquireFeishuAccountRuntimeLeaseRejectsLostOrExpiredResult(t *testing.T) {
	now := time.Date(2026, time.August, 4, 9, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name  string
		lease store.FeishuAccountRuntimeLease
	}{
		{
			name: "ownership changed before return",
			lease: store.FeishuAccountRuntimeLease{
				AccountID:      "feishu:cli_test",
				OwnerID:        "runtime_replacement",
				AcquiredAt:     now,
				HeartbeatAt:    now,
				LeaseExpiresAt: now.Add(time.Minute),
			},
		},
		{
			name: "lease expired before return",
			lease: store.FeishuAccountRuntimeLease{
				AccountID:      "feishu:cli_test",
				OwnerID:        "runtime_requested",
				AcquiredAt:     now.Add(-time.Minute),
				HeartbeatAt:    now.Add(-time.Minute),
				LeaseExpiresAt: now,
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st := &stubFeishuAccountRuntimeLeaseStore{acquireLease: tt.lease}
			lease, err := acquireFeishuAccountRuntimeLease(st, "feishu:cli_test", feishuAccountRuntimeLeaseOptions{
				OwnerID:           "runtime_requested",
				TTL:               30 * time.Second,
				HeartbeatInterval: 10 * time.Second,
				Now:               func() time.Time { return now },
			})
			if !errors.Is(err, store.ErrFeishuAccountRuntimeLeaseLost) {
				t.Fatalf("acquireFeishuAccountRuntimeLease lease=%#v err=%v, want ErrFeishuAccountRuntimeLeaseLost", lease, err)
			}
		})
	}
}

func TestFeishuAccountRuntimeLeaseLossCancelsRuntime(t *testing.T) {
	first, second := openSharedFeishuApprovalTestStores(t)
	runtimeLease, err := acquireFeishuAccountRuntimeLease(first, "feishu:cli_test", feishuAccountRuntimeLeaseOptions{
		OwnerID:           "runtime_old",
		TTL:               100 * time.Millisecond,
		HeartbeatInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("acquire old runtime lease: %v", err)
	}
	current := time.Now().UTC()
	if _, err := second.AcquireFeishuAccountRuntimeLease("feishu:cli_test", "runtime_new", current.Add(time.Second), time.Second); err != nil {
		t.Fatalf("take over old runtime lease: %v", err)
	}
	t.Cleanup(func() {
		if err := second.ReleaseFeishuAccountRuntimeLease("feishu:cli_test", "runtime_new"); err != nil {
			t.Errorf("release replacement runtime lease: %v", err)
		}
	})

	runtimeCtx, cancelRuntime := context.WithCancel(context.Background())
	defer cancelRuntime()
	ownershipCtx, cancelOwnership := context.WithCancel(context.Background())
	defer cancelOwnership()
	heartbeat := runtimeLease.startHeartbeat(func() {
		cancelOwnership()
		cancelRuntime()
	})
	select {
	case <-runtimeCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("runtime context was not canceled after lease loss")
	}
	select {
	case <-ownershipCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("runtime ownership context was not canceled after lease loss")
	}
	if err := heartbeat.stopAndWait(); !errors.Is(err, store.ErrFeishuAccountRuntimeLeaseLost) {
		t.Fatalf("heartbeat error = %v, want ErrFeishuAccountRuntimeLeaseLost", err)
	}
	if err := runtimeLease.release(); !errors.Is(err, store.ErrFeishuAccountRuntimeLeaseLost) {
		t.Fatalf("stale runtime release error = %v, want ErrFeishuAccountRuntimeLeaseLost", err)
	}
}

func TestFeishuAccountRuntimeLeaseHeartbeatRetriesTransientRenewalError(t *testing.T) {
	now := time.Now().UTC()
	transientErr := errors.New("temporary sqlite busy")
	st := &transientRenewFeishuAccountRuntimeLeaseStore{
		firstErr: transientErr,
		renewed: store.FeishuAccountRuntimeLease{
			AccountID:      "feishu:cli_test",
			OwnerID:        "runtime_retry",
			AcquiredAt:     now,
			HeartbeatAt:    now.Add(20 * time.Millisecond),
			LeaseExpiresAt: now.Add(time.Second),
		},
		secondRenewed: make(chan struct{}),
	}
	runtimeLease := &feishuAccountRuntimeLease{
		store:             st,
		accountID:         "feishu:cli_test",
		ownerID:           "runtime_retry",
		ttl:               time.Second,
		heartbeatInterval: 10 * time.Millisecond,
		now:               time.Now,
		lease: store.FeishuAccountRuntimeLease{
			AccountID:      "feishu:cli_test",
			OwnerID:        "runtime_retry",
			AcquiredAt:     now,
			HeartbeatAt:    now,
			LeaseExpiresAt: now.Add(time.Second),
		},
	}
	runtimeCtx, cancelRuntime := context.WithCancel(context.Background())
	defer cancelRuntime()
	heartbeat := runtimeLease.startHeartbeat(cancelRuntime)

	select {
	case <-st.secondRenewed:
	case <-runtimeCtx.Done():
		t.Fatalf("runtime canceled after transient renewal error: %v", heartbeat.stopAndWait())
	case <-time.After(time.Second):
		t.Fatal("heartbeat did not retry the transient renewal error")
	}
	select {
	case <-runtimeCtx.Done():
		t.Fatal("runtime was canceled even though the lease renewal recovered")
	default:
	}
	if err := heartbeat.stopAndWait(); err != nil {
		t.Fatalf("heartbeat stop returned error after recovered renewal: %v", err)
	}
}

func TestFeishuAccountRuntimeShutdownKeepsLeaseWhileTasksDrain(t *testing.T) {
	activeStore, competingStore := openSharedFeishuApprovalTestStores(t)
	runtimeLease, err := acquireFeishuAccountRuntimeLease(activeStore, "feishu:cli_test", feishuAccountRuntimeLeaseOptions{
		OwnerID:           "runtime_draining",
		TTL:               200 * time.Millisecond,
		HeartbeatInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("acquire draining runtime lease: %v", err)
	}
	heartbeat := runtimeLease.startHeartbeat(nil)
	approvals := &operationApprovalService{}
	taskStarted := make(chan struct{})
	releaseTask := make(chan struct{})
	if ok := approvals.tasks.Go(func() {
		close(taskStarted)
		<-releaseTask
	}); !ok {
		t.Fatal("approval task was rejected before shutdown")
	}
	<-taskStarted

	runtimeCtx, cancelRuntime := context.WithCancel(context.Background())
	type shutdownResult struct {
		heartbeatErr error
		releaseErr   error
	}
	shutdownDone := make(chan shutdownResult, 1)
	go func() {
		heartbeatErr, releaseErr := shutdownFeishuAccountRuntime(
			cancelRuntime,
			nil,
			nil,
			nil,
			approvals,
			nil,
			heartbeat,
			runtimeLease,
		)
		shutdownDone <- shutdownResult{heartbeatErr: heartbeatErr, releaseErr: releaseErr}
	}()
	select {
	case <-runtimeCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("runtime lifecycle was not canceled at shutdown start")
	}

	time.Sleep(250 * time.Millisecond)
	if _, err := competingStore.AcquireFeishuAccountRuntimeLease(
		"feishu:cli_test",
		"runtime_competing",
		time.Now().UTC(),
		time.Second,
	); !errors.Is(err, store.ErrFeishuAccountRuntimeLeaseHeld) {
		t.Fatalf("competing acquire during task drain error = %v, want ErrFeishuAccountRuntimeLeaseHeld", err)
	}
	select {
	case result := <-shutdownDone:
		t.Fatalf("shutdown completed before admitted task drained: %#v", result)
	default:
	}

	close(releaseTask)
	select {
	case result := <-shutdownDone:
		if result.heartbeatErr != nil || result.releaseErr != nil {
			t.Fatalf("shutdown result = %#v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown did not finish after admitted task drained")
	}
	if _, err := activeStore.GetFeishuAccountRuntimeLease("feishu:cli_test"); !errors.Is(err, store.ErrFeishuAccountRuntimeLeaseNotFound) {
		t.Fatalf("lease after drained shutdown error = %v, want ErrFeishuAccountRuntimeLeaseNotFound", err)
	}
}

func TestFeishuAccountRuntimeShutdownClosesCallbackAdmissionBeforeCancel(t *testing.T) {
	runtimeCtx, cancelRuntime := context.WithCancel(context.Background())
	continuationDone := make(chan struct{})
	resourceAccess := &resourceAccessManager{}
	type shutdownResult struct {
		heartbeatErr error
		releaseErr   error
	}
	shutdownDone := make(chan shutdownResult, 1)
	go func() {
		heartbeatErr, releaseErr := shutdownFeishuAccountRuntime(
			cancelRuntime,
			continuationDone,
			nil,
			nil,
			nil,
			resourceAccess,
			nil,
			nil,
		)
		shutdownDone <- shutdownResult{heartbeatErr: heartbeatErr, releaseErr: releaseErr}
	}()

	select {
	case <-runtimeCtx.Done():
	case <-time.After(time.Second):
		close(continuationDone)
		t.Fatal("runtime lifecycle was not canceled at shutdown start")
	}

	releaseTask, accepted := resourceAccess.tasks.Reserve()
	if accepted {
		releaseTask()
	}
	close(continuationDone)
	select {
	case result := <-shutdownDone:
		if result.heartbeatErr != nil || result.releaseErr != nil {
			t.Fatalf("shutdown result = %#v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown did not finish after continuation worker stopped")
	}
	if accepted {
		t.Fatal("resource callback admission remained open after shutdown began")
	}
}

func TestFeishuAccountRuntimeShutdownWaitsForInFlightMessage(t *testing.T) {
	runtimeCtx, cancelRuntime := context.WithCancel(context.Background())
	processor := &uncancelableTestProcessor{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	b := &bot{
		handler: processor,
		sender:  &fakeSender{},
		runCtx:  runtimeCtx,
	}
	if err := b.handleMessage(t.Context(), feishuEvent("p2p", "text", `{"text":"hi"}`, nil)); err != nil {
		t.Fatalf("handleMessage returned error: %v", err)
	}
	select {
	case <-processor.started:
	case <-time.After(time.Second):
		t.Fatal("message processor did not start")
	}

	shutdownDone := make(chan struct{})
	go func() {
		_, _ = shutdownFeishuAccountRuntime(
			cancelRuntime,
			nil,
			nil,
			b,
			nil,
			nil,
			nil,
			nil,
		)
		close(shutdownDone)
	}()
	select {
	case <-shutdownDone:
		close(processor.release)
		t.Fatal("runtime shutdown completed while an admitted message was still processing")
	case <-time.After(50 * time.Millisecond):
	}
	close(processor.release)
	select {
	case <-shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("runtime shutdown did not finish after the message processor returned")
	}
}

type uncancelableTestProcessor struct {
	started chan struct{}
	release chan struct{}
}

func (p *uncancelableTestProcessor) Handle(context.Context, core.InboundMessage, core.Sender) error {
	close(p.started)
	<-p.release
	return nil
}

type stubFeishuAccountRuntimeLeaseStore struct {
	acquireLease store.FeishuAccountRuntimeLease
	acquireErr   error
}

type transientRenewFeishuAccountRuntimeLeaseStore struct {
	mu            sync.Mutex
	firstErr      error
	renewed       store.FeishuAccountRuntimeLease
	secondRenewed chan struct{}
	calls         int
}

func (s *transientRenewFeishuAccountRuntimeLeaseStore) AcquireFeishuAccountRuntimeLease(string, string, time.Time, time.Duration) (store.FeishuAccountRuntimeLease, error) {
	panic("unexpected AcquireFeishuAccountRuntimeLease call")
}

func (s *transientRenewFeishuAccountRuntimeLeaseStore) RenewFeishuAccountRuntimeLease(string, string, time.Time, time.Duration) (store.FeishuAccountRuntimeLease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.calls == 1 {
		return store.FeishuAccountRuntimeLease{}, s.firstErr
	}
	if s.calls == 2 {
		close(s.secondRenewed)
	}
	return s.renewed, nil
}

func (s *transientRenewFeishuAccountRuntimeLeaseStore) ReleaseFeishuAccountRuntimeLease(string, string) error {
	panic("unexpected ReleaseFeishuAccountRuntimeLease call")
}

func (s *stubFeishuAccountRuntimeLeaseStore) AcquireFeishuAccountRuntimeLease(string, string, time.Time, time.Duration) (store.FeishuAccountRuntimeLease, error) {
	return s.acquireLease, s.acquireErr
}

func (s *stubFeishuAccountRuntimeLeaseStore) RenewFeishuAccountRuntimeLease(string, string, time.Time, time.Duration) (store.FeishuAccountRuntimeLease, error) {
	panic("unexpected RenewFeishuAccountRuntimeLease call")
}

func (s *stubFeishuAccountRuntimeLeaseStore) ReleaseFeishuAccountRuntimeLease(string, string) error {
	panic("unexpected ReleaseFeishuAccountRuntimeLease call")
}
