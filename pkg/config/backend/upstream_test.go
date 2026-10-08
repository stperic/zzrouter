package backend

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stperic/zzrouter/pkg/constants"
)

// A provider's runtime.api is what decides what it is sent.
func TestForProvider(t *testing.T) {
	bearer := &config.APIConfig{AuthType: config.AuthTypeBearer, Token: "${K}"}
	assert.Equal(t, CallerKey(), ForProvider(nil), "no api block: an external endpoint checks the caller")
	assert.Equal(t, CallerKey(), ForProvider(&config.APIConfig{AuthType: config.AuthTypeCaller}))
	assert.Equal(t, Engine(), ForProvider(&config.APIConfig{AuthType: config.AuthTypeNone}))
	assert.Equal(t, Credential(bearer), ForProvider(bearer))
	assert.Equal(t, Engine(), Upstream{}, "the zero value sends nothing")
}

// The shipped providers resolve to what they are owed: Ollama, a daemon
// that takes no credential, gets no key; a cloud API its own credential.
func TestResolve_ShippedProviders(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, templates.InstallDefaults(dir))
	store, err := config.NewAppsConfigStore(dir)
	require.NoError(t, err)
	require.NoError(t, store.SetProviderEnabled(constants.AppOllama, true))
	require.NoError(t, store.SetProviderEnabled("openai", true))
	r := NewResolver(store.Config)

	ollama, ok := r.Resolve(constants.AppOllama)
	require.True(t, ok)
	assert.Equal(t, Engine(), ollama.Upstream)
	assert.False(t, ollama.Cloud)

	openai, ok := r.Resolve("openai")
	require.True(t, ok)
	assert.True(t, openai.Cloud)
	assert.False(t, openai.Upstream.ForwardsCallerKey())
	require.NotNil(t, openai.Upstream.API(), "a cloud API is sent its own credential")
}

// A request zzRouter makes on its own goes to path under the endpoint,
// whatever prefix or trailing slash the endpoint was configured with, and
// carries only the provider's own credential.
func TestResolvedNewRequest(t *testing.T) {
	bearer := Credential(&config.APIConfig{AuthType: config.AuthTypeBearer, Token: "t"})
	for endpoint, want := range map[string]string{
		"http://nas.lan:11434":        "http://nas.lan:11434/api/tags",
		"http://nas.lan:11434/":       "http://nas.lan:11434/api/tags",
		"https://gw.example/ollama/":  "https://gw.example/ollama/api/tags",
		"https://gw.example/ollama//": "https://gw.example/ollama/api/tags",
	} {
		req, err := (&Resolved{Endpoint: endpoint, Upstream: bearer}).NewRequest(context.Background(), http.MethodGet, "/api/tags", nil)
		require.NoError(t, err)
		assert.Equal(t, want, req.URL.String(), endpoint)
		assert.Equal(t, "Bearer t", req.Header.Get("Authorization"))
	}
	req, err := (&Resolved{Endpoint: "http://h", Upstream: CallerKey()}).NewRequest(context.Background(), http.MethodGet, "/", nil)
	require.NoError(t, err)
	assert.Empty(t, req.Header, "no caller, so nothing of a caller's")
}
