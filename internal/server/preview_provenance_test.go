package server

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/config"
)

// preMergeForRequest flattens the tier tree into a plain map of values
// before the request leaves the coordinator. The node that receives it
// sees every value arrive as a request parameter and can only report
// them as `request` -- accurate about the wire it saw, wrong about
// where the values came from. An operator reading that in the CLI is
// told they sent values they never sent.
//
// The coordinator owns the provider tree (a worker's copy is a cache),
// so it is the only place that can answer this truthfully, which is why
// the tiers are carried out of the merge rather than recomputed later.
func TestPreMergeForRequest_ReportsTheTierEachValueCameFrom(t *testing.T) {
	cfg := &config.AppsConfig{}
	require.NoError(t, cfg.AddApp("vllm", config.ServiceConfig{
		Enabled:  new(true),
		Name:     "vLLM",
		Protocol: config.ProtocolOpenAI,
		Mode:     "on-demand",
		Runtime: &config.AppRuntimeConfig{
			PortRange: []int{8100, 8105},
			BasePort:  8100,
			Execution: config.ExecutionConfig{Type: "cli", Command: "vllm"},
		},
		Defaults: &config.AppDefaultsConfig{
			Parameters: map[string]string{
				"gpu-memory-utilization": "0.95",
				"max-model-len":          "8192",
			},
		},
		Capabilities: &config.AppCapabilities{WireEndpoints: []string{"chat_completions"}},
	}))

	req := &LoadModelRequest{
		Provider:  "vllm",
		ModelName: "some-model",
		// The one value the caller really did send.
		Parameters: map[string]string{"max-model-len": "4096"},
	}

	sources := preMergeForRequest(cfg, req, "worker1")
	require.NotNil(t, sources)

	assert.Equal(t, config.TierDefault.String(), sources["gpu-memory-utilization"],
		"a value from the provider's defaults was reported as caller-supplied")
	assert.Equal(t, config.TierRequest.String(), sources["max-model-len"],
		"a value the caller actually sent must still read as request")

	// The merge itself must be unchanged: request wins over defaults.
	assert.Equal(t, "4096", req.Parameters["max-model-len"])
	assert.Equal(t, "0.95", req.Parameters["gpu-memory-utilization"])
}

// Nothing to merge means nothing to claim. An unknown provider or a
// missing config must not invent provenance.
func TestPreMergeForRequest_ClaimsNothingWithoutATree(t *testing.T) {
	req := &LoadModelRequest{Provider: "vllm", ModelName: "m"}
	assert.Nil(t, preMergeForRequest(nil, req, "worker1"))

	empty := &config.AppsConfig{}
	assert.Nil(t, preMergeForRequest(empty, &LoadModelRequest{Provider: "nope", ModelName: "m"}, "worker1"))
	assert.Nil(t, preMergeForRequest(empty, &LoadModelRequest{ModelName: "m"}, "worker1"))

	// No target is the same as no tree: there is no node whose tiers
	// this could be resolving. Asserted against a config that DOES hold
	// the provider — against `empty` it would pass on the provider
	// lookup failing first, and would go on passing with the target
	// check deleted.
	assert.Nil(t, preMergeForRequest(tierProbeConfig(t), &LoadModelRequest{Provider: "vllm", ModelName: "m"}, ""))
}

// The unit test above proves the tiers are computed. This drives the
// actual route, because the tiers being right in the merge and the
// response reporting them are two separate things -- and the second is
// where they were lost. Removing the controller's stamp passes every
// other test in this package.
func TestIntegration_PreviewReportsWhereValuesCameFrom(t *testing.T) {
	cfg := DefaultTestNodeConfig()
	cfg.SeedProvidersDir = true
	server := createTestNode(t, cfg)

	// Give the node a provider whose defaults exist, rather than hoping
	// the shared test config has one: what is under test is the
	// reporting, and a skipped test reports nothing.
	const provider = "previewprov"
	require.NoError(t, server.appsConfig.AddApp(provider, config.ServiceConfig{
		Enabled:  new(true),
		Name:     "Preview Provider",
		Protocol: config.ProtocolOpenAI,
		Mode:     "on-demand",
		Runtime: &config.AppRuntimeConfig{
			PortRange: []int{8300, 8305},
			BasePort:  8300,
			Execution: config.ExecutionConfig{Type: "cli", Command: "previewprov"},
		},
		Defaults: &config.AppDefaultsConfig{
			Parameters: map[string]string{"gpu-memory-utilization": "0.95"},
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
