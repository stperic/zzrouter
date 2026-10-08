// Package clocktest provides a deterministic fake for
// [github.com/stperic/zzrouter/pkg/utils/clock.Clock]. A FakeClock
// never advances on its own — tests call [FakeClock.Advance] to fire
// every pending event whose deadline falls in the stepped window.
//
// Simultaneous-deadline events fire in registration order (slice
// insertion order), so tests are deterministic across runs.
package clocktest

import (
	"sync"
	"time"

	"github.com/stperic/zzrouter/pkg/utils/clock"
)

// pending is one scheduled event. period==0 means one-shot.
type pending struct {
	deadline time.Time
	ch       chan time.Time
	period   time.Duration
	stopped  bool
}

// FakeClock is a deterministic [clock.Clock] for tests.
type FakeClock struct {
	mu      sync.Mutex
	now     time.Time
	pending []*pending
}

// NewFakeClock returns a clock starting at t.
func NewFakeClock(t time.Time) *FakeClock { return &FakeClock{now: t} }

// Now reports the fake's current time. Returned values carry no
// monotonic reading.
func (fc *FakeClock) Now() time.Time {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return fc.now
}

// Since returns fc.Now().Sub(t). Computed against the internal
// counter, not [time.Since], so it remains correct for fake-produced
// times that carry no monotonic reading.
func (fc *FakeClock) Since(t time.Time) time.Duration {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return fc.now.Sub(t)
}

// NewTicker returns a [clock.Ticker] that fires every d. Panics on
// non-positive d to mirror stdlib.
func (fc *FakeClock) NewTicker(d time.Duration) clock.Ticker {
	if d <= 0 {
		panic("clocktest: non-positive NewTicker duration")
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	p := &pending{
		deadline: fc.now.Add(d),
		ch:       make(chan time.Time, 1),
		period:   d,
	}
	fc.pending = append(fc.pending, p)
	return &fakeTicker{fc: fc, p: p}
}

// After returns a channel that receives once after d.
func (fc *FakeClock) After(d time.Duration) <-chan time.Time {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	p := &pending{
		deadline: fc.now.Add(d),
		ch:       make(chan time.Time, 1),
	}
	fc.pending = append(fc.pending, p)
	return p.ch
}

// Advance moves the clock forward by d, firing every pending event
// whose deadline falls in (old_now, old_now+d] in strict deadline
// order. Simultaneous-deadline events fire in registration order.
// One-shot events self-remove after fire; tickers reschedule.
func (fc *FakeClock) Advance(d time.Duration) {
	if d < 0 {
		panic("clocktest: negative Advance duration")
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	target := fc.now.Add(d)
	for fc.fireNext(target) {
	}
	if target.After(fc.now) {
		fc.now = target
	}
}

// fireNext fires the earliest non-stopped pending event with
// deadline <= target. Returns false if nothing fires.
func (fc *FakeClock) fireNext(target time.Time) bool {
	var earliest *pending
	for _, p := range fc.pending {
		if p.stopped {
			continue
		}
		if p.deadline.After(target) {
			continue
		}
		if earliest == nil || p.deadline.Before(earliest.deadline) {
			earliest = p
		}
	}
	if earliest == nil {
		return false
	}
	if earliest.deadline.After(fc.now) {
		fc.now = earliest.deadline
	}
	// Non-blocking send matches stdlib: a Ticker/After channel drops
	// the tick if the receiver hasn't drained the previous one.
	select {
	case earliest.ch <- fc.now:
	default:
	}
	if earliest.period > 0 {
		earliest.deadline = earliest.deadline.Add(earliest.period)
	} else {
		earliest.stopped = true
	}
	return true
}

type fakeTicker struct {
	fc *FakeClock
	p  *pending
}

func (t *fakeTicker) Chan() <-chan time.Time { return t.p.ch }

func (t *fakeTicker) Stop() {
	t.fc.mu.Lock()
	defer t.fc.mu.Unlock()
	t.p.stopped = true
}
