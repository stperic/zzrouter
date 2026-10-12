package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	clustersignal "github.com/stperic/zzrouter/pkg/cluster/signal"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/model/cache"
	"github.com/stperic/zzrouter/pkg/model/integrity"
	modelsync "github.com/stperic/zzrouter/pkg/model/sync"
	"github.com/stperic/zzrouter/pkg/modelregistry"
	"github.com/stperic/zzrouter/pkg/routing"
	"github.com/stretchr/testify/require"
)

func TestIncompleteModel_ListRefuseAndDelete(t *testing.T) {
	root := t.TempDir()
	modelregistry.SetModelsRootDirOverride(root)
	t.Cleanup(modelregistry.ClearModelsRootDirOverride)
	repos := []string{"google/gemma-4-E2B-it", "lmstudio-community/Qwen3.8-27B-MLX-4bit", "org/split-GGUF"}
	for _, repo := range repos {
		dir := filepath.Join(root, filepath.FromSlash(repo))
		require.NoError(t, os.MkdirAll(dir, 0o755))
		for _, name := range []string{"config.json", "generation_config.json", "chat_template.jinja", "README.md"} {
			require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o600))
		}
		if repo == repos[0] {
			require.NoError(t, os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte("x"), 0o600))
			manifest, err := integrity.CreateManifest(t.Context(), dir, repo, "")
			require.NoError(t, err)
			require.NoError(t, integrity.WriteManifest(dir, manifest))
		}
		if repo == repos[2] {
			require.NoError(t, os.WriteFile(filepath.Join(dir, "m-00001-of-00002.gguf"), []byte("weights"), 0o600))
		}
	}
	s := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, ClusterMode: config.ClusterModeCoordinator, NodeName: "coordinator", SeedProvidersDir: true})
	resp := makeAuthRequest(t, s, http.MethodGet, "/zzrouter/v1/models", TestAdminKey, nil)
	require.Equal(t, http.StatusOK, resp.Code, string(resp.Body))
	var listed struct {
		Data []*cache.CachedModel `json:"data"`
	}
	require.NoError(t, json.Unmarshal(resp.Body, &listed))
	require.Len(t, listed.Data, len(repos))
	for _, model := range listed.Data {
		require.True(t, model.Incomplete, model.Name)
		resp = makeAuthRequest(t, s, http.MethodPost, "/v1/chat/completions", TestAdminKey, map[string]any{"model": model.Name, "messages": []map[string]string{{"role": "user", "content": "hi"}}})
		require.Equal(t, http.StatusConflict, resp.Code, string(resp.Body))
		require.Contains(t, string(resp.Body), `"code":"model_incomplete"`)
		resp = makeAuthRequest(t, s, http.MethodDelete, "/zzrouter/v1/models", TestAdminKey, DeleteModelsRequest{Models: []ModelToDelete{{Name: model.Name, Registry: "huggingface"}}})
		require.Equal(t, http.StatusOK, resp.Code, string(resp.Body))
		require.NoDirExists(t, filepath.Join(root, filepath.FromSlash(model.Name)))
	}
}

func mutationNodes(t *testing.T, location string) (target, reader *Server, dir string) {
	t.Helper()
	root := t.TempDir()
	modelregistry.SetModelsRootDirOverride(root)
	t.Cleanup(modelregistry.ClearModelsRootDirOverride)
	dir = filepath.Join(root, "org", "model")
	require.NoError(t, os.MkdirAll(dir, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "weights.gguf"), []byte("GGUF"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sentinel.gguf"), []byte("GGUF"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"vision_config":{}}`), 0600))
	mode := config.ClusterModeCoordinator
	if location == "worker" {
		mode = config.ClusterModeWorker
	}
	target = createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, ClusterMode: mode, NodeName: location, SeedProvidersDir: true})
	require.NoError(t, target.configStore.SetProviderEnabled("llamacpp", true))
	reader = target
	if location == "worker" {
		reader = createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, ClusterMode: config.ClusterModeCoordinator, NodeName: "coordinator"})
		reader.model.Cache = cache.New(cache.Config{
			Node:                      reader.node,
			Registry:                  func() *modelregistry.Registry { return nil },
			AppsConfig:                func() *config.AppsConfig { return target.appsConfig },
			ClusterClient:             func() mesh.ClusterClient { return deploymentCatalogClient{worker: target} },
			ResolveEndpointToNodename: func(string) string { return "" },
		})
		engine := buildCoordInternalEngine(reader)
		client := &http.Client{Transport: mcpRoundTripper(func(r *http.Request) (*http.Response, error) {
			rec := httptest.NewRecorder()
			engine.ServeHTTP(rec, r)
			return rec.Result(), nil
		})}
		target.cluster.signaler = clustersignal.New(clustersignal.Config{
			IsWorker:       target.node.IsWorker,
			MTLSClient:     func() *http.Client { return client },
			CoordinatorURL: func() string { return "https://coordinator.test" },
		})
		target.cluster.signaler.Start(t.Context())
		t.Cleanup(func() { target.cluster.signaler.Stop(context.Background()) })
	}
	require.NoError(t, target.model.Cache.RefreshCacheSync(t.Context()))
	require.NoError(t, reader.model.Cache.RefreshCacheSync(t.Context()))
	return
}

func mutationContext(t *testing.T, method, path string, body any) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		require.NoError(t, err)
	}
	c.Request = httptest.NewRequest(method, path, bytes.NewReader(raw))
	c.Request.Header.Set("Content-Type", "application/json")
	return c, rec
}

func TestCatalogMutationDeletePublishesBeforeAdmission(t *testing.T) {
	for _, location := range []string{"coordinator", "worker"} {
		for _, kind := range []string{"paths", "pattern", "models", "partial", "feature"} {
			t.Run(location+"/"+kind, func(t *testing.T) {
				target, reader, dir := mutationNodes(t, location)
				req := DeleteModelsRequest{Paths: []string{filepath.Join(dir, "weights.gguf")}}
				if kind == "pattern" {
					req = DeleteModelsRequest{Pattern: "weights"}
				}
				if kind == "models" {
					req = DeleteModelsRequest{Models: []ModelToDelete{{Name: "weights", Registry: "huggingface"}}}
				}
				if kind == "partial" {
					req.Paths = append(req.Paths, filepath.Join(dir, "absent.gguf"))
				}
				if kind == "feature" {
					require.NoError(t, os.WriteFile(filepath.Join(dir, "mmproj-F16.gguf"), []byte("projector"), 0600))
					require.NoError(t, target.publishCatalog())
					req.Paths = []string{filepath.Join(dir, "mmproj-F16.gguf")}
				}
				c, rec := mutationContext(t, http.MethodDelete, "/zzrouter/v1/internal/models", req)
				target.newInternalExecutor().HandleInternalDeleteModels(c)
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				for _, node := range []*Server{target, reader} {
					require.NotNil(t, node.model.Cache.GetAllModels(), "admission must read a published catalog without refreshing")
					m, found := node.model.Cache.LookupByNode("weights", location)
					if kind == "feature" {
						require.True(t, found)
						require.Equal(t, "available", m.FeatureState("vision"))
					} else {
						require.False(t, found, "deleted model remains available to admission")
						replicas, err := node.model.Cache.LookupReplicas(t.Context(), "weights", "llamacpp", location, "")
						require.NoError(t, err)
						require.Empty(t, replicas)
					}
					require.True(t, node.model.Cache.HasModel("sentinel"))
				}
			})
		}
	}
}

func TestCatalogMutationRescanPublishesBeforeResponse(t *testing.T) {
	for _, location := range []string{"coordinator", "worker"} {
		t.Run(location, func(t *testing.T) {
			target, reader, dir := mutationNodes(t, location)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "fresh.gguf"), []byte("GGUF"), 0600))
			c, rec := mutationContext(t, http.MethodPost, "/zzrouter/v1/internal/models/rescan", nil)
			target.newInternalExecutor().HandleInternalRescanModels(c)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.True(t, target.model.Cache.HasModel("fresh"))
			require.True(t, reader.model.Cache.HasModel("fresh"))
		})
	}
}

func TestCatalogMutationWorkerWaitsForAcknowledgement(t *testing.T) {
	target, reader, dir := mutationNodes(t, "worker")
	entered, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	engine := buildCoordInternalEngine(reader)
	client := &http.Client{Transport: mcpRoundTripper(func(r *http.Request) (*http.Response, error) {
		close(entered)
		<-release
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, r)
		return rec.Result(), nil
	})}
	target.cluster.signaler.Stop(context.Background())
	target.cluster.signaler = clustersignal.New(clustersignal.Config{IsWorker: target.node.IsWorker, MTLSClient: func() *http.Client { return client }, CoordinatorURL: func() string { return "https://coordinator.test" }})
	signalCtx, cancelSignal := context.WithCancel(context.Background())
	t.Cleanup(cancelSignal)
	target.cluster.signaler.Start(signalCtx)
	c, rec := mutationContext(t, http.MethodDelete, "/zzrouter/v1/internal/models", DeleteModelsRequest{Paths: []string{filepath.Join(dir, "weights.gguf")}})
	go func() { target.newInternalExecutor().HandleInternalDeleteModels(c); close(returned) }()
	t.Cleanup(func() {
		close(release)
		<-returned
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.False(t, reader.model.Cache.HasModel("weights"))
	})
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("deletion did not request coordinator publication")
	}
	select {
	case <-returned:
		t.Fatal("delete answered before acknowledgement")
	default:
	}
	require.False(t, target.model.Cache.HasModel("weights"))
	require.True(t, reader.model.Cache.HasModel("weights"))
}

func TestCatalogMutationProviderWritesPublish(t *testing.T) {
	for _, operation := range []string{"variant-add", "variant-remove", "provider-enable", "provider-disable", "cloud-register", "cloud-delete", "cloud-disconnect", "provider-instance", "provider-verify", "reload", "asset-put", "asset-delete", "list-refresh", "scan"} {
		t.Run(operation, func(t *testing.T) {
			target, _, dir := mutationNodes(t, "coordinator")
			method, path, body := http.MethodPatch, "/zzrouter/v1/providers/llamacpp/parameters", any(map[string]any{"models": map[string]any{"alias": map[string]any{"from": "weights"}}})
			expected, present := "alias", true
			switch operation {
			case "variant-remove":
				resp := makeAuthRequest(t, target, method, path, TestAdminKey, body)
				require.Equal(t, http.StatusOK, resp.Code, string(resp.Body))
				body, present = map[string]any{"models": map[string]any{"alias": nil}}, false
			case "provider-enable", "provider-disable":
				require.NoError(t, target.configStore.SetProviderEnabled("llamacpp", false))
				require.NoError(t, target.publishCatalog())
				method, path, body = http.MethodPatch, "/zzrouter/v1/providers/llamacpp", map[string]any{"enabled": true}
				expected = "weights"
				if operation == "provider-disable" {
					require.NoError(t, target.configStore.SetProviderEnabled("llamacpp", true))
					require.NoError(t, target.publishCatalog())
					body = map[string]any{"enabled": false}
				}
			case "cloud-register", "cloud-delete", "cloud-disconnect":
				require.NoError(t, target.configStore.AddProviderInstance("cloud-test", &config.CloudProvider{Enabled: new(true), Runtime: config.CloudRuntime{Endpoint: "https://cloud.test", API: config.APIConfig{AuthType: config.AuthTypeBearer, Token: "test-token"}}, Capabilities: &config.AppCapabilities{Models: []string{"cloud-old"}, WireEndpoints: []string{"chat_completions"}}}))
				require.NoError(t, target.publishCatalog())
				if operation == "cloud-register" {
					c, rec := mutationContext(t, http.MethodPost, "/zzrouter/v1/internal/deployments", nil)
					target.newDeploymentsExecutor().registerCloudModel(c, "cloud-test", "cloud-new", "cloud-job")
					require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
					require.True(t, target.model.Cache.HasModel("cloud-new"))
					return
				}
				expected, present = "cloud-old", false
				method, path, body = http.MethodDelete, "/zzrouter/v1/models", DeleteModelsRequest{Models: []ModelToDelete{{Name: "cloud-old", Registry: "cloud-test"}}}
				if operation == "cloud-disconnect" {
					path, body = "/zzrouter/v1/providers/cloud-test", nil
				}
			case "provider-verify":
				require.NoError(t, target.configStore.AddProviderInstance("cloud-test", &config.CloudProvider{Enabled: new(false), Runtime: config.CloudRuntime{Endpoint: "https://cloud.test", API: config.APIConfig{AuthType: config.AuthTypeBearer, Token: "test-token"}}, Capabilities: &config.AppCapabilities{Models: []string{"cloud-new"}, WireEndpoints: []string{"chat_completions"}}}))
				require.NoError(t, target.publishCatalog())
				transport := http.DefaultTransport
				http.DefaultTransport = mcpRoundTripper(func(*http.Request) (*http.Response, error) { return httptest.NewRecorder().Result(), nil })
				t.Cleanup(func() { http.DefaultTransport = transport })
				method, path, body = http.MethodPost, "/zzrouter/v1/providers/cloud-test/verify", nil
				expected = "cloud-new"

			case "provider-instance":
				method, path, body = http.MethodPost, "/zzrouter/v1/providers/instances", map[string]any{"type": "ollama-connect", "name": "extra", "endpoint": "http://127.0.0.1:1"}
				expected = "sentinel"
			case "reload", "list-refresh", "scan", "asset-put", "asset-delete":
				require.NoError(t, os.WriteFile(filepath.Join(dir, "fresh.gguf"), []byte("GGUF"), 0600))
				expected = "fresh"
				method, path, body = http.MethodPost, "/zzrouter/v1/config/reload", nil
				if operation == "list-refresh" {
					method, path = http.MethodGet, "/zzrouter/v1/models?refresh=true"
				}
				if operation == "scan" {
					path = "/zzrouter/v1/models/scan?node=coordinator"
				}
				if strings.HasPrefix(operation, "asset-") {
					method, path = http.MethodPut, "/zzrouter/v1/providers/llamacpp/assets/test.jinja"
					if operation == "asset-delete" {
						require.NoError(t, func() error {
							_, err := target.configStore.WriteAsset("llamacpp", "test.jinja", []byte("template"))
							return err
						}())
						method = http.MethodDelete
					}
					rec := assetRequest(t, target, method, path, []byte("template"))
					require.Less(t, rec.Code, http.StatusBadRequest, rec.Body.String())
					require.False(t, target.model.Cache.HasModel(expected), "asset contents do not change the catalog")
					return
				}
			}
			resp := makeAuthRequest(t, target, method, path, TestAdminKey, body)
			require.Less(t, resp.Code, http.StatusBadRequest, string(resp.Body))
			require.NotNil(t, target.model.Cache.GetAllModels())
			require.Equal(t, present, target.model.Cache.HasModel(expected))
			if operation == "provider-disable" {
				m, found := target.model.Cache.LookupByName(expected)
				require.True(t, found)
				require.NotEqual(t, "llamacpp", m.Provider)
			}
		})
	}
}

func TestCatalogMutationProviderSyncPublishesBeforeResponse(t *testing.T) {
	target, reader, _ := mutationNodes(t, "worker")
	raw, err := target.configStore.ReadProviderConfigBytes("llamacpp")
	require.NoError(t, err)
	raw = append(raw, []byte("\nmodels:\n  alias:\n    from: weights\n")...)
	c, rec := mutationContext(t, http.MethodPost, "/zzrouter/v1/internal/sync/providers/llamacpp", ProviderSyncBody{Kind: config.KindOnDemand, ConfigYAML: raw})
	c.Params = gin.Params{{Key: "name", Value: "llamacpp"}}
	target.HandleProviderSync(c)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.True(t, target.model.Cache.HasModel("alias"))
	require.True(t, reader.model.Cache.HasModel("alias"))
	root, err := modelregistry.GetModelsRootDir()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, "fresh.gguf"), []byte("GGUF"), 0600))
	c, rec = mutationContext(t, http.MethodPost, "/zzrouter/v1/internal/sync/providers/llamacpp", ProviderSyncBody{Kind: config.KindOnDemand, ConfigYAML: raw})
	c.Params = gin.Params{{Key: "name", Value: "llamacpp"}}
	target.HandleProviderSync(c)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"changed":false`)
	require.True(t, reader.model.Cache.HasModel("fresh"))

}

func TestCatalogMutationLifecyclePublishesBeforeJobDone(t *testing.T) {
	for _, operation := range []string{"install", "upgrade", "uninstall", "execute-step"} {
		t.Run(operation, func(t *testing.T) {
			target, reader, dir := mutationNodes(t, "worker")
			e := NewProvidersExecutor(target.providers.appMgr, "worker", target.GetLocalAppsInfo, func() *config.AppsConfig { return target.appsConfig }, target.configStore, target.FinalizeOnboarding, target.FinalizeOffboarding, target.publishSelfSnapshot)
			e.publishCatalog = target.publishCatalog
			h, err := target.jobs.StartDetached(jobs.KindInstall, "llamacpp", nil)
			require.NoError(t, err)
			err = e.afterLifecycle(func() error { return os.WriteFile(filepath.Join(dir, "fresh.gguf"), []byte("GGUF"), 0600) })()
			if err != nil {
				h.Fail(err)
			} else {
				h.Done()
			}
			require.Equal(t, jobs.PhaseDone, waitJob(t, target.jobs, h.ID()).Phase)
			require.True(t, target.model.Cache.HasModel("fresh"))
			require.True(t, reader.model.Cache.HasModel("fresh"))
		})
	}
}

func TestCatalogMutationPublicationFailuresAreReported(t *testing.T) {
	failure := errors.New("catalog publication failed")
	for _, operation := range []string{"delete", "rescan", "cloud-register", "lifecycle"} {
		t.Run(operation, func(t *testing.T) {
			target, _, dir := mutationNodes(t, "coordinator")
			e := target.newInternalExecutor()
			e.publishCatalog = func(context.Context) error { return failure }
			c, rec := mutationContext(t, http.MethodDelete, "/models", DeleteModelsRequest{Paths: []string{filepath.Join(dir, "weights.gguf")}})
			switch operation {
			case "delete":
				e.HandleInternalDeleteModels(c)
			case "rescan":
				e.HandleInternalRescanModels(c)
			case "cloud-register":
				require.NoError(t, target.configStore.AddProviderInstance("cloud-test", &config.CloudProvider{Enabled: new(true), Runtime: config.CloudRuntime{Endpoint: "https://cloud.test", API: config.APIConfig{AuthType: config.AuthTypeBearer, Token: "test-token"}}, Capabilities: &config.AppCapabilities{WireEndpoints: []string{"chat_completions"}}}))
				deploy := target.newDeploymentsExecutor()
				deploy.publishCatalog = func() error { return failure }
				deploy.registerCloudModel(c, "cloud-test", "model", "job")
			case "lifecycle":
				e := &ProvidersExecutor{publishCatalog: func() error { return failure }}
				require.ErrorIs(t, e.afterLifecycle(nil)(), failure)
				return
			}
			require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
			require.Contains(t, rec.Body.String(), failure.Error())
		})
	}
}

func TestCatalogMutationOllamaCompletionOrdering(t *testing.T) {
	for _, streamed := range []bool{false, true} {
		for _, fails := range []bool{false, true} {
			t.Run(strings.Join([]string{map[bool]string{false: "json", true: "ndjson"}[streamed], map[bool]string{false: "success", true: "failure"}[fails]}, "/"), func(t *testing.T) {
				body, contentType := `{"status":"success"}`, "application/json"
				if streamed {
					body, contentType = "{\"status\":\"downloading\"}\n{\"status\":\"success\"}\n", "application/x-ndjson"
				}
				c, rec := mutationContext(t, http.MethodPost, "/api/pull", nil)
				published := false
				upstream := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{contentType}}, Body: io.NopCloser(strings.NewReader(body))}
				err := writeOllamaMutation(c, upstream, func() error {
					require.NotContains(t, rec.Body.String(), "success")
					if streamed {
						require.Contains(t, rec.Body.String(), "downloading")
					}
					published = true
					if fails {
						return errors.New("publication refused")
					}
					return nil
				})
				require.True(t, published)
				if fails {
					require.Error(t, err)
					require.NotContains(t, rec.Body.String(), "success")
					require.Contains(t, rec.Body.String(), `"error":`)
					require.Contains(t, rec.Body.String(), "publication refused")
					if !streamed {
						require.Equal(t, http.StatusServiceUnavailable, rec.Code)
					}
				} else {
					require.NoError(t, err)
					require.Contains(t, rec.Body.String(), "success")
				}
			})
		}
	}
}

type mutationRouter struct {
	routing.Router
	engine *gin.Engine
}

func (r mutationRouter) Route(ctx context.Context, req *routing.Request) (*routing.Response, error) {
	rec := httptest.NewRecorder()
	request := httptest.NewRequest(req.Method, req.Path, bytes.NewReader(req.Body)).WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")
	r.engine.ServeHTTP(rec, request)
	return &routing.Response{StatusCode: rec.Code, Body: rec.Body.Bytes()}, nil
}

func (r mutationRouter) Broadcast(ctx context.Context, path, method string, body []byte) (*routing.Response, error) {
	return r.Route(ctx, &routing.Request{Path: path, Method: method, Body: body})
}

func (r mutationRouter) MultiRoute(ctx context.Context, path, method string, bodies map[string][]byte) (*routing.Response, error) {
	var responses []map[string]any
	for node, body := range bodies {
		resp, err := r.Route(ctx, &routing.Request{Path: path, Method: method, Body: body})
		if err != nil {
			return nil, err
		}
		var decoded any
		if err := json.Unmarshal(resp.Body, &decoded); err != nil {
			return nil, err
		}
		responses = append(responses, map[string]any{"node": node, "status_code": resp.StatusCode, "body": decoded})
	}
	raw, err := json.Marshal(responses)
	return &routing.Response{StatusCode: http.StatusOK, Body: raw}, err
}

func TestCatalogMutationRoutedDeleteAndScansKeepPublication(t *testing.T) {
	for _, operation := range []string{"delete", "multi-delete", "scan-target", "scan-broadcast"} {
		t.Run(operation, func(t *testing.T) {
			target, reader, dir := mutationNodes(t, "worker")
			engine := gin.New()
			e := target.newInternalExecutor()
			engine.DELETE("/zzrouter/v1/internal/models", e.HandleInternalDeleteModels)
			engine.POST("/zzrouter/v1/internal/models/rescan", e.HandleInternalRescanModels)
			svc := NewModelService(reader, reader, reader, reader, mutationRouter{engine: engine}, func() *config.AppsConfig { return target.appsConfig })
			if strings.Contains(operation, "delete") {
				req := &DeleteModelsRequest{Node: "worker", Paths: []string{filepath.Join(dir, "weights.gguf")}}
				if operation == "multi-delete" {
					req = &DeleteModelsRequest{Models: []ModelToDelete{{Name: "weights", Registry: "huggingface", Node: "worker"}}}
				}
				result, err := svc.DeleteModels(t.Context(), req)
				require.NoError(t, err)
				require.Equal(t, 1, result.Deleted)
				require.NotNil(t, reader.model.Cache.GetAllModels())
				require.False(t, reader.model.Cache.HasModel("weights"))
			} else {
				require.NoError(t, os.WriteFile(filepath.Join(dir, "fresh.gguf"), []byte("GGUF"), 0600))
				selector := "worker"
				if operation == "scan-broadcast" {
					selector = "*"
				}
				c, rec := mutationContext(t, http.MethodPost, "/models/scan?node="+selector, nil)
				svc.handleRescanModels(c)
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				require.True(t, reader.model.Cache.HasModel("fresh"))
			}
		})
	}
}

func TestCatalogMutationRoutedDeleteReportsPublicationFailure(t *testing.T) {
	for _, multi := range []bool{false, true} {
		t.Run(fmt.Sprint(multi), func(t *testing.T) {
			target, _, dir := mutationNodes(t, "coordinator")
			engine := gin.New()
			e := target.newInternalExecutor()
			e.publishCatalog = func(context.Context) error { return errors.New("publication refused") }
			engine.DELETE("/zzrouter/v1/internal/models", e.HandleInternalDeleteModels)
			svc := NewModelService(target, target, target, target, mutationRouter{engine: engine}, func() *config.AppsConfig { return target.appsConfig })
			req := &DeleteModelsRequest{Paths: []string{filepath.Join(dir, "weights.gguf")}}
			if multi {
				req = &DeleteModelsRequest{Models: []ModelToDelete{{Name: "weights", Registry: "huggingface"}}}
			}
			_, err := svc.DeleteModels(t.Context(), req)
			require.ErrorContains(t, err, "publication refused")
		})
	}
}

func TestCatalogMutationSyncPullPublishesBeforeJobDone(t *testing.T) {
	target, reader, _ := mutationNodes(t, "worker")
	data := []byte("GGUF")
	manifest := modelsync.SyncManifest{Model: "org/model", Format: "gguf", TotalSize: int64(len(data)), Files: []modelsync.FileChecksum{{Name: "fresh.gguf", Size: int64(len(data)), SHA256: fmt.Sprintf("%x", sha256.Sum256(data))}}}
	client := &http.Client{Transport: mcpRoundTripper(func(r *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		if strings.Contains(r.URL.Path, "manifest") {
			require.NoError(t, json.NewEncoder(rec).Encode(manifest))
		} else {
			_, _ = rec.Write(data)
		}
		return rec.Result(), nil
	})}
	e := NewSyncExecutor(func() *http.Client { return client }, 0, func() context.Context { return t.Context() }, target.publishCatalog, nil).WithJobs(target.jobs)
	h, err := target.jobs.StartDetached(jobs.KindSync, "", nil)
	require.NoError(t, err)
	e.executeSyncPull(SyncPullRequest{Model: "org/model", Format: "gguf"}, h, "sync-key", modelsync.NewClient(modelsync.ClientConfig{HTTPClient: client}), "https://source.test")
	require.Equal(t, jobs.PhaseDone, waitJob(t, target.jobs, h.ID()).Phase)
	require.True(t, target.model.Cache.HasModel("fresh"))
	require.True(t, reader.model.Cache.HasModel("fresh"))
}

func TestCatalogMutationOllamaPathsPublishBeforeSuccess(t *testing.T) {
	for _, path := range []string{"/api/pull", "/api/create", "/api/copy", "/api/delete"} {
		t.Run(path, func(t *testing.T) {
			target, _, dir := mutationNodes(t, "coordinator")
			if path == "/api/delete" {
				present, _, _, _ := deletionDaemon(t, target, "ollama-publish", func() error { return os.WriteFile(filepath.Join(dir, "fresh.gguf"), []byte("GGUF"), 0600) })
				response := makeAuthRequest(t, target, "DELETE", path, TestAdminKey, map[string]any{"model": "ollama-publish/m"})
				require.Equal(t, 200, response.Code, string(response.Body))
				require.False(t, present.Load())
				require.True(t, target.model.Cache.HasModel("fresh"))
				return
			}
			client := &http.Client{Transport: mcpRoundTripper(func(*http.Request) (*http.Response, error) {
				require.NoError(t, os.WriteFile(filepath.Join(dir, "fresh.gguf"), []byte("GGUF"), 0600))
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"status":"success"}`))}, nil
			})}
			daemon := &backend.Resolved{Endpoint: "http://ollama.test", Upstream: backend.Engine()}
			c, rec := mutationContext(t, http.MethodPost, path, OllamaManagementRequest{Model: "weights"})
			target.httpStreamingClient = client
			if path == "/api/pull" {
				s := &OllamaServiceImpl{ollamaDaemon: func() (*backend.Resolved, error) { return daemon, nil }, httpStreamingClient: client, publishCatalog: target.publishCatalog}
				require.NoError(t, s.pullLocalProxy(c, "fresh", false, new(false)))
			} else {
				s := &OllamaServiceImpl{ollamaDaemon: func() (*backend.Resolved, error) { return daemon, nil }, proxyToOllamaProvider: target.proxyToOllamaProvider, publishCatalog: target.publishCatalog}
				require.NoError(t, s.ManagementCommand(c, path, false))
			}
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.True(t, target.model.Cache.HasModel("fresh"))
		})
	}
}

func TestCatalogMutationWorkerRefreshListPublishesBeforeResponse(t *testing.T) {
	target, reader, dir := mutationNodes(t, "worker")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "fresh.gguf"), []byte("GGUF"), 0600))
	_, err := target.services.Model.ListModels(t.Context(), &ListModelsRequest{Refresh: true})
	require.NoError(t, err)
	require.True(t, reader.model.Cache.HasModel("fresh"))
}

func TestCatalogMutationOllamaCommandSelection(t *testing.T) {
	for _, command := range []string{"create", "copy", "delete", "show", "push"} {
		t.Run(command, func(t *testing.T) {
			if command == "delete" {
				node := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, SeedProvidersDir: true})
				present, calls, _, _ := deletionDaemon(t, node, "ollama-selection")
				response := makeAuthRequest(t, node, "DELETE", "/api/delete", TestAdminKey, map[string]any{"model": "ollama-selection/m"})
				require.Equal(t, 200, response.Code, string(response.Body))
				require.False(t, present.Load())
				require.EqualValues(t, 1, calls.Load())
				require.False(t, node.model.Cache.HasModel("m"))
				return
			}
			called := false
			s := &OllamaServiceImpl{
				ollamaDaemon:   func() (*backend.Resolved, error) { return &backend.Resolved{}, nil },
				publishCatalog: func() error { return nil },
				proxyToOllamaProvider: func(_ *gin.Context, _ *backend.Resolved, _ string, _ []byte, publish func() error) {
					called = true
					require.Equal(t, command == "create" || command == "copy" || command == "delete", publish != nil)
				},
			}
			c, _ := mutationContext(t, http.MethodPost, "/api/"+command, OllamaManagementRequest{Model: "weights"})
			require.NoError(t, s.ManagementCommand(c, "/api/"+command, false))
			require.True(t, called)
		})
	}
}
