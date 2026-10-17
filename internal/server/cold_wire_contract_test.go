package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Configured synthetic cloud models exercise cold resolution without disk or process fixtures.
func TestColdWireContractInvalidCatalog(t *testing.T) {
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
				s := createTestNodeWithDefaults(t)
				cfg := &config.AppsConfig{}
				eps := []string{"chat_completions"}
				if native {
					eps = append(eps, endpoint)
				}
				require.NoError(t, cfg.AddApp("gateway", config.ServiceConfig{
					Enabled: new(true), Mode: "cloud", Protocol: config.ProtocolOpenAI,
					Runtime:      &config.AppRuntimeConfig{Endpoint: engine.URL, API: &config.APIConfig{AuthType: config.AuthTypeBearer, Token: "synthetic-cold-wire-test-token"}},
					Capabilities: &config.AppCapabilities{Models: []string{"cold-model"}, WireEndpoints: eps},
				}))
				s.configStore = config.NewMemoryAppsConfigStore(cfg)
				s.providers.appMgr.ReloadConfig(cfg)
				s.model.Cache.Invalidate()
				require.Empty(t, s.providers.appMgr.Instances().List())
				require.Empty(t, s.model.Cache.GetAllModels())
				model := "cold-model@" + s.node.Nodename()
				body := `{"model":"` + model + `","max_tokens":8,"system":"Be terse.","messages":[{"role":"user","content":"bye"}]}`
				if endpoint == "responses" {
					body = `{"model":"` + model + `","input":"bye","max_output_tokens":8}`
				}
				response := postAsAdmin(t, s, "/v1/"+endpoint, body)
				require.Equal(t, http.StatusOK, response.Code, response.Body.String())
				require.Len(t, requests, 1, "one upstream request, no retry")
				sent := <-requests
				assert.Equal(t, wantPath, sent.Path)
				var payload map[string]any
				require.NoError(t, json.Unmarshal(sent.Body, &payload))
				assert.Equal(t, "cold-model", payload["model"])
				if endpoint == "messages" && !native {
					assert.NotContains(t, payload, "system")
					assert.Equal(t, map[string]any{"role": "system", "content": "Be terse."}, payload["messages"].([]any)[0])
				}
			})
		}
	}
}
