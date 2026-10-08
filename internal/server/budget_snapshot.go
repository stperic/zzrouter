package server

import (
	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/quota"
	"github.com/stperic/zzrouter/pkg/access/teams"
	"github.com/stperic/zzrouter/pkg/observability/spend"
)

// budgetSnapshotter implements spend.BudgetSnapshotter against zzrouter's
// virtual-key store, the AccessControl-fronted spend ledger, and the
// team store (for team_alias). Constructed at server startup once
// AccessControl is wired and registered with the observability spend
// package's observable budget gauges.
//
// The OTel callback that drives this snapshotter runs synchronously
// inside the metric collection cycle. We accept that cost (a list +
// per-key map lookup) over the alternatives:
//   - Cache stale: a budget gauge that lags by minutes is worse than
//     spending a few microseconds on each scrape.
//   - Cache fresh: would need a write path on every Create/Update/
//     Delete/Settle, multiplying the surface area where the gauges
//     can drift from reality.
type budgetSnapshotter struct {
	keys   keys.KeyStore
	teams  teams.TeamStore
	access *AccessControl
}

func newBudgetSnapshotter(ks keys.KeyStore, ts teams.TeamStore, ac *AccessControl) *budgetSnapshotter {
	return &budgetSnapshotter{keys: ks, teams: ts, access: ac}
}

// SnapshotKeyBudgets returns one row per virtual key with a
// non-zero SpendLimit. Keys without a configured cap are skipped —
// SpendLimit=0 means "unenforced" by zzrouter convention; emitting
// "max=0, remaining=0" rows would imply a $0 hard cap, which is the
// opposite of the actual behaviour.
//
// Performance contract: walks the keystore + spend-ledger snapshot
// under the OTel collection cycle's lock. Pre-fetches the entire
// SpendState map in a single call so the per-key loop doesn't take
// the tracker mutex N times — a deployment with thousands of keys
// would otherwise contend with concurrent settle traffic on every
// scrape.
func (s *budgetSnapshotter) SnapshotKeyBudgets() []spend.KeyBudget {
	if s == nil || s.keys == nil {
		return nil
	}
	all := s.keys.List()
	if len(all) == 0 {
		return nil
	}
	// Single mutex acquisition on the spend tracker; keyed by
	// SpendScope.String() = "key:<id>:". Empty Model segment matches
	// the unscoped per-key entry RecordSpendByKey writes.
	spendStates := s.access.GetAllSpendStates()
	out := make([]spend.KeyBudget, 0, len(all))
	for _, vk := range all {
		if vk == nil || vk.SpendLimit <= 0 {
			continue
		}
		var (
			currentSpendUSD float64
			teamAlias       string
		)
		// LiteLLM's litellm_remaining_api_key_budget_metric reflects
		// the live in-period available headroom, which the enforcer
		// computes as limit - spend - reserved (see
		// quota/tracker.go's reserveBudgetLocked admission gate).
		// Including reserved here keeps the gauge consistent with
		// the gate so {remaining < N} alert queries fire at the same
		// boundary the enforcer denies new traffic at.
		if state := spendStates[(quota.SpendScope{Kind: quota.ScopeKey, EntityID: vk.ID}).String()]; state != nil {
			currentSpendUSD = quota.MicroToUSD(state.SpendMicro + state.ReservedMicro)
		}
		if vk.TeamID != "" && s.teams != nil {
			if team := s.teams.Get(vk.TeamID); team != nil {
				teamAlias = team.Name
			}
		}
		// hashed_api_key is intentionally empty on budget gauges:
		// the runtime metrics derive it from KeyFingerprint(rawKey) at
		// auth time, but the keystore only retains an Argon2id digest
		// of the raw secret, not a fingerprint that matches the runtime
		// label. Persisting a stable fingerprint on VirtualKey at
		// create-time is the right long-term fix; until then,
		// dashboards should group budget gauges by api_key_alias (or
		// the team_alias / team labels), not by hashed_api_key.
		out = append(out, spend.KeyBudget{
			Labels: spend.CallerLabels{
				APIKeyAlias: vk.Name,
				Team:        vk.TeamID,
				TeamAlias:   teamAlias,
			},
			SpendLimitUSD:   vk.SpendLimit,
			CurrentSpendUSD: currentSpendUSD,
		})
	}
	return out
}
