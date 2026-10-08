package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/access/quota"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/fallback"
	"github.com/stperic/zzrouter/pkg/model"
	"github.com/stperic/zzrouter/pkg/model/cache"
	"github.com/stperic/zzrouter/pkg/model/resolver"
	"github.com/stperic/zzrouter/pkg/modelregistry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func featureRoutingServer(t *testing.T) *Server {
	t.Helper()
	cfg := &pkgConfig.AppsConfig{}
	for _, provider := range []string{"text", "vision", "unknown"} {
		require.NoError(t, cfg.AddApp(provider, pkgConfig.ServiceConfig{Enabled: new(true), Name: provider, Mode: "cloud", Protocol: pkgConfig.ProtocolOpenAI, Runtime: &pkgConfig.AppRuntimeConfig{Endpoint: "https://example.test", API: &pkgConfig.APIConfig{AuthType: pkgConfig.AuthTypeBearer, Token: "test-token"}}, Capabilities: &pkgConfig.AppCapabilities{Models: []string{"same-model"}, WireEndpoints: []string{"messages"}}}))
	}
	nc := &pkgConfig.NodeConfig{}
	nc.Node.Name = "worker"
	s := &Server{node: NewNodeIdentity(nc), appsConfig: cfg, model: &model.Subsystem{}}
	s.model.Cache = cache.New(cache.Config{Node: s.node, NodeConfig: nc, Registry: func() *modelregistry.Registry { return nil }, AppsConfig: func() *pkgConfig.AppsConfig { return cfg }, ClusterClient: s.getClusterClient, ModelDetails: func(provider, model string, d map[string]any) map[string]any {
		state := "unknown"
		eps := []string{"messages"}
		if provider == "text" {
			state = "available"
		}
		if provider == "vision" {
			state = "present"
			eps = []string{"messages_compat"}
		}
		return map[string]any{"features": map[string]string{"vision": state}, "wire_endpoints": eps}
	}})
	require.NoError(t, s.model.Cache.RefreshCacheSync(context.Background()))
	return s
}

func TestFeatureReplicaNarrowingPrecedesEndpointAndFallback(t *testing.T) {
	s := featureRoutingServer(t)
	candidates := []fallback.Candidate{{Name: "first", Model: "same-model", App: "text", Node: "worker"}, {Name: "second", Model: "same-model", App: "vision", Node: "worker"}, {Name: "third", Model: "same-model", App: "unknown", Node: "worker"}}
	r := &resolver.Resolved{ModelName: "same-model", Provider: "text", Node: "worker", Candidates: candidates}
	require.True(t, s.narrowToVision(context.Background(), r))
	require.Len(t, r.Candidates, 1)
	assert.Equal(t, "vision", r.Provider)
	assert.Equal(t, "second", r.Candidates[0].Name)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/messages", nil)
	mode, ok := s.narrowToWireEndpoint(c, "group", r, "messages", "messages_compat")
	require.True(t, ok)
	assert.Equal(t, wireModeCompat, mode)
	assert.Equal(t, "text", candidates[0].App, "input chain is not mutated")
	assert.Equal(t, wireModeNative, s.targetWireMode(context.Background(), "same-model", "text", "worker", "messages", "messages_compat"))
}

func TestFeatureReplicaUnknownAndNoEligible(t *testing.T) {
	s := featureRoutingServer(t)
	r := &resolver.Resolved{Candidates: []fallback.Candidate{{Model: "same-model", App: "text", Node: "worker"}, {Model: "same-model", App: "unknown", Node: "worker"}}}
	require.True(t, s.narrowToVision(context.Background(), r))
	assert.Equal(t, "unknown", r.Provider)
	r.Candidates = []fallback.Candidate{{Model: "same-model", App: "text", Node: "worker"}}
	assert.False(t, s.narrowToVision(context.Background(), r))
	assert.False(t, s.narrowToVision(context.Background(), &resolver.Resolved{ModelName: "same-model", Provider: "text", Node: "worker"}))
	assert.True(t, s.narrowToVision(context.Background(), &resolver.Resolved{ModelName: "missing", Provider: "text", Node: "other"}))
}

func TestCatalogVisionCapabilitiesIgnoreDeclaredProviderNameHeuristic(t *testing.T) {
	cfg := &pkgConfig.AppsConfig{}
	require.NoError(t, cfg.AddApp("engine", pkgConfig.ServiceConfig{Enabled: new(false), Mode: "external", Protocol: pkgConfig.ProtocolOpenAI, Runtime: &pkgConfig.AppRuntimeConfig{Endpoint: "http://localhost"}, Features: map[string]pkgConfig.Feature{"vision": {}}}))
	for _, state := range []string{"present", "available", "unsupported", "unknown"} {
		m := &cache.CachedModel{Name: "llava-vision", Provider: "engine", Details: map[string]any{"features": map[string]string{"vision": state}}}
		assert.Equal(t, state == "present", deriveCapabilities(m, cfg).Vision, state)
	}
	assert.False(t, deriveCapabilities(&cache.CachedModel{Name: "llava-vision", Provider: "engine"}, cfg).Vision)
}

func TestResponsesAdmissionOnceForNativeTranslationAndAffinity(t *testing.T) {
	for _, mode := range []string{"native", "translated", "affinity"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				if mode == "translated" {
					_, _ = w.Write([]byte(`{"id":"chat-test","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
					return
				}
				_, _ = w.Write([]byte(`{"id":"resp_test","object":"response"}`))
			}))
			t.Cleanup(upstream.Close)
			s := createTestNodeWithDefaults(t)
			s.appsConfig = &pkgConfig.AppsConfig{}
			eps := []string{"chat_completions", "responses"}
			if mode == "translated" {
				eps = []string{"chat_completions", "responses_compat"}
			}
			require.NoError(t, s.appsConfig.AddApp("gateway", pkgConfig.ServiceConfig{Enabled: new(true), Mode: "cloud", Protocol: pkgConfig.ProtocolOpenAI, Runtime: &pkgConfig.AppRuntimeConfig{Endpoint: upstream.URL, API: &pkgConfig.APIConfig{AuthType: pkgConfig.AuthTypeBearer, Token: "test-token"}}, Capabilities: &pkgConfig.AppCapabilities{Models: []string{"test-model"}, WireEndpoints: eps}}))
			s.model.Cache.Invalidate()
			s.model.Resolver = resolver.NewDefault(func(string, ...string) (string, string) { return "", "gateway" })
			s.access, _ = newGatewayWithEnforcer(t)
			body := `{"model":"test-model","input":"hello"}`
			if mode == "affinity" {
				s.inference.affinity.Record("resp_previous", "gateway")
				body = `{"model":"test-model","input":"hello","previous_response_id":"resp_previous"}`
			}
			request := func(suspended bool) *httptest.ResponseRecorder {
				w := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)
				c.Request = httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
				setAccessContext(c, &AccessContext{Key: &KeyPrincipal{ID: "once", IsVirtual: true, Role: RoleUser, Suspended: suspended, Quotas: quota.QuotaConfig{RPMLimit: 1, MaxParallelRequests: 1}}})
				s.handleResponses(c)
				return w
			}
			denied := request(true)
			assert.Equal(t, http.StatusForbidden, denied.Code)
			assert.Zero(t, calls.Load())
			first := request(false)
			require.Equal(t, http.StatusOK, first.Code, first.Body.String())
			assert.EqualValues(t, 1, calls.Load())
			second := request(false)
			assert.Equal(t, http.StatusTooManyRequests, second.Code)
			assert.EqualValues(t, 1, calls.Load())
		})
	}
}
