package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/model/cache"
	modelgroup "github.com/stperic/zzrouter/pkg/model/group"
	"github.com/stperic/zzrouter/pkg/model/resolver"
	"github.com/stperic/zzrouter/pkg/modelregistry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// imageAdmissionNode serves "same-model" from one engine whose catalog
// reports vision as state, and counts the requests the engine receives.
func imageAdmissionNode(t *testing.T, state string) (*Server, *atomic.Int32) {
	t.Helper()
	calls := &atomic.Int32{}
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_test","choices":[],"usage":{"prompt_tokens":1},"done":true}`))
	}))
	t.Cleanup(engine.Close)
	s := createTestNodeWithDefaults(t)
	s.appsConfig = &pkgConfig.AppsConfig{}
	require.NoError(t, s.appsConfig.AddApp("ollama", pkgConfig.ServiceConfig{Enabled: new(true), Mode: "cloud", Protocol: pkgConfig.ProtocolOpenAI, Runtime: &pkgConfig.AppRuntimeConfig{Endpoint: engine.URL, API: &pkgConfig.APIConfig{AuthType: pkgConfig.AuthTypeBearer, Token: "test-token"}}, Capabilities: &pkgConfig.AppCapabilities{Models: []string{"same-model"}, WireEndpoints: []string{"chat_completions", "messages", "responses"}}}))
	s.model.Cache = cache.New(cache.Config{Node: s.node, NodeConfig: &pkgConfig.NodeConfig{}, Registry: func() *modelregistry.Registry { return nil }, AppsConfig: func() *pkgConfig.AppsConfig { return s.appsConfig }, ClusterClient: s.getClusterClient, ModelDetails: func(_, _ string, _ map[string]any) map[string]any {
		return map[string]any{"features": map[string]string{"vision": state}}
	}})
	require.NoError(t, s.model.Cache.RefreshCacheSync(context.Background()))
	s.model.Resolver = resolver.NewDefault(func(string, ...string) (string, string) { return "", "ollama" })
	return s, calls
}

// A model whose vision nobody can vouch for is served, never refused.
func TestImageAdmissionUnknownIsServed(t *testing.T) {
	s, calls := imageAdmissionNode(t, "unknown")
	w := postAsAdmin(t, s, "/v1/chat/completions", `{"model":"same-model","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"x"}}]}]}`)
	require.Equal(t, 200, w.Code, w.Body.String())
	assert.Equal(t, int32(1), calls.Load())
}

func TestImageAdmissionAllSurfaces(t *testing.T) {
	s, calls := imageAdmissionNode(t, "unsupported")
	for _, tc := range []struct{ name, path, body string }{
		{"chat", "/v1/chat/completions", `{"model":"same-model","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"x"}}]}]}`},
		{"messages", "/v1/messages", `{"model":"same-model","max_tokens":1,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"x"}}]}]}`},
		{"count", "/v1/messages/count_tokens", `{"model":"same-model","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":[{"type":"image","source":{"type":"url","url":"x"}}]}]}]}`},
		{"responses", "/v1/responses", `{"model":"same-model","input":[{"role":"user","content":[{"type":"input_image","image_url":"x"}]}]}`},
		{"affinity", "/v1/responses", `{"model":"same-model","previous_response_id":"resp_pinned","input":[{"role":"user","content":[{"type":"input_image","image_url":"x"}]}]}`},
		{"ollama chat", "/api/chat", `{"model":"same-model","messages":[{"role":"user","content":"describe","images":["AAA"]}]}`},
		{"ollama generate", "/api/generate", `{"model":"same-model","prompt":"describe","images":["AAA"]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s.inference.affinity.Record("resp_pinned", "ollama")
			for _, stream := range []bool{false, true} {
				body := tc.body
				if stream {
					body = strings.TrimSuffix(body, "}") + `,"stream":true}`
				}
				req := httptest.NewRequest("POST", tc.path, strings.NewReader(body))
				req.Header.Set("Authorization", "Bearer "+TestAdminKey)
				w := httptest.NewRecorder()
				s.engine.ServeHTTP(w, req)
				require.Equal(t, 400, w.Code, w.Body.String())
				assert.Contains(t, w.Body.String(), "vision")
				assert.Contains(t, w.Body.String(), "restart")
			}
		})
	}
	require.NoError(t, s.model.Groups.Set("image-group", modelgroup.ModelGroup{Strategy: modelgroup.StrategyPriority, Replicas: []modelgroup.Replica{{Name: "text", App: "ollama", Model: "same-model"}}}))
	s.model.Resolver = resolver.NewGroup(s.model.Groups, s.model.Resolver)
	for _, path := range []string{"/api/chat", "/api/generate"} {
		w := postAsAdmin(t, s, path, `{"model":"image-group","images":["AAA"],"messages":[{"role":"user","content":"describe"}]}`)
		require.Equal(t, 400, w.Code, w.Body.String())
	}
	w := postAsAdmin(t, s, "/v1/responses", `{"model":"image-group","previous_response_id":"resp_pinned","input":[{"role":"user","content":[{"type":"input_image","image_url":"x"}]}]}`)
	require.Equal(t, 400, w.Code, w.Body.String())
	assert.Zero(t, calls.Load(), "known ineligible requests never reach a backend")
}

func TestColdStreamClassifiesUpstreamErrors(t *testing.T) {
	for _, tc := range []struct {
		status       int
		message, typ string
	}{
		{500, "image input is not supported", "invalid_request_error"},
		{500, "out of memory", "api_error"},
		{400, "bad input", "invalid_request_error"},
		{401, "bad key", "authentication_error"},
		{403, "forbidden", "permission_error"},
		{429, "too many requests", "rate_limit_error"},
	} {
		t.Run(tc.typ+tc.message, func(t *testing.T) {
			engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"error":"` + tc.message + `"}`))
			}))
			t.Cleanup(engine.Close)
			s := createTestNodeWithDefaults(t)
			req := routedRequest(httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"org/model","stream":true}`)))
			w := httptest.NewRecorder()
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			s.streamFromInstance(w, req, instanceFor(t, engine, "org/model", ""), dialectOf(req).(httperr.InBandStreamer))
			assert.Equal(t, 200, w.Code)
			assert.Contains(t, w.Body.String(), `"type":"`+tc.typ+`"`)
			assert.Contains(t, w.Body.String(), "[DONE]")
		})
	}
}

func TestImageGroupSelectsVisionBeforeProtocol(t *testing.T) {
	for _, state := range []string{"present", "unknown", "unsupported"} {
		t.Run(state, func(t *testing.T) {
			backends := map[string]*serviceBackend{
				"text":   {wire: []string{"messages", "responses"}, status: 200},
				"vision": {wire: []string{"chat_completions", "messages_compat", "responses_compat"}, status: 200, reply: chatReplyJSON},
			}
			s := newServiceGroupNode(t, []string{"text", "vision"}, backends)
			for _, name := range []string{"text", "vision"} {
				require.NoError(t, s.appsConfig.UpdateApp(name, func(cfg *pkgConfig.ServiceConfig) error {
					cfg.Mode = "cloud"
					cfg.Capabilities.Models = []string{name + "-model"}
					cfg.Runtime.API = &pkgConfig.APIConfig{AuthType: pkgConfig.AuthTypeBearer, Token: "test-token"}
					return nil
				}))
			}
			s.model.Cache = cache.New(cache.Config{Node: s.node, NodeConfig: &pkgConfig.NodeConfig{}, Registry: func() *modelregistry.Registry { return nil }, AppsConfig: func() *pkgConfig.AppsConfig { return s.appsConfig }, ClusterClient: s.getClusterClient, ModelDetails: func(provider, _ string, _ map[string]any) map[string]any {
				value := "unsupported"
				if provider == "vision" {
					value = state
				}
				return map[string]any{"features": map[string]string{"vision": value}}
			}})
			require.NoError(t, s.model.Cache.RefreshCacheSync(context.Background()))
			for _, tc := range []struct{ path, body string }{
				{"/v1/messages", `{"model":"claude","max_tokens":1,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":[{"type":"text","text":"capture"},{"type":"image","source":{"type":"url","url":"https://example.test/image"}}]}]}]}`},
				{"/v1/responses", `{"model":"claude","input":[{"role":"user","content":[{"type":"input_image","image_url":"https://example.test/image"}]}]}`},
			} {
				w := postAsAdmin(t, s, tc.path, tc.body)
				if state == "unsupported" {
					assert.Equal(t, 400, w.Code, w.Body.String())
				} else {
					require.Equal(t, 200, w.Code, w.Body.String())
				}
			}
			assert.Empty(t, backends["text"].requests(), "native text replica must not hide a compatible vision replica")
			if state == "unsupported" {
				assert.Empty(t, backends["vision"].requests())
			} else {
				hits := backends["vision"].requests()
				require.Len(t, hits, 2)
				for _, hit := range hits {
					assert.Equal(t, "/v1/chat/completions", hit.Path)
					assert.Contains(t, string(hit.Body), `"type":"image_url"`)
					assert.Contains(t, string(hit.Body), "https://example.test/image")
				}
			}
		})
	}
}

func TestImageAdmissionSourceAliasKeepsProviderAndNode(t *testing.T) {
	for _, node := range []string{"worker", "remote-worker"} {
		t.Run(node, func(t *testing.T) {
			s := featureRoutingServer(t)
			var items []map[string]any
			for _, m := range s.model.Cache.GetAllModels() {
				items = append(items, map[string]any{"name": m.Name, "source_id": "org/source", "node": node, "assigned_app": m.Provider, "details": m.Details})
			}
			body, err := json.Marshal(map[string]any{"models": items})
			require.NoError(t, err)
			peer := &imageAdmissionCatalogPeer{body: body}
			s.model.Cache = cache.New(cache.Config{Node: s.node, NodeConfig: &pkgConfig.NodeConfig{}, Registry: func() *modelregistry.Registry { return nil }, AppsConfig: func() *pkgConfig.AppsConfig { return nil }, ClusterClient: func() mesh.ClusterClient { return peer }, ResolveEndpointToNodename: func(node string) string { return node }})
			require.NoError(t, s.model.Cache.RefreshCacheSync(t.Context()))
			for _, provider := range []string{"text", "vision", "unknown"} {
				resolved := &resolver.Resolved{ModelName: "ORG/SOURCE", Provider: provider, Node: node}
				got := s.narrowToVision(context.Background(), resolved)
				assert.Equal(t, provider != "text", got, provider)
			}
			assert.True(t, s.narrowToVision(context.Background(), &resolver.Resolved{ModelName: "org/source", Provider: "text", Node: "different-worker"}), "another node remains unknown")
			assert.True(t, s.narrowToVision(context.Background(), &resolver.Resolved{ModelName: "org/source", Provider: "different-provider", Node: node}), "another provider remains unknown")
		})
	}
}

func TestResponsesTranslatedUnsupportedImageBeforeAndAfterStream(t *testing.T) {
	for _, stream := range []bool{false, true} {
		backends := map[string]*serviceBackend{"chat": {wire: []string{"chat_completions", "responses_compat"}, status: 500, reply: `{"error":{"message":"image input is not supported","type":"api_error"}}`}}
		s := newServiceGroupNode(t, []string{"chat"}, backends)
		body := `{"model":"claude","input":[{"role":"user","content":[{"type":"input_image","image_url":"x"}]}]}`
		if stream {
			body = strings.TrimSuffix(body, "}") + `,"stream":true}`
		}
		w := postAsAdmin(t, s, "/v1/responses", body)
		require.Equal(t, 400, w.Code, w.Body.String())
		assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
		assert.Contains(t, w.Body.String(), `"type":"invalid_request_error"`)
		assert.NotContains(t, w.Body.String(), "response.completed")
		assert.Len(t, backends["chat"].requests(), 1)
	}
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"error":"images are not supported"}`))
	}))
	t.Cleanup(engine.Close)
	s := createTestNodeWithDefaults(t)
	req := routedRequest(httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"org/model","stream":true}`)))
	req.URL.Path = "/v1/chat/completions"
	raw := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(raw)
	writer := newResponsesStreamWriter(c.Writer, "org/model")
	writer.WriteHeader(200)
	s.streamFromInstance(writer, req, instanceFor(t, engine, "org/model", ""), dialectOf(req).(httperr.InBandStreamer))
	writer.Close()
	assert.Equal(t, 200, raw.Code)
	assert.Contains(t, raw.Body.String(), "event: response.failed")
	assert.Contains(t, raw.Body.String(), `"type":"invalid_request_error"`)
	assert.NotContains(t, raw.Body.String(), "response.completed")
	assert.NotImplements(t, (*httperr.InBandStreamer)(nil), newOllamaResponder(), "Ollama keeps pre-stream failures as HTTP errors")
}

func TestResponseAffinityTargetAmbiguityRemainsUnknown(t *testing.T) {
	s := featureRoutingServer(t)
	s.model.Groups = modelgroup.NewGroupStore()
	require.NoError(t, s.model.Groups.Set("group", modelgroup.ModelGroup{Strategy: modelgroup.StrategyPriority, Replicas: []modelgroup.Replica{{Name: "a", App: "text", Model: "same-model"}, {Name: "b", App: "text", Model: "other-model"}}}))
	target := s.responseAffinityTarget("group", "text")
	assert.Equal(t, "group", target.ModelName)
	assert.True(t, s.narrowToVision(context.Background(), target))
}

// imageAdmissionCatalogPeer populates replica identities through the cache's
// normal worker-report path instead of mutating already-indexed entries.
type imageAdmissionCatalogPeer struct {
	mesh.ClusterClient
	body []byte
}

func (p *imageAdmissionCatalogPeer) Broadcast(context.Context, string, *mesh.QueryParams) (*mesh.BroadcastResponse, error) {
	return &mesh.BroadcastResponse{Responses: []*mesh.NodeResponse{{Response: &mesh.Response{StatusCode: 200, Body: p.body}}}}, nil
}
