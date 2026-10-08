package prov_apps

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
)

// withFamilyDefault adds a release-shipped default for the llama family
// to the tier provider, which already sets ctx-size at every operator
// tier for llama3.
func withFamilyDefault(svc *config.ServiceConfig) {
	svc.ModelDefaults = map[string]config.ModelSpec{
		"llama*": {Parameters: map[string]string{"template": "family.jinja", "ctx-size": "family"}},
	}
}

// An on-demand load carries no parameters of its own, so the family's
// template has to come out of the launch walk itself. Every operator
// tier and the request still beat it.
func TestResolveLaunchParams_ModelDefaultIsBelowEveryOperatorTier(t *testing.T) {
	cfg := &config.AppsConfig{}
	svc := tierServiceConfig()
	withFamilyDefault(&svc)
	require.NoError(t, cfg.AddApp("vllm", svc))
	m, err := NewProviderAppManager(cfg, WithNodename(func() string { return tierTestNode }))
	require.NoError(t, err)
	cleanupProviderManager(t, m)
	stored, ok := m.appsConfig.LookupApp("vllm")
	require.True(t, ok)

	params, _ := m.resolveLaunchParams(stored, LaunchRequest{Provider: "vllm", Model: "llama3"}, string(EndpointChat))
	assert.Equal(t, "family.jinja", params["template"], "a load from stored config gets the family default")
	assert.Equal(t, "tier3", params["ctx-size"], "the operator's tiers beat the family default")

	params, _ = m.resolveLaunchParams(stored, LaunchRequest{Provider: "vllm", Model: "llama-other"}, string(EndpointChat))
	assert.Equal(t, "tier2", params["ctx-size"], "the node tier beats it with no model cell either")
	assert.Equal(t, "family.jinja", params["template"])

	params, _ = m.resolveLaunchParams(stored, LaunchRequest{
		Provider: "vllm", Model: "llama3", Parameters: map[string]string{"template": "auto"},
	}, string(EndpointChat))
	assert.NotContains(t, params, "template", "auto drops the family's file")
}

// A release that adds a default for a family makes a run launched before
// it stale, which is what lets ?restart=affected bring it onto the new
// template after an upgrade.
func TestParametersStatus_NewModelDefaultIsStale(t *testing.T) {
	m := tierManager(t, "")
	inst, err := m.LaunchInstance(t.Context(), LaunchRequest{Provider: "vllm", Model: "llama-other"})
	require.NoError(t, err)
	require.Equal(t, instance.ParametersCurrent, m.ParametersStatus(inst).State)

	reloadWith(t, m, withFamilyDefault)
	got := m.ParametersStatus(inst)
	// ctx-size moves too: with no node or model entry for this model, the
	// family default beats the provider-wide one.
	assert.Equal(t, instance.ParametersStale, got.State)
	assert.Equal(t, []string{"parameters.ctx-size", "parameters.template"}, got.Changed)
}
