package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
)

func TestConfiguredParameterScopeKeepsHiddenTiersSeparate(t *testing.T) {
	t.Parallel()
	cfg := &pkgConfig.ServiceConfig{
		Defaults: &pkgConfig.AppDefaultsConfig{Parameters: map[string]string{"ctx": "4096", "false-value": "false"}, Environment: map[string]string{"TOKEN": "defaults-secret"},
			Endpoints: map[string]pkgConfig.EndpointOverlay{"embeddings": {Parameters: map[string]string{"ctx": "512"}}}},
		Models: map[string]pkgConfig.ModelSpec{"model": {Parameters: map[string]string{"ctx": "8192"}}, "model*": {Parameters: map[string]string{"ctx": "9999"}}},
		Nodes: map[string]pkgConfig.NodeSpec{"worker": {Parameters: map[string]string{"ctx": "16384"},
			Models: map[string]pkgConfig.NodeModelSpec{"model": {Parameters: map[string]string{"ctx": "32768"}}}}},
	}
	for _, tc := range []struct {
		scope, node, model, endpoint, want string
	}{
		{"defaults", "", "", "", "4096"},
		{"model", "", "model", "", "8192"},
		{"model", "", "model*", "", "9999"},
		{"node", "worker", "", "", "16384"},
		{"node-model", "worker", "model", "", "32768"},
		{"defaults", "", "", "embeddings", "512"},
		{"model", "", "model", "embeddings", ""},
		{"node-model", "worker", "missing", "", ""},
	} {
		t.Run(tc.scope+tc.node+tc.model+tc.endpoint, func(t *testing.T) {
			t.Parallel()
			out, err := configuredParameterScope(cfg, tc.scope, tc.node, tc.model, tc.endpoint)
			require.NoError(t, err)
			assert.Equal(t, tc.want, out.Parameters["ctx"])
			require.NotNil(t, out.Parameters)
			require.NotNil(t, out.Environment)
			if tc.scope != "defaults" || tc.endpoint != "" {
				assert.Empty(t, out.Environment, "a missing exact value must not inherit the defaults secret")
			}
		})
	}
	assert.Equal(t, "4096", cfg.Defaults.Parameters["ctx"])
}

func TestConfiguredParameterScopeRejectsAmbiguousSelectors(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ scope, node, model, endpoint string }{
		{"", "", "", ""}, {"unknown", "", "", ""},
		{"defaults", "worker", "", ""}, {"defaults", "", "model", ""},
		{"model", "", "", ""}, {"model", "worker", "model", ""},
		{"node", "", "", ""}, {"node", "worker", "model", ""},
		{"node-model", "worker", "", ""}, {"node-model", "", "model", ""},
		{"defaults", "", "", "not-an-endpoint"},
	} {
		_, err := configuredParameterScope(&pkgConfig.ServiceConfig{}, tc.scope, tc.node, tc.model, tc.endpoint)
		require.Error(t, err, "%+v", tc)
	}
}

func TestConfiguredParameterScopeUsesPatchFlagIdentity(t *testing.T) {
	t.Parallel()
	for _, scope := range []string{"defaults", "model", "node", "node-model"} {
		for _, endpoint := range []string{"", "embeddings"} {
			t.Run(scope+"/"+endpoint, func(t *testing.T) {
				t.Parallel()
				raw := map[string]string{"ctx_size": "4096", "same_flag": "false", "same-flag": "false"}
				env := map[string]string{"TOKEN_NAME": "upper", "token_name": "lower"}
				eps := map[string]pkgConfig.EndpointOverlay{"embeddings": {Parameters: raw, Environment: env}}
				cfg := &pkgConfig.ServiceConfig{
					Defaults: &pkgConfig.AppDefaultsConfig{Parameters: raw, Environment: env, Endpoints: eps},
					Models:   map[string]pkgConfig.ModelSpec{"model": {Parameters: raw, Environment: env, Endpoints: eps}},
					Nodes: map[string]pkgConfig.NodeSpec{"worker": {Parameters: raw, Environment: env, Endpoints: eps,
						Models: map[string]pkgConfig.NodeModelSpec{"model": {Parameters: raw, Environment: env, Endpoints: eps}}}},
				}
				node, model := "", ""
				if scope == "node" || scope == "node-model" {
					node = "worker"
				}
				if scope == "model" || scope == "node-model" {
					model = "model"
				}
				before, err := configuredParameterScope(cfg, scope, node, model, endpoint)
				require.NoError(t, err)
				assert.Equal(t, map[string]string{"ctx-size": "4096", "same-flag": "false"}, before.Parameters)
				assert.Equal(t, env, before.Environment, "environment names are not flag names")
				assert.Contains(t, raw, "ctx_size", "an editing read must not mutate stored spellings")
				assert.NotContains(t, raw, "ctx-size")
				raw["ctx_size"] = "16384"
				after, err := configuredParameterScope(cfg, scope, node, model, endpoint)
				require.NoError(t, err)
				assert.Equal(t, "16384", after.Parameters["ctx-size"], "a changed alias must change canonical editing evidence")
				assert.Equal(t, "4096", before.Parameters["ctx-size"], "the prior projection remains a snapshot")
			})
		}
	}
}

func TestConfiguredParameterScopeRefusesConflictingStoredSpellings(t *testing.T) {
	t.Parallel()
	cfg := &pkgConfig.ServiceConfig{Defaults: &pkgConfig.AppDefaultsConfig{Parameters: map[string]string{"ctx-size": "4096", "ctx_size": "16384"}}}
	_, err := configuredParameterScope(cfg, "defaults", "", "", "")
	var conflicts paramErrors
	require.ErrorAs(t, err, &conflicts)
	require.Len(t, conflicts, 2)
	for _, problem := range conflicts {
		assert.Equal(t, "duplicate_flag", problem.Code)
		assert.Contains(t, []string{"ctx-size", "ctx_size"}, problem.Key)
	}
}

func TestConfiguredParameterReadUsesCoordinatorAndPreservesUnscopedRead(t *testing.T) {
	cfg := &pkgConfig.AppsConfig{}
	require.NoError(t, cfg.AddApp("engine", pkgConfig.ServiceConfig{
		Name: "engine", Mode: "external", Protocol: pkgConfig.ProtocolOpenAI, Enabled: new(false),
		Runtime:      &pkgConfig.AppRuntimeConfig{Endpoint: "http://engine.invalid"},
		Capabilities: &pkgConfig.AppCapabilities{WireEndpoints: []string{"chat_completions"}},
		Defaults:     &pkgConfig.AppDefaultsConfig{Parameters: map[string]string{"ctx": "4096"}},
		Nodes:        map[string]pkgConfig.NodeSpec{"coordinator": {Parameters: map[string]string{"ctx": "8192"}}},
	}))
	worker := false
	e := NewParamsExecutor(func() *pkgConfig.AppsConfig { return cfg }, nil, func() string { return "coordinator" }, nil, nil, nil,
		func() bool { return worker }, func() string { return "https://coordinator.example" })
	router := gin.New()
	router.GET("/providers/:name/parameters", e.HandleInternalGetAppParameters)
	read := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		return w
	}
	w := read("/providers/engine/parameters?scope=defaults")
	require.Equal(t, http.StatusOK, w.Code)
	var exact configuredParameters
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &exact))
	assert.Equal(t, "4096", exact.Parameters["ctx"])
	assert.Equal(t, "engine", exact.Provider)
	assert.Equal(t, []string{"chat"}, exact.AvailableEndpoints, "endpoint choices follow config admission, even without overlay schema")
	w = read("/providers/engine/parameters")
	require.Equal(t, http.StatusOK, w.Code)
	var old AppParametersResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &old))
	require.Len(t, old.Parameters, 1)
	assert.Equal(t, "4096", old.Parameters[0].Value)
	assert.Equal(t, http.StatusBadRequest, read("/providers/engine/parameters?scope=node").Code)
	assert.Equal(t, http.StatusNotFound, read("/providers/missing/parameters?scope=defaults").Code)
	worker = true
	w = read("/providers/engine/parameters?scope=defaults")
	assert.Equal(t, http.StatusMisdirectedRequest, w.Code)
	assert.Equal(t, "https://coordinator.example/providers/engine/parameters?scope=defaults", w.Header().Get("Location"))
	assert.Equal(t, http.StatusOK, read("/providers/engine/parameters").Code, "existing cached reads stay available")
}

func TestConfiguredParameterReadRefusesConflictingAliases(t *testing.T) {
	cfg := &pkgConfig.AppsConfig{}
	require.NoError(t, cfg.AddApp("engine", pkgConfig.ServiceConfig{
		Name: "engine", Mode: "external", Protocol: pkgConfig.ProtocolOpenAI, Enabled: new(false),
		Runtime:  &pkgConfig.AppRuntimeConfig{Endpoint: "http://engine.invalid"},
		Defaults: &pkgConfig.AppDefaultsConfig{Parameters: map[string]string{"ctx-size": "4096", "ctx_size": "16384"}},
	}))
	e := NewParamsExecutor(func() *pkgConfig.AppsConfig { return cfg }, nil, nil, nil, nil, nil, nil, nil)
	router := gin.New()
	router.GET("/providers/:name/parameters", e.HandleInternalGetAppParameters)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/providers/engine/parameters?scope=defaults", nil))
	assert.Equal(t, http.StatusConflict, w.Code)
	assert.Contains(t, w.Body.String(), `"code":"duplicate_flag"`)
	assert.NotContains(t, w.Body.String(), "4096", "conflict diagnostics need not disclose values")
	assert.NotContains(t, w.Body.String(), "16384")
}

func TestConfiguredParameterScopeNodeServiceHasNoEndpointOverlay(t *testing.T) {
	t.Parallel()
	cfg := &pkgConfig.ServiceConfig{RunScope: pkgConfig.RunScopeNode}
	out, err := configuredParameterScope(cfg, "defaults", "", "", "")
	require.NoError(t, err)
	assert.Empty(t, out.AvailableEndpoints)
	_, err = configuredParameterScope(cfg, "defaults", "", "", "chat")
	require.Error(t, err)
}
