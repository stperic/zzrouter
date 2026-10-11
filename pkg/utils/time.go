package utils

import (
	"sync/atomic"
	"time"
)

// Time handling in zzrouter follows three rules:
//
//  1. Now() preserves the Go monotonic clock reading, so time.Since /
//     time.Until / a.Sub(b) computed from Now()-sourced timestamps are
//     robust against wall-clock jumps (NTP step, DST, manual adjustment,
//     VM host time drift). Use Now() for every duration measurement.
//
//  2. NowUTC() returns the same instant as Now() but stripped of the
//     monotonic reading and normalised to UTC. Use it when the timestamp
//     is about to be persisted (YAML/JSON), serialised over the wire,
//     compared across process boundaries, or displayed in UTC. Calling
//     .UTC() on a monotonic-bearing time.Time value strips the monotonic
//     reading, so keep the two paths distinct.
//
// Tests can swap the clock with SetClock for deterministic timestamps.
// SetClock mutates package-global state and is NOT safe for t.Parallel()
// — see the godoc on SetClock for the constraints.

// Standard time layouts used across the codebase. Centralised so that a
// display-convention change is a one-line edit.
const (
	// LayoutDateTime is the human display format used in detail views.
	LayoutDateTime = "2006-01-02 15:04:05"

	// LayoutDateTimeMs is the detail-view format with millisecond resolution.
	LayoutDateTimeMs = "2006-01-02 15:04:05.000"

	// LayoutDate is the date-only display format.
	LayoutDate = "2006-01-02"

	// LayoutTime is the wall-clock time with seconds.
	LayoutTime = "15:04:05"

	// LayoutShortTime is the minute-resolution time used in compact rows.
	LayoutShortTime = "15:04"

	// LayoutTimestampTag is the filename-safe timestamp used for rotated
	// files and one-shot backup suffixes.
	LayoutTimestampTag = "20060102-150405"

	// LayoutBuildMeta is the compact build-metadata suffix (MMDDHHMM)
	// used in semver "+MMDDHHMM" build identifiers.
	LayoutBuildMeta = "01021504"
)

// Clock is the minimal interface required to read the current wall-clock
// time. Production code uses the system clock; tests may inject a fake via
// SetClock.
//
// Implementations of Clock.Now should return a time.Time that carries the
// monotonic reading when possible — i.e. call the stdlib now function
// directly rather than stripping via .UTC() on the way out. Callers that
// need UTC should normalise at the persistence/serialisation boundary via
// NowUTC.
type Clock interface {
	Now() time.Time
}

type systemClock struct{}

// Now returns the wall-clock time WITH the monotonic reading attached.
// Strip it explicitly (via NowUTC or .UTC()) only at serialisation
// boundaries.
func (systemClock) Now() time.Time { return time.Now() } // lint:allow time.Now

// fixedClock reports a single instant. Used in tests where monotonic
// behaviour is irrelevant.
type fixedClock struct{ t time.Time }

func (f fixedClock) Now() time.Time { return f.t }

// currentClock holds the active Clock. atomic.Pointer keeps Now() lock-free
// on the hot path used by request handlers and health checks, and avoids the
// "inconsistently typed value" panic that atomic.Value raises when different
// concrete Clock implementations are swapped in.
var currentClock atomic.Pointer[Clock]

func init() {
	c := Clock(systemClock{})
	currentClock.Store(&c)
}

// Now returns the current time with the monotonic reading preserved.
//
// Use Now() for every in-process time measurement: time.Since, time.Until,
// a.Sub(b), TTL checks, timers. These stay correct across NTP steps, DST,
// and manual clock adjustments because Go compares the monotonic reading
// when both operands carry one.
//
// For timestamps that will be persisted, logged, or sent over the wire, use
// NowUTC() so the value is UTC-normalised and does not
// embed the host's local offset.
func Now() time.Time {
	if p := currentClock.Load(); p != nil && *p != nil {
		return (*p).Now()
	}
	return time.Now() // lint:allow time.Now
}

// NowUTC returns the current instant in UTC, with the monotonic reading
// stripped. Use for persistence, serialisation, filenames, and any value
// that will cross a process boundary.
func NowUTC() time.Time {
	return Now().UTC()
}

// SetClock swaps the global Clock and returns a function that restores the
// previous value. Intended for tests:
//
//	restore := utils.SetClock(utils.FixedClock(t))
//	defer restore()
//
// IMPORTANT: SetClock mutates package-global state and is NOT safe when the
// test calls t.Parallel(). Parallel tests that need a fixed clock should
// either avoid SetClock (pass a Clock explicitly to the unit under test) or
// use a subtest without t.Parallel() for the clock-dependent section.
func SetClock(c Clock) (restore func()) {
	if c == nil {
		c = systemClock{}
	}
	prev := currentClock.Load()
	currentClock.Store(&c)
	return func() {
		if prev != nil {
			currentClock.Store(prev)
			return
		}
		sys := Clock(systemClock{})
		currentClock.Store(&sys)
	}
}

// FixedClock returns a Clock that always reports t. The returned time has
// no monotonic reading, which is fine for tests that assert on wall-clock
// equality but means time.Since() against it falls back to wall-clock
// subtraction.
func FixedClock(t time.Time) Clock {
	return fixedClock{t: t}
}
