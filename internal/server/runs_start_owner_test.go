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
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/utils/clock"
	"github.com/stretchr/testify/require"
)

func TestRunsExecutorPublishesCanonicalOwner(t *testing.T) {
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
			executor := NewRunsExecutor(manager, func() string { return "canonical-worker" }, manager.AppsConfig, nil, nil, nil)
			response, err := startForTest(t, t.Context(), executor, LaunchRunRequest{Model: "m", Provider: provider, Node: "requested-alias"})
			require.NoError(t, err)
			require.Equal(t, "canonical-worker", response.Node)
			require.Equal(t, withRun, response.ID != "")
		})
	}
}

func TestStartRunRefusesMissingOwner(t *testing.T) {
	for _, target := range []string{"@master", "worker", "requested-alias"} {
		t.Run(target, func(t *testing.T) {
			status := http.StatusAccepted
			if target == "@master" {
				status = http.StatusOK
			}
			router := &runResultRouter{status: status, body: `{"id":"run","job_id":"job"}`, node: "192.0.2.10"}
			controller := NewRunsController(NewRunsService(router), nil, nil, nil, nil)
			c, w := postJSON(t, `{"provider":"fake","model":"m","node":"`+target+`"}`)
			controller.LaunchRun(c)
			require.Equal(t, http.StatusBadGateway, w.Code, w.Body.String())
			problem := requireStartProblem(t, parseRoutedError(w.Code, w.Body.Bytes(), w.Header()), http.StatusBadGateway, string(httperr.CodeUpstreamShape))
			require.Contains(t, problem.Detail, "missing its owning node")
			require.Equal(t, target, problem.Node)
		})
	}
}

// Exercise the existing client watch against an actual worker-owned job registry.
// The transport's IP is deliberately not a registered node selector.
func TestStartRunWatchPreservesWorkerJobOwner(t *testing.T) {
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
	loadBody, err := json.Marshal(LaunchRunResponse{ID: "ready-run", Provider: "fake", Status: "running", Node: "canonical-worker", JobID: job.ID()})
	require.NoError(t, err)
	service := NewRunsService(&runResultRouter{status: http.StatusAccepted, body: string(loadBody), node: "192.0.2.10"})
	coordinator := gin.New()
	coordinator.POST("/zzrouter/v1/runs", NewRunsController(service, nil, nil, nil, nil).LaunchRun)
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
	loaded, err := client.StartRun(t.Context(), pkgClient.StartRunRequest{Node: "requested-alias", Model: "m", Provider: "fake"})
	require.NoError(t, err)
	terminal, err := client.SubscribeJob(t.Context(), loaded.JobID, loaded.Node, nil)
	require.NoError(t, err)
	require.NotNil(t, terminal)
	require.Equal(t, "canonical-worker", loaded.Node)
	require.Equal(t, "canonical-worker", terminal.Node)
	require.Equal(t, string(jobs.PhaseDone), terminal.Phase)
}
