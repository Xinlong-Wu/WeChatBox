package monitor

import (
	"context"
	"strings"
	"testing"
	"time"

	"lingobridge/internal/core"
)

func TestNewFeishuRuntimeExecutionOwnerIDIsUnique(t *testing.T) {
	first, err := newFeishuRuntimeExecutionOwnerID()
	if err != nil {
		t.Fatalf("newFeishuRuntimeExecutionOwnerID returned error: %v", err)
	}
	second, err := newFeishuRuntimeExecutionOwnerID()
	if err != nil {
		t.Fatalf("second newFeishuRuntimeExecutionOwnerID returned error: %v", err)
	}
	if !strings.HasPrefix(first, "runtime_") || !strings.HasPrefix(second, "runtime_") || first == second {
		t.Fatalf("runtime execution owners = %q, %q; want distinct runtime_ identifiers", first, second)
	}
}

func TestFeishuRuntimeShutdownWaitsForAdmittedTasks(t *testing.T) {
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
	shutdownDone := make(chan struct{})
	go func() {
		shutdownFeishuRuntime(cancelRuntime, nil, nil, nil, approvals, nil)
		close(shutdownDone)
	}()
	select {
	case <-runtimeCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("runtime lifecycle was not canceled at shutdown start")
	}
	select {
	case <-shutdownDone:
		close(releaseTask)
		t.Fatal("shutdown completed before admitted task drained")
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseTask)
	select {
	case <-shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not finish after admitted task drained")
	}
}

func TestFeishuRuntimeShutdownClosesCallbackAdmissionBeforeCancel(t *testing.T) {
	runtimeCtx, cancelRuntime := context.WithCancel(context.Background())
	continuationDone := make(chan struct{})
	resourceAccess := &resourceAccessManager{}
	shutdownDone := make(chan struct{})
	go func() {
		shutdownFeishuRuntime(cancelRuntime, continuationDone, nil, nil, nil, resourceAccess)
		close(shutdownDone)
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
	case <-shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not finish after continuation worker stopped")
	}
	if accepted {
		t.Fatal("resource callback admission remained open after shutdown began")
	}
}

func TestFeishuRuntimeShutdownWaitsForInFlightMessage(t *testing.T) {
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
		shutdownFeishuRuntime(cancelRuntime, nil, nil, b, nil, nil)
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
