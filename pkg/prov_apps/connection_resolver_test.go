package prov_apps

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stperic/common/secrets"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stretchr/testify/require"
)

func TestProviderManagerUsesCurrentConnectionForProbeAndAvailability(t *testing.T) {
	t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
	t.Setenv("CONNECTION_MANAGER_TOKEN", "external-fixture")
	var auth atomic.Value
	auth.Store("")
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth.Store(r.Header.Get("Authorization"))
		w.WriteHeader(200)
	}))
	defer endpoint.Close()
	cfg := &config.AppsConfig{}
	require.NoError(t, cfg.AddApp("fixture", config.ServiceConfig{Mode: "cloud", Enabled: new(true), Runtime: &config.AppRuntimeConfig{Endpoint: endpoint.URL, API: &config.APIConfig{AuthType: config.AuthTypeBearer, Token: "${CONNECTION_MANAGER_TOKEN}"}}, Capabilities: &config.AppCapabilities{WireEndpoints: []string{"chat_completions"}}, Connection: &config.ConnectionConfig{Fields: []config.ConnectionField{{ID: "token", Label: "Token", Type: "text", Env: "CONNECTION_MANAGER_TOKEN", Secret: true, Required: true}}}}))
	vault, err := secrets.Open(filepath.Join(t.TempDir(), "connections.enc"), bytes.Repeat([]byte{9}, 32))
	require.NoError(t, err)
	defer func() { _ = vault.Close() }()
	resolver := backend.NewResolver(func() *config.AppsConfig { return cfg }, vault)
	manager, err := NewProviderAppManager(cfg, WithConnectionResolver(resolver), WithHTTPClient(endpoint.Client()))
	require.NoError(t, err)
	defer func() { _ = manager.Stop(t.Context()) }()
	for i, token := range []string{"first-managed", "second-managed"} {
		_, err = vault.Replace(t.Context(), "fixture", uint64(i), map[string]string{"token": token, backend.ConnectionEndpointField: endpoint.URL})
		require.NoError(t, err)
		require.True(t, manager.ProviderEndpointHealthy(t.Context(), "fixture"))
		require.Equal(t, "Bearer "+token, auth.Load())
		state, _ := manager.lifecycleFor("fixture", cfg.Find("fixture"))
		require.Equal(t, StateCloudAvailable, state)
	}
	_, err = vault.Remove(t.Context(), "fixture", 2)
	require.NoError(t, err)
	require.False(t, manager.ProviderEndpointHealthy(t.Context(), "fixture"))
	state, _ := manager.lifecycleFor("fixture", cfg.Find("fixture"))
	require.Equal(t, StateNotInstalled, state)
}
