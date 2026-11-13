package search

import (
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stperic/zzrouter/pkg/ui"
	"github.com/stretchr/testify/assert"
)

func TestCloudDiscoveryDetailPreservesTokenUnitsAndBothPrices(t *testing.T) {
	for _, tc := range []struct {
		name, prompt, completion, price string
	}{
		{"completion only", "0", "0.000002", "$0.000/$2.00"},
		{"subcent", "0.000000002", "0.000000003", "$0.002/$0.003"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := presentationResult(metadata.DiscoveryResult{Discovery: metadata.Discovery{
				Provider: "openai", APIID: "model", DisplayName: "Friendly model", ContextLength: 131072,
				PricePrompt: tc.prompt, PriceComplete: tc.completion,
			}})
			m := activeSearch(t, &discoveryFixture{})
			m.styles = ui.NewStyles(ui.CatppuccinMocha())
			m.providerConfigs = map[string]searchProviderConfig{"openai": {IsCloud: true}}
			m.selectedItem = &result
			rendered := ansi.Strip(m.viewDetail())
			assert.Contains(t, rendered, "131K")
			assert.Contains(t, rendered, tc.price, "both per-token prices must be scaled once to per-million units")
			assert.Contains(t, rendered, "Price (in/out, per M)")
			assert.NotContains(t, rendered, "Price (out)", "completion price belongs to the shared input/output presentation")
		})
	}
}
