package monitor

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestBackgroundTaskGroupCloseAndWaitWaitsForInFlightTask(t *testing.T) {
	var group backgroundTaskGroup
	started := make(chan struct{})
	release := make(chan struct{})
	if ok := group.Go(func() {
		close(started)
		<-release
	}); !ok {
		t.Fatal("Go rejected a task before the group was closed")
	}
	<-started

	closed := make(chan struct{})
	go func() {
		group.CloseAndWait()
		close(closed)
	}()

	select {
	case <-closed:
		t.Fatal("CloseAndWait returned before the in-flight task completed")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("CloseAndWait did not return after the in-flight task completed")
	}
}

func TestBackgroundTaskGroupRejectsTasksAfterClose(t *testing.T) {
	var group backgroundTaskGroup
	group.CloseAndWait()
	var called atomic.Bool
	if ok := group.Go(func() { called.Store(true) }); ok {
		t.Fatal("Go accepted a task after the group was closed")
	}
	if called.Load() {
		t.Fatal("rejected task was executed")
	}
}
