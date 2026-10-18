package server

import (
	"context"
	"encoding/json"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/internal/testproc"
	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stperic/zzrouter/pkg/routing"
	"github.com/stperic/zzrouter/pkg/version"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func lifecycleRecordManager(t *testing.T, saved ...prov_apps.EnsureRequest) (*prov_apps.ProviderAppManager, *config.AppsConfig, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Unix process fixture")
	}
	t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
	path := filepath.Join(config.Paths().GetDataDir(), "pinned_runs.json")
	if len(saved) > 0 {
		data, err := json.Marshal(map[string]any{"version": 1, "runs": saved})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, data, 0600))
	}
	engine := filepath.Join(t.TempDir(), "engine")
	require.NoError(t, os.WriteFile(engine, []byte(testproc.Script("printf 'READY\\n'\nwhile kill -0 $test_owner; do /bin/sleep 1 & wait $!; done\n")), 0700))
	cfg := &config.AppsConfig{}
	require.NoError(t, cfg.AddApp("generic", config.ServiceConfig{Name: "generic", Enabled: new(true), Mode: "on-demand", Protocol: config.ProtocolOpenAI,
		Runtime:      &config.AppRuntimeConfig{BasePort: 8740, PortRange: []int{8740, 8749}, Execution: config.ExecutionConfig{Type: "cli", Command: engine}, HealthCheck: config.HealthcheckConfig{ReadinessProbe: &config.ProbeConfig{Timeout: "5s", LogPatterns: config.LogPatternsConfig{Success: []config.PatternConfig{{Pattern: "READY"}}}}}},
		Capabilities: &config.AppCapabilities{WireEndpoints: []string{"chat_completions"}},
		Defaults:     &config.AppDefaultsConfig{Parameters: map[string]string{"budget": "0.55"}},
	}))
	parsed, err := schema.LoadYAMLSchema([]byte("parameters:\n  budget: {type: float}\n"))
	require.NoError(t, err)
	m, err := prov_apps.NewProviderAppManager(cfg, prov_apps.WithProviderSchema(func(name string) (*schema.ProviderSchema, error) {
		declared, errs := schema.Merge(name, parsed)
		require.Empty(t, errs)
		return declared, nil
	}))
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		assert.NoError(t, m.Stop(ctx))
	})
	return m, cfg, path
}

func lifecycleRecordExecutor(m *prov_apps.ProviderAppManager, cfg *config.AppsConfig, node string) *RunsExecutor {
	return NewRunsExecutor(m, func() string { return node }, func() *config.AppsConfig { return cfg }, nil, nil, nil)
}

func lifecycleRecordRoutes(e *RunsExecutor) *gin.Engine {
	engine := gin.New()
	group := engine.Group("/zzrouter/v1/internal")
	group.GET("/runs", e.HandleInternalListRuns)
	group.GET("/runs/:id", e.HandleInternalGetRun)
	group.DELETE("/runs/:id", e.HandleInternalStopRun)
	group.POST("/runs/:id/restart", e.HandleInternalRestartRun)
	group.POST("/runs", e.HandleInternalLaunchRun)
	return engine
}

func TestRunLifecycleRecordExecutor(t *testing.T) {
	for _, action := range []string{"stop", "restart"} {
		t.Run(action, func(t *testing.T) {
			saved := prov_apps.EnsureRequest{RunID: "pin-handle", Provider: "generic", Model: "recorded", Parameters: map[string]string{"keep-alive": "-1", "ghost": "1"}}
			m, cfg, _ := lifecycleRecordManager(t, saved)
			_, err := m.EnsureRun(t.Context(), prov_apps.EnsureRequest{Provider: saved.Provider, Model: saved.Model})
			require.ErrorIs(t, err, prov_apps.ErrParameterValidation)
			executor := lifecycleRecordExecutor(m, cfg, "worker")
			info, err := executor.GetLocalRun(t.Context(), saved.RunID)
			require.NoError(t, err)
			require.NotNil(t, info.LastRefusal)
			assert.Equal(t, string(httperr.CodeUnknownFlag), info.LastRefusal.Code)
			for _, withStatus := range []bool{false, true} {
				listed, err := executor.GetLocalRuns(t.Context(), withStatus)
				require.NoError(t, err)
				require.Len(t, listed, 1)
				assert.Equal(t, info.LastRefusal, listed[0].LastRefusal)
				assert.Equal(t, saved.Parameters, listed[0].Parameters)
			}
			engine := lifecycleRecordRoutes(executor)
			listed, err := NewRunsService(&lifecycleRouter{engine: engine}).ListRuns(t.Context(), &ListRunsRequest{})
			require.NoError(t, err)
			require.Len(t, listed.Data, 1)
			assert.Equal(t, saved.RunID, listed.Data[0].ID)
			expected, err := json.Marshal(info.LastRefusal)
			require.NoError(t, err)
			actual, err := json.Marshal(listed.Data[0].LastRefusal)
			require.NoError(t, err)
			assert.JSONEq(t, string(expected), string(actual))
			var refusal map[string]any
			require.NoError(t, json.Unmarshal(actual, &refusal))
			assert.Equal(t, string(httperr.CodeUnknownFlag), refusal["code"])
			controller := NewRunsController(NewRunsService(&lifecycleRouter{engine: engine}), nil, nil, &NodeIdentity{name: "worker"}, nil)
			public := gin.New()
			public.GET("/runs", controller.ListRuns)
			filtered := httptest.NewRecorder()
			public.ServeHTTP(filtered, httptest.NewRequest(http.MethodGet, "/runs?status=stopped", nil))
			require.Equal(t, http.StatusOK, filtered.Code, filtered.Body.String())
			assert.Contains(t, filtered.Body.String(), saved.RunID)
			public.DELETE("/runs/:id", controller.StopRun)
			public.POST("/runs/:id/restart", controller.RestartRun)
			method, path := http.MethodDelete, "/runs/"+saved.RunID
			if action == "restart" {
				// Reloading a corrected request exercises restart without a registered run.
				saved.Parameters = map[string]string{"keep-alive": "-1"}
				m, cfg, _ = lifecycleRecordManager(t, saved)
				executor.appMgr, executor.appsConfig = m, func() *config.AppsConfig { return cfg }
				method, path = http.MethodPost, path+"/restart"
			}
			rec := httptest.NewRecorder()
			public.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
			if action == "stop" {
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				assert.Empty(t, m.ListInstances("worker"))
			} else {
				require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
				var response struct {
					Data RestartRunResponse `json:"data"`
				}
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
				assert.Equal(t, saved.RunID, response.Data.OldID)
				run, exists := m.GetInstance(response.Data.ID)
				require.True(t, exists)
				assert.True(t, run.IdleSnapshot().Pinned)
				assert.Equal(t, saved.Parameters, run.SnapshotConfig().Parameters)
			}
		})
	}
}

func TestRunLifecycleReplayResponse(t *testing.T) {
	saved := prov_apps.EnsureRequest{RunID: "pin-handle", Provider: "generic", Model: "replayed", Parameters: map[string]string{"keep-alive": "-1", "budget": "0.37"}}
	m, cfg, _ := lifecycleRecordManager(t, saved)
	engine := lifecycleRecordRoutes(lifecycleRecordExecutor(m, cfg, "worker"))
	service := NewRunsService(&lifecycleRouter{engine: engine})
	response, err := service.LaunchRun(t.Context(), &LaunchRunRequest{Provider: saved.Provider, Model: saved.Model, LaunchMode: "native", Node: "worker"})
	require.NoError(t, err)
	assert.True(t, response.Pinned)
	assert.Equal(t, saved.Parameters, response.Parameters)
	assert.Equal(t, len(saved.Parameters), response.ParametersApplied)
	// D3: a reused run reports its own settings even when the caller stated others.
	response, err = service.LaunchRun(t.Context(), &LaunchRunRequest{Provider: saved.Provider, Model: saved.Model, LaunchMode: "native", Node: "worker", Parameters: schema.ParameterValues{"keep-alive": "30m"}})
	require.NoError(t, err)
	assert.True(t, response.Pinned)
	assert.Equal(t, saved.Parameters, response.Parameters)
}

type lifecycleRefreshRuns struct {
	*RunsService
	beforeRestart func()
}

func (r lifecycleRefreshRuns) RestartRun(ctx context.Context, req *RestartRunRequest) (*RestartRunResponse, error) {
	r.beforeRestart()
	return r.RunsService.RestartRun(ctx, req)
}

func TestRunLifecycleAffectedJobKeepsRefusedRun(t *testing.T) {
	m, cfg, _ := lifecycleRecordManager(t)
	result, err := m.EnsureRun(t.Context(), prov_apps.EnsureRequest{Provider: "generic", Model: "affected", Parameters: map[string]string{"keep-alive": "-1"}})
	require.NoError(t, err)
	run := result.Run
	select {
	case <-run.LeftStarting():
	case <-time.After(5 * time.Second):
		t.Fatal("run never ready")
	}
	require.Equal(t, instance.StatusRunning, run.GetStatus())
	reload := func(key, value string) {
		t.Helper()
		fresh := &config.AppsConfig{}
		cfg.RangeApps(func(name string, svc config.ServiceConfig) bool {
			defaults := *svc.Defaults
			defaults.Parameters = maps.Clone(defaults.Parameters)
			svc.Defaults = &defaults
			if name == "generic" {
				svc.Defaults.Parameters[key] = value
			}
			require.NoError(t, fresh.AddApp(name, svc))
			return true
		})
		m.ReloadConfig(fresh)
		cfg = fresh
	}
	reload("budget", "0.37")
	service := NewRunsService(&lifecycleRouter{engine: lifecycleRecordRoutes(lifecycleRecordExecutor(m, cfg, "worker"))})
	refresher := &runsRefresher{runs: lifecycleRefreshRuns{RunsService: service, beforeRestart: func() {
		reload("ghost", "1")
	}}, jobs: newTestJobsRegistry(t)}
	report := refresher.report(t.Context(), "generic", landedOn("worker"), true)
	require.Len(t, report.Stale, 1)
	require.NotEmpty(t, report.RestartJobID)
	ev := waitJob(t, refresher.jobs, report.RestartJobID)
	assert.Equal(t, jobs.PhaseFailed, ev.Phase)
	assert.Contains(t, ev.Err, run.ID)
	assert.Contains(t, ev.Err, "ghost")
	current, exists := m.GetInstance(run.ID)
	require.True(t, exists)
	assert.Same(t, run, current)
	assert.Equal(t, instance.StatusRunning, run.GetStatus())
}

func TestIntegrationRunLifecycleWorkerRecord(t *testing.T) {
	saved := prov_apps.EnsureRequest{Provider: "generic", Model: "worker-only", Parameters: map[string]string{"keep-alive": "-1"}}
	worker, cfg, path := lifecycleRecordManager(t)
	started, err := worker.EnsureRun(t.Context(), saved)
	require.NoError(t, err)
	select {
	case <-started.Run.LeftStarting():
	case <-time.After(5 * time.Second):
		t.Fatal("worker run never ready")
	}
	require.Equal(t, instance.StatusRunning, started.Run.GetStatus())
	require.NoError(t, worker.Stop(context.Background()))
	saved.RunID = worker.ListInstances("worker")[0].ID
	require.NoError(t, cfg.UpdateApp("generic", func(s *config.ServiceConfig) error { s.Defaults.Parameters["ghost"] = "1"; return nil }))
	worker, err = prov_apps.NewProviderAppManager(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, worker.Stop(context.Background())) })
	_, err = worker.EnsureRun(t.Context(), prov_apps.EnsureRequest{Provider: saved.Provider, Model: saved.Model, Origin: prov_apps.OriginRestore})
	require.Error(t, err)
	internal := lifecycleRecordRoutes(lifecycleRecordExecutor(worker, cfg, "worker"))
	internal.GET("/zzrouter/v1/internal/version", func(c *gin.Context) {
		info := version.GetCurrentVersionInfo(version.ServiceTypeHost, version.ServerCapabilities)
		info.Version = version.ClusterCompatibility.MinSupportedVersion
		c.JSON(http.StatusOK, info)
	})
	workerServer := httptest.NewTLSServer(internal)
	t.Cleanup(workerServer.Close)
	coord := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, NodeName: "coord", ClusterMode: config.ClusterModeCoordinator})
	cluster, err := mesh.NewCluster(&mesh.Config{NodeName: "coord"}, "http://coord.invalid", coord, nil, mesh.NewConnector(mesh.ConnectorConfig{HTTPClient: workerServer.Client(), ClusterPort: workerServer.Listener.Addr().(*net.TCPAddr).Port}))
	require.NoError(t, err)
	t.Cleanup(cluster.Stop)
	require.NoError(t, cluster.RegisterEndpoint(&mesh.Endpoint{URL: workerServer.URL, ClusterURL: workerServer.URL, NodeName: "worker", Status: mesh.StatusUp}))
	client := mesh.NewClusterClient(cluster, coord, "http://coord.invalid")
	coord.services.Runs.router = routing.NewClusterAwareRouterWithClient("coord", client, "0", coord.ServeLocalRequest, workerServer.Client())
	result := callMCPWireTool(t, coord, "zz_runs", map[string]any{})
	require.NotEqual(t, true, result["isError"], result)
	entries := result["structuredContent"].(map[string]any)["runs"].([]any)
	require.Len(t, entries, 1)
	entry := entries[0].(map[string]any)
	assert.Equal(t, saved.RunID, entry["run_id"])
	assert.Equal(t, "worker", entry["node"])
	assert.Equal(t, true, entry["pinned"])
	assert.Equal(t, true, entry["record_only"])
	assert.Equal(t, "stopped", entry["status"])
	assert.Equal(t, string(httperr.CodeUnknownFlag), entry["last_refusal"].(map[string]any)["code"])
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(raw), saved.RunID)
	assert.NoFileExists(t, filepath.Join(config.Paths().GetDataDir(), "pinned_runs.json"))
	assert.Empty(t, coord.providers.appMgr.ListInstances("coord"))
	logs := &LogsHandlers{getClusterClient: func() mesh.ClusterClient { return client }}
	assert.Nil(t, logs.findInstanceOnCluster(saved.RunID), "a record has no routable run log")
}

func TestRunLifecycleModelDeletionKeepsRecord(t *testing.T) {
	root := t.TempDir()
	detailWeights(t, root, "org/pinned", "base", 4096)
	s := createSeededTestNode(t, seededTestNodeConfig{TestNodeConfig: TestNodeConfig{AdminKey: TestAdminKey, NodeName: "delete-node"}, modelsDir: root, providers: []string{"llamacpp", "ollama", "huggingface"}})
	saved := prov_apps.EnsureRequest{RunID: "deleted-pin", Provider: "llamacpp", Model: "base", Parameters: map[string]string{"keep-alive": "-1"}}
	data, err := json.Marshal(map[string]any{"version": 1, "runs": []prov_apps.EnsureRequest{saved}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(config.Paths().GetDataDir(), "pinned_runs.json"), data, 0600))
	m, err := prov_apps.NewProviderAppManager(s.appsConfig())
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, m.Stop(context.Background())) })
	executor := s.newInternalExecutor()
	executor.deletionManager = m
	response, err := executor.deleteLocal(t.Context(), &DeleteModelsRequest{Model: "base"})
	require.NoError(t, err)
	assert.Equal(t, 1, response.Deleted)
	assert.NoFileExists(t, filepath.Join(root, "org", "pinned", "base.gguf"))
	listed := m.ListInstances("node")
	require.Len(t, listed, 1)
	assert.Equal(t, saved.RunID, listed[0].ID)
	assert.True(t, listed[0].RecordOnly)
}

func TestRunLifecycleMCPRecordActions(t *testing.T) {
	for _, tool := range []string{"zz_run_restart", "zz_run_stop"} {
		t.Run(tool, func(t *testing.T) {
			saved := prov_apps.EnsureRequest{RunID: "mcp-pin", Provider: "generic", Model: "recorded", Parameters: map[string]string{"keep-alive": "-1"}}
			m, cfg, _ := lifecycleRecordManager(t, saved)
			s := createTestNodeWithDefaults(t)
			s.services.Runs.router = &lifecycleRouter{engine: lifecycleRecordRoutes(lifecycleRecordExecutor(m, cfg, "worker"))}
			result := callMCPWireTool(t, s, tool, map[string]any{"run_id": saved.RunID})
			require.NotEqual(t, true, result["isError"], result)
			content := result["structuredContent"].(map[string]any)
			if tool == "zz_run_stop" {
				assert.Equal(t, saved.RunID, content["run_id"])
				assert.Empty(t, m.ListInstances("worker"))
			} else {
				assert.Equal(t, saved.RunID, content["old_run_id"])
				id := content["run_id"].(string)
				live, exists := m.GetInstance(id)
				require.True(t, exists)
				assert.True(t, live.IdleSnapshot().Pinned)
			}
		})
	}
}

type lifecycleRecordClient struct {
	mesh.ClusterClient
	body []byte
}

func (c lifecycleRecordClient) Broadcast(context.Context, string, *mesh.QueryParams) (*mesh.BroadcastResponse, error) {
	return &mesh.BroadcastResponse{Responses: []*mesh.NodeResponse{{NodeName: "worker", Response: &mesh.Response{StatusCode: http.StatusOK, Body: c.body}}}}, nil
}

func TestRunLifecycleRecordHasNoLogRoute(t *testing.T) {
	saved := prov_apps.EnsureRequest{RunID: "pin", Provider: "generic", Model: "recorded", Parameters: map[string]string{"keep-alive": "-1"}}
	m, cfg, _ := lifecycleRecordManager(t, saved)
	live := instance.NewInstance("live", "generic", "live", 0, 0, 0)
	live.MarkRunning(false, nil)
	require.NoError(t, m.Instances().Register(live))
	listed, err := lifecycleRecordExecutor(m, cfg, "worker").GetLocalRuns(t.Context(), false)
	require.NoError(t, err)
	data, err := json.Marshal(map[string]any{"instances": listed})
	require.NoError(t, err)
	logs := &LogsHandlers{getClusterClient: func() mesh.ClusterClient { return lifecycleRecordClient{body: data} }}
	assert.Nil(t, logs.findInstanceOnCluster(saved.RunID))
	found := logs.findInstanceOnCluster(live.ID)
	require.NotNil(t, found)
	assert.Equal(t, live.ID, found["id"])
}

func TestRunLifecycleStopRecordRefusalCode(t *testing.T) {
	saved := prov_apps.EnsureRequest{RunID: "pin", Provider: "generic", Model: "held", Parameters: map[string]string{"keep-alive": "-1"}}
	m, cfg, _ := lifecycleRecordManager(t, saved)
	live := instance.NewInstance("live-run", "generic", saved.Model, 0, 0, 0)
	live.MarkRunning(false, nil)
	require.NoError(t, m.Instances().Register(live))
	rec := httptest.NewRecorder()
	lifecycleRecordRoutes(lifecycleRecordExecutor(m, cfg, "worker")).ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/zzrouter/v1/internal/runs/pin", nil))
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "pin_run_active", body["code"])
	assert.Contains(t, body["detail"], live.ID)
	assert.Contains(t, body["detail"], "zz_run_stop with run_id "+live.ID)
	require.NoError(t, m.Instances().Remove(live.ID))
	assert.Equal(t, saved.RunID, m.ListInstances("worker")[0].ID)
}

func TestRunRefusalWire(t *testing.T) {
	info := instance.InstanceInfo{ID: "pin", RecordOnly: true, Refusal: httperr.Error{
		Code: "unknown_flag", Status: http.StatusBadRequest, Message: "invalid flag",
		Type: "invalid_request_error", Title: "Bad flag", Extra: map[string]any{"private": "hidden"}, RetryAfter: time.Second,
	}}
	classifyRunInfo(&info)
	body, err := json.Marshal(info)
	require.NoError(t, err)
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(body, &raw))
	assert.JSONEq(t, `{"code":"unknown_flag","status":400,"message":"invalid flag"}`, string(raw["last_refusal"]))
	var client pkgClient.Instance
	require.NoError(t, json.Unmarshal(body, &client))
	require.NotNil(t, client.LastRefusal)
	assert.Equal(t, info.LastRefusal, client.LastRefusal)
	assert.Equal(t, "pin (no run): unknown_flag", client.DisplayStatus())
	schemas := loadSpecTree(t)["components"].(map[string]any)["schemas"].(map[string]any)
	runProps := schemas["Run"].(map[string]any)["properties"].(map[string]any)
	assert.Equal(t, map[string]any{"$ref": "#/components/schemas/RunRefusal"}, runProps["last_refusal"])
	refusalSchema := schemas["RunRefusal"].(map[string]any)
	assert.Equal(t, false, refusalSchema["additionalProperties"])
	assert.ElementsMatch(t, []any{"code", "status", "message"}, refusalSchema["required"])
	props := refusalSchema["properties"].(map[string]any)
	require.Len(t, props, 3)
	for key, kind := range map[string]string{"code": "string", "status": "integer", "message": "string"} {
		assert.Equal(t, kind, props[key].(map[string]any)["type"])
	}
}
