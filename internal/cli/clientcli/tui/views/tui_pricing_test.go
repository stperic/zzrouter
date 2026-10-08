package views

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/ui"
)

func newPricingView(t *testing.T, overrides []pkgClient.PricingOverride, unpriced map[string]int64) *PricingViewModel {
	t.Helper()
	v := NewPricingViewModel(nil, ui.NewStyles(ui.CatppuccinMocha()))
	updated, _ := v.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	v = updated.(*PricingViewModel)

	updated, _ = v.Update(pricingLoadedMsg{
		overrides: overrides,
		status:    &pkgClient.PricingStatus{Enabled: true, Unpriced: unpriced, UnpricedCount: len(unpriced)},
	})
	return updated.(*PricingViewModel)
}

// The tally key is "provider/model", and model ids carry slashes of their
// own — only the first segment is the provider.
func TestPricing_SplitsUnpricedKeyOnFirstSlashOnly(t *testing.T) {
	provider, model := splitUnpricedKey("openrouter/meta-llama/llama-3.3-70b-instruct")
	assert.Equal(t, "openrouter", provider)
	assert.Equal(t, "meta-llama/llama-3.3-70b-instruct", model)

	provider, model = splitUnpricedKey("bare-model")
	assert.Empty(t, provider)
	assert.Equal(t, "bare-model", model)
}

// The un-priced list is a work queue: the model burning the most requests
// at $0 must be at the top.
func TestPricing_UnpricedSortedByRequestsDescending(t *testing.T) {
	v := newPricingView(t, nil, map[string]int64{
		"groq/llama-3.3-70b-versatile": 3,
		"ollama-cloud/gpt-oss:120b":    99,
		"cloudflare/@cf/meta/llama":    50,
	})

	require.Len(t, v.unpriced, 3)
	assert.Equal(t, "gpt-oss:120b", v.unpriced[0].model)
	assert.Equal(t, int64(99), v.unpriced[0].requests)
	assert.Equal(t, "llama-3.3-70b-versatile", v.unpriced[2].model)
}

// Enter on an un-priced row is the whole workflow: it must arrive in the
// form already scoped to that model, not blank.
func TestPricing_EnterOnUnpricedPrefillsForm(t *testing.T) {
	v := newPricingView(t, nil, map[string]int64{"ollama-cloud/gpt-oss:120b": 5})

	updated, _ := v.Update(keyPress("u"))
	v = updated.(*PricingViewModel)
	require.Equal(t, pricingViewUnpriced, v.mode)

	updated, _ = v.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	v = updated.(*PricingViewModel)

	require.Equal(t, pricingViewForm, v.mode)
	assert.Equal(t, "ollama-cloud", v.formProvider.Value())
	assert.Equal(t, "gpt-oss:120b", v.formModel.Value())
	assert.False(t, v.editing, "pricing an un-priced model creates a rate, it does not edit one")
}

func TestPricing_FormRejectsUnscopedWildcardAndBadRate(t *testing.T) {
	v := newPricingView(t, nil, nil)

	v.openForm(pkgClient.PricingOverride{Model: "*"}, false)
	cmd := v.saveForm()
	assert.Nil(t, cmd, "a wildcard with no provider must not reach the server")
	assert.Contains(t, v.statusMsg, "wildcard")

	v.openForm(pkgClient.PricingOverride{Provider: "groq", Model: "llama"}, false)
	v.formInput.SetValue("free please")
	cmd = v.saveForm()
	assert.Nil(t, cmd)
	assert.Contains(t, v.statusMsg, "Input rate")
}

// Blank and "0" both mean zero, and a leading $ is what an operator
// copying from a price page will paste.
func TestPricing_ParseRateAcceptsBlankZeroAndDollarSign(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want float64
	}{{"", 0}, {"0", 0}, {" 0.5 ", 0.5}, {"$2.50", 2.5}} {
		got, err := parseRate(tc.in)
		require.NoError(t, err, "input %q", tc.in)
		assert.InDelta(t, tc.want, got, 1e-9, "input %q", tc.in)
	}
	_, err := parseRate("cheap")
	assert.Error(t, err)
}

// A zero rate is a deliberate declaration, not a missing value, so it
// must not render as blank.
func TestPricing_ZeroRateRendersAsFree(t *testing.T) {
	assert.Equal(t, "free", formatRate(0))
	assert.Equal(t, "$0.5", formatRate(0.5))

	v := newPricingView(t, []pkgClient.PricingOverride{{
		Provider: "ollama-cloud", Model: "*", AppliesToAllModels: true,
		Note: "subscription billed",
	}}, nil)

	out := v.viewContent()
	assert.Contains(t, out, "free")
	assert.Contains(t, out, "all models")
}

// An operator with nothing overridden but traffic settling at $0 needs to
// be told where to go, not shown a bare "nothing here".
func TestPricing_EmptyStateNamesTheUnpricedBacklog(t *testing.T) {
	v := newPricingView(t, nil, map[string]int64{"ollama-cloud/gpt-oss:120b": 5})
	assert.Contains(t, v.emptyText(), "Press U")

	v = newPricingView(t, nil, nil)
	assert.Contains(t, v.emptyText(), "Upstream pricing")
}

func TestPricing_DeleteAsksBeforeRemoving(t *testing.T) {
	v := newPricingView(t, []pkgClient.PricingOverride{{Provider: "groq", Model: "llama-3.3-70b-versatile"}}, nil)

	updated, _ := v.Update(keyPress("d"))
	v = updated.(*PricingViewModel)
	require.True(t, v.confirmPending)
	assert.Contains(t, v.viewContent(), "Upstream pricing takes over")

	updated, _ = v.Update(keyPress("n"))
	v = updated.(*PricingViewModel)
	assert.False(t, v.confirmPending, "declining must leave the override in place")
}
