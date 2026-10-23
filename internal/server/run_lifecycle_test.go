package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/model/resolver"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLifecycleNoneAllRunCallers(t *testing.T) {
	for _, mode := range []string{"external", "cloud"} {
		for _, catalog := range []bool{false, true} {
			t.Run(mode+map[bool]string{false: " absent", true: " cataloged"}[catalog], func(t *testing.T) {
				cfg := &config.AppsConfig{}
				provider := "endpoint"
				require.NoError(t, cfg.AddApp(provider, config.ServiceConfig{Name: provider, Enabled: new(true), Mode: mode, Protocol: config.ProtocolOpenAI, Runtime: &config.AppRuntimeConfig{Endpoint: "https://provider.invalid", API: &config.APIConfig{AuthType: config.AuthTypeBearer, Token: "test-provider-token"}}, Capabilities: &config.AppCapabilities{WireEndpoints: []string{"chat_completions"}}}))
				listed := `{"data":[]}`
				if catalog {
					listed = `{"data":[{"id":"m"}]}`
				}
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, listed)
				}))
				t.Cleanup(upstream.Close)
				mgr, err := prov_apps.NewProviderAppManager(cfg,
					prov_apps.WithLocalCatalog(func(context.Context, string) (bool, error) {
						t.Error("a provider without runs read the weights catalog")
						return false, nil
					}),
					prov_apps.WithBackendResolver(func(string) (*backend.Resolved, bool) { return &backend.Resolved{Endpoint: upstream.URL}, true }))
				require.NoError(t, err)
				t.Cleanup(func() { assert.NoError(t, mgr.Stop(context.Background())) })
				_, previewErr := mgr.PreviewLaunch(t.Context(), prov_apps.EnsureRequest{Provider: provider, Model: "m"}, 0)
				require.ErrorIs(t, previewErr, prov_apps.ErrNoLocalRun)
				assert.Equal(t, http.StatusBadRequest, answeredStatus(previewErr))
				executor := NewRunsExecutor(mgr, func() string { return "worker" }, func() *config.AppsConfig { return cfg }, nil, nil, nil)
				executor.modelInPool = func(string) bool { return false }
				executor.deploy = func(context.Context, *DeployRequest) (*Deployment, error) {
					t.Fatal("none auto-deployed")
					return nil, nil
				}
				engine := gin.New()
				engine.POST("/zzrouter/v1/internal/runs", executor.HandleInternalLaunchRun)
				service := NewRunsService(&lifecycleRouter{engine: engine}).WithAutoDeploy(&DeploymentsService{}, func(string) bool { return false }, func() *config.AppsConfig { return cfg }, newJobsForChainTest(t)).WithModelResolver(func(context.Context, string) (*resolver.Resolved, error) {
					return &resolver.Resolved{Provider: provider, ModelName: "m", Node: "worker"}, nil
				})
				launch, err := service.LaunchRun(t.Context(), &LaunchRunRequest{Provider: provider, Model: "m", LaunchMode: "native", Node: "worker", AutoDeploy: true})
				if catalog {
					require.NoError(t, err)
					assert.Equal(t, "running", launch.Status)
					assert.Empty(t, launch.ID)
					assert.Empty(t, launch.JobID)
				} else {
					assert.Equal(t, http.StatusNotFound, answeredStatus(err))
					assert.Contains(t, err.Error(), "not in this provider's catalog")
				}
				ensured, err := service.EnsureRun(t.Context(), &EnsureRunRequest{Model: "m"})
				if catalog {
					require.NoError(t, err)
					assert.Empty(t, ensured.RunID)
					assert.Empty(t, ensured.JobID)
				} else {
					assert.Equal(t, http.StatusNotFound, answeredStatus(err))
				}
				loaded, err := NewLoadExecutor(mgr, emptyModelStore{}, func() string { return "worker" }).LoadLocalModel(t.Context(), &LoadModelRequest{Provider: provider, ModelName: "m"})
				if catalog {
					require.NoError(t, err)
					assert.Empty(t, loaded.InstanceID)
					assert.Empty(t, loaded.JobID)
					assert.Equal(t, "running", loaded.Status)
				} else {
					assert.ErrorIs(t, err, prov_apps.ErrModelNotLocal)
				}
				utility := NewRunsUtilityService(mgr, func() *config.AppsConfig { return cfg })
				launches, failed, err := utility.BatchLaunch(t.Context(), []LaunchRequest{{Provider: provider, Model: "m", LaunchMode: "native"}})
				require.NoError(t, err)
				if catalog {
					assert.Len(t, launches, 1)
					assert.Empty(t, failed)
				} else {
					assert.Empty(t, launches)
					assert.Len(t, failed, 1)
				}
				assert.Empty(t, mgr.Instances().List())
				// Detached callers must get the same synchronous policy answer.
				result, err := mgr.EnsureRun(t.Context(), prov_apps.EnsureRequest{Provider: provider, Model: "m", Detached: true})
				if catalog {
					require.NoError(t, err)
					assert.Nil(t, result.Run, "ready with no run")
				} else {
					assert.ErrorIs(t, err, prov_apps.ErrModelNotLocal)
				}
			})
		}
	}
}

// A daemon that cannot be reached answers every caller waiting on the load 503, and leaves no run.
func TestLifecycleServedUnavailableUses503(t *testing.T) {
	cfg := &config.AppsConfig{}
	require.NoError(t, cfg.AddApp("daemon", config.ServiceConfig{Name: "daemon", Enabled: new(true), Mode: "external", Protocol: config.ProtocolOllama, Runtime: &config.AppRuntimeConfig{Endpoint: "http://daemon:11434"}, Capabilities: &config.AppCapabilities{WireEndpoints: []string{"chat_completions"}}}))
	refused := &http.Client{Transport: mcpRoundTripper(func(*http.Request) (*http.Response, error) { return nil, errors.New("connect: connection refused") })}
	mgr, err := prov_apps.NewProviderAppManager(cfg, prov_apps.WithHTTPClient(refused))
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, mgr.Stop(context.Background())) })
	s := &Server{}
	s.providers.appMgr = mgr
	_, err = s.loadModelViaNodeAPI("m", "daemon", "chat")
	require.ErrorIs(t, err, prov_apps.ErrProviderUnavailable)
	assert.Equal(t, http.StatusServiceUnavailable, answeredStatus(err))
	assert.Empty(t, mgr.Instances().List())
}
