package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps/protocol"
)

// A call zzRouter makes to a provider on its own resolves the provider's
// endpoint and credential from live config: a token-protected Ollama
// (ollama connect --token) is sent its token, and an edit to the token
// applies on the next call.
func TestProviderCallsSendTheConfiguredCredential(t *testing.T) {
	var auth string
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/ps" {
			auth = r.Header.Get("Authorization")
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
	ollama := protocol.NewOllamaProvider(nil)
	e := NewParamsExecutor(func() *pkgConfig.AppsConfig { return cfg }, nil, nil, nil, nil, nil,
		func(string) (protocol.FullProvider, bool) { return ollama, true }, nil, nil, nil)

	e.listRunningModelNames(context.Background(), "ollama-nas")
	assert.Equal(t, "Bearer first-token", auth)

	require.NoError(t, cfg.UpdateApp("ollama-nas", func(sc *pkgConfig.ServiceConfig) error {
		sc.Runtime.API = &pkgConfig.APIConfig{AuthType: pkgConfig.AuthTypeBearer, Token: "second-token"}
		return nil
	}))
	e.listRunningModelNames(context.Background(), "ollama-nas")
	assert.Equal(t, "Bearer second-token", auth, "no snapshot: the edited token is sent")
}
