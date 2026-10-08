package role

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/stperic/zzrouter/pkg/utils"
)

// Transition describes a single role change fanned out to subscribers.
type Transition struct {
	From   Role
	To     Role
	Reason string
	At     time.Time
}

// Subscriber is notified on every role change. It runs synchronously
// under the transition mutex; expensive work (subsystem teardown) is
// expected. Panics are recovered so one bad subscriber does not break
// the chain.
type Subscriber func(Transition)

// Manager is the single driver of cluster-mode transitions. All role
// reads and writes flow through Manager; ad-hoc config sampling is
// a drift hazard the architecture guard test is designed to catch.
//
// Transitions are idempotent-by-equality (Set to the current role is
// a no-op) and serialized under an internal mutex (concurrent Set
// calls process one at a time). Subscribers run in registration
// order, synchronously, while the mutex is held.
type Manager struct {
	mu      sync.Mutex
	current Role
	subs    []subscription
	nextID  uint64
	now     func() time.Time // injectable for tests
}

type subscription struct {
	id uint64
	fn Subscriber
}

// NewManager constructs a Manager with the given initial role. An
// invalid initial role returns an error.
func NewManager(initial Role) (*Manager, error) {
	if err := initial.Validate(); err != nil {
		return nil, fmt.Errorf("role.NewManager: %w", err)
	}
	return &Manager{
		current: initial,
		now:     utils.Now,
	}, nil
}

// Current returns the active role. Safe for concurrent callers.
func (m *Manager) Current() Role {
	if m == nil {
		return RoleDisabled
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.current
}

// Set transitions to next. If next equals the current role, Set is a
// no-op and returns nil — callers never need to pre-check. Reason is
// recorded in the Transition for audit and is typically the event
// that triggered the change ("pairing succeeded", "renewal expired",
// "config reload").
//
// Returns an error if next is not a valid Role. Subscribers that
// panic are recovered; their error is not propagated.
func (m *Manager) Set(ctx context.Context, next Role, reason string) error {
	if m == nil {
		return fmt.Errorf("role.Manager: nil receiver")
	}
	if err := next.Validate(); err != nil {
		return fmt.Errorf("role.Set: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("role.Set: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.current == next {
		return nil
	}

	t := Transition{
		From:   m.current,
		To:     next,
		Reason: reason,
		At:     m.now(),
	}
	m.current = next

	for _, s := range m.subs {
		m.runSubscriber(s.fn, t)
	}
	return nil
}

// Subscribe registers fn to receive every transition. Returns an
// unsubscribe function that is safe to call multiple times and from
// any goroutine. Subscribers registered after a transition do not
// see historical events.
func (m *Manager) Subscribe(fn Subscriber) func() {
	if m == nil || fn == nil {
		return func() {}
	}
	m.mu.Lock()
	m.nextID++
	id := m.nextID
	m.subs = append(m.subs, subscription{id: id, fn: fn})
	m.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			for i, s := range m.subs {
				if s.id == id {
					m.subs = append(m.subs[:i], m.subs[i+1:]...)
					return
				}
			}
		})
	}
}

// Start is part of the subsystem lifecycle contract. Today it is a
// no-op; the Manager has no background work. Kept for the Start/Stop
// pairing invariant so future workers (e.g. config-reload watchers)
// can be added without changing callers.
func (m *Manager) Start(_ context.Context) error { return nil }

// Stop is part of the subsystem lifecycle contract. No-op today.
func (m *Manager) Stop(_ context.Context) error { return nil }

// runSubscriber recovers panics so a misbehaving subscriber does not
// prevent the remaining subscribers from observing the transition.
func (m *Manager) runSubscriber(fn Subscriber, t Transition) {
	defer func() { _ = recover() }()
	fn(t)
}
