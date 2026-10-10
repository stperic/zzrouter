package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnsureCallersRefuseUncatalogedNameOnlyModel(t *testing.T) {
	checks := 0
	cfg := aliasLoadConfig(t)
	mgr, err := prov_apps.NewProviderAppManager(cfg, prov_apps.WithLocalCatalog(func(_ context.Context, weights string) (bool, error) {
		assert.Equal(t, "not-downloaded", weights)
		checks++
		return false, nil
	}))
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, mgr.Stop(context.Background())) })
	s := &Server{}
	s.providers.appMgr = mgr
	_, err = s.loadModelViaNodeAPI("not-downloaded", "fake", "chat")
	require.ErrorIs(t, err, prov_apps.ErrModelNotLocal)
	assert.Equal(t, http.StatusNotFound, answeredStatus(err))
	err = newChainServerDeps(s).StartOnDemand("not-downloaded", "fake")
	require.ErrorIs(t, err, prov_apps.ErrModelNotLocal)
	assert.Equal(t, http.StatusNotFound, answeredStatus(err))
	runs := &RunsExecutor{appMgr: mgr, appsConfig: func() *config.AppsConfig { return cfg }}
	c, rec := postJSON(t, `{"model_name":"not-downloaded","provider":"fake","launch_mode":"native"}`)
	runs.HandleInternalLaunchRun(c)
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	load := NewLoadExecutor(mgr, emptyModelStore{}, func() string { return "node" })
	c, rec = postJSON(t, `{"model_name":"not-downloaded","provider":"fake","force":true}`)
	load.HandleInternalLoadModel(c)
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	assert.Equal(t, 4, checks)
	registry := newJobsForChainTest(t)
	deploy, err := registry.StartDetached(jobs.KindInstall, "", nil)
	require.NoError(t, err)
	deploy.Done()
	chained, err := registry.StartDetached(jobs.KindRun, "", nil)
	require.NoError(t, err)
	runs.jobs = registry
	runs.waitDeployThenLaunch(chained, deploy.ID(), prov_apps.EnsureRequest{Model: "not-downloaded", Provider: "fake"})
	event, err := registry.Get(chained.ID())
	require.NoError(t, err)
	assert.Equal(t, jobs.PhaseFailed, event.Phase)
	assert.Equal(t, 5, checks)
	assert.Empty(t, mgr.Instances().ListAll())
}

func TestConcurrentInferenceAndChainEnsureOneRun(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix process fixture")
	}
	cfg := aliasLoadConfig(t)
	svc, _ := cfg.LookupApp("fake")
	engine := filepath.Join(t.TempDir(), "engine")
	require.NoError(t, os.WriteFile(engine, []byte("#!/bin/sh\nprintf 'READY\\n'\nexec /bin/sleep 10\n"), 0o700))
	svc.Runtime.Execution = config.ExecutionConfig{Type: "cli", Command: engine}
	svc.Runtime.HealthCheck.ReadinessProbe = &config.ProbeConfig{Timeout: "5s", LogPatterns: config.LogPatternsConfig{Success: []config.PatternConfig{{Pattern: "READY"}}}}
	require.NoError(t, cfg.UpdateApp("fake", func(dst *config.ServiceConfig) error { *dst = svc; return nil }))
	var starts atomic.Int32
	release, entered := make(chan struct{}), make(chan struct{})
	mgr, err := prov_apps.NewProviderAppManager(cfg,
		prov_apps.WithModelCanonicalizer(func(string) string { return "canonical" }),
		prov_apps.WithLocalCatalog(func(context.Context, string) (bool, error) { return true, nil }),
		prov_apps.WithModelSource(func(context.Context, string) string { starts.Add(1); close(entered); <-release; return "" }),
	)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, mgr.Stop(context.Background())) })
	s := &Server{}
	s.providers.appMgr = mgr
	type outcome struct {
		run *instance.Instance
		err error
	}
	loaded := make(chan outcome, 2)
	load := func(model string) {
		run, err := s.loadModelViaNodeAPI(model, "fake", "chat")
		loaded <- outcome{run, err}
	}
	go load("repo/model")
	<-entered
	go load("canonical")
	chainDone := make(chan error, 1)
	go func() { chainDone <- newChainServerDeps(s).StartOnDemand("canonical", "fake") }()
	select {
	case err := <-chainDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("chain waited for process preparation")
	}
	close(release)
	first, second := <-loaded, <-loaded
	require.NoError(t, first.err)
	require.NoError(t, second.err)
	require.Same(t, first.run, second.run)
	assert.Equal(t, int32(1), starts.Load())
	require.Len(t, mgr.Instances().ListAll(), 1)
}

func TestEnsureLaunchAndLoadShareAcceptedJob(t *testing.T) {
	registry := newJobsForChainTest(t)
	cfg := aliasLoadConfig(t)
	mgr, err := prov_apps.NewProviderAppManager(cfg, prov_apps.WithJobsRegistry(registry))
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, mgr.Stop(context.Background())) })
	runs := &RunsExecutor{appMgr: mgr, getNodename: func() string { return "node" }, appsConfig: func() *config.AppsConfig { return cfg }}
	c, rec := postJSON(t, `{"model_name":"local","provider":"fake","launch_mode":"native"}`)
	runs.HandleInternalLaunchRun(c)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	var launched struct {
		ID    string
		JobID string `json:"job_id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &launched))
	require.NotEmpty(t, launched.JobID)
	load := NewLoadExecutor(mgr, emptyModelStore{}, func() string { return "node" })
	c, rec = postJSON(t, `{"model_name":"local","provider":"fake"}`)
	load.HandleInternalLoadModel(c)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	var loaded LoadModelResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &loaded))
	assert.Equal(t, launched.ID, loaded.InstanceID)
	assert.Equal(t, launched.JobID, loaded.JobID)
	controller := NewRunsController(nil, NewLoadService(nil, &loadResultRouter{status: http.StatusConflict, body: rec.Body.String()}), nil, nil, nil)
	c, rec = postJSON(t, `{"model_name":"local","provider":"fake","node":"worker"}`)
	controller.LoadModel(c)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"success":true`)
	assert.Contains(t, rec.Body.String(), launched.ID)
	assert.Contains(t, rec.Body.String(), launched.JobID)
}

func TestEnsureServedLaunchAndLoadUseBackendWithoutPortPool(t *testing.T) {
	cfg := &config.AppsConfig{}
	require.NoError(t, cfg.AddApp("ollama", config.ServiceConfig{Enabled: new(true), Name: "ollama", Protocol: config.ProtocolOllama, Mode: "external", Runtime: &config.AppRuntimeConfig{Endpoint: "http://backend:11434"}, Capabilities: &config.AppCapabilities{WireEndpoints: []string{"chat_completions"}}}))
	calls := 0
	client := &http.Client{Transport: mcpRoundTripper(func(req *http.Request) (*http.Response, error) {
		calls++
		assert.Equal(t, "http://backend:11434/api/generate", req.URL.String())
		var body map[string]any
		require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
		if calls <= 2 {
			assert.Equal(t, "local", body["model"])
		} else {
			assert.Empty(t, body["model"])
		}
		assert.Equal(t, false, body["stream"])
		assert.Equal(t, "-1", body["keep_alive"])
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{}`)), Header: http.Header{}}, nil
	})}
	mgr, err := prov_apps.NewProviderAppManager(cfg, prov_apps.WithHTTPClient(client), prov_apps.WithLocalCatalog(func(context.Context, string) (bool, error) { return true, nil }), prov_apps.WithBackendResolver(func(string) (*backend.Resolved, bool) {
		return &backend.Resolved{Endpoint: "http://backend:11434"}, true
	}))
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, mgr.Stop(context.Background())) })
	runs := &RunsExecutor{appMgr: mgr, getNodename: func() string { return "node" }, appsConfig: func() *config.AppsConfig { return cfg }}
	c, rec := postJSON(t, `{"model_name":"local","provider":"ollama","launch_mode":"native","parameters":{"keep-alive":"-1"}}`)
	runs.HandleInternalLaunchRun(c)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var launched LaunchRunResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &launched))
	assert.True(t, launched.Accepted)
	assert.Equal(t, "native", launched.LaunchMode)
	assert.Equal(t, 11434, launched.Port)
	assert.Empty(t, launched.ID)
	assert.NotEmpty(t, launched.Message)
	response, err := NewLoadExecutor(mgr, emptyModelStore{}, func() string { return "node" }).LoadLocalModel(t.Context(), &LoadModelRequest{ModelName: "local", Provider: "ollama", Parameters: map[string]string{"keep-alive": "-1"}})
	require.NoError(t, err)
	assert.Equal(t, 11434, response.Port)
	assert.Empty(t, response.InstanceID)
	assert.Equal(t, "running", response.Status)
	utility := NewRunsUtilityService(mgr, func() *config.AppsConfig { return cfg })
	batch, failed, err := utility.BatchLaunch(t.Context(), []LaunchRequest{{Provider: "ollama", LaunchMode: "native", Parameters: map[string]string{"keep-alive": "-1"}}})
	require.NoError(t, err)
	assert.Empty(t, failed)
	require.Len(t, batch, 1)
	assert.NotContains(t, batch[0], "id")
	assert.Equal(t, 3, calls)
	assert.Empty(t, mgr.Instances().ListAll())
}

func TestRunsLoadProviderRefusalsUseFailureTable(t *testing.T) {
	for _, provider := range []string{"missing", "fake"} {
		t.Run(provider, func(t *testing.T) {
			cfg := aliasLoadConfig(t)
			status := http.StatusNotFound
			if provider == "fake" {
				status = http.StatusConflict
				require.NoError(t, cfg.UpdateApp("fake", func(svc *config.ServiceConfig) error { svc.Enabled = new(false); return nil }))
			}
			mgr, err := prov_apps.NewProviderAppManager(cfg)
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, mgr.Stop(context.Background())) })
			load := NewLoadExecutor(mgr, emptyModelStore{}, func() string { return "node" })
			c, internal := postJSON(t, `{"model_name":"local","provider":"`+provider+`"}`)
			load.HandleInternalLoadModel(c)
			require.Equal(t, status, internal.Code, internal.Body.String())
			ctrl := NewRunsController(nil, NewLoadService(nil, &loadResultRouter{status: internal.Code, body: internal.Body.String()}), nil, nil, nil)
			c, public := postJSON(t, `{"model_name":"local","provider":"`+provider+`","node":"worker"}`)
			ctrl.LoadModel(c)
			assert.Equal(t, status, public.Code, public.Body.String())
			assert.NotContains(t, public.Body.String(), `"success":true`)
		})
	}
}

func TestRunsLoadReusesRunningAndStartingWith409Success(t *testing.T) {
	for _, status := range []instance.Status{instance.StatusRunning, instance.StatusStarting} {
		t.Run(string(status), func(t *testing.T) {
			mgr, err := prov_apps.NewProviderAppManager(aliasLoadConfig(t), prov_apps.WithLocalCatalog(func(context.Context, string) (bool, error) { t.Error("reuse queried catalog"); return false, nil }))
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, mgr.Stop(context.Background())) })
			run := instance.NewInstance("warm", "fake", "local", 8300, 0, 0)
			run.StreamJobID = "existing-job"
			run.SetStatus(status)
			require.NoError(t, mgr.Instances().Register(run))
			c, internal := postJSON(t, `{"model_name":"local","provider":"fake"}`)
			NewLoadExecutor(mgr, emptyModelStore{}, func() string { return "node" }).HandleInternalLoadModel(c)
			require.Equal(t, http.StatusConflict, internal.Code, internal.Body.String())
			ctrl := NewRunsController(nil, NewLoadService(nil, &loadResultRouter{status: internal.Code, body: internal.Body.String()}), nil, nil, nil)
			c, public := postJSON(t, `{"model_name":"local","provider":"fake","node":"worker"}`)
			ctrl.LoadModel(c)
			require.Equal(t, http.StatusConflict, public.Code, public.Body.String())
			var response struct {
				Success bool
				Data    LoadModelResponse
			}
			require.NoError(t, json.Unmarshal(public.Body.Bytes(), &response))
			assert.True(t, response.Success)
			assert.Equal(t, run.ID, response.Data.InstanceID)
			assert.Equal(t, run.StreamJobID, response.Data.JobID)
			assert.Equal(t, run.Port, response.Data.Port)
			assert.Equal(t, string(status), response.Data.Status)
			assert.Equal(t, "worker", response.Data.Node)
		})
	}
}

func TestLoadProviderAutodetectResolvesVariant(t *testing.T) {
	cfg := aliasLoadConfig(t)
	const variant = "base+long"
	require.NoError(t, cfg.UpdateApp("fake", func(svc *config.ServiceConfig) error {
		svc.Models = map[string]config.ModelSpec{variant: {From: "base"}}
		return nil
	}))
	mgr, err := prov_apps.NewProviderAppManager(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, mgr.Stop(context.Background())) })
	run := instance.NewInstance("variant", "fake", variant, 8300, 0, 0)
	run.MarkRunning()
	require.NoError(t, mgr.Instances().Register(run))
	response, err := NewLoadExecutor(mgr, emptyModelStore{}, func() string { return "node" }).LoadLocalModel(t.Context(), &LoadModelRequest{ModelName: variant})
	require.NoError(t, err)
	assert.Equal(t, "fake", response.Provider)
	assert.Equal(t, run.ID, response.InstanceID)
	assert.True(t, response.Reused)
}

func TestChainColdStartReturnsBeforeProcessPreparation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})
		mgr, err := prov_apps.NewProviderAppManager(aliasLoadConfig(t), prov_apps.WithModelSource(func(context.Context, string) string { close(entered); <-release; return "" }))
		require.NoError(t, err)
		t.Cleanup(func() { assert.NoError(t, mgr.Stop(context.Background())) })
		s := &Server{}
		s.providers.appMgr = mgr
		require.NoError(t, newChainServerDeps(s).StartOnDemand("cold", "fake"))
		<-entered
		close(release)
	})
}

func TestEnsureServedJoinRefusesWithoutRealRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := &config.AppsConfig{}
		for _, provider := range []string{"ollama", "other"} {
			require.NoError(t, cfg.AddApp(provider, config.ServiceConfig{Enabled: new(true), Name: provider, Protocol: config.ProtocolOllama, Mode: "external", Runtime: &config.AppRuntimeConfig{Endpoint: "http://backend:11434"}, Capabilities: &config.AppCapabilities{WireEndpoints: []string{"chat_completions"}}}))
		}
		release := make(chan struct{})
		calls := 0
		client := &http.Client{Transport: mcpRoundTripper(func(*http.Request) (*http.Response, error) {
			calls++
			<-release
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{}`)), Header: http.Header{}}, nil
		})}
		mgr, err := prov_apps.NewProviderAppManager(cfg, prov_apps.WithHTTPClient(client), prov_apps.WithBackendResolver(func(string) (*backend.Resolved, bool) {
			return &backend.Resolved{Endpoint: "http://backend:11434"}, true
		}))
		require.NoError(t, err)
		t.Cleanup(func() { assert.NoError(t, mgr.Stop(context.Background())) })
		leader, joined := make(chan error, 1), make(chan error, 1)
		go func() {
			_, err := mgr.EnsureRun(t.Context(), prov_apps.EnsureRequest{Provider: "ollama", Model: "local"})
			leader <- err
		}()
		synctest.Wait()
		go func() {
			_, err := mgr.EnsureRun(t.Context(), prov_apps.EnsureRequest{Provider: "other", Model: "local"})
			joined <- err
		}()
		synctest.Wait()
		close(release)
		require.NoError(t, <-leader)
		require.ErrorIs(t, <-joined, prov_apps.ErrInstanceNotFound)
		assert.Equal(t, 1, calls)
		assert.Empty(t, mgr.Instances().ListAll())
	})
}

func TestRunsLoadStoppingRunRefusesWithRetryAfter(t *testing.T) {
	mgr, err := prov_apps.NewProviderAppManager(aliasLoadConfig(t), prov_apps.WithLocalCatalog(func(context.Context, string) (bool, error) {
		t.Error("stopping refusal must not query the catalog")
		return true, nil
	}))
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, mgr.Stop(context.Background())) })
	run := instance.NewInstance("stopping", "fake", "local", 8300, 0, 0)
	run.SetStatus(instance.StatusStopping)
	require.NoError(t, mgr.Instances().Register(run))
	for _, force := range []string{"false", "true"} {
		c, response := postJSON(t, `{"model_name":"local","provider":"fake","force":`+force+`}`)
		NewLoadExecutor(mgr, emptyModelStore{}, func() string { return "node" }).HandleInternalLoadModel(c)
		require.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
		assert.Equal(t, "5", response.Header().Get("Retry-After"))
		assert.Contains(t, response.Body.String(), "run is stopping")
		assert.Contains(t, response.Body.String(), "retry shortly")
		assert.NotContains(t, response.Body.String(), `"success":true`)
		ctrl := NewRunsController(nil, NewLoadService(nil, &loadResultRouter{status: response.Code, body: response.Body.String(), headers: response.Header()}), nil, nil, nil)
		c, public := postJSON(t, `{"model_name":"local","provider":"fake","node":"worker"}`)
		ctrl.LoadModel(c)
		require.Equal(t, http.StatusServiceUnavailable, public.Code, public.Body.String())
		assert.Equal(t, "5", public.Header().Get("Retry-After"))
		assert.Contains(t, public.Body.String(), "run is stopping")
		c, ollama := postJSON(t, `{}`)
		RespondToErrorOllama(c, parseRoutedError(response.Code, response.Body.Bytes(), response.Header()))
		assert.Equal(t, http.StatusServiceUnavailable, ollama.Code)
		assert.Equal(t, "5", ollama.Header().Get("Retry-After"))
	}
}
