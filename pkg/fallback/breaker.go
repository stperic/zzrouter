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
// request is admitted at a time so recovery cannot flood an upstream.
type Breaker struct {
	key string

	mu              sync.RWMutex
	state           BreakerState
	failureCount    int
	successCount    int
	lastFailureTime time.Time
	lastStateChange time.Time
	probing         bool // guarded by mu; true while a half-open probe is in flight
	generation      uint64
	retained        bool // guarded by the manager

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

// ExecuteAny preserves request-driven recovery for non-mesh consumers.
func (b *Breaker) ExecuteAny(fn func() (any, error)) (any, error) {
	permit, err := b.Acquire(true)
	if err != nil {
		return nil, err
	}
	outcome := Neutral
	defer func() { permit.Complete(outcome) }()
	result, err := fn()
	outcome = Success
	if err != nil {
		outcome = Failure
	}
	return result, err
}

// Outcome distinguishes reachability evidence from caller-local failures.
type Outcome int

const (
	Neutral Outcome = iota
	Success
	Failure
)

// Permit prevents late completions from changing a newer breaker generation.
type Permit struct {
	breaker    *Breaker
	generation uint64
	probing    bool
	once       sync.Once
}

// Completion reports accepted evidence and its atomic breaker state change.
type Completion struct {
	Applied  bool
	From, To BreakerState
}

// Acquire reserves recovery for probes; ordinary requests require Closed.
func (b *Breaker) Acquire(probe bool) (*Permit, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state != BreakerClosed && (!probe || b.probing ||
		(b.state == BreakerOpen && utils.Now().Sub(b.lastStateChange) < b.cfg.Timeout)) {
		return nil, fmt.Errorf("circuit breaker open for %s", b.key)
	}
	if b.state == BreakerOpen {
		b.state = BreakerHalfOpen
		b.successCount = 0
		b.lastStateChange = utils.Now()
		b.generation++
	}
	probing := b.state == BreakerHalfOpen
	b.probing = probing
	return &Permit{breaker: b, generation: b.generation, probing: probing}, nil
}

// Complete releases even neutral probes without inventing reachability evidence.
func (p *Permit) Complete(outcome Outcome) (result Completion) {
	p.once.Do(func() {
		b := p.breaker
		b.mu.Lock()
		defer b.mu.Unlock()
		if p.generation != b.generation {
			return
		}
		if p.probing {
			b.probing = false
		}
		result.Applied = true
		prev := b.state
		switch outcome {
		case Failure:
			b.onFailureLocked()
		case Success:
			b.onSuccessLocked()
		}
		if b.state != prev {
			b.generation++
		}
		result.From, result.To = prev, b.state
	})
	return result
}

// Invalidate discards in-flight evidence when a peer is authoritatively down.
func (b *Breaker) Invalidate() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.generation++
	b.probing = false
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
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.getBreakerLocked(key)
}

// Retain ties a registered endpoint's breaker to its registration lifetime.
func (m *BreakerManager) Retain(key string) *Breaker {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.getBreakerLocked(key)
	b.retained = true
	return b
}

// Release discards evidence only when the endpoint is unregistered.
func (m *BreakerManager) Release(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.breakers, key)
}

func (m *BreakerManager) getBreakerLocked(key string) *Breaker {
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
		if !b.retained && idle && state == BreakerOpen {
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
		if b.retained {
			continue
		}
		b.mu.RLock()
		candidates = append(candidates, candidate{key, b.lastStateChange})
		b.mu.RUnlock()
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].lastChange.Before(candidates[j].lastChange)
	})
	for i := 0; i < min(evictCount, len(candidates)); i++ {
		delete(m.breakers, candidates[i].key)
	}
}
