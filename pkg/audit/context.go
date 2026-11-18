package audit

import "context"

type identityKey struct{}
type identity struct{ actor, requestID string }

// WithIdentity carries an already authenticated actor, never request credentials.
func WithIdentity(ctx context.Context, actor, requestID string) context.Context {
	return context.WithValue(ctx, identityKey{}, identity{actor, requestID})
}

// ActorLocalOperator identifies a verified local installation operator.
const ActorLocalOperator = "local:operator"

func withIdentity(ctx context.Context, event Event) Event {
	id, _ := ctx.Value(identityKey{}).(identity)
	if event.Actor == "" {
		event.Actor = id.actor
		if event.Actor == "" {
			event.Actor = "unknown"
		}
	}
	if event.RequestID == "" {
		event.RequestID = id.requestID
	}
	return event
}
