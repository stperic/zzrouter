package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWorkerCompatRootHandshake(t *testing.T) {
	s := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, ClusterMode: pkgConfig.ClusterModeWorker})
	endpoint := httptest.NewTLSServer(buildWorkerCompatEngine(s))
	defer endpoint.Close()
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		req, err := http.NewRequestWithContext(t.Context(), method, endpoint.URL+"/", nil)
		require.NoError(t, err)
		req.Header.Set("Accept", "*/*")
		resp, err := endpoint.Client().Do(req)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Contains(t, resp.Header.Get("Content-Type"), "text/plain")
		if method == http.MethodHead {
			assert.Empty(t, body)
		} else {
			assert.Equal(t, ollamaRootProbeBody, string(body))
		}
		rec := httptest.NewRecorder()
		s.engine.ServeHTTP(rec, httptest.NewRequest(method, "/", nil))
		assert.Equal(t, http.StatusNotFound, rec.Code)
	}
}

// A worker registers no management API, and building that API is what used
// to construct the services the compat handlers call. So the worker's mTLS
// compat engine served /api/* handlers holding a nil OllamaService, and
// every coordinator-proxied Ollama inference request panicked on the first
// method call. The coordinator surfaced that as "backend returned status
// 500" -- blaming the model backend for a fault in the router.
func TestWorkerCompatEngineBindsInferenceServices(t *testing.T) {
	s := createTestNode(t, TestNodeConfig{
		AdminKey:    TestAdminKey,
		ClusterKey:  TestClusterKey,
		ClusterMode: pkgConfig.ClusterModeWorker,
	})

	require.NotNil(t, buildWorkerCompatEngine(s))
	require.NotNil(t, s.ollamaHandlers, "the compat engine must register the Ollama surface")

	assert.NotNil(t, s.ollamaHandlers.ollama,
		"/api/chat, /api/generate and /api/embed all call this; nil panics on the first request")
	assert.NotNil(t, s.ollamaHandlers.modelService,
		"/api/tags and /api/show read the catalog through this")
}

// The coordinator reaches the same binding through the management API. It
// is asserted separately because the two roles arrive by different routes,
// and only one of them was ever exercised.
func TestCoordinatorBindsInferenceServices(t *testing.T) {
	s := createTestNodeWithDefaults(t)

	require.NotNil(t, s.ollamaHandlers)
	assert.NotNil(t, s.ollamaHandlers.ollama)
	assert.NotNil(t, s.ollamaHandlers.modelService)
}
