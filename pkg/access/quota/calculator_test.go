package quota

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/stperic/zzrouter/pkg/model/pricing"
)

func TestCalculateCostMicro_ProviderSource(t *testing.T) {
	cost := 0.000001989
	micro, src := CalculateCostMicro(&cost, nil, "openrouter", []string{"gpt-4"}, 19, 3, 0, 0)
	assert.Equal(t, USDToMicro(0.000001989), micro)
	assert.Equal(t, CostSourceProvider, src)
}

func TestCalculateCostMicro_NoSource(t *testing.T) {
	micro, src := CalculateCostMicro(nil, nil, "", []string{"unknown"}, 10, 5, 0, 0)
	assert.Equal(t, int64(0), micro)
	assert.Equal(t, "", src,
		"no provider cost AND no pricing store ⇒ empty source ⇒ omit from wire")
}

func TestProviderCostPresence(t *testing.T) {
	store := storeWith(t, map[string]pricing.ModelPricing{"gpt-4o": gpt4o})
	for _, tc := range []struct {
		name   string
		cost   *float64
		micro  int64
		source string
	}{
		{"positive", new(0.000001989), 1, CostSourceProvider},
		{"sub micro", new(0.0000004), 0, CostSourceProvider},
		{"zero", new(0.0), 0, CostSourceProvider},
		{"missing", nil, 7500, CostSourceZZRouter},
		{"negative credit", new(-0.01), 7500, CostSourceZZRouter},
	} {
		t.Run(tc.name, func(t *testing.T) {
			micro, source := CalculateCostMicro(tc.cost, store, "openai", []string{"gpt-4o"}, 1000, 500, 0, 0)
			assert.Equal(t, tc.micro, micro)
			assert.Equal(t, tc.source, source)
		})
	}
}
