package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/modelregistry"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/utils/clock/clocktest"
	"github.com/stretchr/testify/require"
)

func TestRunReuseOmitsGoneJob(t *testing.T) {
	for _, gone := range []string{"expired", "never-registered"} {
		t.Run(gone, func(t *testing.T) {
			fc := clocktest.NewFakeClock(time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC))
			registry, err := jobs.NewRegistry(jobs.Config{NodeName: "node", Clock: fc, JanitorInterval: 2 * constants.IdempotencyReplayWindow})
			require.NoError(t, err)
			t.Cleanup(registry.Stop)
			id := "run_never-registered"
			if gone == "expired" {
				h, startErr := registry.StartDetached(jobs.KindRun, "", nil)
				require.NoError(t, startErr)
				h.Done()
				id = h.ID()
				fc.Advance(constants.IdempotencyReplayWindow)
			}
			cfg := aliasLoadConfig(t)
			manager, err := prov_apps.NewProviderAppManager(cfg, prov_apps.WithJobsRegistry(registry))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, manager.Stop(context.Background())) })
			run := instance.NewInstance("warm", "fake", "local", 8300, 0, 0)
			run.StreamJobID = id
			run.MarkRunning(false, nil)
			require.NoError(t, manager.Instances().Register(run))
			executor := NewRunsExecutor(manager, func() string { return "node" }, func() *config.AppsConfig { return cfg }, nil, nil, nil)
			for _, path := range []string{"start", "load"} {
				bodyJSON := `{"provider":"fake","model_name":"local"}`
				if path == "start" {
					bodyJSON = `{"provider":"fake","model_name":"local","launch_mode":"native"}`
				}
				c, response := postJSON(t, bodyJSON)
				if path == "start" {
					executor.HandleInternalLaunchRun(c)
					require.Equal(t, http.StatusAccepted, response.Code, response.Body.String())
				} else {
					NewLoadExecutor(manager, emptyModelStore{}, func() string { return "node" }).HandleInternalLoadModel(c)
					require.Equal(t, http.StatusConflict, response.Code, response.Body.String())
				}
				var body map[string]any
				require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
				require.NotContains(t, body, "job_id", path)
				require.NotContains(t, body, "stream_job_id", "the response must not leak the instance's stale ID")
				require.Contains(t, body["message"], "zz_runs", path)
			}
		})
	}
}

func TestAutoDeployGoneJobRefusesWithOutcomeTool(t *testing.T) {
	for _, id := range []string{"", "dl_never-registered"} {
		t.Run(id, func(t *testing.T) {
			cfg := aliasLoadConfig(t)
			require.NoError(t, cfg.UpdateApp("fake", func(svc *config.ServiceConfig) error {
				svc.Search = &config.AppSearchConfig{Registry: constants.RepoHuggingFace}
				return nil
			}))
			executor := &RunsExecutor{
				jobs: newJobsForChainTest(t), appsConfig: func() *config.AppsConfig { return cfg },
				getNodename: func() string { return "node" },
				deploy: func(context.Context, *DeployRequest) (*Deployment, error) {
					return &Deployment{Nodes: []modelregistry.DeploymentNode{{Node: "node", JobID: id}}}, nil
				},
			}
			c, response := postJSON(t, `{}`)
			executor.handleAutoDeployLaunch(c, "fake", "", prov_apps.EnsureRequest{Model: "org/model"})
			require.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
			require.Contains(t, response.Body.String(), "zz_models")
			require.NotContains(t, response.Body.String(), `"job_id":`)
			require.Empty(t, executor.jobs.List(jobs.KindRun), "no unobservable chain may start")
		})
	}
}

func TestDownloadReuseGoneJobRefuses(t *testing.T) {
	e := &DeploymentsExecutor{jobs: newTestJobsRegistry(t), jobKeys: map[string]string{}}
	attempt := beginTestDownload(t, e, "shared")
	e.jobKeys["shared"] = "dl_never-registered"
	c, response := postJSON(t, `{}`)
	require.Nil(t, e.beginDownload(c, downloadAdmission{key: "shared", model: "llama3", registry: constants.RepoOllama}))
	require.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), "zz_models")
	require.NotContains(t, response.Body.String(), `"job_id":`)
	require.NoError(t, attempt.ctx.Err(), "refusal must not replace the physical transfer")
}

func TestSyncReuseRenewsOrRefusesGoneJob(t *testing.T) {
	for _, gone := range []bool{false, true} {
		t.Run(strconv.FormatBool(gone), func(t *testing.T) {
			fc := clocktest.NewFakeClock(time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC))
			registry, err := jobs.NewRegistry(jobs.Config{NodeName: "node", Clock: fc, JanitorInterval: 2 * constants.IdempotencyReplayWindow})
			require.NoError(t, err)
			t.Cleanup(registry.Stop)
			h, err := registry.StartDetached(jobs.KindSync, "", jobs.Meta{"source": "source"})
			require.NoError(t, err)
			h.Done()
			fc.Advance(23 * time.Hour)
			id := h.ID()
			if gone {
				id = "sync_never-registered"
			}
			e := NewSyncExecutor(func() *http.Client { return http.DefaultClient }, 9091, context.Background, nil, func() []*mesh.Endpoint { return nil }).WithJobs(registry)
			key := "org/model|gguf|" + deployPlanKey("", nil, nil)
			_, claimed := e.claimInflightDeploy(key, id)
			require.True(t, claimed)
			c, response := postJSON(t, `{"model":"org/model","format":"gguf","source_node":"source","source_url":"http://127.0.0.1:9090"}`)
			e.HandleSyncPull(c)
			if gone {
				require.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
				require.Contains(t, response.Body.String(), "zz_models")
				require.NotContains(t, response.Body.String(), `"existing_job_id":`)
			} else {
				require.Equal(t, http.StatusConflict, response.Code, response.Body.String())
				ev, getErr := registry.Get(id)
				require.NoError(t, getErr)
				require.Equal(t, fc.Now().Add(constants.IdempotencyReplayWindow), ev.ExpiresAt)
				var body map[string]any
				require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
				require.Equal(t, id, body["existing_job_id"])
			}
		})
	}
}

func TestSyncAcceptanceOmitsAbsentParentJob(t *testing.T) {
	source := httptest.NewTLSServer(http.NotFoundHandler())
	defer source.Close()
	u, err := url.Parse(source.URL)
	require.NoError(t, err)
	port, err := strconv.Atoi(u.Port())
	require.NoError(t, err)
	e := NewSyncExecutor(source.Client, port, context.Background, nil, func() []*mesh.Endpoint { return nil }).WithJobs(newTestJobsRegistry(t))
	c, response := postJSON(t, `{"model":"org/model","format":"gguf","source_node":"source","source_url":"`+source.URL+`"}`)
	e.HandleSyncPull(c)
	require.Equal(t, http.StatusAccepted, response.Code, response.Body.String())
	var body map[string]any
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	require.NotContains(t, body, "job_id")
	require.NotEmpty(t, body["local_job_id"])
	access := &jobAccess{registry: e.jobs}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	require.Error(t, access.Wait(ctx, body["local_job_id"].(string), ""), "the real source refuses the manifest")
}

func TestStillWarmingOmitsGoneJob(t *testing.T) {
	run := instance.NewInstance("run-1", "fake", "m", 8080, time.Minute, 0)
	wire := loadFailureWire(loadStillWarming("still loading").forRun(run, ""))
	response := httptest.NewRecorder()
	writeError(response, routedRequest(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)), wire)
	var body map[string]any
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	require.Equal(t, http.StatusServiceUnavailable, response.Code)
	require.NotContains(t, body, "job_id")
	require.Equal(t, run.ID, body["run_id"])
	require.Contains(t, response.Body.String(), "zz_runs")
}
