package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	"github.com/stperic/zzrouter/pkg/cluster/role"
	clustersignal "github.com/stperic/zzrouter/pkg/cluster/signal"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/model/cache"
	"github.com/stperic/zzrouter/pkg/model/resolver"
	"github.com/stperic/zzrouter/pkg/modelregistry"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stretchr/testify/require"
)

type deploymentCatalogClient struct {
	mesh.ClusterClient
	worker *Server
}

func (c deploymentCatalogClient) Broadcast(ctx context.Context, path string, _ *mesh.QueryParams) (*mesh.BroadcastResponse, error) {
	models, _, err := c.worker.newInternalExecutor().localModelSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(map[string]any{"models": models})
	if err != nil {
		return nil, err
	}
	return &mesh.BroadcastResponse{Responses: []*mesh.NodeResponse{{NodeName: c.worker.node.Nodename(), Response: &mesh.Response{StatusCode: http.StatusOK, Body: body}}}}, nil
}

func TestDeploymentCompletionPublishesImageAdmissionEvidence(t *testing.T) {
	for _, location := range []string{"coordinator", "worker"} {
		for _, feature := range []string{"projector", "mlx"} {
			t.Run(location+"/"+feature, func(t *testing.T) {
				root := t.TempDir()
				modelregistry.SetModelsRootDirOverride(root)
				t.Cleanup(modelregistry.ClearModelsRootDirOverride)
				dir := filepath.Join(root, "org", "model")
				require.NoError(t, os.MkdirAll(dir, 0700))
				provider, modelName, want := "llamacpp", "weights", "present"
				if feature == "projector" {
					require.NoError(t, os.WriteFile(filepath.Join(dir, "weights.gguf"), []byte("GGUF"), 0600))
					require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"vision_config":{}}`), 0600))
				} else {
					provider, modelName, want = "mlx", "org/model", "unsupported"
				}
				mode := config.ClusterModeCoordinator
				if location == "worker" {
					mode = config.ClusterModeWorker
				}
				target := createSeededTestNode(t, seededTestNodeConfig{TestNodeConfig: TestNodeConfig{AdminKey: TestAdminKey, ClusterMode: mode, NodeName: location}, providers: []string{"llamacpp", "mlx", "huggingface"}, enabled: []string{provider}})
				reader := target
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
						IsWorker:        target.node.IsWorker,
						MTLSClient:      func() *http.Client { return client },
						CoordinatorURL:  func() string { return "https://coordinator.test" },
						WorkerPublicURL: func() string { return "http://worker.test" },
					})
					target.cluster.signaler.Start(t.Context())
					t.Cleanup(func() { target.cluster.signaler.Stop(context.Background()) })
				}
				require.NoError(t, target.model.Cache.RefreshCacheSync(t.Context()))
				require.NoError(t, reader.model.Cache.RefreshCacheSync(t.Context()))
				before, err := reader.model.Cache.LookupTarget(t.Context(), modelName, provider, location)
				require.NoError(t, err)
				if feature == "projector" {
					require.NotNil(t, before)
					require.Equal(t, "available", before.FeatureState("vision"))
				} else {
					require.Nil(t, before)
				}
				executor := target.newDeploymentsExecutor()
				executor.ensureFeatures = func(context.Context, string, string, []string, []string, func(string)) error {
					if feature == "projector" {
						return os.WriteFile(filepath.Join(dir, "mmproj-F16.gguf"), []byte("projector"), 0600)
					}
					if err := os.WriteFile(filepath.Join(dir, "mlx_model.safetensors"), []byte("weights"), 0600); err != nil {
						return err
					}
					return os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"model_type":"qwen2"}`), 0600)
				}
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/zzrouter/v1/internal/deployments", nil)
				executor.downloadHuggingFace(c, testHuggingFaceAdmission(t, executor, metadata.DownloadRequest{Repo: "org/model", Files: []metadata.DownloadFile{}}, "catalog-download", provider, []string{}))
				require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
				var result internalDeploymentResult
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &result))
				require.Equal(t, jobs.PhaseDone, waitJob(t, target.jobs, result.JobID).Phase)
				after, err := reader.model.Cache.LookupTarget(t.Context(), modelName, provider, location)
				require.NoError(t, err)
				require.NotNil(t, after)
				require.Equal(t, want, after.FeatureState("vision"))
				require.Equal(t, feature == "projector", reader.narrowToVision(t.Context(), &resolver.Resolved{ModelName: modelName, Provider: provider, Node: location}))
				require.Equal(t, "completed", target.model.Downloads.GetDownloadStatus("catalog-download").Status)
				require.NoError(t, target.publishCatalog())
				after, err = reader.model.Cache.LookupTarget(t.Context(), modelName, provider, location)
				require.NoError(t, err)
				require.NotNil(t, after)
				require.Equal(t, want, after.FeatureState("vision"))
			})
		}
	}
}

func TestDeploymentCatalogPublicationFailureFailsJob(t *testing.T) {
	modelregistry.SetModelsRootDirOverride(t.TempDir())
	t.Cleanup(modelregistry.ClearModelsRootDirOverride)
	target := createTestNodeWithDefaults(t)
	executor := target.newDeploymentsExecutor()
	executor.publishCatalog = func() error { return errors.New("catalog unavailable") }
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/zzrouter/v1/internal/deployments", nil)
	executor.downloadHuggingFace(c, testHuggingFaceAdmission(t, executor, metadata.DownloadRequest{Repo: "org/model", Files: []metadata.DownloadFile{}}, "catalog-download", "", nil))
	var result internalDeploymentResult
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &result))
	event := waitJob(t, target.jobs, result.JobID)
	require.Equal(t, jobs.PhaseFailed, event.Phase)
	require.Contains(t, event.Err, "catalog unavailable")
	require.Equal(t, "failed", target.model.Downloads.GetDownloadStatus("catalog-download").Status)
}

func TestFinalizeCatalogNotificationDoesNotBlockCallback(t *testing.T) {
	target := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, ClusterMode: config.ClusterModeWorker})
	workerRole, err := role.NewManager(role.RoleWorker)
	require.NoError(t, err)
	target.role = workerRole
	entered, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	client := &http.Client{Transport: mcpRoundTripper(func(r *http.Request) (*http.Response, error) {
		close(entered)
		<-release
		return httptest.NewRecorder().Result(), nil
	})}
	target.cluster.signaler = clustersignal.New(clustersignal.Config{
		IsWorker:       target.node.IsWorker,
		MTLSClient:     func() *http.Client { return client },
		CoordinatorURL: func() string { return "https://coordinator.test" },
	})
	target.cluster.signaler.Start(t.Context())
	t.Cleanup(func() {
		close(release)
		<-returned
		target.refreshWG.Wait()
		target.cluster.signaler.Stop(context.Background())
	})
	go func() {
		target.onFinalize.Load().fn("ollama")
		close(returned)
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("notification did not reach coordinator")
	}
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("finalize callback blocked on coordinator acknowledgement")
	}
	drained := make(chan struct{})
	go func() {
		target.refreshWG.Wait()
		close(drained)
	}()
	select {
	case <-drained:
		t.Fatal("background notification was not tracked")
	case <-time.After(50 * time.Millisecond):
	}
}
