// Package route_events delivers lifecycle events from the routes
// agent-control surface to live SSE subscribers. Tier 5 of the
// routes-agent-control-api plan.
//
// SCOPE: coord-originated events only. The fallback breaker and
// CooldownManager on a worker stay local — worker-served direct compat
// traffic will not appear on this stream. Cross-node bus is a future
// arc; today's contract is "watch the coord, miss worker-local state".
//
// Design mirrors SpendEventBus (internal/server/spend_events.go) so an
// agent's SSE decode logic is uniform across /spend/events and
// /model-groups/events:
//   - bounded per-subscriber buffer
//   - drop-on-overflow with a per-subscriber dropped counter
//   - subscribe returns (events, dropped, unsub)
//   - unsub removes the subscriber from the bus but does not close the
//     channel — consumers detect shutdown via their own context. The
//     bus snapshots the subscriber set under its lock then sends
//     outside it, so closing from unsub would race a snapshotted send.
package routeevents

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/stperic/zzrouter/pkg/utils"
)

// EventType is the closed-enum tag identifying which lifecycle moment
// produced the event. Stable on the wire — adding a new value is a
// code change so dashboards relying on these strings don't silently
// drift.
type EventType string

const (
	EventRouteCreated       EventType = "route_created"
	EventRouteUpdated       EventType = "route_updated"
	EventRouteDeleted       EventType = "route_deleted"
	EventRouteExpired       EventType = "route_expired"
	EventReplicaAdded       EventType = "replica_added"
	EventReplicaRemoved     EventType = "replica_removed"
	EventBreakerOpened      EventType = "breaker_opened"
	EventBreakerClosed      EventType = "breaker_closed"
	EventCooldownStarted    EventType = "cooldown_started"
	EventCooldownEnded      EventType = "cooldown_ended"
	EventHealthChanged      EventType = "health_changed"
	EventAutoRouteGenerated EventType = "auto_route_generated"
	EventAutoRouteRevoked   EventType = "auto_route_revoked"
)

// AllEventTypes returns every closed-enum tag this package knows
// about. Used by /schema to advertise the vocabulary in one place.
func AllEventTypes() []EventType {
	return []EventType{
		EventRouteCreated, EventRouteUpdated, EventRouteDeleted, EventRouteExpired,
		EventReplicaAdded, EventReplicaRemoved,
		EventBreakerOpened, EventBreakerClosed,
		EventCooldownStarted, EventCooldownEnded,
		EventHealthChanged,
		EventAutoRouteGenerated, EventAutoRouteRevoked,
	}
}

// Event is the on-the-wire shape carried by every SSE chunk. Fields
// are omitempty so a route_created event doesn't ship a useless empty
// replica name, etc. Timestamp is set by the publisher (Bus.Publish)
// so subscribers see consistent clocks across event types.
type Event struct {
	Type      EventType         `json:"type"`
	Route     string            `json:"route,omitempty"`
	Replica   string            `json:"replica,omitempty"`
	Timestamp time.Time         `json:"timestamp"`
	Detail    map[string]string `json:"detail,omitempty"`
}

// eventBufferSize bounds memory per subscriber. 64 ≈ 8 KB at typical
// event size — same headroom as SpendEventBus. A stuck subscriber
// drops events instead of pinning the bus's publish path.
const eventBufferSize = 64

// Bus implements the publish/subscribe surface for route lifecycle
// events. Concrete type, not an interface — callers that don't want
// to publish pass nil and check before calling.
type Bus struct {
	mu          sync.Mutex
	subscribers map[*subscriber]struct{}
}

type subscriber struct {
	ch      chan Event
	dropped atomic.Int64
	filter  func(Event) bool // nil = accept all
}

// NewBus returns a ready-to-use bus. Cheap to construct.
func NewBus() *Bus {
	return &Bus{subscribers: map[*subscriber]struct{}{}}
}

// Publish broadcasts ev to every live subscriber. Sets ev.Timestamp
// if the caller left it zero so subscribers see the publish moment,
// not the upstream event moment (consistency over precision — agents
// poll the underlying state if they need finer timing).
//
// Non-blocking per subscriber: a full channel increments the
// subscriber's dropped counter and the broadcast moves on. Publishers
// (breaker transitions, cooldown timers) MUST NOT pay for slow SSE
// clients.
//
// Concurrency: the bus mutex is held only to snapshot the live
// subscriber set; the per-subscriber filter+send runs outside the lock
// so a new Subscribe or unsub can make progress while a publish is
// fanning out. Safe because subscriber channels are never closed by
// the bus — see Subscribe's unsub for why.
func (b *Bus) Publish(ev Event) {
	if b == nil {
		return
	}
	if ev.Timestamp.IsZero() {
		ev.Timestamp = utils.NowUTC()
	}
	b.mu.Lock()
	subs := make([]*subscriber, 0, len(b.subscribers))
	for sub := range b.subscribers {
		subs = append(subs, sub)
	}
	b.mu.Unlock()
	for _, sub := range subs {
		if sub.filter != nil && !sub.filter(ev) {
			continue
		}
		select {
		case sub.ch <- ev:
		default:
			sub.dropped.Add(1)
		}
	}
}

// Subscribe registers a new subscriber with an optional server-side
// filter. The filter runs outside the bus lock during fan-out — keep
// it cheap (string compares, glob match, no allocations). Returns the
// event channel, a pointer to the per-subscriber dropped counter, and
// an unsub function the caller MUST invoke (typically via defer).
//
// Channel lifetime: unsub removes the subscriber from the bus but does
// NOT close the channel. A real subscriber detects shutdown via its
// own context (the SSE controller's request ctx). The bus never closes
// a subscriber's channel because Publish releases b.mu before sending
// — closing here would race a snapshotted send. The channel is GC'd
// once the consumer drops its reference.
func (b *Bus) Subscribe(filter func(Event) bool) (events <-chan Event, dropped *atomic.Int64, unsub func()) {
	if b == nil {
		ch := make(chan Event)
		close(ch)
		var zero atomic.Int64
		return ch, &zero, func() {}
	}
	sub := &subscriber{
		ch:     make(chan Event, eventBufferSize),
		filter: filter,
	}
	b.mu.Lock()
	b.subscribers[sub] = struct{}{}
	b.mu.Unlock()
	return sub.ch, &sub.dropped, func() {
		b.mu.Lock()
		delete(b.subscribers, sub)
		b.mu.Unlock()
	}
}

// SubscriberCount is exposed for tests pinning the cleanup contract.
func (b *Bus) SubscriberCount() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subscribers)
}
