// Package affinity provides session-affinity tracking for zzRouter's
// stateful /v1/responses surface.
//
// /v1/responses is stateful: the backend that created a response holds the
// authoritative copy, so retrieval/cancel/delete calls on the same id must
// land on the same backend. In a multi-backend deployment the default-
// backend pass-through silently breaks this, so we record
// (response_id → provider_key) on create and use that mapping on subsequent
// calls. Cache miss falls through to the default backend, matching the
// pre-affinity behaviour.
//
// TTL matches OpenAI's documented 24h stored-response retention; older
// entries cannot point at a resource the backend still holds.
// Persistence across restarts is out of scope — restart degrades
// gracefully to the default-backend path.
//
// The sweeper goroutine is owned by the Affinity value. Callers MUST NOT
// wrap Start in a `go` statement.
package affinity

import (
	"sync"
	"time"

	"github.com/stperic/zzrouter/pkg/utils"
)

const defaultTTL = 24 * time.Hour

// Affinity maps a response_id to the provider key that produced it.
// Zero value is not usable — call New.
type Affinity struct {
	mu      sync.RWMutex
	entries map[string]entry
	ttl     time.Duration
	now     func() time.Time // injectable for tests

	// sweepInterval of zero disables the background sweeper; entries are
	// still cleaned lazily on Lookup.
	sweepInterval time.Duration

	// started/stopped gate Start/Stop idempotency. Start on a running
	// sweeper is a no-op (prevents goroutine leak under repeat-Start);
	// Stop on a never-started sweeper is a no-op. Start after Stop rearms
	// the stop channel, so a coordinator→worker→coordinator role flip
	// produces a working sweeper on the second Start.
	started bool
	stopped bool
	stop    chan struct{}
}

type entry struct {
	ProviderKey string
	ExpiresAt   time.Time
}

// New constructs a ready-to-use Affinity. A zero ttl is replaced by the
// 24h default.
func New(ttl time.Duration) *Affinity {
	if ttl <= 0 {
		ttl = defaultTTL
	}
	return &Affinity{
		entries:       make(map[string]entry),
		ttl:           ttl,
		now:           utils.Now,
		sweepInterval: 5 * time.Minute,
		stop:          make(chan struct{}),
	}
}

// Record ignores empty inputs so callers can invoke it with best-effort
// extraction results without guarding every call site.
func (a *Affinity) Record(responseID, providerKey string) {
	if a == nil || responseID == "" || providerKey == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries[responseID] = entry{
		ProviderKey: providerKey,
		ExpiresAt:   a.now().Add(a.ttl),
	}
}

// Lookup purges expired entries lazily as a side effect so the map does
// not grow unbounded when the background sweeper is disabled.
//
// The TOCTOU between the RLock expiry check and the Lock-delete is
// intentional: if Record re-inserts a fresh entry in the gap we might
// delete a live one, but the consequence is one cache miss that falls
// through to the configured default backend — already the documented
// miss behaviour. The alternative (holding the write lock across every
// read) would serialize the hot retrieval path.
func (a *Affinity) Lookup(responseID string) (providerKey string, ok bool) {
	if a == nil || responseID == "" {
		return "", false
	}
	a.mu.RLock()
	e, found := a.entries[responseID]
	a.mu.RUnlock()
	if !found {
		return "", false
	}
	if a.now().After(e.ExpiresAt) {
		a.mu.Lock()
		delete(a.entries, responseID)
		a.mu.Unlock()
		return "", false
	}
	return e.ProviderKey, true
}

// Delete removes an entry by id. No-op when missing.
func (a *Affinity) Delete(responseID string) {
	if a == nil || responseID == "" {
		return
	}
	a.mu.Lock()
	delete(a.entries, responseID)
	a.mu.Unlock()
}

// Size returns the current entry count. Nil-receiver safe.
func (a *Affinity) Size() int {
	if a == nil {
		return 0
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.entries)
}

// Start launches the background sweeper. Idempotent: calling Start on a
// running sweeper is a no-op. Safe to call after Stop — the stop channel
// is rearmed under lock so a restart after a prior Stop produces a
// working sweeper (needed for coordinator→worker→coordinator role flips).
//
// Affinity owns its sweeper goroutine — callers MUST NOT wrap Start in a
// `go` statement.
func (a *Affinity) Start() {
	if a == nil || a.sweepInterval <= 0 {
		return
	}
	a.mu.Lock()
	if a.started {
		a.mu.Unlock()
		return
	}
	if a.stopped {
		a.stop = make(chan struct{})
		a.stopped = false
	}
	a.started = true
	stop := a.stop
	a.mu.Unlock()

	utils.GoSafe("affinity.sweeper", func() {
		ticker := time.NewTicker(a.sweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				a.sweepExpired()
			}
		}
	})
}

// Stop signals the sweeper to exit. Idempotent. Safe to call even when
// the sweeper was never started. Returns without waiting for the
// goroutine to actually exit — the logical state flips immediately so a
// subsequent Start rearms without overlap concerns.
//
// BENIGN OVERLAP: if Start is called immediately after Stop while the
// previous goroutine is still draining (before it observes the closed
// stop channel on its next select), both goroutines briefly coexist.
// Both serialize through `mu` for entries access so there is no data
// race; the old goroutine exits on its next tick. Worst case is one
// extra sweep of work. Not a leak — the old goroutine is guaranteed
// to exit because `stop` is closed.
func (a *Affinity) Stop() {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stopped {
		return
	}
	a.stopped = true
	a.started = false
	close(a.stop)
}

func (a *Affinity) sweepExpired() {
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	for id, e := range a.entries {
		if now.After(e.ExpiresAt) {
			delete(a.entries, id)
		}
	}
}
