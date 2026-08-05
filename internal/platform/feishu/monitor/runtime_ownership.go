package monitor

import (
	"context"
	"time"

	feishuidempotency "lingobridge/internal/platform/feishu/idempotency"
)

type feishuRuntimeOwnershipContextKey struct{}

// withFeishuRuntimeOwnership records the side-effect lifetime in the runtime
// context. Lifecycle cancellation may be ignored while admitted work drains;
// explicit runtime ownership cancellation must never be ignored.
func withFeishuRuntimeOwnership(ctx, ownership context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if ownership == nil {
		ownership = context.Background()
	}
	ctx = feishuidempotency.WithRetryContext(ctx, ownership)
	return context.WithValue(ctx, feishuRuntimeOwnershipContextKey{}, ownership)
}

func feishuRuntimeOwnershipContext(ctx context.Context) context.Context {
	if ctx != nil {
		if ownership, ok := ctx.Value(feishuRuntimeOwnershipContextKey{}).(context.Context); ok && ownership != nil {
			return ownership
		}
	}
	return context.Background()
}

func feishuRuntimeOwnershipLost(ctx context.Context) bool {
	return feishuRuntimeOwnershipContext(ctx).Err() != nil
}

type feishuRuntimeDrainValueContext struct {
	values    context.Context
	ownership context.Context
}

func (c feishuRuntimeDrainValueContext) Deadline() (time.Time, bool) {
	return c.ownership.Deadline()
}

func (c feishuRuntimeDrainValueContext) Done() <-chan struct{} {
	return c.ownership.Done()
}

func (c feishuRuntimeDrainValueContext) Err() error {
	return c.ownership.Err()
}

func (c feishuRuntimeDrainValueContext) Value(key any) any {
	if value := c.values.Value(key); value != nil {
		return value
	}
	// Preserve the underlying cancelCtx identity used internally by context so
	// WithTimeout/WithCancel children are registered directly and ownership
	// cancellation remains synchronous instead of falling back to a watcher
	// goroutine.
	return c.ownership.Value(key)
}

// feishuRuntimeDrainContext detaches from ordinary lifecycle cancellation so
// already-admitted work can drain while retaining explicit runtime ownership
// cancellation.
func feishuRuntimeDrainContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	ownership := feishuRuntimeOwnershipContext(ctx)
	// Return ownership.Done directly rather than forwarding cancellation through
	// context.AfterFunc. AfterFunc callbacks run asynchronously, leaving a small
	// interval in which a caller could start a remote side effect after runtime
	// ownership had already been canceled.
	return feishuRuntimeDrainValueContext{
		values:    context.WithoutCancel(ctx),
		ownership: ownership,
	}, func() {}
}
