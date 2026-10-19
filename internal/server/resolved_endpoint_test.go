package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
)

// An endpoint overlay value is part of what that endpoint's launch gets,
// so /resolved shows it, with its overlay, only for that endpoint.
func TestResolvedEndpointShowsItsOverlay(t *testing.T) {
	s, _ := assetTestNode(t)
	require.NoError(t, s.configStore.ApplyParameterPatch("llamacpp", func(svc *pkgConfig.ServiceConfig) error {
		svc.Defaults.Endpoints = map[string]pkgConfig.EndpointOverlay{"embeddings": {Parameters: map[string]string{"pooling": "mean"}}}
		return nil
	}))
	resolved := func(query string) (int, resolvedResponse) {
		w := assetRequest(t, s, http.MethodGet, "/zzrouter/v1/providers/llamacpp/resolved"+query, nil)
		var r resolvedResponse
		if w.Code == http.StatusOK {
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &r))
		}
		return w.Code, r
	}

	code, r := resolved("?endpoint=embeddings")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "mean", r.Parameters["pooling"].Value)
	assert.Equal(t, "embeddings", r.Parameters["pooling"].Endpoint)

	code, r = resolved("")
	require.Equal(t, http.StatusOK, code)
	assert.NotContains(t, r.Parameters, "pooling", "the chat launch does not get the embeddings overlay")

	code, _ = resolved("?endpoint=summaries")
	assert.Equal(t, http.StatusBadRequest, code)
}

func TestResolvedEndpointReachesTheWorker(t *testing.T) {
	worker := createSeededTestNode(t, seededTestNodeConfig{TestNodeConfig: TestNodeConfig{AdminKey: TestAdminKey, ClusterMode: pkgConfig.ClusterModeWorker, NodeName: "endpoint-worker"}})
	router := &featureResolvedRouter{worker: buildInternalEngine(worker)}
	executor := worker.newParamsExecutor()
	executor.nodename = func() string { return "coordinator" }
	executor.router = router
	engine := gin.New()
	engine.GET("/providers/:name/resolved", executor.HandleResolved)
	out := httptest.NewRecorder()
	engine.ServeHTTP(out, httptest.NewRequest(http.MethodGet, "/providers/llamacpp/resolved?node=endpoint-worker&model=vendor/model&endpoint=embeddings", nil))
	require.Equal(t, http.StatusOK, out.Code, out.Body.String())
	require.NotNil(t, router.request)
	assert.Equal(t, "/zzrouter/v1/internal/providers/llamacpp/resolved?endpoint=embeddings&model=vendor%2Fmodel", router.request.Path)
}
