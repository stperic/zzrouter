// Package affinity binds stored Responses identifiers to their caller and immutable origin.
// Entries expire after the backend retention window. A missing entry proves no
// destination: callers must refuse it rather than select a default backend.
// The Affinity value owns its background sweeper.
package affinity

import (
	"sync"
	"time"

	"github.com/stperic/zzrouter/pkg/utils"
)

const defaultTTL = 24 * time.Hour

// Affinity maps each caller's response identifier to its immutable origin.
// Zero value is not usable — call New.
type Affinity struct {
	mu      sync.RWMutex
	entries map[resourceKey]entry
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

// Owner scopes resources to the initiating principal and tenant.
type Owner struct{ Kind, ID, TeamID string }

// Origin is dispatch evidence captured before sending a create request.
// A remote Node delegates exact-run ownership to that node's own affinity.
type Origin struct {
	ProviderKey, ConnectionRevision, Node, RunID, Endpoint string
	Model, ClientModel, Route                              string
}

type resourceKey struct {
	ID    string
	Owner Owner
}
type entry struct {
	Origin    Origin
	ExpiresAt time.Time
}

// New constructs a ready-to-use Affinity. A zero ttl is replaced by the
// 24h default.
func New(ttl time.Duration) *Affinity {
	if ttl <= 0 {
		ttl = defaultTTL
	}
	return &Affinity{
		entries:       make(map[resourceKey]entry),
		ttl:           ttl,
		now:           utils.Now,
		sweepInterval: 5 * time.Minute,
		stop:          make(chan struct{}),
	}
}

// Record retains an origin without allowing another backend to take over a live identifier.
func (a *Affinity) Record(responseID string, owner Owner, origin Origin) {
	if a == nil || responseID == "" || owner.ID == "" || origin.ProviderKey == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	key := resourceKey{responseID, owner}
	if previous, ok := a.entries[key]; ok && !a.now().After(previous.ExpiresAt) && previous.Origin != origin {
		return
	}
	a.entries[key] = entry{Origin: origin, ExpiresAt: a.now().Add(a.ttl)}
}

// Lookup returns only the initiating caller's unexpired origin.
func (a *Affinity) Lookup(responseID string, owner Owner) (Origin, bool) {
	if a == nil || responseID == "" || owner.ID == "" {
		return Origin{}, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	key := resourceKey{responseID, owner}
	e, found := a.entries[key]
	if !found {
		return Origin{}, false
	}
	if a.now().After(e.ExpiresAt) {
		delete(a.entries, key)
		return Origin{}, false
	}
	return e.Origin, true
}

// Delete forgets only the initiating caller's resource.
func (a *Affinity) Delete(responseID string, owner Owner) {
	if a == nil {
		return
	}
	a.mu.Lock()
	delete(a.entries, resourceKey{responseID, owner})
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
