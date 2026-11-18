package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/teams"
	"github.com/stperic/zzrouter/pkg/config"
	modelgroup "github.com/stperic/zzrouter/pkg/model/group"
	"github.com/stretchr/testify/require"
)

func TestIntegrationOllamaMutationRoleAdmission(t *testing.T) {
	var mutations atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/tags":
			_, _ = w.Write([]byte(`{"models":[{"name":"m","model":"m","details":{"format":"gguf"}}]}`))
		case "/api/show":
			_, _ = w.Write([]byte(`{"capabilities":["completion"]}`))
		case "/api/ps":
			_, _ = w.Write([]byte(`{"models":[]}`))
		case "/api/version":
			_, _ = w.Write([]byte(`{"version":"0.0.0-fixture"}`))
		case "/api/me":
			t.Error("a route-only handshake must not reveal the upstream account")
		default:
			mutations.Add(1)
			_, _ = w.Write([]byte(`{"status":"success"}`))
		}
	}))
	t.Cleanup(upstream.Close)
	s := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, UserKey: TestUserKey})
	provider := config.NewOllamaConnectProvider("ollama", upstream.URL, "")
	cfg := &config.AppsConfig{}
	require.NoError(t, cfg.AddApp("ollama", config.ServiceConfig{Enabled: provider.Enabled, Mode: "external", Protocol: provider.Protocol, Runtime: &config.AppRuntimeConfig{Endpoint: upstream.URL, API: provider.Runtime.API}, Capabilities: provider.Capabilities}))
	s.configStore = config.NewMemoryAppsConfigStore(cfg)
	s.providers.appMgr.ReloadConfig(cfg)
	s.model.Registry.ReloadConfig(cfg)
	s.model.Cache.Invalidate()

	for _, tc := range []struct{ method, path string }{
		{"POST", "/api/create"}, {"POST", "/api/pull"}, {"POST", "/api/copy"},
		{"POST", "/api/push"}, {"DELETE", "/api/delete"}, {"POST", "/api/blobs/sha256:fixture"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			body := map[string]any{"model": "m", "source": "m", "destination": "copy", "stream": false}
			before := mutations.Load()
			for _, credential := range []string{TestUserKey, ""} {
				response := makeAuthRequest(t, s, tc.method, tc.path, credential, body)
				want := http.StatusForbidden
				if credential == "" {
					want = http.StatusUnauthorized
				}
				require.Equal(t, want, response.Code, string(response.Body))
				require.Equal(t, before, mutations.Load(), "rejection must precede upstream mutation")
			}
			response := makeAuthRequest(t, s, tc.method, tc.path, TestAdminKey, body)
			require.Equal(t, http.StatusOK, response.Code, string(response.Body))
			require.Greater(t, mutations.Load(), before)
		})
	}
	// Trusted worker dispatch still receives the synthetic admin context.
	worker := buildWorkerCompatEngine(s)
	request := httptest.NewRequest(http.MethodPost, "/api/create", strings.NewReader(`{"model":"m"}`))
	request.Header.Set("X-API-Key", "coordinator-key-not-a-worker-key")
	result := httptest.NewRecorder()
	worker.ServeHTTP(result, request)
	require.Equal(t, http.StatusOK, result.Code, result.Body.String())

	require.NoError(t, s.model.Groups.Set("coding", modelgroup.ModelGroup{Replicas: []modelgroup.Replica{{Name: "local", App: "ollama", Model: "m"}}}))
	_, err := s.services.Teams.CreateTeam(&CreateTeamRequest{ID: "routes", Name: "Routes", RouteAccess: teams.RouteAccess{Mode: "selected", Routes: []string{"coding"}}}, "admin")
	require.NoError(t, err)
	routeKey := loadTestKey(t, s, "ollama-route-client", keys.VirtualKey{Name: "Route client", Role: "user", TeamID: "routes"})
	for _, tc := range []struct {
		method, path string
		status       int
	}{{"GET", "/api/version", 200}, {"POST", "/api/me", 401}} {
		response := makeAuthRequest(t, s, tc.method, tc.path, routeKey, nil)
		require.Equal(t, tc.status, response.Code, string(response.Body))
	}
}
