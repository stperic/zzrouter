package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps"
)

// The node's runs listing reports parameters_status only when asked: it
// re-resolves every live run, and most callers (a launch looking for an
// instance to reuse) have no use for it.
func TestInternalListRuns_ParametersStatusOnlyWhenAsked(t *testing.T) {
	cfg := &pkgConfig.AppsConfig{}
	require.NoError(t, cfg.AddApp("vllm", pkgConfig.ServiceConfig{
		Enabled: new(true), Name: "vLLM", Protocol: pkgConfig.ProtocolOpenAI, Mode: "on-demand",
		Runtime: &pkgConfig.AppRuntimeConfig{
			PortRange: []int{8100, 8105}, BasePort: 8100,
			Execution: pkgConfig.ExecutionConfig{Type: "python", Command: "python3", Args: []string{"--port", "${PORT}"}},
		},
		Capabilities: &pkgConfig.AppCapabilities{WireEndpoints: []string{"chat_completions"}},
	}))
	mgr, err := prov_apps.NewProviderAppManager(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Stop(t.Context()) })
	_, err = mgr.LaunchInstance(t.Context(), prov_apps.LaunchRequest{Provider: "vllm", Model: "llama3"})
	require.NoError(t, err)
	e := &RunsExecutor{appMgr: mgr, getNodename: func() string { return "n" }, appsConfig: func() *pkgConfig.AppsConfig { return cfg }}

	list := func(path string) map[string]any {
		c, rec := testGinContext(http.MethodGet, path)
		e.HandleInternalListRuns(c)
		require.Equal(t, http.StatusOK, rec.Code)
		var body struct {
			Instances []map[string]any `json:"instances"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		require.Len(t, body.Instances, 1)
		return body.Instances[0]
	}
	assert.NotContains(t, list("/zzrouter/v1/internal/runs"), "parameters_status")
	assert.Contains(t, list("/zzrouter/v1/internal/runs?include=parameters_status"), "parameters_status")
}
