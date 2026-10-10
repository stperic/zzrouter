package utils

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func hasMono(t time.Time) bool {
	// If the monotonic reading is present, stripping it produces a
	// strictly different internal representation even though the wall
	// clock instant is identical. The canonical detection is
	// t.String() — monotonic times print with " m=+..." suffix.
	return strings.Contains(t.String(), " m=")
}

func TestNowPreservesMonotonic(t *testing.T) {
	got := Now()
	if !hasMono(got) {
		t.Fatalf("Now() stripped monotonic reading: %v", got)
	}
	// time.Since uses the monotonic reading when available. We assert
	// that the elapsed duration matches a small sleep within a sane
	// bound — the wall-clock fallback would also satisfy this, but the
	// hasMono check above is the real regression guard.
	time.Sleep(2 * time.Millisecond)
	if d := time.Since(got); d <= 0 || d > 500*time.Millisecond {
		t.Fatalf("time.Since(Now()) = %v, want small positive", d)
	}
}

func TestNowUTCStripsMonotonic(t *testing.T) {
	got := NowUTC()
	if got.Location() != time.UTC {
		t.Fatalf("NowUTC() location = %s, want UTC", got.Location())
	}
	if hasMono(got) {
		t.Fatalf("NowUTC() kept monotonic reading: %v", got)
	}
}

func TestNowDefaultLocation(t *testing.T) {
	// Now() returns time in time.Local (or whatever the test clock
	// supplies). The critical property is that it does NOT force UTC.
	got := Now()
	if got.Location() == time.UTC && time.Local != time.UTC {
		t.Fatalf("Now() returned UTC on a non-UTC host; monotonic may be stripped")
	}
}

func TestSetClockOverridesAndRestores(t *testing.T) {
	fixed := time.Date(2026, 4, 16, 12, 0, 0, 0, time.UTC)
	restore := SetClock(FixedClock(fixed))
	if !Now().Equal(fixed) {
		t.Fatalf("Now() = %s, want %s", Now(), fixed)
	}
	restore()
	// After restore, Now() must move forward with real time.
	if delta := time.Since(Now()); delta < 0 || delta > time.Second {
		t.Fatalf("Now() did not return to system clock; delta=%s", delta)
	}
}

func TestSetClockNilFallsBackToSystem(t *testing.T) {
	restore := SetClock(nil)
	defer restore()
	// Must not panic; must return a sensible current time.
	got := Now()
	if time.Since(got) > time.Second {
		t.Fatalf("SetClock(nil) produced bogus time: %v", got)
	}
}

func TestFixedClockRoundTrip(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("tz data unavailable: %v", err)
	}
	local := time.Date(2026, 4, 16, 8, 0, 0, 0, loc)
	got := FixedClock(local).Now()
	// FixedClock preserves the input location so tests that assert on
	// .Location() get predictable results. Callers that persist must
	// go through NowUTC().
	if !got.Equal(local) {
		t.Fatalf("FixedClock.Now() = %s, want equal to %s", got, local)
	}
}

func TestNowAndSetClockConcurrent(t *testing.T) {
	// Writer goroutine flips between two clocks while readers hammer
	// Now(). atomic.Pointer + interface dispatch must not race or panic.
	fixed := FixedClock(time.Date(2026, 4, 16, 12, 0, 0, 0, time.UTC))

	var stop atomic.Bool
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			restore := SetClock(fixed)
			restore()
		}
	}()

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10_000; j++ {
				_ = Now()
			}
		}()
	}

	// Let the readers do real work before signalling stop.
	time.Sleep(20 * time.Millisecond)
	stop.Store(true)
	wg.Wait()
}

// BenchmarkNow documents the overhead of the centralised clock vs raw
// time.Now(). Keep the delta small — Now() is on the hot path of every
// request handler, rate-limit check, and observability middleware.
func BenchmarkNow(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_ = Now()
	}
}

func BenchmarkStdlibNow(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_ = time.Now()
	}
}
