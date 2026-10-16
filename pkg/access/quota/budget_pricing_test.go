package quota

import (
	"testing"

	"github.com/stperic/zzrouter/pkg/model/pricing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBudgetPricingRollbackAndOrder(t *testing.T) {
	for _, kind := range []string{"rpm", "tpm"} {
		t.Run(kind, func(t *testing.T) {
			e := newTestEnforcer()
			cfg := QuotaConfig{SpendLimit: 1}
			scope := ThrottleScope{Kind: ScopeKey, EntityID: "key"}
			if kind == "rpm" {
				cfg.RPMLimit = 1
				require.True(t, e.rateLimiter.AllowRPM(scope, 1))
			} else {
				cfg.TPMLimit = 1
				e.RecordTokens(scope, 1)
			}
			d, res := e.Check([]QuotaScope{{Scope: ScopeKey, EntityID: "key", Quotas: cfg}}, []pricing.Quote{{Provider: "cloud", Model: "unknown"}})
			assert.Equal(t, kind+"_limit_exceeded", d.Reason)
			assert.Empty(t, res)
			assert.Nil(t, e.GetSpendState(SpendScope{Kind: ScopeKey, EntityID: "key"}))
		})
	}
	for _, kind := range []string{"rpm", "tpm", "budget"} {
		t.Run("refund-"+kind, func(t *testing.T) {
			e := newTestEnforcer()
			scope := ThrottleScope{Kind: ScopeTeam, EntityID: "team"}
			cfg := QuotaConfig{SpendLimit: 0.000001, DefaultMaxTokens: 1}
			if kind == "rpm" {
				cfg.RPMLimit = 1
				require.True(t, e.rateLimiter.AllowRPM(scope, 1))
			}
			if kind == "tpm" {
				cfg.TPMLimit = 1
				e.RecordTokens(scope, 1)
			}
			chain := []QuotaScope{{Scope: ScopeKey, EntityID: "key", Quotas: QuotaConfig{SpendLimit: 1}}, {Scope: ScopeTeam, EntityID: "team", Quotas: cfg}}
			d, res := checkPaid(e, chain)
			require.False(t, d.Allowed)
			assert.Empty(t, res)
			state := e.GetSpendState(SpendScope{Kind: ScopeKey, EntityID: "key"})
			require.NotNil(t, state)
			assert.Zero(t, state.ReservedMicro)
		})
	}
}
