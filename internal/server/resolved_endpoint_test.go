package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/assets"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/utils"
)

// An endpoint overlay value is part of what that endpoint's launch gets,
// so /resolved shows it, with its overlay, only for that endpoint.
func TestResolvedEndpointShowsItsOverlay(t *testing.T) {
	s, _ := assetTestNode(t)
	require.NoError(t, s.configStore.ApplyParameterPatch("llamacpp", func(svc *pkgConfig.ServiceConfig) error {
		svc.Defaults.Endpoints = map[string]pkgConfig.EndpointOverlay{"embeddings": {Parameters: map[string]string{"pooling": "mean"}}}
		return nil
	}))
	resolved := func(query string) (int, resolvedResponse) {
		w := assetRequest(t, s, http.MethodGet, "/zzrouter/v1/providers/llamacpp/resolved"+query, nil)
		var r resolvedResponse
		if w.Code == http.StatusOK {
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &r))
		}
		return w.Code, r
	}

	code, r := resolved("?endpoint=embeddings")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "mean", r.Parameters["pooling"].Value)
	assert.Equal(t, "embeddings", r.Parameters["pooling"].Endpoint)

	code, r = resolved("")
	require.Equal(t, http.StatusOK, code)
	assert.NotContains(t, r.Parameters, "pooling", "the chat launch does not get the embeddings overlay")

	code, _ = resolved("?endpoint=summaries")
	assert.Equal(t, http.StatusBadRequest, code)
}

func TestResolvedEndpointReachesTheWorker(t *testing.T) {
	worker := createSeededTestNode(t, seededTestNodeConfig{TestNodeConfig: TestNodeConfig{AdminKey: TestAdminKey, ClusterMode: pkgConfig.ClusterModeWorker, NodeName: "endpoint-worker"}})
	require.NoError(t, worker.configStore.ApplyParameterPatch("llamacpp", func(svc *pkgConfig.ServiceConfig) error {
		svc.Defaults.Endpoints = map[string]pkgConfig.EndpointOverlay{"embeddings": {Parameters: map[string]string{"pooling": "mean"}}}
		return nil
	}))
	router := &featureResolvedRouter{worker: buildInternalEngine(worker)}
	executor := worker.newParamsExecutor()
	executor.nodename = func() string { return "coordinator" }
	executor.router = router
	engine := gin.New()
	engine.GET("/providers/:name/resolved", executor.HandleResolved)
	out := httptest.NewRecorder()
	engine.ServeHTTP(out, httptest.NewRequest(http.MethodGet, "/providers/llamacpp/resolved?node=endpoint-worker&model=vendor/model&endpoint=embeddings", nil))
	require.Equal(t, http.StatusOK, out.Code, out.Body.String())
	require.NotNil(t, router.request)
	assert.Equal(t, "/zzrouter/v1/internal/providers/llamacpp/resolved?endpoint=embeddings&model=vendor%2Fmodel", router.request.Path)
	var got resolvedResponse
	require.NoError(t, json.Unmarshal(out.Body.Bytes(), &got))
	assert.Equal(t, "mean", got.Parameters["pooling"].Value)
	assert.Equal(t, "embeddings", got.Parameters["pooling"].Endpoint)
}

func TestResolvedEndpointAdmission(t *testing.T) {
	s := createSeededTestNode(t, seededTestNodeConfig{TestNodeConfig: DefaultTestNodeConfig(), providers: []string{"llamacpp", "mlx", "comfyui"}})
	require.NoError(t, s.configStore.ApplyParameterPatch("llamacpp", func(sc *pkgConfig.ServiceConfig) error {
		sc.Capabilities.WireEndpoints = []string{"embeddings"}
		return nil
	}))
	for _, tc := range []struct {
		provider, endpoint string
		status             int
	}{
		{"llamacpp", "", http.StatusOK},
		{"llamacpp", "chat", http.StatusBadRequest},
		{"llamacpp", "embeddings", http.StatusOK},
		{"mlx", "embeddings", http.StatusOK},
		{"mlx", "reranking", http.StatusOK},
		{"comfyui", "", http.StatusOK},
		{"comfyui", "chat", http.StatusBadRequest},
	} {
		t.Run(tc.provider+"/"+tc.endpoint, func(t *testing.T) {
			w := assetRequest(t, s, http.MethodGet, "/zzrouter/v1/providers/"+tc.provider+"/resolved?endpoint="+tc.endpoint, nil)
			require.Equal(t, tc.status, w.Code, w.Body.String())
		})
	}
	w := makeAuthRequest(t, s, http.MethodPost, "/zzrouter/v1/runs/preview", TestAdminKey, map[string]any{"provider": "comfyui", "endpoint": "chat"})
	require.Equal(t, http.StatusBadRequest, w.Code, string(w.Body))
}

func TestCloudEndpointAdmission(t *testing.T) {
	s := createSeededTestNode(t, seededTestNodeConfig{TestNodeConfig: DefaultTestNodeConfig(), providers: []string{"openai"}})
	for _, endpoint := range []string{"", "chat", "embeddings", "reranking", "typo"} {
		t.Run(endpoint, func(t *testing.T) {
			status := http.StatusOK
			if endpoint == "typo" {
				status = http.StatusBadRequest
			}
			w := assetRequest(t, s, http.MethodGet, "/zzrouter/v1/providers/openai/resolved?endpoint="+endpoint, nil)
			require.Equal(t, status, w.Code, w.Body.String())
		})
	}
	before, found := s.appsConfig().LookupApp("openai")
	require.True(t, found)
	w := makeAuthRequest(t, s, http.MethodPatch, "/zzrouter/v1/providers/openai/parameters?endpoint=typo", TestAdminKey,
		map[string]any{"defaults": map[string]any{"environment": map[string]any{"THREADS": "2"}}})
	require.Equal(t, http.StatusBadRequest, w.Code, string(w.Body))
	after, found := s.appsConfig().LookupApp("openai")
	require.True(t, found)
	assert.Equal(t, before, after)
}

func TestPatchEndpointView(t *testing.T) {
	s, _ := assetTestNode(t)
	require.NoError(t, s.configStore.ApplyParameterPatch("llamacpp", func(sc *pkgConfig.ServiceConfig) error {
		sc.Capabilities.WireEndpoints = []string{"chat_completions"}
		return nil
	}))
	body := map[string]any{"defaults": map[string]any{"endpoints": map[string]any{"embeddings": map[string]any{"parameters": map[string]any{"pooling": "mean"}}}}}
	code, raw := patchParams(t, s, body)
	require.Equal(t, http.StatusOK, code, raw)
	var got resolvedResponse
	require.NoError(t, json.Unmarshal([]byte(raw), &got))
	assert.Equal(t, "mean", got.Parameters["pooling"].Value)
	assert.Equal(t, "embeddings", got.Parameters["pooling"].Endpoint)

	w := makeAuthRequest(t, s, http.MethodPatch, "/zzrouter/v1/providers/llamacpp/parameters?endpoint=chat", TestAdminKey, body)
	require.Equal(t, http.StatusOK, w.Code, string(w.Body))
	var base resolvedResponse
	require.NoError(t, json.Unmarshal(w.Body, &base))
	assert.NotContains(t, base.Parameters, "pooling")
}

func TestPatchBaseViewOnEmbeddingsOnlyProvider(t *testing.T) {
	s, _ := assetTestNode(t)
	require.NoError(t, s.configStore.ApplyParameterPatch("llamacpp", func(sc *pkgConfig.ServiceConfig) error {
		sc.Capabilities.WireEndpoints = []string{"embeddings"}
		return nil
	}))
	code, raw := patchParams(t, s, map[string]any{"defaults": map[string]any{"parameters": map[string]any{"ctx-size": 2048}}})
	require.Equal(t, http.StatusOK, code, raw)
	var got resolvedResponse
	require.NoError(t, json.Unmarshal([]byte(raw), &got))
	assert.Equal(t, "2048", got.Parameters["ctx-size"].Value)
	assert.Empty(t, got.Parameters["ctx-size"].Endpoint)
}

func TestPatchEndpointRefusalLeavesTreeUnchanged(t *testing.T) {
	s, _ := assetTestNode(t)
	require.NoError(t, s.configStore.ApplyParameterPatch("llamacpp", func(sc *pkgConfig.ServiceConfig) error {
		sc.Capabilities.WireEndpoints = []string{"chat_completions"}
		return nil
	}))
	cfg, found := s.appsConfig().LookupApp("llamacpp")
	require.True(t, found)
	before, err := json.Marshal(cfg)
	require.NoError(t, err)
	w := makeAuthRequest(t, s, http.MethodPatch, "/zzrouter/v1/providers/llamacpp/parameters?endpoint=embeddings", TestAdminKey,
		map[string]any{"defaults": map[string]any{"parameters": map[string]any{"ctx-size": 2048}}})
	require.Equal(t, http.StatusBadRequest, w.Code, string(w.Body))
	cfg, found = s.appsConfig().LookupApp("llamacpp")
	require.True(t, found)
	after, err := json.Marshal(cfg)
	require.NoError(t, err)
	assert.JSONEq(t, string(before), string(after))
}

func TestPatchUnknownOverlayNames(t *testing.T) {
	s, _ := assetTestNode(t)
	node := s.newParamsExecutor().nodename()
	for _, leaf := range []map[string]any{{"parameters": map[string]any{"ctx-size": 2048}}, {"environment": map[string]any{"THREADS": "2"}}} {
		overlay := map[string]any{"endpoints": map[string]any{"embedding": leaf}}
		for path, body := range map[string]map[string]any{
			"defaults.endpoints.embedding":                    {"defaults": overlay},
			"models.m.endpoints.embedding":                    {"models": map[string]any{"m": overlay}},
			"nodes." + node + ".endpoints.embedding":          {"nodes": map[string]any{node: overlay}},
			"nodes." + node + ".models.m.endpoints.embedding": {"models": map[string]any{"m": map[string]any{}}, "nodes": map[string]any{node: map[string]any{"models": map[string]any{"m": overlay}}}},
		} {
			t.Run(path+"/"+fmt.Sprint(leaf), func(t *testing.T) {
				cfg, found := s.appsConfig().LookupApp("llamacpp")
				require.True(t, found)
				before, err := json.Marshal(cfg)
				require.NoError(t, err)
				w := makeAuthRequest(t, s, http.MethodPatch, "/zzrouter/v1/providers/llamacpp/parameters", TestAdminKey, body)
				require.Equal(t, http.StatusBadRequest, w.Code, string(w.Body))
				var problem utils.ProblemDetails
				require.NoError(t, json.Unmarshal(w.Body, &problem))
				require.Len(t, problem.Errors, 1)
				assert.Equal(t, path, problem.Errors[0].Key)
				assert.Equal(t, string(httperr.CodeInvalidValue), problem.Errors[0].Code)
				cfg, found = s.appsConfig().LookupApp("llamacpp")
				require.True(t, found)
				after, err := json.Marshal(cfg)
				require.NoError(t, err)
				assert.JSONEq(t, string(before), string(after))
			})
		}
	}
}

func TestResolvedEndpointAssetDigest(t *testing.T) {
	s := createSeededTestNode(t, seededTestNodeConfig{
		TestNodeConfig: DefaultTestNodeConfig(), providers: []string{"llamacpp"},
		prepareProviders: func(dir string) {
			provider := filepath.Join(dir, "on-demand", "llamacpp")
			schemaPath := filepath.Join(provider, "schema.yaml")
			data, err := os.ReadFile(schemaPath)
			require.NoError(t, err)
			data = []byte(strings.Replace(string(data), "endpoints:\n  embeddings:\n    parameters:\n", "endpoints:\n  embeddings:\n    parameters:\n      endpoint-file: {type: asset}\n", 1))
			require.NoError(t, os.WriteFile(schemaPath, data, 0o600))
			_, err = assets.NewDir(provider).Write("endpoint.txt", []byte("endpoint content"))
			require.NoError(t, err)
		},
	})
	w := makeAuthRequest(t, s, http.MethodPatch, "/zzrouter/v1/providers/llamacpp/parameters", TestAdminKey,
		map[string]any{"defaults": map[string]any{"endpoints": map[string]any{"embeddings": map[string]any{"parameters": map[string]any{"endpoint-file": "endpoint.txt"}}}}})
	require.Equal(t, http.StatusOK, w.Code, string(w.Body))
	for _, data := range [][]byte{w.Body, assetRequest(t, s, http.MethodGet, "/zzrouter/v1/providers/llamacpp/resolved?endpoint=embeddings", nil).Body.Bytes()} {
		var got resolvedResponse
		require.NoError(t, json.Unmarshal(data, &got))
		assert.Equal(t, assets.Digest([]byte("endpoint content")), got.Parameters["endpoint-file"].SHA256)
	}
}

func TestPatchViewEndpointAcrossAllTiers(t *testing.T) {
	eps := map[string]*endpointPatch{"embeddings": {}}
	for name, patch := range map[string]paramPatchBody{
		"defaults":                        {Defaults: &defaultsPatch{Endpoints: eps}},
		"models":                          {Models: map[string]*modelPatch{"a": {specPatch: specPatch{Endpoints: eps}}, "b": nil}},
		"nodes":                           {Nodes: map[string]*nodePatch{"a": {Endpoints: eps}, "b": nil}},
		"node models":                     {Nodes: map[string]*nodePatch{"a": {Models: map[string]*specPatch{"a": {Endpoints: eps}, "b": nil}}, "b": nil}},
		"same endpoint at multiple tiers": {Defaults: &defaultsPatch{Endpoints: eps}, Models: map[string]*modelPatch{"m": {specPatch: specPatch{Endpoints: eps}}}},
		"deleted overlay":                 {Defaults: &defaultsPatch{Endpoints: map[string]*endpointPatch{"embeddings": nil}}},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, endpoint := patchView(&patch)
			assert.Equal(t, "embeddings", endpoint)
		})
	}
	_, _, endpoint := patchView(&paramPatchBody{Defaults: &defaultsPatch{Endpoints: eps}, Models: map[string]*modelPatch{"m": {specPatch: specPatch{Endpoints: map[string]*endpointPatch{"reranking": {}}}}}})
	assert.Empty(t, endpoint, "two endpoints have no single overlay view")
}
