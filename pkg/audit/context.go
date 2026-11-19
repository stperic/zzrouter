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

// ActorUnknown is the actor of an event no caller is known for. No app key
// may take it as its ID.
const ActorUnknown = "unknown"

// KindActor names a caller that is not an app key by its kind and ID, as
// request principals and audit actors do ("static:admin", "person:<id>"). An
// app key's ID never holds ':', so no app key takes such a name.
func KindActor(kind, id string) string { return kind + ":" + id }

// Attribute fills an event's missing actor and request ID from ctx; an actor
// still unknown is recorded as ActorUnknown.
func Attribute(ctx context.Context, event Event) Event {
	id, _ := ctx.Value(identityKey{}).(identity)
	if event.Actor == "" {
		event.Actor = id.actor
		if event.Actor == "" {
			event.Actor = ActorUnknown
		}
	}
	if event.RequestID == "" {
		event.RequestID = id.requestID
	}
	return event
}
