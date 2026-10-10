package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/prov_apps"
)

// A call zzRouter makes to a provider on its own resolves the provider's
// endpoint and credential from live config: a token-protected Ollama
// (ollama connect --token) is sent its token, and an edit to the token
// applies on the next call.
func TestProviderCallsSendTheConfiguredCredential(t *testing.T) {
	loads := make(chan string, 2)
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/generate" {
			loads <- r.Header.Get("Authorization")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[],"data":[]}`))
	}))
	t.Cleanup(daemon.Close)

	cfg := &pkgConfig.AppsConfig{Version: "1.0", Name: "test"}
	nas := pkgConfig.NewOllamaConnectProvider("ollama-nas", daemon.URL, "first-token")
	require.NoError(t, cfg.AddApp("ollama-nas", pkgConfig.ServiceConfig{
		Enabled: nas.Enabled, Name: "ollama-nas", Mode: "external", Protocol: nas.Protocol,
		Runtime:      &pkgConfig.AppRuntimeConfig{Endpoint: daemon.URL, API: nas.Runtime.API},
		Capabilities: nas.Capabilities,
	}))
	mgr, err := prov_apps.NewProviderAppManager(cfg, prov_apps.WithBackendResolver(backend.NewResolver(func() *pkgConfig.AppsConfig { return cfg }).Resolve))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, mgr.Stop(context.Background())) })
	load := func(model string) string {
		_, err := mgr.EnsureRun(t.Context(), prov_apps.EnsureRequest{Provider: "ollama-nas", Model: model})
		require.NoError(t, err)
		select {
		case auth := <-loads:
			return auth
		case <-time.After(readinessWait):
			t.Fatal("the model was never loaded")
			return ""
		}
	}

	assert.Equal(t, "Bearer first-token", load("a"))

	require.NoError(t, cfg.UpdateApp("ollama-nas", func(sc *pkgConfig.ServiceConfig) error {
		sc.Runtime.API = &pkgConfig.APIConfig{AuthType: pkgConfig.AuthTypeBearer, Token: "second-token"}
		return nil
	}))
	assert.Equal(t, "Bearer second-token", load("b"), "no snapshot: the edited token is sent")
}
