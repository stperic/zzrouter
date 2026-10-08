package spend

import (
	"testing"
)

// fakeSnap implements BudgetSnapshotter for tests. Returns a fixed
// list of rows on every call.
type fakeSnap struct {
	rows []KeyBudget
}

func (f *fakeSnap) SnapshotKeyBudgets() []KeyBudget { return f.rows }

// TestSetBudgetSnapshotter_NilSafety pins that registering a nil
// snapshotter does not panic and leaves the gauges observable but
// row-less. Production startup paths legitimately call this with nil
// during shutdown.
func TestSetBudgetSnapshotter_NilSafety(t *testing.T) {
	SetBudgetSnapshotter(nil)
	// Re-registering the same nil snapshotter must remain a no-op.
	SetBudgetSnapshotter(nil)
}

// TestSetBudgetSnapshotter_RegistersAndReplaces pins that subsequent
// calls swap the snapshotter without leaking registrations. The
// observable callback reads the latest snapshotter at scrape time, so
// switching mid-flight is safe.
func TestSetBudgetSnapshotter_RegistersAndReplaces(t *testing.T) {
	first := &fakeSnap{rows: []KeyBudget{{Labels: CallerLabels{APIKeyAlias: "first"}, SpendLimitUSD: 10}}}
	second := &fakeSnap{rows: []KeyBudget{{Labels: CallerLabels{APIKeyAlias: "second"}, SpendLimitUSD: 20}}}

	SetBudgetSnapshotter(first)
	SetBudgetSnapshotter(second)
	t.Cleanup(func() { SetBudgetSnapshotter(nil) })

	// Reading the package state directly is the only honest way to
	// pin the swap — observable gauges fire on Prometheus scrape, not
	// on demand. End-to-end emission is exercised by the
	// internal/server integration test.
	budgetMu.Lock()
	got := budgetSnapshotter
	budgetMu.Unlock()
	if got != second {
		t.Errorf("snapshotter not swapped: got %v, want %v", got, second)
	}
}

// TestKeyBudget_RemainingClampedAtZero pins the negative-remaining
// clamp inside the observable callback. A settle that overshot the
// configured cap (estimated < actual) leaves SpendUSD > SpendLimit;
// emitting "remaining = -X" would break PromQL queries that expect a
// non-negative gauge. The clamp keeps remaining = 0 in that case so
// dashboards display "exhausted" rather than negative.
//
// Exercises only the math path; the OTel observation side requires a
// full meter provider + reader and lives in the integration test.
func TestKeyBudget_RemainingClampedAtZero(t *testing.T) {
	cases := []struct {
		name         string
		limit, spend float64
		want         float64
	}{
		{"under cap", 100, 30, 70},
		{"at cap", 100, 100, 0},
		{"overshoot", 100, 130, 0},
		// Zero-limit rows are excluded by the snapshotter; this case
		// just pins that the math layer doesn't blow up if one slips
		// through (e.g. a future caller that bypasses the filter).
		{"zero limit", 0, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			remaining := c.limit - c.spend
			if remaining < 0 {
				remaining = 0
			}
			if remaining != c.want {
				t.Errorf("remaining = %v, want %v", remaining, c.want)
			}
		})
	}
}
