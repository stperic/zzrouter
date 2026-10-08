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

func TestFormatForDisplayUsesConfiguredLocation(t *testing.T) {
	utcMidnight := time.Date(2026, 4, 16, 0, 0, 0, 0, time.UTC)

	eastern, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("tz data unavailable: %v", err)
	}
	restore := SetDisplayLocation(eastern)
	defer restore()

	got := FormatForDisplay(utcMidnight, LayoutDateTime)
	// 2026-04-16 00:00 UTC → 2026-04-15 20:00 EDT.
	if !strings.HasPrefix(got, "2026-04-15 20:00") {
		t.Fatalf("FormatForDisplay = %q, want EDT 2026-04-15 20:00:xx", got)
	}
}

func TestFormatForDisplayZeroTime(t *testing.T) {
	if got := FormatForDisplay(time.Time{}, LayoutDateTime); got != "" {
		t.Fatalf("FormatForDisplay(zero) = %q, want empty", got)
	}
}

func TestFormatRFC3339AlwaysUTC(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("tz data unavailable: %v", err)
	}
	local := time.Date(2026, 4, 16, 8, 0, 0, 0, loc)
	got := FormatRFC3339(local)
	if !strings.HasSuffix(got, "Z") {
		t.Fatalf("FormatRFC3339 = %q, want trailing Z (UTC)", got)
	}
}

func TestLoadDisplayLocation(t *testing.T) {
	prev, err := LoadDisplayLocation("America/New_York")
	if err != nil {
		t.Skipf("tz data unavailable: %v", err)
	}
	defer displayLocation.Store(prev)
	if DisplayLocation().String() != "America/New_York" {
		t.Fatalf("DisplayLocation = %s, want America/New_York", DisplayLocation())
	}

	if _, err := LoadDisplayLocation("Not/A_Real_Zone"); err == nil {
		t.Fatal("LoadDisplayLocation(invalid) returned nil error")
	}

	if _, err := LoadDisplayLocation(""); err != nil {
		t.Fatalf("LoadDisplayLocation(\"\") err = %v, want nil", err)
	}
	if DisplayLocation() != time.Local {
		t.Fatalf("empty name should select time.Local, got %s", DisplayLocation())
	}
}

func TestSetDisplayLocationNilFallsBackToLocal(t *testing.T) {
	restore := SetDisplayLocation(nil)
	defer restore()
	if DisplayLocation() != time.Local {
		t.Fatalf("SetDisplayLocation(nil) = %s, want time.Local", DisplayLocation())
	}
}

func TestParseRFC3339ReturnsUTC(t *testing.T) {
	got, err := ParseRFC3339("2026-04-16T08:00:00-04:00")
	if err != nil {
		t.Fatalf("ParseRFC3339 err = %v", err)
	}
	if got.Location() != time.UTC {
		t.Fatalf("ParseRFC3339 location = %s, want UTC", got.Location())
	}
	want := time.Date(2026, 4, 16, 12, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("ParseRFC3339 = %s, want %s", got, want)
	}
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
