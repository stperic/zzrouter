package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/inferencelog"
	"github.com/stperic/zzrouter/pkg/model/pricing"
	"github.com/stperic/zzrouter/pkg/observability/llm"
)

type bridgePricingConfig struct{}

func (bridgePricingConfig) IsEnabled() bool                   { return true }
func (bridgePricingConfig) GetSource() string                 { return "" }
func (bridgePricingConfig) GetRefreshInterval() time.Duration { return time.Hour }

func bridgeWithPricing(t *testing.T, models map[string]pricing.ModelPricing) *InferenceLogBridge {
	t.Helper()
	dir := t.TempDir()
	blob, err := json.Marshal(map[string]any{"metadata": map[string]any{}, "models": models})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "pricing-cache.json"), blob, 0o600))

	store := pricing.NewStore(bridgePricingConfig{}, dir)
	return NewInferenceLogBridge(inferencelog.NewStore(64, 0), "test-node", false, store)
}

var bridgeGPT4o = pricing.ModelPricing{
	InputCostPerToken:  0.0000025,
	OutputCostPerToken: 0.00001,
	Provider:           "openai",
	Mode:               "chat",
}

// A group-routed request asks for an alias no price table knows. The
// response model is the priced id, and pricing it is the difference
// between a real charge and a silent $0 that no budget can catch.
func TestBridge_PricesGroupAliasViaResponseModel(t *testing.T) {
	b := bridgeWithPricing(t, map[string]pricing.ModelPricing{"gpt-4o": bridgeGPT4o})

	b.OnInferenceComplete(llm.InferenceLogData{
		App:           "openai",
		Model:         "fast-chat",
		ResponseModel: "gpt-4o",
		TokensIn:      1000,
		TokensOut:     500,
		Status:        "success",
	})

	entries := b.store.Query(inferencelog.QueryFilter{Limit: 10})
	require.Len(t, entries, 1)
	assert.Equal(t, "zzrouter", entries[0].CostSource)
	assert.InDelta(t, 1000*0.0000025+500*0.00001, entries[0].Cost, 1e-9)
	assert.Empty(t, b.pricingStore.Misses())
}

func TestBridge_RecordsMissWhenNothingPrices(t *testing.T) {
	b := bridgeWithPricing(t, map[string]pricing.ModelPricing{"gpt-4o": bridgeGPT4o})

	b.OnInferenceComplete(llm.InferenceLogData{
		App:           "ollama-cloud",
		Model:         "gpt-oss:120b",
		ResponseModel: "gpt-oss:120b",
		TokensIn:      1000,
		TokensOut:     500,
		Status:        "success",
	})

	assert.Equal(t, map[string]int64{"ollama-cloud/gpt-oss:120b": 1}, b.pricingStore.Misses())
}

// A provider-authoritative cost is not a pricing gap, and neither is a
// request that moved no tokens. Neither may pollute the operator's
// un-priced list.
func TestBridge_NoMissForProviderCostOrEmptyRequest(t *testing.T) {
	b := bridgeWithPricing(t, nil)

	b.OnInferenceComplete(llm.InferenceLogData{
		App: "openrouter", Model: "z-ai/glm-4.6",
		TokensIn: 1000, TokensOut: 500, Cost: 0.0042, Status: "success",
	})
	b.OnInferenceComplete(llm.InferenceLogData{
		App: "openai", Model: "gpt-4o", Status: "error", ErrorType: "upstream",
	})

	assert.Empty(t, b.pricingStore.Misses())
}
