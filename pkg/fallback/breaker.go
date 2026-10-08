package fallback

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/stperic/zzrouter/pkg/utils"
)

// BreakerState is the operational state of a single endpoint breaker.
type BreakerState int

const (
	BreakerClosed   BreakerState = iota // Normal: requests pass through.
	BreakerOpen                         // Failing: requests rejected without contacting the endpoint.
	BreakerHalfOpen                     // Probing: a single test request is allowed through to check recovery.
)

// String returns a human-readable name for use in logs and metrics.
func (s BreakerState) String() string {
	switch s {
	case BreakerClosed:
		return "closed"
	case BreakerOpen:
		return "open"
	case BreakerHalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

// BreakerConfig tunes a Breaker. Zero values fall back to package defaults.
type BreakerConfig struct {
	MaxFailures      int           // Consecutive failures before transitioning Closed → Open.
	Timeout          time.Duration // Time to remain Open before allowing a half-open probe.
	SuccessThreshold int           // Consecutive successes in HalfOpen needed to close.
}

// DefaultBreakerConfig returns the defaults used when callers do not specify
// values: open after 3 failures, retry after 30s, close after 2 successes.
// These values were originally hardcoded in pkg/cluster and pkg/connectivity;
// they're consolidated here so all consumers share one tuning surface.
func DefaultBreakerConfig() BreakerConfig {
	return BreakerConfig{
		MaxFailures:      3,
		Timeout:          30 * time.Second,
		SuccessThreshold: 2,
	}
}

// Breaker is a per-endpoint circuit breaker. Use BreakerManager.GetBreaker
// to obtain an instance scoped to a key (typically a URL).
//
// Breaker is safe for concurrent use. In HalfOpen state at most one probe
// request is admitted at a time, guarded by an atomic CAS so concurrent
// callers race exactly one execution rather than flooding a recovering
// upstream.
type Breaker struct {
	key string

	mu              sync.RWMutex
	state           BreakerState
	failureCount    int
	successCount    int
	lastFailureTime time.Time
	lastStateChange time.Time
	probing         bool // guarded by mu; true while a half-open probe is in flight

	cfg BreakerConfig
}

// State returns the current breaker state.
func (b *Breaker) State() BreakerState {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.state
}

// GetState is an alias for State, kept for legacy callers that pre-dated
// the consolidation. New code should prefer State.
func (b *Breaker) GetState() BreakerState { return b.State() }

// IsOpen reports whether the breaker is currently rejecting requests.
// Note: a breaker whose Open timeout has elapsed will report Open here
// until the next Execute / ExecuteAny call promotes it to HalfOpen.
func (b *Breaker) IsOpen() bool {
	return b.State() == BreakerOpen
}

// Metrics returns a snapshot of the breaker's current state and counters.
// The returned map is freshly allocated per call and safe to mutate.
func (b *Breaker) Metrics() map[string]any {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return map[string]any{
		"key":               b.key,
		"state":             b.state.String(),
		"failure_count":     b.failureCount,
		"success_count":     b.successCount,
		"last_failure":      b.lastFailureTime,
		"last_state_change": b.lastStateChange,
	}
}

// GetMetrics is an alias for Metrics, kept for legacy callers.
func (b *Breaker) GetMetrics() map[string]any { return b.Metrics() }

// Execute runs fn through the breaker. If the breaker is Open and the
// timeout has not elapsed, fn is not called and an error is returned. If
// the timeout has elapsed, the breaker transitions to HalfOpen and admits
// fn as the probe request.
//
// fn is invoked without the breaker lock held so a slow upstream cannot
// block other callers reading state.
func (b *Breaker) Execute(fn func() error) error {
	_, err := b.ExecuteAny(func() (any, error) { return nil, fn() })
	return err
}

// ExecuteAny is the value-returning variant of Execute, for callers that
// need to thread a result through the breaker.
//
// The release path runs in defer so a panicking fn does not leak the
// half-open probe slot — without this, one panicking probe would wedge
// the breaker into HalfOpen forever and reject every future call.
func (b *Breaker) ExecuteAny(fn func() (any, error)) (any, error) {
	probing, err := b.acquire()
	if err != nil {
		return nil, err
	}

	var (
		result any
		fnErr  error
	)
	defer func() { b.release(probing, fnErr) }()

	result, fnErr = fn()
	return result, fnErr
}

// acquire transitions state for an incoming call and returns whether this
// call holds the half-open probe slot. Callers must pair with release —
// the ExecuteAny defer ensures pairing even on fn panic.
func (b *Breaker) acquire() (probing bool, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case BreakerOpen:
		remaining := b.cfg.Timeout - time.Since(b.lastStateChange)
		if remaining > 0 {
			return false, fmt.Errorf("circuit breaker open for %s (will retry in %v)", b.key, remaining)
		}
		// Cooldown elapsed — promote to HalfOpen and let this caller probe.
		b.state = BreakerHalfOpen
		b.successCount = 0
		b.lastStateChange = utils.Now()
		probing = true

	case BreakerHalfOpen:
		probing = true
	}

	// Only one probe in flight at a time. The check sits under b.mu so it
	// races correctly with concurrent acquire() calls — the prior atomic
	// CAS was redundant since b.probing is never read or written without
	// holding the lock.
	if probing && b.probing {
		return false, fmt.Errorf("circuit breaker open for %s", b.key)
	}
	if probing {
		b.probing = true
	}
	return probing, nil
}

func (b *Breaker) release(probing bool, fnErr error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if probing {
		b.probing = false
	}
	if fnErr != nil {
		b.onFailureLocked()
		return
	}
	b.onSuccessLocked()
}

func (b *Breaker) onFailureLocked() {
	b.failureCount++
	b.lastFailureTime = utils.Now()

	switch b.state {
	case BreakerClosed:
		if b.failureCount >= b.cfg.MaxFailures {
			b.state = BreakerOpen
			b.lastStateChange = utils.Now()
		}
	case BreakerHalfOpen:
		// Any failure during a probe sends us straight back to Open,
		// resetting the success ladder so recovery requires a fresh streak.
		b.state = BreakerOpen
		b.failureCount = 0
		b.successCount = 0
		b.lastStateChange = utils.Now()
	}
}

func (b *Breaker) onSuccessLocked() {
	switch b.state {
	case BreakerClosed:
		b.failureCount = 0
	case BreakerHalfOpen:
		b.successCount++
		if b.successCount >= b.cfg.SuccessThreshold {
			b.state = BreakerClosed
			b.failureCount = 0
			b.successCount = 0
			b.lastStateChange = utils.Now()
		}
	}
}

// maxBreakers caps BreakerManager's map to bound memory growth in
// long-running coordinators that have routed through many transient hosts.
const maxBreakers = 1000

// BreakerManager owns Breakers keyed by an endpoint identifier (typically
// a URL). It deduplicates concurrent Get calls and evicts the oldest
// breakers in batches once the cap is reached.
type BreakerManager struct {
	mu       sync.RWMutex
	breakers map[string]*Breaker
	cfg      BreakerConfig
}

// NewBreakerManager returns a manager that uses DefaultBreakerConfig for
// every breaker it creates.
func NewBreakerManager() *BreakerManager {
	return NewBreakerManagerWithConfig(DefaultBreakerConfig())
}

// NewBreakerManagerWithConfig is the explicit-config variant of
// NewBreakerManager. Useful for callers (tests, alternate tuning) that
// need non-default thresholds.
func NewBreakerManagerWithConfig(cfg BreakerConfig) *BreakerManager {
	return &BreakerManager{
		breakers: make(map[string]*Breaker),
		cfg:      cfg,
	}
}

// GetBreaker returns the Breaker for key, creating one on first use.
// Safe for concurrent callers.
func (m *BreakerManager) GetBreaker(key string) *Breaker {
	m.mu.RLock()
	if b, ok := m.breakers[key]; ok {
		m.mu.RUnlock()
		return b
	}
	m.mu.RUnlock()

	m.mu.Lock()
	defer m.mu.Unlock()

	if b, ok := m.breakers[key]; ok {
		return b
	}

	if len(m.breakers) >= maxBreakers {
		m.evictStaleLocked()
	}

	b := &Breaker{
		key:             key,
		state:           BreakerClosed,
		cfg:             m.cfg,
		lastStateChange: utils.Now(),
	}
	m.breakers[key] = b
	return b
}

// PruneStale removes any breaker that has been Open with no state change
// for longer than maxAge. Returns the number removed. Safe to call from a
// background ticker.
//
// Lock ordering: this method holds m.mu while reading each Breaker's mu.
// Breaker methods must not call back into BreakerManager or this would
// deadlock — they currently don't.
func (m *BreakerManager) PruneStale(maxAge time.Duration) int {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := utils.Now()
	removed := 0
	for key, b := range m.breakers {
		b.mu.RLock()
		idle := now.Sub(b.lastStateChange) > maxAge
		state := b.state
		b.mu.RUnlock()
		if idle && state == BreakerOpen {
			delete(m.breakers, key)
			removed++
		}
	}
	return removed
}

// evictStaleLocked drops the oldest 10% of breakers (minimum one) by
// lastStateChange. Batch eviction amortizes the scan cost across many
// inserts. Caller must hold m.mu.
func (m *BreakerManager) evictStaleLocked() {
	evictCount := max(len(m.breakers)/10, 1)

	type candidate struct {
		key        string
		lastChange time.Time
	}
	candidates := make([]candidate, 0, len(m.breakers))
	for key, b := range m.breakers {
		b.mu.RLock()
		candidates = append(candidates, candidate{key, b.lastStateChange})
		b.mu.RUnlock()
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].lastChange.Before(candidates[j].lastChange)
	})
	for i := 0; i < evictCount; i++ {
		delete(m.breakers, candidates[i].key)
	}
}
