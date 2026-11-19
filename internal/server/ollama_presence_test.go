package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/config"
)

func TestOllamaPresenceIncludesWorkerEnable(t *testing.T) {
	on, off := true, false
	for _, tc := range []struct {
		name, provider string
		protocol       config.AppProtocol
		local, worker  *bool
		want           int
	}{
		{"worker only", "ollama", config.ProtocolOllama, &off, &on, http.StatusNoContent},
		{"named Ollama provider", "lab", config.ProtocolOllama, &off, &on, http.StatusNoContent},
		{"local only", "ollama", config.ProtocolOllama, &on, &off, http.StatusNoContent},
		{"all disabled", "ollama", config.ProtocolOllama, &off, &off, http.StatusServiceUnavailable},
		{"unset worker", "ollama", config.ProtocolOllama, &off, nil, http.StatusServiceUnavailable},
		{"unrelated worker", "lab", config.ProtocolOpenAI, &off, &on, http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.AppsConfig{}
			require.NoError(t, cfg.AddApp(tc.provider, config.ServiceConfig{
				Mode: "external", Protocol: tc.protocol, Enabled: tc.local,
				Runtime:      &config.AppRuntimeConfig{Endpoint: "http://127.0.0.1:11434"},
				Nodes:        map[string]config.NodeSpec{"worker": {Enabled: tc.worker}},
				Capabilities: &config.AppCapabilities{WireEndpoints: []string{"chat_completions"}},
			}))
			h := &OllamaHandlers{appsConfig: func() *config.AppsConfig { return cfg }}
			router := gin.New()
			router.GET("/api/ps", h.requireOllamaPresence(), func(c *gin.Context) { c.Status(http.StatusNoContent) })
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/ps", nil))
			require.Equal(t, tc.want, response.Code, response.Body.String())
		})
	}
}

func TestIntegrationOllamaWorkerOnlyMutationAdmission(t *testing.T) {
	s := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, UserKey: TestUserKey})
	on, off := true, false
	cfg := &config.AppsConfig{}
	require.NoError(t, cfg.AddApp("ollama", config.ServiceConfig{
		Mode: "external", Protocol: config.ProtocolOllama, Enabled: &off,
		Runtime:      &config.AppRuntimeConfig{Endpoint: "http://127.0.0.1:11434"},
		Nodes:        map[string]config.NodeSpec{"worker": {Enabled: &on}},
		Capabilities: &config.AppCapabilities{WireEndpoints: []string{"chat_completions"}},
	}))
	s.configStore = config.NewMemoryAppsConfigStore(cfg)
	for _, path := range []string{"/api/create", "/api/copy", "/api/pull", "/api/push"} {
		body := map[string]any{"model": "m", "source": "m", "destination": "copy", "stream": false}
		response := makeAuthRequest(t, s, http.MethodPost, path, TestUserKey, body)
		require.Equal(t, http.StatusForbidden, response.Code, string(response.Body))
	}
}
