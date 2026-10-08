// Package clock provides a minimal testable wall-clock abstraction.
//
// Production code calls [System] once at wiring time and passes the
// returned Clock to constructors that need time-dependent behaviour.
// Tests construct a [clocktest.FakeClock] instead and drive it with
// explicit Advance calls — no wall-clock sleeps, no flakes.
//
// The interface is deliberately small. It covers only the methods
// the current consumers need; grow it when a real consumer needs
// more, not speculatively.
package clock

import "time"

// Clock is a testable wall-clock surface.
type Clock interface {
	Now() time.Time
	Since(t time.Time) time.Duration
	NewTicker(d time.Duration) Ticker
	After(d time.Duration) <-chan time.Time
}

// Ticker is the analogue of *time.Ticker. Call Stop to release resources.
type Ticker interface {
	Chan() <-chan time.Time
	Stop()
}

// System returns the production clock backed by stdlib time. Pass it
// explicitly to constructors; there is no nil-default.
func System() Clock { return systemClock{} }

type systemClock struct{}

func (systemClock) Now() time.Time                         { return time.Now() } // lint:allow time.Now
func (systemClock) Since(t time.Time) time.Duration        { return time.Since(t) }
func (systemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
func (systemClock) NewTicker(d time.Duration) Ticker {
	return systemTicker{t: time.NewTicker(d)}
}

type systemTicker struct{ t *time.Ticker }

func (s systemTicker) Chan() <-chan time.Time { return s.t.C }
func (s systemTicker) Stop()                  { s.t.Stop() }
