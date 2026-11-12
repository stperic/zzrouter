package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/apipath"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/modelregistry"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stperic/zzrouter/pkg/modelregistry/search"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func discoveryConfig(t *testing.T, endpoint, token string) *config.AppsConfig {
	t.Helper()
	t.Setenv("TEST_DISCOVERY_TOKEN", "")
	cfg := &config.AppsConfig{}
	require.NoError(t, cfg.AddApp("huggingface", config.ServiceConfig{Mode: "registry", Connection: &config.ConnectionConfig{Fields: []config.ConnectionField{{ID: "token", Label: "Token", Type: "text", Secret: true, Env: "TEST_DISCOVERY_TOKEN"}}}, Enabled: new(true), Runtime: &config.AppRuntimeConfig{Endpoint: endpoint, API: &config.APIConfig{AuthType: config.AuthTypeBearer, Token: token}}}))
	return cfg
}

func discoveryHTTPClient(t *testing.T, s *ModelSearchService) *pkgClient.Client {
	t.Helper()
	engine := gin.New()
	engine.GET(apipath.Search, s.HandleSearchPublic)
	engine.GET(apipath.Models+"/card/:provider/*id", s.handleGetModelCard)
	node := httptest.NewServer(engine)
	t.Cleanup(node.Close)
	return pkgClient.NewClient(config.ClientNodeConfig{Address: node.URL, APIKey: "fixture"})
}

func TestDiscoveryHTTPContractKeepsParameterCountDistinctFromStorage(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer fixture", r.Header.Get("Authorization"))
		w.Header().Set("Link", `<https://huggingface.co/api/models?cursor=next-page>; rel="next"`)
		_, _ = w.Write([]byte(`[{"id":"org/model","safetensors":{"total":7000000000},"siblings":[{"rfilename":"model-Q4_K_M-00001-of-00002.gguf","size":10},{"rfilename":"model-Q4_K_M-00002-of-00002.gguf","size":20}]}]`))
	}))
	defer upstream.Close()
	cfg := discoveryConfig(t, upstream.URL, "fixture")
	client := discoveryHTTPClient(t, NewModelSearchService(func() *config.AppsConfig { return cfg }, nil))
	page, err := client.SearchModels(context.Background(), metadata.SearchQuery{Provider: "huggingface", Query: "model", Full: true})
	require.NoError(t, err)
	require.Len(t, page.Results, 1)
	d := page.Results[0].Discovery
	assert.Equal(t, int64(7000000000), d.ParameterCount)
	assert.Equal(t, int64(30), d.SizeBytes)
	assert.Equal(t, 1, d.VariantCount)
	assert.False(t, page.IsTotalExact)
	assert.True(t, page.Pagination.HasNext)
	assert.Equal(t, "org/model", page.Results[0].Native["id"])
}

func TestDiscoverySearchCachePartitionsCredentialRevision(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var newCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("Authorization")
		id := "org/new"
		if token == "Bearer old" {
			close(started)
			<-release
			id = "org/old"
		} else {
			newCalls.Add(1)
			assert.Equal(t, "Bearer new", token)
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{{"id": id}})
	}))
	defer upstream.Close()
	var configs atomic.Pointer[config.AppsConfig]
	configs.Store(discoveryConfig(t, upstream.URL, "old"))
	service := NewModelSearchService(configs.Load, nil)
	client := discoveryHTTPClient(t, service)
	query := metadata.SearchQuery{Provider: "huggingface", Query: "model", Full: true}
	oldDone := make(chan error, 1)
	go func() { _, err := client.SearchModels(context.Background(), query); oldDone <- err }()
	select {
	case <-started:
	case err := <-oldDone:
		t.Fatalf("old request completed before its credential barrier: %v", err)
	}
	configs.Store(discoveryConfig(t, upstream.URL, "new"))
	page, err := client.SearchModels(context.Background(), query)
	require.NoError(t, err)
	require.Len(t, page.Results, 1)
	assert.Equal(t, "org/new", page.Results[0].Discovery.APIID)
	close(release)
	require.NoError(t, <-oldDone)
	page, err = client.SearchModels(context.Background(), query)
	require.NoError(t, err)
	assert.Equal(t, "org/new", page.Results[0].Discovery.APIID)
	assert.Equal(t, int32(1), newCalls.Load())
}

func TestCloudDiscoveryHTTPRetainsNativeCapabilities(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer fixture", r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{"data":[{"id":"provider-id","display_name":"Display name","capabilities":{"vision":true},"custom_field":[1,2]}]}`))
	}))
	defer upstream.Close()
	cfg := &config.AppsConfig{}
	const provider = "discovery-fixture"
	require.NoError(t, cfg.AddApp(provider, config.ServiceConfig{Enabled: new(true), Name: "Fixture", Mode: "cloud", Protocol: config.ProtocolOpenAI, Runtime: &config.AppRuntimeConfig{Endpoint: upstream.URL, API: &config.APIConfig{AuthType: config.AuthTypeBearer, Token: "fixture"}}, Capabilities: &config.AppCapabilities{WireEndpoints: []string{"chat_completions"}}}))
	resolver := backend.NewResolver(func() *config.AppsConfig { return cfg })
	search.RegisterCloudProvidersFromConfig(func() *config.AppsConfig { return cfg }, resolver)
	client := discoveryHTTPClient(t, NewModelSearchService(func() *config.AppsConfig { return cfg }, nil, resolver))
	page, err := client.SearchModels(context.Background(), metadata.SearchQuery{Provider: provider})
	require.NoError(t, err)
	require.Len(t, page.Results, 1)
	assert.Equal(t, "provider-id", page.Results[0].Discovery.APIID)
	assert.Equal(t, map[string]any{"vision": true}, page.Results[0].Native["capabilities"])
	card, err := client.GetModelCard(context.Background(), provider, "provider-id")
	require.NoError(t, err)
	assert.Equal(t, "Display name", card.Discovery.DisplayName)
	assert.Equal(t, []any{float64(1), float64(2)}, card.Native["custom_field"])
}

func TestVariantControllerAndClientShareActualEnvelope(t *testing.T) {
	t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
	t.Setenv("ZZROUTER_MODEL_DIR", t.TempDir())
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer fixture", r.Header.Get("Authorization"))
		assert.Equal(t, "true", r.URL.Query().Get("blobs"))
		_, _ = w.Write([]byte(`{"siblings":[{"rfilename":"model-Q4_K_M-00001-of-00002.gguf","size":10},{"rfilename":"model-Q4_K_M-00002-of-00002.gguf","size":20},{"rfilename":"mmproj.gguf","size":6}]}`))
	}))
	defer upstream.Close()
	cfg := discoveryConfig(t, upstream.URL, "fixture")
	require.NoError(t, cfg.AddApp("llamacpp", config.ServiceConfig{Mode: "external", Protocol: config.ProtocolOpenAI, Runtime: &config.AppRuntimeConfig{Endpoint: upstream.URL}, Capabilities: &config.AppCapabilities{WireEndpoints: []string{"chat_completions"}}, Enabled: new(true), Features: map[string]config.Feature{"vision": {Flag: "--mmproj", Files: []string{"mmproj*.gguf"}}}}))
	registry, err := modelregistry.NewRegistry(func() *config.AppsConfig { return cfg })
	require.NoError(t, err)
	t.Cleanup(registry.Stop)
	engine := gin.New()
	NewRegistriesController(func() *modelregistry.Registry { return registry }, func() *config.AppsConfig { return cfg }).RegisterPublicRoutes(engine.Group(apipath.Base))
	node := httptest.NewServer(engine)
	defer node.Close()
	client := pkgClient.NewClient(config.ClientNodeConfig{Address: node.URL})
	variants, err := client.GetHuggingFaceVariants(context.Background(), "org/model", "llamacpp")
	require.NoError(t, err)
	require.Len(t, variants.Variants, 1)
	assert.Equal(t, 2, variants.Variants[0].Shards)
	assert.Equal(t, int64(30), variants.Variants[0].SizeBytes)
	require.Len(t, variants.Features, 1)
	assert.Equal(t, "vision", variants.Features[0].Feature)
}

func TestOllamaTagsRouteIsAuthenticatedAndValidatesIdentifier(t *testing.T) {
	server := createTestNodeWithDefaults(t)
	unauth := makeRequest(t, server, TestRequest{Method: http.MethodGet, Path: apipath.RegistriesOllamaTags + "?id=qwen3"})
	assert.Equal(t, http.StatusUnauthorized, unauth.Code)
	invalid := makeAuthRequest(t, server, http.MethodGet, apipath.RegistriesOllamaTags+"?id=../other", TestAdminKey, nil)
	assert.Equal(t, http.StatusBadRequest, invalid.Code)
}

func TestDiscoveryUnknownStorageDoesNotUseParameterCount(t *testing.T) {
	service := NewModelSearchService(func() *config.AppsConfig { return nil }, nil)
	result := service.discoveryResult("huggingface", map[string]any{"id": "org/model", "safetensors": map[string]any{"total": float64(7_000_000_000)}, "siblings": []any{map[string]any{"rfilename": "weights.safetensors"}}})
	assert.Equal(t, int64(7_000_000_000), result.Discovery.ParameterCount)
	assert.Zero(t, result.Discovery.SizeBytes)
}
