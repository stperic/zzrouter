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
//  3. Display formatting goes through FormatForDisplay + the display-
//     timezone knob. Processes that want local-time rendering set the
//     zone once (usually from config) instead of sprinkling t.Local()
//     across render code.
//
// Tests can swap the clock with SetClock for deterministic timestamps.
// SetClock mutates package-global state and is NOT safe for t.Parallel()
// — see the godoc on SetClock for the constraints.

// Standard time layouts used across the codebase. Centralised so that a
// display-convention change is a one-line edit.
const (
	// LayoutRFC3339 is the canonical wire format for serialised timestamps.
	LayoutRFC3339 = time.RFC3339

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
// NowUTC or FormatRFC3339.
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
// NowUTC() or FormatRFC3339() so the value is UTC-normalised and does not
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

// displayLocation holds the *time.Location used by the display helpers.
// Default is time.Local, matching Go defaults and the pre-existing
// .Local().Format(...) usage sprinkled through the TUI.
//
// Intended to be set ONCE at process startup from config. The getter is
// lock-free; the setter is not safe to race with itself, but races with
// the reader are fine because *time.Location is immutable.
var displayLocation atomic.Pointer[time.Location]

func init() {
	displayLocation.Store(time.Local)
}

// SetDisplayLocation overrides the timezone used by FormatForDisplay and
// the related helpers. Pass time.UTC for UTC display, time.Local for the
// host timezone, or a value from time.LoadLocation for a named zone.
//
// Returns a function that restores the previous location. Intended for
// startup wiring and tests; concurrent callers racing on SetDisplayLocation
// may leave the "previous" value stale.
func SetDisplayLocation(loc *time.Location) (restore func()) {
	if loc == nil {
		loc = time.Local
	}
	prev := DisplayLocation()
	displayLocation.Store(loc)
	return func() { displayLocation.Store(prev) }
}

// DisplayLocation returns the current display timezone.
func DisplayLocation() *time.Location {
	if loc := displayLocation.Load(); loc != nil {
		return loc
	}
	return time.Local
}

// LoadDisplayLocation resolves a timezone name and installs it as the display
// zone. Returns the previous location and any parse error. An empty name
// selects time.Local, matching "unset" semantics in config files.
func LoadDisplayLocation(name string) (prev *time.Location, err error) {
	prev = DisplayLocation()
	if name == "" {
		displayLocation.Store(time.Local)
		return prev, nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return prev, err
	}
	displayLocation.Store(loc)
	return prev, nil
}

// FormatForDisplay renders t in the configured display timezone using layout.
// A zero time returns the empty string so call sites do not need to guard.
func FormatForDisplay(t time.Time, layout string) string {
	if t.IsZero() {
		return ""
	}
	return t.In(DisplayLocation()).Format(layout)
}

// FormatRFC3339 renders t in UTC using the wire format. Used by API responses
// where a stable timezone-tagged representation is required regardless of the
// process display zone. Zero values render as the empty string.
func FormatRFC3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(LayoutRFC3339)
}

// ParseRFC3339 parses an RFC3339 timestamp and returns it in UTC. Returns the
// zero time (with the error) on failure.
func ParseRFC3339(s string) (time.Time, error) {
	t, err := time.Parse(LayoutRFC3339, s)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}
