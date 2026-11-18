package affinity

import "context"

type observerKey struct{}

// WithObserver attaches a request-local observer to existing dispatch paths.
func WithObserver(ctx context.Context, observe func(Origin)) context.Context {
	return context.WithValue(ctx, observerKey{}, observe)
}

// Observe publishes the exact target already selected by dispatch, before sending.
func Observe(ctx context.Context, origin Origin) {
	if observe, ok := ctx.Value(observerKey{}).(func(Origin)); ok {
		observe(origin)
	}
}

type expectedKey struct{}

// WithExpected binds subsequent forwarding to the original target.
func WithExpected(ctx context.Context, origin Origin) context.Context {
	return context.WithValue(ctx, expectedKey{}, origin)
}

// Expected reports a stateful request's immutable origin, when present.
func Expected(ctx context.Context) (Origin, bool) {
	origin, ok := ctx.Value(expectedKey{}).(Origin)
	return origin, ok
}
