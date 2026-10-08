package mesh

import (
	"errors"
	"testing"
	"time"
)

func TestLiveness_UnknownToUpOnFirstSuccess(t *testing.T) {
	t.Parallel()
	l := NewLiveness(2)
	now := time.Unix(1000, 0)

	tr := l.Observe(ProbeResult{OK: true, Quality: QualityFull}, now)

	if tr.IsZero() {
		t.Fatal("expected transition on first probe, got zero")
	}
	if tr.From != StatusUnknown || tr.To != StatusUp {
		t.Errorf("want UNKNOWN->UP, got %s->%s", tr.From, tr.To)
	}
	if l.Status() != StatusUp {
		t.Errorf("want state UP, got %s", l.Status())
	}
}

func TestLiveness_HysteresisUpToDegradedToDown(t *testing.T) {
	t.Parallel()
	l := NewLiveness(2)
	base := time.Unix(1000, 0)

	// Seed UP
	l.Observe(ProbeResult{OK: true, Quality: QualityFull}, base)

	// First miss: UP -> DEGRADED (not DOWN)
	tr := l.Observe(ProbeResult{OK: false, Err: errors.New("blip")}, base.Add(30*time.Second))
	if tr.From != StatusUp || tr.To != StatusDegraded {
		t.Errorf("first miss: want UP->DEGRADED, got %s->%s", tr.From, tr.To)
	}

	// Second miss: DEGRADED -> DOWN
	tr = l.Observe(ProbeResult{OK: false, Err: errors.New("still down")}, base.Add(60*time.Second))
	if tr.From != StatusDegraded || tr.To != StatusDown {
		t.Errorf("second miss: want DEGRADED->DOWN, got %s->%s", tr.From, tr.To)
	}
}

func TestLiveness_SuccessResetsFailCounter(t *testing.T) {
	t.Parallel()
	l := NewLiveness(2)
	base := time.Unix(1000, 0)

	l.Observe(ProbeResult{OK: true, Quality: QualityFull}, base)
	l.Observe(ProbeResult{OK: false, Err: errors.New("blip")}, base.Add(30*time.Second))
	// Recovery clears the fail counter
	l.Observe(ProbeResult{OK: true, Quality: QualityFull}, base.Add(60*time.Second))

	// One miss after recovery should only produce DEGRADED, not DOWN
	tr := l.Observe(ProbeResult{OK: false, Err: errors.New("blip2")}, base.Add(90*time.Second))
	if tr.To != StatusDegraded {
		t.Errorf("after recovery + 1 miss: want DEGRADED, got %s", tr.To)
	}
}

func TestLiveness_DownToUpOnReconnect(t *testing.T) {
	t.Parallel()
	l := NewLiveness(2)
	base := time.Unix(1000, 0)

	l.Observe(ProbeResult{OK: false, Err: errors.New("e")}, base)
	l.Observe(ProbeResult{OK: false, Err: errors.New("e")}, base.Add(30*time.Second)) // now DOWN

	conn := &Connection{NodeName: "w1"}
	tr := l.Observe(ProbeResult{OK: true, Quality: QualityFull, Snapshot: conn}, base.Add(60*time.Second))
	if tr.From != StatusDown || tr.To != StatusUp {
		t.Errorf("want DOWN->UP, got %s->%s", tr.From, tr.To)
	}
	if tr.Snapshot != conn {
		t.Error("snapshot should be propagated on reconnect transition")
	}
}

func TestLiveness_DownRecoversToDegradedOnPublicFallback(t *testing.T) {
	t.Parallel()
	l := NewLiveness(2)
	base := time.Unix(1000, 0)

	// Drive to DOWN
	l.Observe(ProbeResult{OK: false, Err: errors.New("e")}, base)
	l.Observe(ProbeResult{OK: false, Err: errors.New("e")}, base.Add(30*time.Second))

	// Worker comes back but only public /health answers (mTLS still broken)
	tr := l.Observe(ProbeResult{OK: true, Quality: QualityDegraded, Snapshot: &Connection{}}, base.Add(60*time.Second))
	if tr.From != StatusDown || tr.To != StatusDegraded {
		t.Errorf("DOWN recover via public-fallback: want DOWN->DEGRADED, got %s->%s", tr.From, tr.To)
	}
}

func TestLiveness_DegradedOnPublicFallback(t *testing.T) {
	t.Parallel()
	l := NewLiveness(2)
	now := time.Unix(1000, 0)

	// mTLS broken but public /health answers → DEGRADED, not UP
	tr := l.Observe(ProbeResult{OK: true, Quality: QualityDegraded, Snapshot: &Connection{}}, now)
	if tr.To != StatusDegraded {
		t.Errorf("public-fallback probe: want DEGRADED, got %s", tr.To)
	}
}

func TestLiveness_NoTransitionOnSteadyState(t *testing.T) {
	t.Parallel()
	l := NewLiveness(2)
	base := time.Unix(1000, 0)

	l.Observe(ProbeResult{OK: true, Quality: QualityFull}, base)
	tr := l.Observe(ProbeResult{OK: true, Quality: QualityFull}, base.Add(30*time.Second))
	if !tr.IsZero() {
		t.Errorf("steady UP: want zero transition, got %s->%s", tr.From, tr.To)
	}

	// DOWN→DOWN also produces no transition (two misses to reach DOWN,
	// then a third miss should stay DOWN).
	l2 := NewLiveness(2)
	l2.Observe(ProbeResult{OK: false, Err: errors.New("e")}, base)                     // -> DEGRADED
	l2.Observe(ProbeResult{OK: false, Err: errors.New("e")}, base.Add(30*time.Second)) // -> DOWN
	tr2 := l2.Observe(ProbeResult{OK: false, Err: errors.New("e")}, base.Add(60*time.Second))
	if !tr2.IsZero() {
		t.Errorf("steady DOWN: want zero transition, got %s->%s", tr2.From, tr2.To)
	}
}

// TestLiveness_ForceDown locks the contract callers depend on: after
// ForceDown, state is StatusDown and the Transition reports To=StatusDown
// carrying the supplied reason; already-DOWN returns a zero Transition.
// Transition.From is deliberately not asserted — the D5 refactor
// re-implements ForceDown via the Observe loop, which surfaces the
// penultimate state (DEGRADED) in From. Callers only log it.
func TestLiveness_ForceDown(t *testing.T) {
	t.Parallel()
	reason := errors.New("goodbye")
	now := time.Unix(2000, 0)

	cases := []struct {
		name       string
		seed       func(*Liveness)
		wantZero   bool
		wantStatus EndpointStatus
	}{
		{
			name:       "unknown_to_down",
			seed:       func(*Liveness) {},
			wantStatus: StatusDown,
		},
		{
			name: "up_to_down",
			seed: func(l *Liveness) {
				l.Observe(ProbeResult{OK: true, Quality: QualityFull}, now.Add(-time.Minute))
			},
			wantStatus: StatusDown,
		},
		{
			name: "degraded_to_down",
			seed: func(l *Liveness) {
				l.Observe(ProbeResult{OK: true, Quality: QualityDegraded, Snapshot: &Connection{}}, now.Add(-time.Minute))
			},
			wantStatus: StatusDown,
		},
		{
			name: "already_down_returns_zero",
			seed: func(l *Liveness) {
				l.Observe(ProbeResult{OK: false, Err: errors.New("e")}, now.Add(-2*time.Minute))
				l.Observe(ProbeResult{OK: false, Err: errors.New("e")}, now.Add(-time.Minute))
			},
			wantZero:   true,
			wantStatus: StatusDown,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := NewLiveness(2)
			tc.seed(l)
			tr := l.ForceDown(now, reason)
			if tc.wantZero {
				if !tr.IsZero() {
					t.Errorf("want zero transition on already-DOWN, got %s->%s", tr.From, tr.To)
				}
			} else {
				if tr.IsZero() {
					t.Fatalf("want transition, got zero")
				}
				if tr.To != StatusDown {
					t.Errorf("Transition.To = %s, want StatusDown", tr.To)
				}
				if !errors.Is(tr.Err, reason) && tr.Err != reason {
					t.Errorf("Transition.Err = %v, want %v", tr.Err, reason)
				}
			}
			if l.Status() != tc.wantStatus {
				t.Errorf("Status = %s, want %s", l.Status(), tc.wantStatus)
			}
		})
	}
}

func TestLiveness_DurationInFrom(t *testing.T) {
	t.Parallel()
	l := NewLiveness(2)
	base := time.Unix(1000, 0)

	l.Observe(ProbeResult{OK: true, Quality: QualityFull}, base)
	tr := l.Observe(ProbeResult{OK: false, Err: errors.New("e")}, base.Add(45*time.Second))
	if tr.DurationInFrom != 45*time.Second {
		t.Errorf("want DurationInFrom=45s, got %v", tr.DurationInFrom)
	}
}
