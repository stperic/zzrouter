package modelregistry

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stperic/zzrouter/pkg/modelregistry/source/huggingface"
	"github.com/stperic/zzrouter/pkg/modelregistry/source/ollama"
)

func TestExternalOpenAIInventoryKeepsProvidersSeparate(t *testing.T) {
	var changed atomic.Bool
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/models", r.URL.Path)
		if changed.Load() {
			_, _ = w.Write([]byte(`{"data":[{"id":"replacement"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"same","status":"unloaded"}]}`))
	}))
	t.Cleanup(first.Close)
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"same"}]}`))
	}))
	t.Cleanup(second.Close)
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(broken.Close)

	cfg := &config.AppsConfig{Version: "1.0", Name: "test"}
	for name, endpoint := range map[string]string{"strata": first.URL, "other-openai": second.URL, "broken-openai": broken.URL} {
		require.NoError(t, cfg.AddApp(name, config.ServiceConfig{
			Name: name, Mode: "external", Protocol: config.ProtocolOpenAI, Enabled: new(true),
			Runtime:      &config.AppRuntimeConfig{Endpoint: endpoint, API: &config.APIConfig{AuthType: config.AuthTypeNone}},
			Capabilities: &config.AppCapabilities{WireEndpoints: []string{"chat_completions"}},
		}))
	}
	root := t.TempDir()
	r := &Registry{modelsRoot: root, ollamaConnector: ollama.NewConnector(), huggingfaceConnector: huggingface.NewConnector(root), appsConfigFn: func() *config.AppsConfig { return cfg }}
	models, failures, err := r.ModelSnapshot(t.Context())
	require.NoError(t, err)
	require.Len(t, models, 2)
	assert.Equal(t, "same", models[0].Name)
	assert.Equal(t, "same", models[1].Name)
	assert.NotEqual(t, models[0].AssignedApp, models[1].AssignedApp)
	for _, model := range models {
		assert.Equal(t, metadata.SourceExternalOpenAI, model.SourceRepo)
		assert.Equal(t, "same", model.SourceID)
	}
	assert.Contains(t, failures, "broken-openai")
	assert.NotContains(t, failures, "strata")

	changed.Store(true)
	r.InvalidateScanCache()
	models, _, err = r.ModelSnapshot(t.Context())
	require.NoError(t, err)
	assert.Contains(t, []string{models[0].Name, models[1].Name}, "replacement")
}
