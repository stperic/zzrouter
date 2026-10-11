package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
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

	withToken := func(token string) *pkgConfig.AppsConfig {
		cfg := &pkgConfig.AppsConfig{Version: "1.0", Name: "test"}
		nas := pkgConfig.NewOllamaConnectProvider("ollama-nas", daemon.URL, token)
		require.NoError(t, cfg.AddApp("ollama-nas", pkgConfig.ServiceConfig{
			Enabled: nas.Enabled, Name: "ollama-nas", Mode: "external", Protocol: nas.Protocol,
			Runtime:      &pkgConfig.AppRuntimeConfig{Endpoint: daemon.URL, API: nas.Runtime.API},
			Capabilities: nas.Capabilities,
		}))
		return cfg
	}
	var current atomic.Pointer[pkgConfig.AppsConfig]
	current.Store(withToken("first-token"))
	mgr, err := prov_apps.NewProviderAppManager(current.Load(), prov_apps.WithBackendResolver(backend.NewResolver(current.Load).Resolve))
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

	// An edit reaches the node as a new config, never as a write to the one in use.
	current.Store(withToken("second-token"))
	mgr.ReloadConfig(current.Load())
	assert.Equal(t, "Bearer second-token", load("b"), "no snapshot: the edited token is sent")
}
