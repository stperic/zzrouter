package config

import (
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	configschema "github.com/stperic/zzrouter/pkg/config/schema"
)

const nodeProviderBody = `run_scope: node
runtime:
  port_range: [8188, 8188]
  base_port: 8188
  execution: {type: python, command: python}
  activity_probe:
    path: /queue
    busy_when_nonempty: [queue_running, queue_pending]
`

func TestNodeScopeConfigRoundTrip(t *testing.T) {
	require.NoError(t, configschema.ValidateOnDemand([]byte(nodeProviderBody)))
	var provider OnDemandProvider
	require.NoError(t, yaml.Unmarshal([]byte(nodeProviderBody), &provider))
	provider.Name = "studio"
	require.NoError(t, provider.Validate())
	view := providerToServiceConfig(&provider)
	require.Equal(t, RunScopeNode, view.RunScope)
	require.Equal(t, provider.Runtime.ActivityProbe, view.Runtime.ActivityProbe)
	converted, err := serviceConfigToProvider(provider.Name, view)
	require.NoError(t, err)
	require.Equal(t, &provider, converted)
	saved, err := yaml.Marshal(converted)
	require.NoError(t, err)
	require.NoError(t, configschema.ValidateOnDemand(saved))
	reloaded, err := DecodeProviderBytes(KindOnDemand, provider.Name, saved)
	require.NoError(t, err)
	require.Equal(t, &provider, reloaded)
}

func TestNodeScopeRejectsModelConfiguration(t *testing.T) {
	for name, mutate := range map[string]func(*OnDemandProvider){
		"protocol": func(p *OnDemandProvider) { p.Protocol = ProtocolOpenAI },
		"models":   func(p *OnDemandProvider) { p.Models = map[string]ModelSpec{"x": {}} },
		"node models": func(p *OnDemandProvider) {
			p.Nodes = map[string]NodeSpec{"n": {Models: map[string]NodeModelSpec{"x": {}}}}
		},
		"formats": func(p *OnDemandProvider) { p.Capabilities = &AppCapabilities{Formats: []string{"gguf"}} },
		"wire":    func(p *OnDemandProvider) { p.Capabilities = &AppCapabilities{WireEndpoints: []string{"embeddings"}} },
		"scope":   func(p *OnDemandProvider) { p.RunScope = "arbitrary" },
	} {
		t.Run(name, func(t *testing.T) {
			var p OnDemandProvider
			require.NoError(t, yaml.Unmarshal([]byte(nodeProviderBody), &p))
			mutate(&p)
			require.Error(t, p.Validate())
		})
	}
}

func TestActivityProbeValidation(t *testing.T) {
	for _, probe := range []*ActivityProbe{
		{Path: "http://remote/queue", BusyWhenNonempty: []string{"running"}},
		{Path: "//remote/queue", BusyWhenNonempty: []string{"running"}},
		{Path: "/queue"},
		{Path: "/queue", BusyWhenNonempty: []string{"running", "running"}},
	} {
		require.Error(t, probe.Validate())
	}
	require.NoError(t, (*ActivityProbe)(nil).Validate())
}
