package ollama

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
)

// The daemon the ollama provider declares is reached as declared: every
// call the connector makes carries the provider's own token, and the
// endpoint and token come from one reload, so they cannot disagree.
func TestConnectorCallsCarryTheDaemonsCredential(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]string{}
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.URL.Path] = r.Header.Get("Authorization")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[],"status":"success"}`))
	}))
	t.Cleanup(daemon.Close)

	cfg := &config.AppsConfig{Version: "1.0", Name: "test"}
	p := config.NewOllamaConnectProvider(constants.AppOllama, daemon.URL, "daemon-token")
	require.NoError(t, cfg.AddApp(constants.AppOllama, config.ServiceConfig{
		Enabled: p.Enabled, Name: constants.AppOllama, Mode: constants.AppModeExternal, Protocol: p.Protocol,
		Runtime:      &config.AppRuntimeConfig{Endpoint: daemon.URL, API: p.Runtime.API},
		Capabilities: p.Capabilities,
	}))
	c := NewConnector()
	c.ReloadConfig(cfg)

	ctx := context.Background()
	_, err := c.ListModels(ctx)
	require.NoError(t, err)
	_, _ = c.ModelExists(ctx, "m")
	_ = c.DeleteModel(ctx, "m")
	_ = c.PullModelWithContext(ctx, "m", nil)

	for _, path := range []string{"/api/tags", "/api/show", "/api/delete", "/api/pull"} {
		assert.Equal(t, "Bearer daemon-token", seen[path], path)
	}
}
