package monitor

import (
	"context"
	"errors"
	"testing"
	"time"
)

type delayedOwnershipAfterFuncContext struct {
	context.Context
	done     chan struct{}
	err      error
	callback func()
}

func newDelayedOwnershipAfterFuncContext() *delayedOwnershipAfterFuncContext {
	return &delayedOwnershipAfterFuncContext{Context: context.Background(), done: make(chan struct{})}
}

func (c *delayedOwnershipAfterFuncContext) Done() <-chan struct{} { return c.done }

func (c *delayedOwnershipAfterFuncContext) Err() error { return c.err }

func (c *delayedOwnershipAfterFuncContext) AfterFunc(callback func()) func() bool {
	c.callback = callback
	return func() bool {
		if c.callback == nil {
			return false
		}
		c.callback = nil
		return true
	}
}

func (c *delayedOwnershipAfterFuncContext) cancelWithoutRunningCallback() {
	c.err = context.Canceled
	close(c.done)
}

func TestFeishuRuntimeDrainContextIsImmediatelyCanceledAfterOwnershipLoss(t *testing.T) {
	ownershipCtx, cancelOwnership := context.WithCancel(context.Background())
	cancelOwnership()
	runtimeCtx := withFeishuRuntimeOwnership(context.Background(), ownershipCtx)

	drainCtx, cancelDrain := feishuRuntimeDrainContext(runtimeCtx)
	defer cancelDrain()
	if drainCtx.Err() == nil {
		t.Fatal("drain context remained usable after account lease ownership was already lost")
	}
}

func TestFeishuRuntimeDrainContextIgnoresLifecycleCancellationWhileOwnershipIsActive(t *testing.T) {
	lifecycleCtx, cancelLifecycle := context.WithCancel(context.Background())
	ownershipCtx, cancelOwnership := context.WithCancel(context.Background())
	defer cancelOwnership()
	runtimeCtx := withFeishuRuntimeOwnership(lifecycleCtx, ownershipCtx)
	cancelLifecycle()

	drainCtx, cancelDrain := feishuRuntimeDrainContext(runtimeCtx)
	defer cancelDrain()
	if err := drainCtx.Err(); err != nil {
		t.Fatalf("drain context inherited orderly lifecycle cancellation: %v", err)
	}
}

func TestFeishuRuntimeDrainContextKeepsSynchronousOwnershipForTimeoutChildren(t *testing.T) {
	ownershipCtx, cancelOwnership := context.WithCancel(context.Background())
	runtimeCtx := withFeishuRuntimeOwnership(context.Background(), ownershipCtx)
	drainCtx, cancelDrain := feishuRuntimeDrainContext(runtimeCtx)
	defer cancelDrain()
	boundedCtx, cancelBounded := context.WithTimeout(drainCtx, time.Hour)
	defer cancelBounded()

	cancelOwnership()
	if err := boundedCtx.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("bounded drain context error after ownership cancellation = %v, want synchronous context.Canceled", err)
	}
}

func TestFeishuRuntimeDrainContextObservesOwnershipCancellationSynchronously(t *testing.T) {
	ownershipCtx := newDelayedOwnershipAfterFuncContext()
	runtimeCtx := withFeishuRuntimeOwnership(context.Background(), ownershipCtx)

	drainCtx, cancelDrain := feishuRuntimeDrainContext(runtimeCtx)
	defer cancelDrain()
	ownershipCtx.cancelWithoutRunningCallback()

	if err := drainCtx.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("drain context error after ownership cancellation = %v, want context.Canceled without waiting for an asynchronous callback", err)
	}
}
