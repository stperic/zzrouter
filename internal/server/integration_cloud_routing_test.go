package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stperic/zzrouter/pkg/apipath"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/modelregistry/search"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stretchr/testify/require"
)

func TestIntegrationCloudRoutingSharedDirectDefaultAndResponseOwner(t *testing.T) {
	var mu sync.Mutex
	var received []recordedRequest
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			_, _ = io.WriteString(w, `{"data":[{"id":"m"}]}`)
			return
		}
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		mu.Lock()
		received = append(received, recordedRequest{Path: r.URL.Path, Query: r.URL.RawQuery, Body: body, Headers: r.Header.Clone()})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_mapped","object":"response","output":[]}`)
	}))
	t.Cleanup(upstream.Close)
	s := createTestNodeWithDefaults(t)
	cfg := &config.AppsConfig{}
	require.NoError(t, cfg.AddApp("mapped", config.ServiceConfig{
		Enabled: new(true), Mode: "cloud", Protocol: config.ProtocolOpenAI,
		Runtime:      &config.AppRuntimeConfig{Endpoint: upstream.URL, OpenAIPath: "/compatible", API: &config.APIConfig{AuthType: config.AuthTypeBearer, Token: "provider-fixture-key"}},
		Capabilities: &config.AppCapabilities{WireEndpoints: []string{"chat_completions", "responses"}},
	}))
	s.configStore = config.NewMemoryAppsConfigStore(cfg)
	s.providers.appMgr.ReloadConfig(cfg)
	s.model.Resolver = staticModelResolver{provider: "mapped"}
	s.config.OpenAICompat.DefaultBackend = "mapped"
	chat := postAsAdmin(t, s, "/v1/chat/completions?trace=fixture", `{"model":"m","messages":[{"role":"user","content":"hello"}],"vendor_option":{"exact":9007199254740993}}`)
	require.Equal(t, http.StatusOK, chat.Code, chat.Body.String())
	created := postAsAdmin(t, s, "/v1/responses", `{"model":"m","input":"hello"}`)
	require.Equal(t, http.StatusOK, created.Code, created.Body.String())
	for _, path := range []string{"/v1/responses/resp_mapped", "/v1/files?purpose=fixture"} {
		reply := makeAuthRequest(t, s, http.MethodGet, path, TestAdminKey, nil)
		require.Equal(t, http.StatusOK, reply.Code, string(reply.Body))
	}
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, received, 4)
	require.Equal(t, []string{"/compatible/chat/completions", "/compatible/responses", "/compatible/responses/resp_mapped", "/compatible/files"}, []string{received[0].Path, received[1].Path, received[2].Path, received[3].Path})
	require.Equal(t, "trace=fixture", received[0].Query)
	require.Equal(t, "purpose=fixture", received[3].Query)
	require.Contains(t, string(received[0].Body), `"exact":9007199254740993`)
	for _, request := range received {
		require.Equal(t, "Bearer provider-fixture-key", request.Headers.Get("Authorization"))
		require.Empty(t, request.Headers.Get("X-API-Key"))
	}
}

func TestIntegrationCloudUnavailableDiscoveryAndProbeDoNotSendCredentials(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	t.Cleanup(upstream.Close)
	h := newConnectionHarness(t, upstream.URL)
	values := map[string]string{"endpoint": upstream.URL, "token": "fixture-token"}
	_, err := h.service.Save(t.Context(), "fixture", 0, values, false)
	require.NoError(t, err)
	require.NoError(t, h.config.ApplyParameterPatch("fixture", func(s *config.ServiceConfig) error {
		s.Runtime.ModelsPath = new("")
		return nil
	}))
	result, err := h.service.Test(t.Context(), "fixture", values)
	require.NoError(t, err)
	require.Equal(t, &ConnectionTestResult{Status: "unknown", Code: "probe_unavailable"}, result)
	search.RegisterCloudProvidersFromConfig(h.config.Config, h.service.resolver)
	svc := NewModelSearchService(h.config.Config, nil, h.service.resolver)
	h.engine.GET(apipath.Search, svc.HandleSearchPublic)
	h.engine.GET(apipath.ModelCard(":provider", "*id"), svc.handleGetModelCard)
	for _, path := range []string{apipath.Search + "?type=models&provider=fixture", apipath.Search + "?type=popular&provider=fixture", apipath.ModelCard("fixture", "m")} {
		reply := httptest.NewRecorder()
		h.engine.ServeHTTP(reply, httptest.NewRequest(http.MethodGet, path, nil))
		require.Equal(t, http.StatusNotImplemented, reply.Code, reply.Body.String())
		var problem utils.ProblemDetails
		require.NoError(t, json.Unmarshal(reply.Body.Bytes(), &problem))
		require.Equal(t, "model_discovery_unavailable", problem.Code)
		require.NotContains(t, reply.Body.String(), "fixture-token")
	}
	require.Zero(t, requests.Load())
	// Explicit catalog paths stay raw even when inference uses another prefix.
	target := &backend.Resolved{Endpoint: upstream.URL, OpenAIPath: "/compatible", ModelsPath: new("/catalog")}
	require.Equal(t, "/catalog", target.ModelListPath())
}
