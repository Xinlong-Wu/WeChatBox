package idempotency

import "context"

type retryContextKey struct{}

// WithRetryContext records the runtime-owned context that must remain valid
// before an independent idempotent reconciliation request may be sent.
func WithRetryContext(ctx, retryCtx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if retryCtx == nil {
		retryCtx = context.Background()
	}
	return context.WithValue(ctx, retryContextKey{}, retryCtx)
}

// RetryContext returns the runtime-owned retry boundary when one is present.
// Standalone callers retain the existing behavior of detaching a reconciliation
// request from the immediate operation deadline.
func RetryContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	if retryCtx, ok := ctx.Value(retryContextKey{}).(context.Context); ok && retryCtx != nil {
		return retryCtx
	}
	return context.WithoutCancel(ctx)
}
