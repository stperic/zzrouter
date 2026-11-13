package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProviderBrowserContractRoundTripsTypedRuntime(t *testing.T) {
	svc := sourceServiceFixture()
	svc.Runtime.Browser = &ProviderBrowser{Name: "Engine", RequiredParameters: map[string]string{"listen": "127.0.0.1", "enable-manager": "false"}}
	typed, err := serviceConfigToProvider("engine", svc)
	require.NoError(t, err)
	require.NoError(t, typed.(*OnDemandProvider).validateBrowser())
	roundTrip := providerToServiceConfig(typed)
	require.Equal(t, svc.Runtime.Browser, roundTrip.Runtime.Browser)
}

func TestProviderBrowserContractRequiresBoundedSourceParameters(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*OnDemandProvider)
	}{
		{"model scoped", func(p *OnDemandProvider) { p.RunScope = "model" }},
		{"package recipe", func(p *OnDemandProvider) { p.Install = nil }},
		{"empty name", func(p *OnDemandProvider) { p.Runtime.Browser.Name = "" }},
		{"empty contract", func(p *OnDemandProvider) { p.Runtime.Browser.RequiredParameters = nil }},
		{"uncanonical flag", func(p *OnDemandProvider) { p.Runtime.Browser.RequiredParameters["enable_manager"] = "false" }},
		{"invalid value", func(p *OnDemandProvider) { p.Runtime.Browser.RequiredParameters["listen"] = "127.0.0.1\n" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &OnDemandProvider{Name: "engine", RunScope: RunScopeNode, Install: &InstallConfig{Runtimes: map[string]InstallRecipe{"engine": sourceRecipeFixture()}}, Runtime: OnDemandRuntime{Browser: &ProviderBrowser{Name: "Engine", RequiredParameters: map[string]string{"listen": "127.0.0.1"}}}}
			tc.change(p)
			require.Error(t, p.validateBrowser())
		})
	}
}
