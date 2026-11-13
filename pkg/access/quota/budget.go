package quota

import (
	"fmt"

	"github.com/stperic/zzrouter/pkg/model/pricing"
)

// budgetEstimate owns the reservation amount, including unknown and zero prices.
func budgetEstimate(qs QuotaScope, quotes []pricing.Quote) (int64, Decision) {
	q := qs.Quotas
	if q.SpendLimit <= 0 {
		return 0, Decision{Allowed: true}
	}
	if len(quotes) == 0 {
		quotes = []pricing.Quote{{}}
	}
	free := true
	for _, quote := range quotes {
		if quote.Rates == nil {
			return 0, Decision{Reason: "model_unpriced", Detail: fmt.Sprintf("model %q on provider %q has no usable pricing; ask an administrator to enable pricing and POST rates to /zzrouter/v1/pricing/overrides", quote.Model, quote.Provider), Scope: qs.Scope, EntityID: qs.EntityID}
		}
		free = free && !quote.Rates.HasPricing() && quote.Rates.CacheReadCostPerToken == 0 && quote.Rates.OutputCostPerReasoningToken == 0
	}
	limitMicro := q.SpendLimitMicro()
	// Per-request budget reservation estimate.
	//
	// When DefaultMaxTokens is set explicitly, use it × 10µUSD
	// (the calibration constant: ~$0.00001/token across most
	// frontier models). When it's unset, derive a sane
	// reservation from the spend limit itself instead of a
	// flat 1024-token fallback.
	//
	// Why: a flat fallback ($0.01024 per reservation) silently
	// breaks any key with a spend limit below ~$0.01 — first
	// reservation trips even though actual cost is sub-cent.
	// The cap (≤ DefaultMaxTokensFallback × 10µ) preserves
	// existing behavior for high-budget keys; large limits
	// keep the historic estimate, which still gives 5+
	// concurrent reservations before tripping.
	var estimatedCostMicro int64
	if q.DefaultMaxTokens > 0 {
		estimatedCostMicro = int64(q.DefaultMaxTokens) * 10
	} else {
		estimatedCostMicro = limitMicro / 2
		if estimatedCostMicro < MinReservationMicro {
			estimatedCostMicro = MinReservationMicro
		}
		if max := int64(DefaultMaxTokensFallback) * 10; estimatedCostMicro > max {
			estimatedCostMicro = max
		}
	}

	if free {
		estimatedCostMicro = 0
	}

	return estimatedCostMicro, Decision{Allowed: true}
}

func budgetAvailable(state *SpendState, limit, estimate int64) bool {
	if estimate == 0 {
		return true
	}
	if state == nil || IsExpired(state.PeriodStart, state.Period) {
		return estimate <= limit
	}
	return state.SpendMicro+state.ReservedMicro+estimate <= limit
}

// PreviewBudget projects admission without reserving, rolling over or creating a row.
// A live request can observe newer state; rate and concurrency checks are separate.
func (e *Enforcer) PreviewBudget(scope QuotaScope, quotes []pricing.Quote) Decision {
	estimate, decision := budgetEstimate(scope, quotes)
	if !decision.Allowed || scope.Quotas.SpendLimit <= 0 || e.tracker == nil {
		return decision
	}
	state := e.tracker.GetState(SpendScope{Kind: scope.Scope, EntityID: scope.EntityID})
	if budgetAvailable(state, scope.Quotas.SpendLimitMicro(), estimate) {
		return Decision{Allowed: true}
	}
	return Decision{Reason: "budget_exhausted", Scope: scope.Scope, EntityID: scope.EntityID}
}
