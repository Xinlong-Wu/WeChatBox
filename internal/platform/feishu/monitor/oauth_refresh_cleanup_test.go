package monitor

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type trackingOAuthRefreshCleanupStore struct {
	resourceAccessStore

	mu            sync.Mutex
	deleteResults []int64
	deleteErr     error
	unsafeCount   int64
	unsafeErr     error
	deleteCalls   int
	unsafeCalls   int
	cutoffs       []time.Time
	limits        []int
	called        chan struct{}
}

func (s *trackingOAuthRefreshCleanupStore) DeleteTerminalFeishuOAuthRefreshAttempts(accountID string, completedBefore time.Time, limit int) (int64, error) {
	s.mu.Lock()
	s.deleteCalls++
	s.cutoffs = append(s.cutoffs, completedBefore)
	s.limits = append(s.limits, limit)
	result := int64(0)
	if len(s.deleteResults) > 0 {
		result = s.deleteResults[0]
		s.deleteResults = s.deleteResults[1:]
	}
	err := s.deleteErr
	called := s.called
	s.mu.Unlock()
	if called != nil {
		select {
		case called <- struct{}{}:
		default:
		}
	}
	return result, err
}

func (s *trackingOAuthRefreshCleanupStore) CountUnsafeTerminalFeishuOAuthRefreshAttempts(accountID string, completedBefore time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unsafeCalls++
	return s.unsafeCount, s.unsafeErr
}

func (s *trackingOAuthRefreshCleanupStore) snapshot() (deleteCalls, unsafeCalls int, cutoffs []time.Time, limits []int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deleteCalls, s.unsafeCalls, append([]time.Time(nil), s.cutoffs...), append([]int(nil), s.limits...)
}

func TestOAuthRefreshAttemptCleanupRunsBoundedBatchesAndChecksUnsafeRows(t *testing.T) {
	manager, _, _ := newTestResourceAccessManager(t, emptyFeishuServer(t), resourceAccessOAuthConfig{})
	tracking := &trackingOAuthRefreshCleanupStore{
		resourceAccessStore: manager.store,
		deleteResults:       []int64{2, 1},
		unsafeCount:         3,
	}
	manager.store = tracking
	manager.refreshAttemptRetention = 30 * 24 * time.Hour
	manager.refreshAttemptCleanupBatchSize = 2

	deleted, err := manager.cleanupTerminalFeishuOAuthRefreshAttempts(t.Context())
	if err != nil || deleted != 3 {
		t.Fatalf("cleanup deleted=%d err=%v, want 3", deleted, err)
	}
	deleteCalls, unsafeCalls, cutoffs, limits := tracking.snapshot()
	if deleteCalls != 2 || unsafeCalls != 1 {
		t.Fatalf("cleanup calls delete=%d unsafe=%d, want 2/1", deleteCalls, unsafeCalls)
	}
	wantCutoff := manager.currentTime().Add(-30 * 24 * time.Hour)
	for i, cutoff := range cutoffs {
		if !cutoff.Equal(wantCutoff) || limits[i] != 2 {
			t.Fatalf("cleanup call %d cutoff=%s limit=%d, want %s/2", i, cutoff, limits[i], wantCutoff)
		}
	}
}

func TestRecoverPersistedRequestsDoesNotFailWhenRefreshCleanupFails(t *testing.T) {
	manager, _, _ := newTestResourceAccessManager(t, emptyFeishuServer(t), resourceAccessOAuthConfig{})
	tracking := &trackingOAuthRefreshCleanupStore{
		resourceAccessStore: manager.store,
		deleteErr:           errors.New("cleanup unavailable"),
	}
	manager.store = tracking
	if err := manager.recoverPersistedRequests(t.Context()); err != nil {
		t.Fatalf("recoverPersistedRequests returned cleanup error: %v", err)
	}
	deleteCalls, _, _, _ := tracking.snapshot()
	if deleteCalls != 1 {
		t.Fatalf("startup cleanup calls = %d, want 1", deleteCalls)
	}
}

func TestOAuthRefreshAttemptCleanupWorkerStopsWithRuntimeContext(t *testing.T) {
	manager, _, _ := newTestResourceAccessManager(t, emptyFeishuServer(t), resourceAccessOAuthConfig{})
	runCtx, cancel := context.WithCancel(context.Background())
	manager.runCtx = runCtx
	tracking := &trackingOAuthRefreshCleanupStore{
		resourceAccessStore: manager.store,
		called:              make(chan struct{}, 8),
	}
	manager.store = tracking
	manager.refreshAttemptCleanupInterval = 5 * time.Millisecond
	if err := manager.startFeishuOAuthRefreshAttemptCleanup(); err != nil {
		t.Fatalf("start cleanup worker: %v", err)
	}
	select {
	case <-tracking.called:
	case <-time.After(time.Second):
		t.Fatal("cleanup worker did not run")
	}
	cancel()
	manager.tasks.CloseAndWait()
	before, _, _, _ := tracking.snapshot()
	time.Sleep(20 * time.Millisecond)
	after, _, _, _ := tracking.snapshot()
	if after != before {
		t.Fatalf("cleanup worker continued after shutdown: before=%d after=%d", before, after)
	}
}

func emptyFeishuServer(t *testing.T) *httptest.Server {
	t.Helper()
	return newTestHTTPServer(t, func(path string) {
		t.Fatalf("unexpected Feishu API call: %s", path)
	})
}

func newTestHTTPServer(t *testing.T, unexpected func(string)) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		unexpected(r.URL.Path)
	}))
	t.Cleanup(server.Close)
	return server
}
