package spend

import (
	"context"
	"log/slog"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

// KeyBudget is the per-virtual-key view consumed by the budget
// observable gauges. CurrentSpendUSD reflects the live in-period
// consumption read from the spend tracker; SpendLimitUSD is the
// configured cap. RemainingUSD is computed by the caller as
// max(SpendLimitUSD - CurrentSpendUSD, 0) so a settle that overshot
// the cap doesn't emit a negative gauge value (most Prometheus query
// patterns don't handle negative gauges gracefully).
type KeyBudget struct {
	Labels          CallerLabels
	SpendLimitUSD   float64
	CurrentSpendUSD float64
}

// BudgetSnapshotter returns the current per-key budget rows the
// observable gauges should report. The snapshotter is queried
// synchronously inside the OTel collection cycle, so implementations
// must return promptly and must be safe for concurrent use.
//
// Only keys with a configured SpendLimit > 0 should appear in the
// snapshot — SpendLimit=0 means "unenforced" by zzrouter convention,
// not "$0 hard cap", and surfacing those rows would emit confusing
// "remaining = -spend" gauges.
type BudgetSnapshotter interface {
	SnapshotKeyBudgets() []KeyBudget
}

// budgetState holds the registered gauges + the current snapshotter.
type budgetState struct {
	maxBudget       metric.Float64ObservableGauge
	remainingBudget metric.Float64ObservableGauge
	registration    metric.Registration
	provider        metric.MeterProvider
}

var (
	budgetMu          sync.Mutex
	budgetGlobal      *budgetState
	budgetSnapshotter BudgetSnapshotter
)

// SetBudgetSnapshotter registers (or replaces) the snapshotter the
// observable gauges query. Lazy-registers the gauges on the current
// global meter provider on first call, and re-registers when the
// provider changes (matches the rebuild-on-provider-change pattern
// used elsewhere in pkg/observability/* so test isolation works).
//
// Pass nil to clear the snapshotter — the gauges will continue to
// observe but contribute no rows. This is the cleanup path for tests
// and the shutdown path for the server.
func SetBudgetSnapshotter(snap BudgetSnapshotter) {
	budgetMu.Lock()
	defer budgetMu.Unlock()
	budgetSnapshotter = snap

	current := otel.GetMeterProvider()
	if budgetGlobal != nil && budgetGlobal.provider == current {
		return
	}
	if budgetGlobal != nil && budgetGlobal.registration != nil {
		_ = budgetGlobal.registration.Unregister()
	}
	state, err := newBudgetState()
	if err != nil {
		slog.Warn("zz budget gauges init failed",
			"error", err,
			"impact", "zz.api.key.max.budget.metric and zz.remaining.api.key.budget.metric will not emit",
		)
		budgetGlobal = nil
		return
	}
	budgetGlobal = state
}

func newBudgetState() (*budgetState, error) {
	meter := otel.Meter("zzrouter.spend")

	maxBudget, err := meter.Float64ObservableGauge(
		"zz.api.key.max.budget.metric",
		metric.WithDescription("Configured spend limit per virtual key, in USD"),
	)
	if err != nil {
		return nil, err
	}
	remainingBudget, err := meter.Float64ObservableGauge(
		"zz.remaining.api.key.budget.metric",
		metric.WithDescription("Remaining in-period budget per virtual key, in USD (clamped at zero)"),
	)
	if err != nil {
		return nil, err
	}

	state := &budgetState{
		maxBudget:       maxBudget,
		remainingBudget: remainingBudget,
		provider:        otel.GetMeterProvider(),
	}

	// One callback observes both gauges from a single snapshot — the
	// per-key walk happens once per scrape, not twice. Registering
	// against both instruments simultaneously is the OTel-spec'd way
	// to keep label tuples consistent across related gauges.
	reg, err := meter.RegisterCallback(
		func(ctx context.Context, observer metric.Observer) error {
			budgetMu.Lock()
			snap := budgetSnapshotter
			budgetMu.Unlock()
			if snap == nil {
				return nil
			}
			rows := snap.SnapshotKeyBudgets()
			for _, b := range rows {
				attrs := metric.WithAttributes(b.Labels.attributes()...)
				observer.ObserveFloat64(state.maxBudget, b.SpendLimitUSD, attrs)
				remaining := b.SpendLimitUSD - b.CurrentSpendUSD
				if remaining < 0 {
					remaining = 0
				}
				observer.ObserveFloat64(state.remainingBudget, remaining, attrs)
			}
			return nil
		},
		maxBudget, remainingBudget,
	)
	if err != nil {
		return nil, err
	}
	state.registration = reg
	return state, nil
}
