package core

import (
	"context"
	"testing"
	"time"
)

func TestSessionLaneSetSerializesSameKeyAndAllowsDifferentSessions(t *testing.T) {
	lanes := newSessionLaneSet()
	keyA := sessionLaneKey{Platform: "feishu", AccountID: "account", UserKey: "user", SessionID: "a"}
	keyB := sessionLaneKey{Platform: "feishu", AccountID: "account", UserKey: "user", SessionID: "b"}
	releaseA, _, err := lanes.acquire(context.Background(), keyA)
	if err != nil {
		t.Fatalf("acquire key A returned error: %v", err)
	}

	sameAcquired := make(chan func(), 1)
	go func() {
		release, _, acquireErr := lanes.acquire(context.Background(), keyA)
		if acquireErr == nil {
			sameAcquired <- release
		}
	}()
	select {
	case release := <-sameAcquired:
		release()
		releaseA()
		t.Fatal("same session lane was acquired concurrently")
	case <-time.After(50 * time.Millisecond):
	}

	releaseB, _, err := lanes.acquire(context.Background(), keyB)
	if err != nil {
		releaseA()
		t.Fatalf("acquire key B returned error: %v", err)
	}
	releaseB()
	releaseA()
	select {
	case release := <-sameAcquired:
		release()
	case <-time.After(time.Second):
		t.Fatal("waiting same-session lane did not acquire after release")
	}
}

func TestSessionLaneAcquireHonorsCancellation(t *testing.T) {
	lanes := newSessionLaneSet()
	key := sessionLaneKey{Platform: "feishu", AccountID: "account", UserKey: "user", SessionID: "session"}
	release, _, err := lanes.acquire(context.Background(), key)
	if err != nil {
		t.Fatalf("first acquire returned error: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := lanes.acquire(ctx, key); err != context.Canceled {
		release()
		t.Fatalf("canceled acquire error = %v, want context.Canceled", err)
	}
	release()
}
