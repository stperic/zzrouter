package mesh

import "time"

// HealthQuality describes which probe path succeeded.
//
// A worker may answer on the mTLS cluster port (full dispatch viability)
// or only on the public /health port (process alive, mTLS broken). The
// distinction matters because a public-only worker cannot serve
// /zzrouter/v1/internal/* dispatch — it should appear as StatusDegraded,
// not StatusUp, so routing can skip it while diagnostics still see it.
type HealthQuality int

const (
	// QualityDegraded is the zero value by design: any Connection constructed
	// outside performHealthCheck (tests, mocks, future admin paths) defaults
	// to the safer "mTLS broken" state instead of masquerading as Full.
	QualityDegraded HealthQuality = iota // Only public-port fallback succeeded
	QualityFull                          // mTLS cluster-port probe succeeded
)

// ProbeResult is the outcome of a single health probe fed into Liveness.
type ProbeResult struct {
	OK       bool
	Quality  HealthQuality
	Err      error
	Snapshot *Connection // populated iff OK
}

// Transition is the state change produced by Liveness.Observe. A zero
// Transition (From == "" && To == "") means the probe did not change state.
type Transition struct {
	From, To       EndpointStatus
	At             time.Time
	DurationInFrom time.Duration
	Err            error
	Snapshot       *Connection
}

// IsZero reports whether the probe left state unchanged.
func (t Transition) IsZero() bool { return t.From == "" && t.To == "" }

// Liveness is a hysteresis-aware state machine for one endpoint.
//
// Transitions (Threshold=2):
//
//	UP          -> miss    -> DEGRADED (still dispatchable)
//	DEGRADED    -> miss    -> DOWN     (evicted from dispatch)
//	any         -> OK+Full -> UP       (fail counter resets)
//	any         -> OK+Deg  -> DEGRADED (mTLS broken, public only)
//
// Callers use Observe and act on the returned Transition — the state
// machine is the single source of truth for when to log, when to fire
// the reconnection callback, and when to flip registry status.
type Liveness struct {
	state      EndpointStatus
	fails      int
	threshold  int
	lastChange time.Time
	lastErr    error
}

// minThreshold is the smallest hysteresis value that keeps DEGRADED
// reachable on the miss path. threshold=1 would collapse the machine
// to UP/DOWN and defeat the grace-probe guarantee.
const minThreshold = 2

// NewLiveness returns a Liveness seeded at StatusUnknown.
// threshold is the number of consecutive misses before UP→DOWN; must
// be at least minThreshold (2) — values below are clamped.
func NewLiveness(threshold int) *Liveness {
	if threshold < minThreshold {
		threshold = minThreshold
	}
	return &Liveness{state: StatusUnknown, threshold: threshold}
}

// Status returns the current endpoint status.
func (l *Liveness) Status() EndpointStatus { return l.state }

// ForceDown transitions the state to StatusDown immediately, bypassing
// the consecutive-miss threshold. Used when a peer declares it is going
// away (graceful shutdown goodbye). Returns the last non-zero Transition
// observed (zero if already DOWN). Reuses the Observe state machine so
// the fail-counter and lastErr bookkeeping stay consistent — callers
// only read Transition.To, so the penultimate From (DEGRADED) and
// DurationInFrom=0 on the final hop are fine.
func (l *Liveness) ForceDown(now time.Time, reason error) Transition {
	var last Transition
	probe := ProbeResult{Err: reason}
	for range l.threshold {
		if t := l.Observe(probe, now); !t.IsZero() {
			last = t
		}
	}
	return last
}

// Observe advances the state machine for one probe and returns the
// resulting Transition (zero if state did not change).
func (l *Liveness) Observe(r ProbeResult, now time.Time) Transition {
	prev := l.state
	prevAt := l.lastChange

	var next EndpointStatus
	if r.OK {
		l.fails = 0
		l.lastErr = nil
		if r.Quality == QualityDegraded {
			next = StatusDegraded
		} else {
			next = StatusUp
		}
	} else {
		l.lastErr = r.Err
		l.fails++
		if l.fails >= l.threshold {
			next = StatusDown
		} else {
			next = StatusDegraded
		}
	}

	if next == prev {
		return Transition{}
	}
	l.state = next
	l.lastChange = now
	return Transition{
		From:           prev,
		To:             next,
		At:             now,
		DurationInFrom: now.Sub(prevAt),
		Err:            r.Err,
		Snapshot:       r.Snapshot,
	}
}
