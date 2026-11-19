package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/utils/clock"
	"github.com/stretchr/testify/require"
)

func TestLoadExecutorPublishesCanonicalOwner(t *testing.T) {
	for _, withRun := range []bool{false, true} {
		t.Run(map[bool]string{false: "endpoint", true: "run"}[withRun], func(t *testing.T) {
			cfg := aliasLoadConfig(t)
			provider := aliasLoadProvider
			if !withRun {
				provider = "endpoint"
				endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"data":[{"id":"m"}]}`))
				}))
				t.Cleanup(endpoint.Close)
				require.NoError(t, cfg.AddApp(provider, config.ServiceConfig{Enabled: new(true), Mode: "external", Protocol: config.ProtocolOpenAI, Runtime: &config.AppRuntimeConfig{Endpoint: endpoint.URL}, Capabilities: &config.AppCapabilities{Models: []string{"m"}, WireEndpoints: []string{"chat_completions"}}}))
			}
			manager, err := prov_apps.NewProviderAppManager(cfg, prov_apps.WithBackendResolver(backend.NewResolver(func() *config.AppsConfig { return cfg }).Resolve), prov_apps.WithLocalCatalog(func(context.Context, string) (bool, error) { return true, nil }))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, manager.Stop(context.Background())) })
			if withRun {
				run := instance.NewInstance("ready", provider, "m", 8300, 0, 0)
				run.MarkRunning(false, nil)
				require.NoError(t, manager.Instances().Register(run))
				t.Cleanup(func() { _ = manager.Instances().Remove(run.ID) })
			}
			executor := NewLoadExecutor(manager, emptyModelStore{}, func() string { return "canonical-worker" })
			response, err := executor.LoadLocalModel(t.Context(), &LoadModelRequest{ModelName: "m", Provider: provider, Node: "requested-alias"})
			require.NoError(t, err)
			require.Equal(t, "canonical-worker", response.Node)
			require.Equal(t, withRun, response.InstanceID != "")
		})
	}
}

func TestLoadServiceRefusesMissingOwner(t *testing.T) {
	service := NewLoadService(nil, &loadResultRouter{status: http.StatusOK, body: `{"instance_id":"run","model":"m","provider":"fake","job_id":"worker-job"}`, node: "192.0.2.10"})
	response, err := service.LoadModel(t.Context(), &LoadModelRequest{ModelName: "m", Provider: "fake", Node: "requested-alias"})
	require.Error(t, err)
	require.Nil(t, response)
	require.Equal(t, http.StatusBadGateway, answeredStatus(err))
}

// Exercise the existing client watch against an actual worker-owned job registry.
// The transport's IP is deliberately not a registered node selector.
func TestLoadModelWatchPreservesWorkerJobOwner(t *testing.T) {
	registry, err := jobs.NewRegistry(jobs.Config{NodeName: "canonical-worker", Clock: clock.System()})
	require.NoError(t, err)
	t.Cleanup(registry.Stop)
	job, err := registry.StartDetached(jobs.KindRun, "", nil)
	require.NoError(t, err)
	job.Done()
	worker := gin.New()
	NewJobsController(registry, "canonical-worker", nil).RegisterInternalRoutes(worker.Group("/zzrouter/v1/internal"))
	workerHTTP := httptest.NewServer(worker)
	t.Cleanup(workerHTTP.Close)
	loadBody, err := json.Marshal(LoadModelResponse{InstanceID: "ready-run", Model: "m", Provider: "fake", Status: "running", Node: "canonical-worker", JobID: job.ID()})
	require.NoError(t, err)
	service := NewLoadService(nil, &loadResultRouter{status: http.StatusOK, body: string(loadBody), node: "192.0.2.10"})
	coordinator := gin.New()
	coordinator.POST("/zzrouter/v1/runs/load", NewRunsController(nil, service, nil, nil, nil, nil).LoadModel)
	proxy := &JobsProxy{
		StreamClient: func() (*http.Client, error) { return workerHTTP.Client(), nil },
		ResolveEndpoint: func(node string) *mesh.Endpoint {
			if node != "canonical-worker" {
				return nil
			}
			return &mesh.Endpoint{NodeName: "canonical-worker", ClusterURL: workerHTTP.URL}
		},
	}
	NewJobsControllerWithProxy(nil, "coordinator", nil, proxy).RegisterPublicRoutes(coordinator.Group("/zzrouter/v1"))
	coordinatorHTTP := httptest.NewServer(coordinator)
	t.Cleanup(coordinatorHTTP.Close)
	client := pkgClient.NewClient(config.ClientNodeConfig{Name: "coordinator", Address: coordinatorHTTP.URL})
	loaded, err := client.LoadModelWithProviderForceParamsAndEnv("requested-alias", "m", "fake", false, nil, nil)
	require.NoError(t, err)
	terminal, err := client.SubscribeJob(t.Context(), loaded.JobID, loaded.Node, nil)
	require.NoError(t, err)
	require.NotNil(t, terminal)
	require.Equal(t, "canonical-worker", loaded.Node)
	require.Equal(t, "canonical-worker", terminal.Node)
	require.Equal(t, string(jobs.PhaseDone), terminal.Phase)
}
