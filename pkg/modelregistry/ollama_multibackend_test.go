package modelregistry

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stperic/zzrouter/pkg/modelregistry/source/huggingface"
	"github.com/stperic/zzrouter/pkg/modelregistry/source/ollama"
)

// TestBuildModelList_IteratesOllamaProtocolExternals validates the central
// claim of the ollama-connect feature: buildModelList walks every enabled
// protocol:ollama external provider, calls /api/tags on each backend, and
// tags the returned models with the source provider key as AssignedApp.
// Without per-backend tagging, the routing pipeline cannot distinguish
// between models hosted on the managed install and models hosted on
// user-added connect instances.
//
// The test uses two httptest servers so it runs without any real Ollama
// daemon. One backend returns "alpha" models; the other returns "beta"
// models. The registry should see all four with AssignedApp set to the
// provider name that owns each.
func TestBuildModelList_IteratesOllamaProtocolExternals(t *testing.T) {
	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[
			{"name":"alpha-1","model":"alpha-1","size":100,"digest":"a1","details":{"format":"gguf"}},
			{"name":"alpha-2","model":"alpha-2","size":200,"digest":"a2","details":{"format":"gguf"}}
		]}`))
	}))
	defer srvA.Close()

	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[
			{"name":"beta-1","model":"beta-1","size":300,"digest":"b1","details":{"format":"gguf"}}
		]}`))
	}))
	defer srvB.Close()

	cfg := newTestAppsConfigWithOllamaBackends(t, map[string]string{
		"ollama-a": srvA.URL,
		"ollama-b": srvB.URL,
	})

	modelsRoot := t.TempDir()
	r := &Registry{
		modelsRoot:           modelsRoot,
		ollamaConnector:      ollama.NewConnector(),
		huggingfaceConnector: huggingface.NewConnector(modelsRoot),
		appsConfigFn:         func() *config.AppsConfig { return cfg },
	}

	models, err := r.ListAllModels()
	require.NoError(t, err)

	// Collect names → AssignedApp tags. We expect the three models from
	// the two backends, each tagged with its owning provider key.
	got := make(map[string]string, len(models))
	for _, m := range models {
		if m.SourceRepo == metadata.SourceOllama {
			got[m.Name] = m.AssignedApp
		}
	}

	assert.Equal(t, "ollama-a", got["alpha-1"], "alpha-1 must be tagged with its source provider")
	assert.Equal(t, "ollama-a", got["alpha-2"], "alpha-2 must be tagged with its source provider")
	assert.Equal(t, "ollama-b", got["beta-1"], "beta-1 must be tagged with its source provider")
}

// TestBuildModelList_ContinuesOnBackendFailure pins the resilience
// invariant: one unreachable backend must not block another from
// contributing its models. Without this, a single misconfigured connect
// instance would empty the entire ollama model catalog.
func TestBuildModelList_ContinuesOnBackendFailure(t *testing.T) {
	srvOK := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"models":[
			{"name":"survivor","model":"survivor","size":100,"digest":"s1","details":{"format":"gguf"}}
		]}`))
	}))
	defer srvOK.Close()

	// Dead backend: closed immediately so connect attempts fail fast.
	srvDead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srvDead.Close()

	cfg := newTestAppsConfigWithOllamaBackends(t, map[string]string{
		"ollama-live": srvOK.URL,
		"ollama-dead": srvDead.URL,
	})

	modelsRoot := t.TempDir()
	r := &Registry{
		modelsRoot:           modelsRoot,
		ollamaConnector:      ollama.NewConnector(),
		huggingfaceConnector: huggingface.NewConnector(modelsRoot),
		appsConfigFn:         func() *config.AppsConfig { return cfg },
	}

	models, err := r.ListAllModels()
	require.NoError(t, err)

	found := false
	for _, m := range models {
		if m.Name == "survivor" && m.SourceRepo == metadata.SourceOllama {
			assert.Equal(t, "ollama-live", m.AssignedApp)
			found = true
		}
	}
	assert.True(t, found, "survivor model must be returned even when the other backend is dead")
	_, failures, err := r.ModelSnapshot(t.Context())
	require.NoError(t, err)
	require.Contains(t, failures, "ollama-dead")
	require.NotContains(t, failures, "ollama-live")
	delete(failures, "ollama-dead")
	_, failures, err = r.ModelSnapshot(t.Context())
	require.NoError(t, err)
	require.Contains(t, failures, "ollama-dead", "callers cannot erase scan evidence")
}

func TestModelSnapshotClearsInventoryFailureAfterRecovery(t *testing.T) {
	var available atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !available.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer srv.Close()
	cfg := newTestAppsConfigWithOllamaBackends(t, map[string]string{"generic-daemon": srv.URL})
	root := t.TempDir()
	r := &Registry{modelsRoot: root, ollamaConnector: ollama.NewConnector(), huggingfaceConnector: huggingface.NewConnector(root), appsConfigFn: func() *config.AppsConfig { return cfg }}
	models, failures, err := r.ModelSnapshot(t.Context())
	require.NoError(t, err)
	require.Empty(t, models)
	require.Contains(t, failures, "generic-daemon", "an empty catalog cannot erase failed discovery")
	available.Store(true)
	r.InvalidateScanCache()
	models, failures, err = r.ModelSnapshot(t.Context())
	require.NoError(t, err)
	require.Empty(t, models, "a valid empty catalog remains a success")
	require.Empty(t, failures)
}

// newTestAppsConfigWithOllamaBackends writes per-kind yaml files for the
// given provider→endpoint map and loads them via config.LoadAppsConfig,
// returning the resulting *AppsConfig. Uses minimal external yaml that
// satisfies schema/external.schema.json validation.
func newTestAppsConfigWithOllamaBackends(t *testing.T, backends map[string]string) *config.AppsConfig {
	t.Helper()
	dir := t.TempDir()
	externalDir := filepath.Join(dir, "external")
	require.NoError(t, os.MkdirAll(externalDir, 0o755))

	for name, endpoint := range backends {
		body := "description: test ollama backend\n" +
			"enabled: true\n" +
			"protocol: ollama\n" +
			"runtime:\n" +
			"  endpoint: " + endpoint + "\n" +
			"capabilities:\n" +
			"  wire_endpoints: [chat_completions]\n"
		providerDir := filepath.Join(externalDir, name)
		require.NoError(t, os.MkdirAll(providerDir, 0o755))
		require.NoError(t, os.WriteFile(
			filepath.Join(providerDir, "config.yaml"),
			[]byte(body),
			0o644,
		))
	}

	cfg, err := config.LoadAppsConfig(dir)
	require.NoError(t, err)
	return cfg
}
