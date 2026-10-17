package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/model/resolver"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func warmWireNode(t *testing.T, engine *httptest.Server) (*Server, *instance.Instance) {
	t.Helper()
	s := createTestNodeWithDefaults(t)
	s.configStore = config.NewMemoryAppsConfigStore(&config.AppsConfig{})
	for _, provider := range []string{"mlx", "other"} {
		require.NoError(t, s.appsConfig().AddApp(provider, config.ServiceConfig{
			Enabled: new(true), Mode: "external", Protocol: config.ProtocolOpenAI,
			Runtime:      &config.AppRuntimeConfig{Endpoint: engine.URL},
			Capabilities: &config.AppCapabilities{WireEndpoints: []string{"chat_completions"}},
		}))
	}
	s.providers.appMgr.ReloadConfig(s.appsConfig())
	run := instanceFor(t, engine, "warm-model", "/weights/warm-model")
	run.Endpoint = "chat"
	require.NoError(t, s.providers.appMgr.Instances().Register(run))
	t.Cleanup(func() { require.NoError(t, s.providers.appMgr.Instances().Remove(run.ID)) })
	return s, run
}

// Synthetic backends reject the wrong wire path, as the captured mlx_lm 404 did.
func TestWarmWireContractInvalidCatalog(t *testing.T) {
	for _, native := range []bool{false, true} {
		for _, endpoint := range []string{"messages", "responses"} {
			t.Run(endpoint+map[bool]string{false: "/translated", true: "/native"}[native], func(t *testing.T) {
				wantPath := "/v1/chat/completions"
				if native {
					wantPath = "/v1/" + endpoint
				}
				requests := make(chan recordedRequest, 2)
				engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
						// A catalog probe, not the request under test.
						_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
						return
					}
					body, _ := io.ReadAll(r.Body)
					requests <- recordedRequest{Path: r.URL.Path, Body: body}
					w.Header().Set("Content-Type", "application/json")
					if r.URL.Path != wantPath {
						w.WriteHeader(http.StatusNotFound)
						_, _ = w.Write([]byte(`{"detail":"Not Found"}`))
						return
					}
					_, _ = w.Write([]byte(`{"id":"answer","type":"message","object":"response","content":[{"type":"text","text":"bye"}],"choices":[{"message":{"role":"assistant","content":"bye"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`))
				}))
				t.Cleanup(engine.Close)
				s, run := warmWireNode(t, engine)
				eps := []string{"chat_completions", "messages_compat", "responses_compat"}
				if native {
					eps = []string{"chat_completions", "messages", "responses"}
				}
				run.SetResolved(instance.Resolved{WireEndpoints: eps})
				s.model.Cache.Invalidate()
				resolved, err := s.model.Resolver.Resolve(t.Context(), resolver.Reference{Model: "warm-model@" + s.node.Nodename()})
				require.NoError(t, err)
				require.Empty(t, resolved.Provider, "management pin keeps evidence-only semantics")
				resolved, err = s.model.Resolver.Resolve(t.Context(), resolver.Reference{Model: "warm-model@" + s.node.Nodename(), Inference: true})
				require.NoError(t, err)
				require.Equal(t, run.Provider, resolved.Provider, "inference binds the warm identity before wire selection")
				body := `{"model":"warm-model@` + s.node.Nodename() + `","max_tokens":8,"system":"Be terse.","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"hello"},{"role":"user","content":"bye"}]}`
				if endpoint == "responses" {
					body = `{"model":"warm-model@` + s.node.Nodename() + `","input":"bye","max_output_tokens":8}`
				}
				response := postAsAdmin(t, s, "/v1/"+endpoint, body)
				require.Equal(t, http.StatusOK, response.Code, response.Body.String())
				require.Len(t, requests, 1, "one request, no retry")
				sent := <-requests
				assert.Equal(t, wantPath, sent.Path)
				var payload map[string]any
				require.NoError(t, json.Unmarshal(sent.Body, &payload))
				assert.Equal(t, "/weights/warm-model", payload["model"])
				if endpoint == "messages" && !native {
					assert.NotContains(t, payload, "system")
					messages := payload["messages"].([]any)
					assert.Equal(t, map[string]any{"role": "system", "content": "Be terse."}, messages[0])
				}
			})
		}
	}
}

func TestWarmWireContractSnapshotGuards(t *testing.T) {
	for _, tc := range []struct {
		name, provider, node string
		status               instance.Status
		eps                  []string
		want                 wireEndpointMode
	}{
		{"matching run", "mlx", "", instance.StatusRunning, []string{"messages", "responses"}, wireModeNative},
		{"other provider", "other", "", instance.StatusRunning, []string{"messages", "responses"}, wireModeCompat},
		{"remote node", "mlx", "elsewhere", instance.StatusRunning, []string{"messages", "responses"}, wireModeCompat},
		{"not running", "mlx", "", instance.StatusUnhealthy, []string{"messages", "responses"}, wireModeCompat},
		{"unknown endpoints", "mlx", "", instance.StatusRunning, nil, wireModeCompat},
		{"empty endpoints", "mlx", "", instance.StatusRunning, []string{}, wireModeUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := httptest.NewServer(http.NotFoundHandler())
			t.Cleanup(engine.Close)
			s, run := warmWireNode(t, engine)
			run.SetResolved(instance.Resolved{WireEndpoints: tc.eps})
			run.SetStatus(tc.status)
			for _, endpoint := range []string{"messages", "responses"} {
				assert.Equal(t, tc.want, s.targetWireMode(t.Context(), &resolver.Resolved{ModelName: "warm-model", Provider: tc.provider, Node: tc.node}, endpoint, endpoint+"_compat"))
			}
		})
	}
}

func TestWarmWireContractAdmissionRetainsProvider(t *testing.T) {
	engine := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(engine.Close)
	s, _ := warmWireNode(t, engine)
	s.model.Cache.Invalidate()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	resolved, ok := s.resolveTarget(c, "warm-model@"+s.node.Nodename(), "", nil)
	require.True(t, ok)
	assert.Equal(t, "mlx", resolved.Provider)
}

func TestWarmWireContractProviderDisplayAlias(t *testing.T) {
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	t.Cleanup(engine.Close)
	s := createTestNodeWithDefaults(t)
	cfg := &config.AppsConfig{}
	require.NoError(t, cfg.AddApp("llamacpp", config.ServiceConfig{Enabled: new(true), Mode: "external", Protocol: config.ProtocolOpenAI, Runtime: &config.AppRuntimeConfig{Endpoint: engine.URL}, Capabilities: &config.AppCapabilities{WireEndpoints: []string{"chat_completions"}}}))
	s.configStore = config.NewMemoryAppsConfigStore(cfg)
	s.providers.appMgr.ReloadConfig(cfg)
	run := instanceFor(t, engine, "alias-model", "alias-model")
	run.Provider, run.Endpoint = "llama.cpp", "chat"
	run.SetResolved(instance.Resolved{WireEndpoints: []string{"responses"}})
	require.NoError(t, s.providers.appMgr.Instances().Register(run))
	t.Cleanup(func() { require.NoError(t, s.providers.appMgr.Instances().Remove(run.ID)) })
	s.model.Cache.Invalidate()
	for _, provider := range []string{"llama.cpp", "llamacpp"} {
		resolved, err := s.model.Resolver.Resolve(t.Context(), resolver.Reference{Model: "alias-model", Provider: provider, Node: s.node.Nodename(), Inference: true})
		require.NoError(t, err)
		require.Equal(t, "llama.cpp", resolved.Provider)
		require.Equal(t, wireModeNative, s.targetWireMode(t.Context(), resolved, "responses", "responses_compat"), "warm snapshot beats translated config")
	}
}
