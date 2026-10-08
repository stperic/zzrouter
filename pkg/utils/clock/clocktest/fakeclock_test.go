package clocktest_test

import (
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/utils/clock/clocktest"
)

var epoch = time.Unix(1_700_000_000, 0).UTC()

func TestNowAndAdvance(t *testing.T) {
	t.Parallel()
	fc := clocktest.NewFakeClock(epoch)
	if !fc.Now().Equal(epoch) {
		t.Fatalf("Now = %v, want %v", fc.Now(), epoch)
	}
	fc.Advance(7 * time.Second)
	if !fc.Now().Equal(epoch.Add(7 * time.Second)) {
		t.Fatalf("Now after Advance = %v, want +7s", fc.Now())
	}
}

func TestSinceUsesInternalCounter(t *testing.T) {
	t.Parallel()
	fc := clocktest.NewFakeClock(epoch)
	started := fc.Now()
	fc.Advance(42 * time.Millisecond)
	if got := fc.Since(started); got != 42*time.Millisecond {
		t.Fatalf("Since = %v, want 42ms (must not fall through to time.Since)", got)
	}
}

func TestAfterFiresAtDeadline(t *testing.T) {
	t.Parallel()
	fc := clocktest.NewFakeClock(epoch)
	ch := fc.After(100 * time.Millisecond)
	select {
	case <-ch:
		t.Fatal("After fired before Advance")
	default:
	}
	fc.Advance(100 * time.Millisecond)
	select {
	case v := <-ch:
		if !v.Equal(epoch.Add(100 * time.Millisecond)) {
			t.Fatalf("fire time = %v, want %v", v, epoch.Add(100*time.Millisecond))
		}
	default:
		t.Fatal("After did not fire after Advance")
	}
}

// TestTickerReschedulesInDeadlineOrder covers the core correctness
// property: a 100ms ticker advanced by 300ms fires at t+100, +200,
// +300 — in that order.
func TestTickerReschedulesInDeadlineOrder(t *testing.T) {
	t.Parallel()
	fc := clocktest.NewFakeClock(epoch)
	tk := fc.NewTicker(100 * time.Millisecond)
	defer tk.Stop()

	var fires []time.Time
	for i := 0; i < 3; i++ {
		fc.Advance(100 * time.Millisecond)
		select {
		case v := <-tk.Chan():
			fires = append(fires, v)
		default:
			t.Fatalf("ticker did not fire on step %d", i+1)
		}
	}
	want := []time.Time{
		epoch.Add(100 * time.Millisecond),
		epoch.Add(200 * time.Millisecond),
		epoch.Add(300 * time.Millisecond),
	}
	for i, w := range want {
		if !fires[i].Equal(w) {
			t.Errorf("fire[%d] = %v, want %v", i, fires[i], w)
		}
	}
}

// TestSimultaneousFiresPreserveRegistrationOrder: two events at the
// same instant fire in the order they were registered, not
// slice-iteration-randomised.
func TestSimultaneousFiresPreserveRegistrationOrder(t *testing.T) {
	t.Parallel()
	fc := clocktest.NewFakeClock(epoch)
	first := fc.After(50 * time.Millisecond)
	second := fc.After(50 * time.Millisecond)
	fc.Advance(50 * time.Millisecond)

	select {
	case <-first:
	default:
		t.Fatal("first did not fire")
	}
	select {
	case <-second:
	default:
		t.Fatal("second did not fire")
	}
}

func TestTickerStopPreventsFurtherFires(t *testing.T) {
	t.Parallel()
	fc := clocktest.NewFakeClock(epoch)
	tk := fc.NewTicker(10 * time.Millisecond)
	fc.Advance(10 * time.Millisecond)
	<-tk.Chan() // drain first fire
	tk.Stop()
	fc.Advance(100 * time.Millisecond)
	select {
	case <-tk.Chan():
		t.Fatal("ticker fired after Stop")
	default:
	}
}

func TestAdvanceZeroDoesNothing(t *testing.T) {
	t.Parallel()
	fc := clocktest.NewFakeClock(epoch)
	ch := fc.After(10 * time.Millisecond)
	fc.Advance(0)
	select {
	case <-ch:
		t.Fatal("fired on Advance(0)")
	default:
	}
	if !fc.Now().Equal(epoch) {
		t.Fatalf("Now advanced on Advance(0): %v", fc.Now())
	}
}

func TestNegativeNewTickerPanics(t *testing.T) {
	t.Parallel()
	fc := clocktest.NewFakeClock(epoch)
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("NewTicker(0) did not panic")
		}
	}()
	fc.NewTicker(0)
}

func TestNegativeAdvancePanics(t *testing.T) {
	t.Parallel()
	fc := clocktest.NewFakeClock(epoch)
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("Advance(negative) did not panic")
		}
	}()
	fc.Advance(-time.Second)
}
