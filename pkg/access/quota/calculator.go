package quota

import "github.com/stperic/zzrouter/pkg/model/pricing"

// Closed-enum source tags returned by CalculateCostMicro. Mirror of
// pkg/dispatch/wire.CostSource but kept in this package to avoid a
// quota → wire dependency. The values agree by string equality, which
// is the contract the bridge relies on when threading the source
// through to the response inject path.
const (
	CostSourceProvider = "provider"
	CostSourceZZRouter = "zzrouter"
)

// CalculateCostMicro returns the cost in microdollars for a request,
// plus a closed-enum tag identifying where the number came from.
// Priority:
//  1. providerCost > 0 ⇒ (USDToMicro(providerCost), "provider")
//  2. pricing-store hit ⇒ (computed, "zzrouter")
//  3. otherwise ⇒ (0, "")
//
// models holds the candidate names to price, most-authoritative first;
// see lookupFirstPriced. Exhausting every candidate yields the empty
// source; settlement callers are expected to report that to
// pricing.Store.RecordMiss so un-priced traffic stays discoverable
// rather than silently free.
//
// An empty source means "no authoritative number available" — callers
// surfacing cost to clients should omit cost fields entirely in this
// case.
//
// Precision floor: the microdollar quantization rounds sub-microUSD
// values to 0. A pricing-store-computed cost of $4e-7 (e.g. tiny
// embedding requests) returns (0, "zzrouter"), distinct from the
// no-source case. Provider-source values pass through verbatim and
// are NOT round-tripped through the micro quantization, so upstream
// can report finer precision than the ledger stores. Both arms agree
// on the ledger micro number; only the LogEntry.Cost / response-body
// cost_usd float can carry extra digits on the provider arm.
func CalculateCostMicro(
	providerCost float64,
	pricingStore *pricing.Store,
	provider string,
	models []string,
	tokensIn, tokensOut, tokensCached, tokensReasoning int64,
) (int64, string) {
	// Priority 1: provider-reported cost
	if providerCost > 0 {
		return USDToMicro(providerCost), CostSourceProvider
	}

	// Priority 2: pricing store lookup
	if pricingStore == nil {
		return 0, ""
	}

	mp, ok := lookupFirstPriced(pricingStore, provider, models)
	if !ok {
		return 0, ""
	}

	baseIn := max(tokensIn-tokensCached, 0)

	cacheRate := mp.CacheReadCostPerToken
	if cacheRate == 0 {
		cacheRate = mp.InputCostPerToken
	}

	baseOut := max(tokensOut-tokensReasoning, 0)

	reasoningRate := mp.OutputCostPerReasoningToken
	if reasoningRate == 0 {
		reasoningRate = mp.OutputCostPerToken
	}

	costUSD := float64(baseIn)*mp.InputCostPerToken +
		float64(tokensCached)*cacheRate +
		float64(baseOut)*mp.OutputCostPerToken +
		float64(tokensReasoning)*reasoningRate

	return USDToMicro(costUSD), CostSourceZZRouter
}

// lookupFirstPriced returns the first candidate that resolves to usable
// pricing. Callers pass candidates most-authoritative first: the model
// the upstream reported serving, then the model the client asked for.
// The two differ whenever model-group routing resolves an alias
// ("fast-chat") to a concrete deployment, and no price table can know
// a locally-invented alias — so looking up only the requested name
// prices every group-routed request at zero.
func lookupFirstPriced(store *pricing.Store, provider string, models []string) (pricing.ModelPricing, bool) {
	// Operator overrides get their own pass ahead of the table: an
	// all-zero override is a deliberate "this provider does not bill per
	// token" declaration, and the HasPricing gate below would discard it
	// as a miss.
	for _, model := range models {
		if model == "" {
			continue
		}
		if mp, ok := store.LookupOverride(provider, model); ok {
			return mp, true
		}
	}

	for _, model := range models {
		if model == "" {
			continue
		}
		var mp pricing.ModelPricing
		var ok bool
		if provider != "" {
			mp, ok = store.LookupByProvider(provider, model)
		}
		if !ok {
			mp, ok = store.Lookup(model)
		}
		if ok && mp.HasPricing() {
			return mp, true
		}
	}
	return pricing.ModelPricing{}, false
}
