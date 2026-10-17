package server

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/config"
)

// A preview reports the tier each value came from in the walk of the node
// that rendered it, so a request that sent no parameters claims none.
func TestIntegration_PreviewReportsWhereValuesCameFrom(t *testing.T) {
	cfg := DefaultTestNodeConfig()
	server := createSeededTestNode(t, seededTestNodeConfig{TestNodeConfig: cfg})

	// Give the node a provider whose defaults exist, rather than hoping
	// the shared test config has one: what is under test is the
	// reporting, and a skipped test reports nothing.
	const provider = "previewprov"
	require.NoError(t, server.appsConfig().AddApp(provider, config.ServiceConfig{
		Enabled:  new(true),
		Name:     "Preview Provider",
		Protocol: config.ProtocolOpenAI,
		Mode:     "on-demand",
		Runtime: &config.AppRuntimeConfig{
			PortRange: []int{8300, 8305},
			BasePort:  8300,
			Execution: config.ExecutionConfig{Type: "cli", Command: "previewprov"},
		},
		// A provider with no schema.yaml declares only the run settings.
		Defaults: &config.AppDefaultsConfig{
			Parameters: map[string]string{"keep-alive": "10m"},
		},
		Capabilities: &config.AppCapabilities{WireEndpoints: []string{"chat_completions"}},
	}))

	resp := makeAuthRequest(t, server, "POST", "/zzrouter/v1/runs/preview", TestAdminKey,
		map[string]any{"model_name": "some-model", "provider": provider})
	require.Equal(t, 200, resp.Code, "body=%s", string(resp.Body))

	var env struct {
		Data struct {
			ParameterSources map[string]string `json:"parameter_sources"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(resp.Body, &env))
	require.NotEmpty(t, env.Data.ParameterSources, "preview reported no provenance at all")

	for key, source := range env.Data.ParameterSources {
		assert.NotEqual(t, config.TierRequest.String(), source,
			"preview says the caller supplied %q, but this request sent no parameters", key)
	}
}
