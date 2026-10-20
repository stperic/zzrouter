package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/internal/testproc"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stperic/zzrouter/pkg/routing"
	"github.com/stretchr/testify/require"
)

func nodeRunFixture(t *testing.T) (*prov_apps.ProviderAppManager, *config.AppsConfig) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Unix process fixture")
	}
	t.Setenv("ZZROUTER_TEST_HOME", t.TempDir()) // a pinned run's record is node state that outlives Stop
	engine := filepath.Join(t.TempDir(), "studio")
	require.NoError(t, os.WriteFile(engine, []byte(testproc.Script("trap 'kill $! 2>/dev/null; exit 0' TERM INT\nprintf 'READY\\n'\nwhile kill -0 $test_owner; do /bin/sleep 1 & wait $!; done\n")), 0o700))
	cfg := &config.AppsConfig{}
	require.NoError(t, cfg.AddApp("studio", config.ServiceConfig{
		Name: "studio", RunScope: config.RunScopeNode, Enabled: new(true), Mode: "on-demand",
		Runtime: &config.AppRuntimeConfig{
			BasePort: 8810, PortRange: []int{8810, 8819}, KeepAlive: "1m",
			Execution:   config.ExecutionConfig{Type: "cli", Command: engine},
			HealthCheck: config.HealthcheckConfig{ReadinessProbe: &config.ProbeConfig{Timeout: "10s", LogPatterns: config.LogPatternsConfig{Success: []config.PatternConfig{{Pattern: "READY"}}}}},
		},
	}))

	require.NoError(t, cfg.AddApp("model-engine", config.ServiceConfig{
		Name: "model-engine", Enabled: new(true), Mode: "on-demand", Protocol: config.ProtocolOpenAI,
		Capabilities: &config.AppCapabilities{WireEndpoints: []string{"chat_completions"}},
		Runtime:      &config.AppRuntimeConfig{BasePort: 8840, PortRange: []int{8840, 8849}, Execution: config.ExecutionConfig{Type: "cli", Command: engine}},
	}))
	// seed is the fixture's own integer parameter: its values exceed float64 precision.
	declared := &schema.YAMLSchema{Parameters: map[string]schema.YAMLParam{"seed": {Type: "int"}}}
	seed := prov_apps.WithProviderSchema(func(provider string) (*schema.ProviderSchema, error) {
		merged, errs := schema.Merge(provider, declared, config.ServiceConfig{})
		return merged, errors.Join(errs...)
	})
	mgr, err := prov_apps.NewProviderAppManager(cfg, seed, prov_apps.WithNodename(func() string { return "worker" }), prov_apps.WithSelfHost(func() string { return "2001:db8::123" }), prov_apps.WithLocalCatalog(func(context.Context, string) (bool, error) {
		t.Error("node run used the model catalog")
		return false, nil
	}))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, mgr.Stop(context.Background())) })
	return mgr, cfg
}

func TestNodeRunWorkerLaunchAndTargetValidation(t *testing.T) {
	mgr, cfg := nodeRunFixture(t)
	executor := NewRunsExecutor(mgr, func() string { return "worker" }, func() *config.AppsConfig { return cfg }, nil, nil, nil)
	worker := gin.New()
	worker.POST("/zzrouter/v1/internal/runs", executor.HandleInternalLaunchRun)
	worker.GET("/zzrouter/v1/internal/runs", executor.HandleInternalListRuns)
	service := NewRunsService(&previewWorkerRouter{worker: worker}).WithAutoDeploy(nil, nil, func() *config.AppsConfig { return cfg }, nil)
	ctrl := &RunsController{service: service}
	coordinator := gin.New()
	coordinator.POST("/zzrouter/v1/runs", ctrl.LaunchRun)
	request := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/zzrouter/v1/runs", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-API-Key", TestAdminKey)
		coordinator.ServeHTTP(rec, req)
		return rec
	}
	first := request(`{"provider":"studio","node":"worker","parameters":{"keep-alive":-1,"evict-idle-on-memory-pressure":false,"seed":9007199254740993}}`)
	require.Equal(t, http.StatusAccepted, first.Code, first.Body.String())
	var response struct {
		Data LaunchRunResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(first.Body.Bytes(), &response))
	require.Equal(t, "studio", response.Data.Provider)
	require.Equal(t, "worker", response.Data.Node)
	require.Equal(t, "http://[2001:db8::123]:8810", response.Data.BaseURL)
	run, ok := mgr.GetInstance(response.Data.ID)
	require.True(t, ok)
	select {
	case <-run.LeftStarting():
	case <-time.After(10 * time.Second):
		t.Fatal("worker run did not become ready")
	}
	require.Equal(t, instance.StatusRunning, run.GetStatus())
	require.Equal(t, "-1", run.Resolved().Parameters["keep-alive"])
	require.Equal(t, "false", run.Resolved().Parameters["evict-idle-on-memory-pressure"])
	require.Equal(t, "9007199254740993", run.Resolved().Parameters["seed"])
	require.Positive(t, run.GetProcessID())
	require.Equal(t, response.Data.BaseURL, run.ToInfo("worker").BaseURL)
	second := request(`{"provider":"studio","node":"worker"}`)
	require.NoError(t, json.Unmarshal(second.Body.Bytes(), &response))
	require.Equal(t, run.ID, response.Data.ID)
	for _, body := range []string{
		`{"provider":"studio","model_name":"forbidden","node":"worker"}`,
		`{"provider":"studio","endpoint":"embeddings","node":"worker"}`,
		`{"provider":"model-engine","node":"worker"}`,
	} {
		rec := request(body)
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	}
	direct := httptest.NewRecorder()
	worker.ServeHTTP(direct, httptest.NewRequest(http.MethodPost, "/zzrouter/v1/internal/runs", bytes.NewBufferString(`{"provider":"studio","launch_mode":"native","model_name":"forbidden"}`)))
	require.Equal(t, http.StatusBadRequest, direct.Code)
}

func TestNodeRunDirectWorkerScalarParameters(t *testing.T) {
	mgr, cfg := nodeRunFixture(t)
	executor := NewRunsExecutor(mgr, func() string { return "worker" }, func() *config.AppsConfig { return cfg }, nil, nil, nil)
	for _, value := range []string{"null", "{}", "[]"} {
		c, rec := postJSON(t, `{"provider":"studio","launch_mode":"native","parameters":{"bad":`+value+`}}`)
		executor.HandleInternalLaunchRun(c)
		require.Equal(t, http.StatusBadRequest, rec.Code)
		require.Contains(t, rec.Body.String(), "parameters.bad")
		require.Empty(t, mgr.Instances().List())
	}
	c, rec := postJSON(t, `{"provider":"studio","launch_mode":"native","parameters":{"keep-alive":-1,"seed":9007199254740993,"evict-idle-on-memory-pressure":false}}`)
	executor.HandleInternalLaunchRun(c)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	var response LaunchRunResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
	run, ok := mgr.GetInstance(response.ID)
	require.True(t, ok)
	select {
	case <-run.LeftStarting():
	case <-time.After(10 * time.Second):
		t.Fatal("worker run did not become ready")
	}
	require.Equal(t, instance.StatusRunning, run.GetStatus())
	require.Equal(t, "-1", run.Resolved().Parameters["keep-alive"])
	require.Equal(t, "9007199254740993", run.Resolved().Parameters["seed"])
	require.Equal(t, "false", run.Resolved().Parameters["evict-idle-on-memory-pressure"])
}

func (r *previewWorkerRouter) Route(ctx context.Context, req *routing.Request) (*routing.Response, error) {
	return r.Unicast(ctx, req.Node, req.Path, req.Method, req.Body)
}
