package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/routing"

	"github.com/stperic/zzrouter/pkg/model/integrity"
	"github.com/stperic/zzrouter/pkg/modelregistry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolvedModelFeatureProvenanceAndPreview(t *testing.T) {
	s, _ := assetTestNode(t)
	root := t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
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
	preview, err := launchParameters(t, s, "vendor/model", "llamacpp", "chat", nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "feature:vision", preview.Sources["mmproj"])
	assert.Equal(t, filepath.Join(dir, "mmproj-F16.gguf"), preview.Params["mmproj"])
	suppressed, err := launchParameters(t, s, "vendor/model", "llamacpp", "chat", map[string]string{"mmproj": "auto"}, nil)
	require.NoError(t, err)
	assert.NotContains(t, suppressed.Params, "mmproj")
}

func TestResolvedWorkerRouteAndRemotePropagation(t *testing.T) {
	worker := createSeededTestNode(t, seededTestNodeConfig{TestNodeConfig: TestNodeConfig{AdminKey: TestAdminKey, ClusterMode: config.ClusterModeWorker, NodeName: "feature-worker"}})
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
	err     error
}

func (r *featureResolvedRouter) Route(ctx context.Context, req *routing.Request) (*routing.Response, error) {
	r.request = req
	r.calls = append(r.calls, *req)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.err != nil {
		return nil, r.err
	}
	response := httptest.NewRecorder()
	r.worker.ServeHTTP(response, httptest.NewRequestWithContext(ctx, req.Method, req.Path, nil))
	return &routing.Response{StatusCode: response.Code, Body: response.Body.Bytes(), Headers: response.Header()}, nil
}

func TestRunPreviewRendersLaunchCommand(t *testing.T) {
	s := variantNode(t)
	defineVariants(t, s)
	response, err := s.newLoadExecutor().PreviewLocalRun(t.Context(), &PreviewRunRequest{Provider: "llamacpp", ModelName: variantAgent, Parameters: map[string]string{"ctx-size": "65536"}})
	require.NoError(t, err)
	require.NotContains(t, response.FullCommand, "${MODEL_PATH}")
	require.NotContains(t, response.FullCommand, "${PORT}")
	require.Contains(t, response.FullCommand, qwenModel+".gguf")
	require.Contains(t, response.FullCommand, "--ctx-size 65536")
	require.Contains(t, response.FullCommand, response.Parameters["ctx-size"])
}

func TestFeatureEvidencePublicSurfaces(t *testing.T) {
	root := t.TempDir()
	modelregistry.SetModelsRootDirOverride(root)
	t.Cleanup(modelregistry.ClearModelsRootDirOverride)
	dir := filepath.Join(root, "vendor/model")
	require.NoError(t, os.MkdirAll(dir, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "model.gguf"), []byte("weights"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), []byte(`invalid`), 0600))
	worker, _ := assetTestNode(t)
	require.NoError(t, worker.configStore.ApplyParameterPatch("llamacpp", func(svc *config.ServiceConfig) error {
		svc.Enabled = new(true)
		svc.Features = map[string]config.Feature{"vision": {Runtime: "llamacpp", When: "vision_config", Execution: &svc.Runtime.Execution, WireEndpoints: []string{"chat_completions"}}}
		return nil
	}))
	prov_apps.WithLocalCatalog(func(context.Context, string) (bool, error) { return true, nil })(worker.providers.appMgr)
	worker.model.Resolver = staticModelResolver{provider: "llamacpp"}
	coordinator, _ := assetTestNode(t)
	router := &previewWorkerRouter{worker: buildInternalEngine(worker)}
	coordinator.services.Load.router = router
	coordinator.services.Runs.router = router
	body := []byte(`{"model":"vendor/model","provider":"llamacpp","node":"feature-worker","launch_mode":"native"}`)
	for _, path := range []string{"/zzrouter/v1/runs", "/zzrouter/v1/runs/preview", "/zzrouter/v1/runs/load"} {
		t.Run(path, func(t *testing.T) {
			requestBody := body
			if path != "/zzrouter/v1/runs" {
				requestBody = []byte(`{"model":"vendor/model","provider":"llamacpp","node":"feature-worker"}`)
			}
			w := assetRequest(t, coordinator, http.MethodPost, path, requestBody)
			require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
			assert.Contains(t, w.Body.String(), `"code":"model_feature_evidence_invalid"`)
			assert.Contains(t, w.Body.String(), "operator")
		})
	}
	result := callMCPWireTool(t, coordinator, "zz_run_preview", map[string]any{"model": "vendor/model", "provider": "llamacpp", "node": "feature-worker"})
	require.Equal(t, true, result["isError"])
	assert.Equal(t, "model_feature_evidence_invalid", result["structuredContent"].(map[string]any)["code"])
	assert.Contains(t, result["content"].([]any)[0].(map[string]any)["text"], "model_feature_evidence_invalid")
	compat := httptest.NewServer(buildWorkerCompatEngine(worker))
	t.Cleanup(compat.Close)
	for _, path := range []string{"/v1/chat/completions", "/v1/messages", "/v1/responses"} {
		t.Run(path, func(t *testing.T) {
			body := []byte(`{"model":"vendor/model","messages":[{"role":"user","content":"hi"}],"input":"hi","max_tokens":1,"stream":false}`)
			req := routedRequest(httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body)))
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = req
			coordinator.proxyToWorkerURL(c, compat.URL, "feature-worker", "llamacpp", body, compat.Client())
			require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
			assert.Contains(t, w.Body.String(), `"code":"model_feature_evidence_invalid"`)
		})
	}
	assert.Empty(t, worker.providers.appMgr.ListInstances(""))
	assert.Empty(t, worker.providers.appMgr.Ports().ListAllAllocated())
	run := instance.NewInstance("evidence-status", "llamacpp", "vendor/model", 8100, 0, 0)
	run.Config = instance.Config{Provider: "llamacpp", Model: "vendor/model", Port: 8100}
	status := worker.providers.appMgr.ParametersStatus(t.Context(), run)
	assert.Equal(t, instance.ParametersUnknown, status.State)
	assert.Contains(t, status.Error, prov_apps.ErrFeatureEvidence.Error())
	details := worker.providers.appMgr.ModelFeatureDetails("llamacpp", "vendor/model", nil)
	assert.Equal(t, "unknown", details["features"].(map[string]string)["vision"])
}
