package quota

import (
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/model/pricing"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stretchr/testify/require"
)

func TestPreviewBudgetMatchesAdmissionWithoutChangingLedger(t *testing.T) {
	paid := pricing.Quote{Provider: "cloud", Model: "paid", Rates: &pricing.ModelPricing{InputCostPerToken: 0.001}}
	free := pricing.Quote{Provider: "local", Model: "free", Rates: &pricing.ModelPricing{}}
	for _, tc := range []struct {
		name        string
		quotes      []pricing.Quote
		spent, held int64
		expired     bool
		tokens      int
	}{
		{name: "new paid", quotes: []pricing.Quote{paid}},
		{name: "unknown price", quotes: []pricing.Quote{{Provider: "cloud", Model: "unknown"}}},
		{name: "new free", quotes: []pricing.Quote{free}},
		{name: "spent", quotes: []pricing.Quote{paid}, spent: 1_000_000},
		{name: "reserved", quotes: []pricing.Quote{paid}, held: 1_000_000},
		{name: "free after limit", quotes: []pricing.Quote{free}, spent: 1_000_000},
		{name: "expired spend", quotes: []pricing.Quote{paid}, spent: 1_000_000, expired: true},
		{name: "oversized reservation", quotes: []pricing.Quote{paid}, tokens: 200_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnforcer()
			scope := QuotaScope{Scope: ScopeTeam, EntityID: "team", Quotas: QuotaConfig{SpendLimit: 1, ResetPeriod: "monthly", DefaultMaxTokens: tc.tokens}}
			if tc.spent > 0 || tc.held > 0 {
				start := PeriodStartFor(utils.NowUTC(), PeriodDaily)
				if tc.expired {
					start = start.Add(-48 * time.Hour)
				}
				e.tracker.states[(SpendScope{Kind: ScopeTeam, EntityID: "team"}).String()] = &SpendState{SpendMicro: tc.spent, ReservedMicro: tc.held, Period: PeriodDaily, PeriodStart: start}
			}
			before := e.GetAllSpendStates()
			preview := e.PreviewBudget(scope, tc.quotes)
			require.Equal(t, before, e.GetAllSpendStates(), "preview cannot create, reserve or roll over a ledger row")
			require.False(t, e.tracker.dirty)
			live, reservations := e.Check([]QuotaScope{scope}, tc.quotes)
			defer e.CancelReservations(reservations)
			require.Equal(t, live.Allowed, preview.Allowed)
			require.Equal(t, live.Reason, preview.Reason)
		})
	}
}
