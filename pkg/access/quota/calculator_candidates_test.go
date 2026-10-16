package quota

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/model/pricing"
)

type stubPricingConfig struct{}

func (stubPricingConfig) IsEnabled() bool                   { return true }
func (stubPricingConfig) GetSource() string                 { return "" }
func (stubPricingConfig) GetRefreshInterval() time.Duration { return time.Hour }

// storeWith seeds the on-disk cache the store lazy-loads, so the test
// exercises the real lookup path without a network fetch.
func storeWith(t *testing.T, models map[string]pricing.ModelPricing) *pricing.Store {
	t.Helper()
	dir := t.TempDir()
	blob, err := json.Marshal(map[string]any{
		"metadata": map[string]any{},
		"models":   models,
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "pricing-cache.json"), blob, 0o600))
	return pricing.NewStore(stubPricingConfig{}, dir)
}

var gpt4o = pricing.ModelPricing{
	InputCostPerToken:  0.0000025,
	OutputCostPerToken: 0.00001,
	Provider:           "openai",
	Mode:               "chat",
}

// A model-group alias exists only inside zzRouter, so no price table can
// know it. Pricing must fall to the id the upstream reported serving.
func TestCalculateCostMicro_PricesResponseModelWhenRequestWasAnAlias(t *testing.T) {
	store := storeWith(t, map[string]pricing.ModelPricing{"gpt-4o": gpt4o})

	micro, src := CalculateCostMicro(0, store, "openai", []string{"gpt-4o", "fast-chat"}, 1000, 500, 0, 0)

	assert.Equal(t, CostSourceZZRouter, src)
	assert.Equal(t, USDToMicro(1000*0.0000025+500*0.00001), micro)
}

// The requested name still prices when the upstream reported nothing
// usable, which is the common non-group case.
func TestCalculateCostMicro_FallsBackToRequestedModel(t *testing.T) {
	store := storeWith(t, map[string]pricing.ModelPricing{"gpt-4o": gpt4o})

	micro, src := CalculateCostMicro(0, store, "openai", []string{"", "gpt-4o"}, 1000, 500, 0, 0)

	assert.Equal(t, CostSourceZZRouter, src)
	assert.Equal(t, USDToMicro(1000*0.0000025+500*0.00001), micro)
}

// A candidate that resolves to a zero-priced entry is not "priced" —
// the search must continue rather than settling the request at $0.
func TestCalculateCostMicro_SkipsZeroPricedCandidate(t *testing.T) {
	store := storeWith(t, map[string]pricing.ModelPricing{
		"ollama/gpt-oss:120b": {Provider: "ollama", Mode: "chat"},
		"gpt-oss:120b-cloud":  gpt4o,
	})

	micro, src := CalculateCostMicro(0, store, "ollama", []string{"gpt-oss:120b", "gpt-oss:120b-cloud"}, 1000, 500, 0, 0)

	assert.Equal(t, CostSourceZZRouter, src)
	assert.Equal(t, USDToMicro(1000*0.0000025+500*0.00001), micro)
}

func TestCalculateCostMicro_AllCandidatesMiss(t *testing.T) {
	store := storeWith(t, map[string]pricing.ModelPricing{"gpt-4o": gpt4o})

	micro, src := CalculateCostMicro(0, store, "ollama-cloud", []string{"gpt-oss:120b", "fast-chat"}, 1000, 500, 0, 0)

	assert.Equal(t, int64(0), micro)
	assert.Equal(t, "", src, "no candidate priced ⇒ empty source ⇒ caller records a miss")
}

func TestPricingExplicitZero_LocalEvidenceBeatsTable(t *testing.T) {
	store := storeWith(t, map[string]pricing.ModelPricing{"model": gpt4o, "ambiguous": {}})
	micro, source := CalculateCostMicro(0, store, "local", []string{"model"}, 100, 50, 0, 0, pricing.Quote{Local: true, Rates: &pricing.ModelPricing{}})
	assert.Zero(t, micro)
	assert.Equal(t, CostSourceZZRouter, source)
	assert.Nil(t, pricing.Resolve(store, "cloud", []string{"ambiguous"}, false).Rates)
}

func TestBudgetQuoteSurvivesLocalOverrideDeletion(t *testing.T) {
	store := storeWith(t, map[string]pricing.ModelPricing{"model": gpt4o})
	require.NoError(t, store.SetOverride(pricing.Override{Provider: "local", Model: "model", InputCostPerToken: 0.001}))
	admitted := pricing.Resolve(store, "local", []string{"model"}, true)
	_, err := store.DeleteOverride("local", "model")
	require.NoError(t, err)
	micro, source := CalculateCostMicro(0, store, "local", []string{"model"}, 10, 0, 0, 0, admitted)
	assert.EqualValues(t, 10_000, micro)
	assert.Equal(t, CostSourceZZRouter, source)
}

func TestBudgetQuotePrefersResponseModel(t *testing.T) {
	store := storeWith(t, map[string]pricing.ModelPricing{"response": {InputCostPerToken: 0.01}})
	admitted := pricing.Quote{Provider: "cloud", Model: "concrete", Rates: &pricing.ModelPricing{InputCostPerToken: 0.001}}
	micro, source := CalculateCostMicro(0, store, "cloud", []string{"response", "concrete"}, 2, 0, 0, 0, admitted)
	assert.EqualValues(t, 20_000, micro)
	assert.Equal(t, CostSourceZZRouter, source)
}
