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
`

func TestNodeScopeConfigRoundTrip(t *testing.T) {
	require.NoError(t, configschema.ValidateOnDemand([]byte(nodeProviderBody)))
	var provider OnDemandProvider
	require.NoError(t, yaml.Unmarshal([]byte(nodeProviderBody), &provider))
	provider.Name = "studio"
	require.NoError(t, provider.Validate())
	view := ProviderServiceConfig(&provider)
	require.Equal(t, RunScopeNode, view.RunScope)
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

func TestNodeScopeRejectsRetiredActivityProbe(t *testing.T) {
	for _, probe := range []string{
		"{path: /queue, busy_when_nonempty: [queue_running]}",
		"{path: http://remote/queue, busy_when_nonempty: [running]}",
		"{path: //remote/queue, busy_when_nonempty: [running]}",
		"{path: /queue}",
		"{path: /queue, busy_when_nonempty: [running, running]}",
	} {
		body := []byte(nodeProviderBody + "  activity_probe: " + probe + "\n")
		require.ErrorContains(t, configschema.ValidateOnDemand(body), "activity_probe")
		_, err := DecodeProviderBytes(KindOnDemand, "studio", body)
		require.ErrorContains(t, err, "activity_probe")
	}
}
