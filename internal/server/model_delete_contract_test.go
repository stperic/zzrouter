package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/routing"
	"github.com/stretchr/testify/require"
)

// Synthetic weights exercise actual REST/MCP selection and filesystem effects.
func TestModelDeleteExactContract(t *testing.T) {
	root := t.TempDir()
	detailWeights(t, root, "org/a", "base", 4096)
	detailWeights(t, root, "org/b", "base-long", 8192)
	s := createTestNodeWithModelsDir(t, TestNodeConfig{AdminKey: TestAdminKey, SeedProvidersDir: true}, root, "delete-node")
	for _, model := range []string{"ase", "org/a", "base*", "missing"} {
		response := makeAuthRequest(t, s, "DELETE", "/zzrouter/v1/models", TestAdminKey, map[string]any{"model": model})
		if model == "base*" {
			require.Equal(t, 400, response.Code, string(response.Body))
		} else {
			require.Equal(t, 404, response.Code, string(response.Body))
		}
	}
	response := makeAuthRequest(t, s, "DELETE", "/zzrouter/v1/models", TestAdminKey, map[string]any{"model": "base"})
	require.Equal(t, 200, response.Code, string(response.Body))
	require.NoFileExists(t, filepath.Join(root, "org", "a", "base.gguf"))
	require.FileExists(t, filepath.Join(root, "org", "b", "base-long.gguf"))
}

type deleteWorkerClient struct {
	deploymentCatalogClient
	t      *testing.T
	engine *gin.Engine
	calls  int
}

func (c *deleteWorkerClient) Unicast(ctx context.Context, node, path string, params *mesh.QueryParams) (*mesh.Response, error) {
	c.calls++
	require.Equal(c.t, "worker", node)
	require.Equal(c.t, "/zzrouter/v1/internal/models", path)
	var selected DeleteModelsRequest
	require.NoError(c.t, json.Unmarshal(params.Body, &selected))
	require.Equal(c.t, "weights", selected.Model)
	require.Equal(c.t, "llamacpp", selected.Provider)
	require.Equal(c.t, "huggingface", selected.Registry)
	require.Equal(c.t, "org/model", selected.SourceID)
	request := httptest.NewRequest(params.Method, path, bytes.NewReader(params.Body)).WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	c.engine.ServeHTTP(recorder, request)
	return &mesh.Response{StatusCode: recorder.Code, Body: recorder.Body.Bytes(), Headers: recorder.Header()}, nil
}

func TestModelDeleteWorkerContract(t *testing.T) {
	worker, coordinator, dir := mutationNodes(t, "worker")
	client := &deleteWorkerClient{deploymentCatalogClient: deploymentCatalogClient{worker: worker}, t: t, engine: buildInternalEngine(worker)}
	coordinator.services.Model.router = routing.NewClusterAwareRouterWithClient(coordinator.node.Nodename(), client, "9090", coordinator.ServeLocalRequest, coordinator.httpClient)
	response := makeAuthRequest(t, coordinator, "DELETE", "/zzrouter/v1/models", TestAdminKey, map[string]any{"model": "weights", "provider": "llamacpp", "node": "worker"})
	require.Equal(t, 200, response.Code, string(response.Body))
	require.Equal(t, 1, client.calls)
	require.NoFileExists(t, filepath.Join(dir, "weights.gguf"))
	require.FileExists(t, filepath.Join(dir, "sentinel.gguf"))
	models, err := coordinator.model.Cache.ListModels(t.Context(), "worker", "", "", "weights")
	require.NoError(t, err)
	require.Empty(t, models, "worker publication must remove the selected catalog entry")
}

func TestModelDeleteAggregateEffectsContract(t *testing.T) {
	service := &ModelService{}
	response := &routing.Response{Body: []byte(`[{"node":"failed","status_code":503,"body":{"title":"Service Unavailable","status":503,"detail":"publication failed","deleted":1,"mutation_applied":true}},{"node":"ok","status_code":200,"body":{"deleted":2}},{"node":"offline","error":"unavailable"}]`)}
	_, err := service.aggregateDeleteResponses(response)
	var routed *RoutedError
	require.ErrorAs(t, err, &routed)
	require.Equal(t, 503, routed.StatusCode)
	require.NotNil(t, routed.ProblemDetails.Deleted)
	require.Equal(t, 3, *routed.ProblemDetails.Deleted, "later successful nodes must contribute completed effects")
	require.True(t, routed.ProblemDetails.MutationApplied)
}

func TestModelDeleteSelectorsContract(t *testing.T) {
	for _, req := range []DeleteModelsRequest{{Pattern: "*", Provider: "llamacpp"}, {Paths: []string{"weights"}, SourceID: "org/a"}, {Models: []ModelToDelete{{Name: "base"}}, Registry: "huggingface"}} {
		require.Error(t, req.Validate(), "top-level selectors cannot be silently ignored by batch deletion")
	}
}

func TestModelDeleteIncompleteExactContract(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "org", "incomplete")
	require.NoError(t, os.MkdirAll(dir, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{}`), 0600))
	s := createTestNodeWithModelsDir(t, TestNodeConfig{AdminKey: TestAdminKey, SeedProvidersDir: true}, root, "delete-node")
	response := makeAuthRequest(t, s, "DELETE", "/zzrouter/v1/models", TestAdminKey, map[string]any{"model": "org/incomplete"})
	require.Equal(t, 200, response.Code, string(response.Body))
	require.NoDirExists(t, dir)
}

func TestModelDeleteSourceIdentityContract(t *testing.T) {
	root := t.TempDir()
	detailWeights(t, root, "org/a", "shared", 4096)
	detailWeights(t, root, "org/b", "shared", 8192)
	s := createTestNodeWithModelsDir(t, TestNodeConfig{AdminKey: TestAdminKey, SeedProvidersDir: true}, root, "delete-node")
	require.NoError(t, s.configStore.SetProviderEnabled("llamacpp", true))
	response := makeAuthRequest(t, s, "DELETE", "/zzrouter/v1/models", TestAdminKey, map[string]any{"model": "shared", "provider": "llamacpp", "registry": "huggingface"})
	require.Equal(t, 409, response.Code, string(response.Body))
	response = makeAuthRequest(t, s, "DELETE", "/zzrouter/v1/models", TestAdminKey, map[string]any{"model": "shared", "provider": "llamacpp", "registry": "huggingface", "source_id": "org/a"})
	require.Equal(t, 200, response.Code, string(response.Body))
	require.NoFileExists(t, filepath.Join(root, "org", "a", "shared.gguf"))
	require.FileExists(t, filepath.Join(root, "org", "b", "shared.gguf"))
}

func TestModelDeleteAppliedFailureReplayContract(t *testing.T) {
	root := t.TempDir()
	detailWeights(t, root, "org/a", "base", 4096)
	s := createTestNodeWithModelsDir(t, TestNodeConfig{AdminKey: TestAdminKey, SeedProvidersDir: true}, root, "delete-node")
	executor := s.newInternalExecutor()
	var publications atomic.Int32
	executor.publishCatalog = func(context.Context) error { publications.Add(1); return errors.New("synthetic publication failure") }
	engine := gin.New()
	engine.DELETE("/zzrouter/v1/internal/models", executor.HandleInternalDeleteModels)
	s.services.Model.router = mutationRouter{engine: engine}
	call := func() map[string]any {
		wire := mcpWireResponse(t, s, mcpWireRequest(t, "tools/call", "2025-06-18", map[string]any{"name": "zz_model_delete", "arguments": map[string]any{"model": "base", "idempotency_key": "partial-delete-once"}}), 200)
		result := wire["result"].(map[string]any)
		require.Equal(t, true, result["isError"], result)
		fields := result["structuredContent"].(map[string]any)
		require.EqualValues(t, 503, fields["status"])
		require.EqualValues(t, 1, fields["deleted"])
		require.Equal(t, true, fields["mutation_applied"])
		return fields
	}
	first := call()
	path := filepath.Join(root, "org", "a", "base.gguf")
	require.NoFileExists(t, path)
	require.NoError(t, os.WriteFile(path, []byte("GGUF"), 0600))
	s.model.Cache.Invalidate()
	listed := makeAuthRequest(t, s, "GET", "/zzrouter/v1/models?model=base", TestAdminKey, nil)
	require.Contains(t, string(listed.Body), `"total":1`)
	second := call()
	require.Equal(t, first["detail"], second["detail"])
	require.FileExists(t, path, "same-key retry must not delete a recreated model")
	require.EqualValues(t, 1, publications.Load())
}

func deletionDaemon(t *testing.T, s *Server, name string, afterDelete ...func() error) (*atomic.Bool, *atomic.Int32, *atomic.Int32, string) {
	t.Helper()
	present := &atomic.Bool{}
	present.Store(true)
	deletes, status := &atomic.Int32{}, &atomic.Int32{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/tags":
			if present.Load() {
				_, _ = w.Write([]byte(`{"models":[{"name":"m","model":"m","details":{"format":"gguf"}}]}`))
			} else {
				_, _ = w.Write([]byte(`{"models":[]}`))
			}
		case "/api/ps":
			_, _ = w.Write([]byte(`{"models":[]}`))
		case "/api/show":
			_, _ = w.Write([]byte(`{"capabilities":["completion"]}`))
		case "/api/delete":
			if status.Load() != 0 {
				w.WriteHeader(int(status.Load()))
				_, _ = w.Write([]byte(`{"error":"synthetic refusal"}`))
				return
			}
			var body struct {
				Name string `json:"name"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || body.Name != "m" {
				w.WriteHeader(400)
				return
			}
			deletes.Add(1)
			present.Store(false)
			for _, finish := range afterDelete {
				if finish() != nil {
					w.WriteHeader(500)
					return
				}
			}
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(up.Close)
	response := makeAuthRequest(t, s, "POST", "/zzrouter/v1/providers/instances", TestAdminKey, map[string]any{"type": "ollama-connect", "name": name, "endpoint": up.URL})
	require.Contains(t, []int{200, 201}, response.Code, string(response.Body))
	return present, deletes, status, up.URL
}

func TestModelDeleteSelectedDaemonContract(t *testing.T) {
	s := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, SeedProvidersDir: true})
	a, ad, _, _ := deletionDaemon(t, s, "ollama-delete-a")
	b, bd, status, _ := deletionDaemon(t, s, "ollama-delete-b")
	status.Store(401)
	response := makeAuthRequest(t, s, "DELETE", "/zzrouter/v1/models", TestAdminKey, map[string]any{"model": "m", "provider": "ollama-delete-b"})
	require.Equal(t, 401, response.Code, string(response.Body))
	require.True(t, a.Load())
	require.True(t, b.Load())
	status.Store(0)
	response = makeAuthRequest(t, s, "DELETE", "/api/delete", TestAdminKey, map[string]any{"model": "ollama-delete-b/m"})
	require.Equal(t, 200, response.Code, string(response.Body))
	require.True(t, a.Load())
	require.False(t, b.Load())
	require.Zero(t, ad.Load())
	require.EqualValues(t, 1, bd.Load())
	response = makeAuthRequest(t, s, "DELETE", "/v1/models/ollama-delete-a/m", TestAdminKey, nil)
	require.Equal(t, 200, response.Code, string(response.Body))
	require.False(t, a.Load())
	require.EqualValues(t, 1, ad.Load())
}

func TestModelDeleteUnknownDaemonEffectContract(t *testing.T) {
	s := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, SeedProvidersDir: true})
	present, deletes, _, _ := deletionDaemon(t, s, "ollama-delete-unknown", func() error { return errors.New("response failed after deletion") })
	call := func() map[string]any {
		wire := mcpWireResponse(t, s, mcpWireRequest(t, "tools/call", "2025-06-18", map[string]any{"name": "zz_model_delete", "arguments": map[string]any{"model": "m", "provider": "ollama-delete-unknown", "idempotency_key": "unknown-delete-once"}}), 200)
		result := wire["result"].(map[string]any)
		require.Equal(t, true, result["isError"])
		fields := result["structuredContent"].(map[string]any)
		require.EqualValues(t, 503, fields["status"])
		require.Equal(t, true, fields["mutation_applied"], "unknown remote outcomes must prevent a second destructive attempt")
		return fields
	}
	first := call()
	require.False(t, present.Load())
	present.Store(true)
	s.model.Cache.Invalidate()
	second := call()
	require.Equal(t, first["detail"], second["detail"])
	require.True(t, present.Load())
	require.EqualValues(t, 1, deletes.Load())
}

func TestModelDeleteUnavailableInventoryContract(t *testing.T) {
	s := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, ClusterMode: config.ClusterModeCoordinator, SeedProvidersDir: true})
	mode := detailDaemon(t, s, "ollama-delete-offline")
	mode.Store(5)
	s.model.Cache.Invalidate()
	for _, provider := range []string{"ollama-delete-offline", "OLLAMA-DELETE-OFFLINE"} {
		response := makeAuthRequest(t, s, "DELETE", "/zzrouter/v1/models", TestAdminKey, map[string]any{"model": "m", "provider": provider})
		require.Equal(t, 503, response.Code, "failed inventory is not proof of absence: %s", response.Body)
		require.NotEmpty(t, s.model.Cache.UnavailableInventories(s.node.Nodename(), strings.ToUpper(provider)), "published inventory selectors share case-insensitive provider identity")
	}
}

type deleteBroadcastContractRouter struct {
	routing.Router
	t *testing.T
}

func (r deleteBroadcastContractRouter) Route(context.Context, *routing.Request) (*routing.Response, error) {
	return &routing.Response{StatusCode: 200, Body: []byte(`{"deleted":2}`)}, nil
}

func (r deleteBroadcastContractRouter) MultiRoute(_ context.Context, path, method string, bodies map[string][]byte) (*routing.Response, error) {
	require.Equal(r.t, "/zzrouter/v1/internal/models", path)
	require.Equal(r.t, "DELETE", method)
	require.Len(r.t, bodies, 1)
	require.Contains(r.t, bodies, "*")
	return &routing.Response{StatusCode: 200, Body: []byte(`[{"node":"failed","status_code":503,"body":{"title":"Service Unavailable","status":503,"deleted":1,"mutation_applied":true}},{"node":"ok","status_code":200,"body":{"deleted":2}}]`)}, nil
}

func TestModelDeletePublicBroadcastContract(t *testing.T) {
	s := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, SeedProvidersDir: true})
	s.services.Model.router = deleteBroadcastContractRouter{t: t}
	response := makeAuthRequest(t, s, "DELETE", "/zzrouter/v1/models", TestAdminKey, map[string]any{"pattern": "*"})
	require.Equal(t, 503, response.Code, string(response.Body))
	var problem map[string]any
	require.NoError(t, json.Unmarshal(response.Body, &problem))
	require.EqualValues(t, 3, problem["deleted"])
	require.Equal(t, true, problem["mutation_applied"])
}

func TestModelDeletePatternContinuesContract(t *testing.T) {
	root := t.TempDir()
	detailWeights(t, root, "org/a", "base", 4096)
	s := createTestNodeWithModelsDir(t, TestNodeConfig{AdminKey: TestAdminKey, SeedProvidersDir: true}, root, "delete-node")
	executor := s.newInternalExecutor()
	response := &DeleteModelsResponse{}
	targets, err := executor.deletionTargets(&DeleteModelsRequest{Pattern: "*"}, []*metadata.ModelMetadata{{Name: "offline", AssignedApp: "missing-daemon", SourceRepo: metadata.SourceOllama}, {Name: "base", FullPath: filepath.Join(root, "org/a/base.gguf"), Format: metadata.FormatGGUF}}, response)
	require.NoError(t, err)
	require.Len(t, response.Errors, 1)
	require.Len(t, targets, 1, "a selection failure cannot drop later pattern matches")
	require.Equal(t, "base", targets[0].Model.Name)
}

func TestModelDeleteMixedCloudContract(t *testing.T) {
	root := t.TempDir()
	detailWeights(t, root, "org/a", "base", 4096)
	s := createTestNodeWithModelsDir(t, TestNodeConfig{AdminKey: TestAdminKey, SeedProvidersDir: true}, root, "delete-node")
	require.NoError(t, s.configStore.AddProviderInstance("cloud-test", &config.CloudProvider{Enabled: new(true), Runtime: config.CloudRuntime{Endpoint: "https://cloud.test", API: config.APIConfig{AuthType: config.AuthTypeBearer, Token: "test-token"}}, Capabilities: &config.AppCapabilities{Models: []string{"cloud-old"}, WireEndpoints: []string{"chat_completions"}}}))
	deletionDaemon(t, s, "ollama-cloud-scope")
	c, recorder := mutationContext(t, "DELETE", "/zzrouter/v1/internal/models", DeleteModelsRequest{Model: "cloud-old", Provider: "cloud-test", Registry: "cloud-test"})
	s.newInternalExecutor().HandleInternalDeleteModels(c)
	require.Equal(t, 409, recorder.Code, "the serving node must independently refuse exact cloud configuration deletion")
	for _, route := range []string{"/v1/models/cloud-old", "/v1/models/cloud-test/cloud-old", "/api/delete", "/zzrouter/v1/models"} {
		body := any(nil)
		if route == "/api/delete" || route == "/zzrouter/v1/models" {
			body = map[string]any{"model": "cloud-test/cloud-old"}
		}
		refusal := makeAuthRequest(t, s, "DELETE", route, TestAdminKey, body)
		require.Equal(t, 409, refusal.Code, "cloud configuration is outside exact physical deletion: %s", refusal.Body)
		require.True(t, s.model.Cache.HasModel("cloud-old"))
	}
	response := makeAuthRequest(t, s, "DELETE", "/zzrouter/v1/models", TestAdminKey, DeleteModelsRequest{Models: []ModelToDelete{{Name: "base", Registry: "huggingface"}, {Name: "cloud-old", Registry: "cloud-test"}}})
	require.Equal(t, 200, response.Code, string(response.Body))
	var body struct {
		Data DeleteModelsResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(response.Body, &body))
	require.Equal(t, 2, body.Data.Deleted)
	require.Empty(t, body.Data.Errors)
	require.NoFileExists(t, filepath.Join(root, "org/a/base.gguf"))
	require.False(t, s.model.Cache.HasModel("cloud-old"))
}

func TestModelDeleteRetargetedInventoryContract(t *testing.T) {
	s := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, SeedProvidersDir: true})
	a, ad, _, _ := deletionDaemon(t, s, "ollama-delete-a")
	b, bd, _, endpoint := deletionDaemon(t, s, "ollama-delete-b")
	models, _, err := s.model.Registry.ModelSnapshot(t.Context())
	require.NoError(t, err)
	var old *metadata.ModelMetadata
	for _, m := range models {
		if m.AssignedApp == "ollama-delete-a" {
			old = m
		}
	}
	require.NotNil(t, old)
	require.NoError(t, s.configStore.ApplyParameterPatch("ollama-delete-a", func(sc *config.ServiceConfig) error { sc.Runtime.Endpoint = endpoint; return nil }))
	_, err = s.model.Registry.SelectDeletion(old)
	require.Error(t, err, "stale inventory must not switch to another daemon")
	require.True(t, a.Load())
	require.True(t, b.Load())
	require.Zero(t, ad.Load())
	require.Zero(t, bd.Load())
}

func TestModelDeleteNativeErrorDialectContract(t *testing.T) {
	root := t.TempDir()
	detailWeights(t, root, "org/a", "base", 4096)
	s := createTestNodeWithModelsDir(t, TestNodeConfig{AdminKey: TestAdminKey, SeedProvidersDir: true}, root, "delete-node")
	run := instance.NewInstance("native-holder", "llamacpp", "base", 0, -1, 0)
	run.SetStatus(instance.StatusRunning)
	require.NoError(t, s.providers.appMgr.Instances().Register(run))
	t.Cleanup(func() { _ = s.providers.appMgr.Instances().Remove(run.ID) })
	response := makeAuthRequest(t, s, "DELETE", "/v1/models/base", TestAdminKey, nil)
	require.Equal(t, 409, response.Code, string(response.Body))
	var body map[string]any
	require.NoError(t, json.Unmarshal(response.Body, &body))
	problem, ok := body["error"].(map[string]any)
	require.True(t, ok, body)
	require.Equal(t, "model_in_use", problem["code"])
	require.False(t, strings.Contains(string(response.Body), `"title"`))
	require.FileExists(t, filepath.Join(root, "org", "a", "base.gguf"))
}

func TestModelDeleteRunGuardContract(t *testing.T) {
	for _, status := range []instance.Status{instance.StatusStarting, instance.StatusRunning, instance.StatusStopping} {
		t.Run(string(status), func(t *testing.T) {
			root := t.TempDir()
			detailWeights(t, root, "org/a", "base", 4096)
			s := createTestNodeWithModelsDir(t, TestNodeConfig{AdminKey: TestAdminKey, SeedProvidersDir: true}, root, "delete-node")
			run := instance.NewInstance("delete-holder", "llamacpp", "base", 0, -1, 0)
			run.SetStatus(status)
			run.StopUnconfirmed.Store(true)
			require.NoError(t, s.providers.appMgr.Instances().Register(run))
			t.Cleanup(func() { _ = s.providers.appMgr.Instances().Remove(run.ID) })
			response := makeAuthRequest(t, s, "DELETE", "/zzrouter/v1/models", TestAdminKey, map[string]any{"model": "base"})
			require.Equal(t, 409, response.Code, string(response.Body))
			require.Contains(t, string(response.Body), run.ID)
			require.FileExists(t, filepath.Join(root, "org", "a", "base.gguf"))
		})
	}
}

func TestModelDeleteVariantGuardContract(t *testing.T) {
	root := t.TempDir()
	detailWeights(t, root, "org/a", "base", 4096)
	s := createTestNodeWithModelsDir(t, TestNodeConfig{AdminKey: TestAdminKey, SeedProvidersDir: true}, root, "delete-node")
	require.NoError(t, s.configStore.ApplyParameterPatch("llamacpp", func(sc *config.ServiceConfig) error {
		sc.Models = map[string]config.ModelSpec{"base+agent": {From: "base"}}
		return nil
	}))
	for _, tc := range []struct{ model, code, remedy string }{
		{"base", "model_referenced", "PATCH /zzrouter/v1/providers/llamacpp/parameters"},
		{"base+agent", "model_not_deletable", "parameters API"},
	} {
		response := makeAuthRequest(t, s, "DELETE", "/zzrouter/v1/models", TestAdminKey, map[string]any{"model": tc.model})
		require.Equal(t, 409, response.Code, string(response.Body))
		require.Contains(t, string(response.Body), tc.code)
		require.Contains(t, string(response.Body), tc.remedy)
	}
	require.FileExists(t, filepath.Join(root, "org", "a", "base.gguf"))
}

func TestMCPModelDeleteContract(t *testing.T) {
	for _, version := range []string{"2026-07-28", "2025-06-18"} {
		t.Run(version, func(t *testing.T) {
			root := t.TempDir()
			detailWeights(t, root, "org/a", "base", 4096)
			s := createTestNodeWithModelsDir(t, TestNodeConfig{AdminKey: TestAdminKey, SeedProvidersDir: true}, root, "delete-node")
			for range 2 {
				wire := mcpWireResponse(t, s, mcpWireRequest(t, "tools/call", version, map[string]any{"name": "zz_model_delete", "arguments": map[string]any{"model": "base", "node": "delete-node", "idempotency_key": "delete-once-" + url.QueryEscape(version)}}), http.StatusOK)
				result, ok := wire["result"].(map[string]any)
				require.True(t, ok, wire)
				require.NotEqual(t, true, result["isError"], result)
				require.EqualValues(t, 1, result["structuredContent"].(map[string]any)["deleted"])
			}
			require.NoFileExists(t, filepath.Join(root, "org", "a", "base.gguf"))
			listed := makeAuthRequest(t, s, "GET", "/zzrouter/v1/models?model=base", TestAdminKey, nil)
			var response struct {
				Total int `json:"total"`
			}
			require.NoError(t, json.Unmarshal(listed.Body, &response))
			require.Zero(t, response.Total)
			_, err := os.Stat(filepath.Join(root, "org", "a", "base.gguf"))
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}
