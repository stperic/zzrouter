package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/routing"

	"github.com/stperic/zzrouter/pkg/model/integrity"
	"github.com/stperic/zzrouter/pkg/modelregistry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolvedModelFeatureProvenanceAndPreview(t *testing.T) {
	s, _ := assetTestNode(t)
	root := t.TempDir()
	previousRoot, err := modelregistry.GetModelsRootDir()
	require.NoError(t, err)
	modelregistry.SetModelsRootDirOverride(root)
	t.Cleanup(func() { modelregistry.SetModelsRootDirOverride(previousRoot) })
	dir := filepath.Join(root, "vendor/model")
	require.NoError(t, os.MkdirAll(dir, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "model.gguf"), []byte("weights"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "mmproj-F16.gguf"), []byte("projector"), 0600))
	require.NoError(t, integrity.RecordFiles(dir, "vendor/model", "", integrity.FileChecksum{RelativePath: "mmproj-F16.gguf", Size: 9, SHA256: "manifest-digest", Feature: "vision"}))
	w := assetRequest(t, s, http.MethodGet, "/zzrouter/v1/providers/llamacpp/resolved?model=vendor/model", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var out resolvedResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	assert.Equal(t, "feature:vision", out.Parameters["mmproj"].Tier)
	assert.Equal(t, "manifest-digest", out.Parameters["mmproj"].SHA256)
	assert.Equal(t, filepath.Join(dir, "mmproj-F16.gguf"), out.Parameters["mmproj"].Value)
	preview, err := s.resolveForLaunch("vendor/model", "llamacpp", "chat", nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "feature:vision", preview.ParamSources["mmproj"])
	assert.Equal(t, filepath.Join(dir, "mmproj-F16.gguf"), preview.Params["mmproj"])
	suppressed, err := s.resolveForLaunch("vendor/model", "llamacpp", "chat", map[string]string{"mmproj": "auto"}, nil)
	require.NoError(t, err)
	assert.NotContains(t, suppressed.Params, "mmproj")
}

func TestResolvedWorkerRouteAndRemotePropagation(t *testing.T) {
	worker := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, ClusterMode: config.ClusterModeWorker, NodeName: "feature-worker", SeedProvidersDir: true})
	require.True(t, worker.node.IsWorker())
	remote := makeInternalRequest(t, worker, TestRequest{Method: http.MethodGet, Path: "/zzrouter/v1/internal/providers/llamacpp/resolved?model=vendor/model"})
	require.Equal(t, http.StatusOK, remote.Code, string(remote.Body))
	router := &featureResolvedRouter{worker: buildInternalEngine(worker)}
	executor := worker.newParamsExecutor()
	executor.nodename = func() string { return "coordinator" }
	executor.router = router
	engine := gin.New()
	engine.GET("/providers/:name/resolved", executor.HandleResolved)
	request := httptest.NewRequest(http.MethodGet, "/providers/llamacpp/resolved?node=feature-worker&model=vendor/model", nil)
	out := httptest.NewRecorder()
	engine.ServeHTTP(out, request)
	assert.Equal(t, http.StatusOK, out.Code)
	assert.Equal(t, string(remote.Body), out.Body.String())
	require.NotNil(t, router.request)
	assert.Equal(t, "feature-worker", router.request.Node)
	assert.Equal(t, "/zzrouter/v1/internal/providers/llamacpp/resolved?model=vendor%2Fmodel", router.request.Path)
	assert.Equal(t, remote.Headers.Get("ETag"), out.Header().Get("ETag"))
	assert.Contains(t, router.request.Path, "model=vendor%2Fmodel")
	assert.NotContains(t, router.request.Path, "node=", "worker resolves its own node tier")
}

type featureResolvedRouter struct {
	fakeRouter
	worker  http.Handler
	request *routing.Request
}

func (r *featureResolvedRouter) Route(ctx context.Context, req *routing.Request) (*routing.Response, error) {
	r.request = req
	response := httptest.NewRecorder()
	r.worker.ServeHTTP(response, httptest.NewRequestWithContext(ctx, req.Method, req.Path, nil))
	return &routing.Response{StatusCode: response.Code, Body: response.Body.Bytes(), Headers: response.Header()}, nil
}
