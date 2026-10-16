package prov_apps

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stperic/zzrouter/pkg/prov_apps/health"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
	"github.com/stretchr/testify/require"
)

func mlxEndpointFixture(t *testing.T, installed bool) (*ProviderAppManager, config.ServiceConfig, string) {
	t.Helper()
	m, _, dir := featureLaunchFixture(t)
	isolateProviderRoot(t)
	providers := t.TempDir()
	require.NoError(t, templates.InstallDefaults(providers))
	cfg, err := config.LoadAppsConfig(providers)
	require.NoError(t, err)
	svc, _ := cfg.LookupApp("mlx")
	svc.Enabled, svc.Platforms = new(true), nil
	require.NoError(t, cfg.UpdateApp("mlx", func(out *config.ServiceConfig) error { *out = svc; return nil }))
	m.ReloadConfig(cfg)
	fake := newFakeInstaller("mlx-vlm")
	fake.installed.Store(installed)
	m.installs.dispatcher.Register("mlx-vlm", fake)
	if installed {
		python := fsroot.ProviderVenvPython("mlx-vlm")
		require.NoError(t, os.MkdirAll(filepath.Dir(python), 0700))
		require.NoError(t, os.WriteFile(python, nil, 0700))
	}
	return m, svc, dir
}

func TestMLXEndpointRuntimeRequired(t *testing.T) {
	m, svc, _ := mlxEndpointFixture(t, false)
	for _, endpoint := range []Endpoint{EndpointEmbeddings, EndpointReranking} {
		req := EnsureRequest{Provider: "mlx", Model: "vendor/model", Endpoint: endpoint}
		_, _, _, err := m.featureRuntime(svc, process.ResolveModel(svc, req.Model), "", string(endpoint))
		require.ErrorIs(t, err, ErrParameterValidation)
		_, err = m.EnsureRun(t.Context(), req)
		require.ErrorIs(t, err, ErrParameterValidation)
		require.ErrorContains(t, err, "mlx-vlm")
		require.ErrorContains(t, err, `POST /zzrouter/v1/providers/mlx/install {"runtime": "mlx-vlm"}`)
		_, err = m.PreviewLaunch(t.Context(), req, 8090)
		require.ErrorIs(t, err, ErrParameterValidation)
	}
	require.Empty(t, m.Instances().List())
}

func TestMLXEndpointLaunchAndCatalog(t *testing.T) {
	m, svc, dir := mlxEndpointFixture(t, true)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"vision_config":{}}`), 0600))
	for _, tc := range []struct {
		endpoint   Endpoint
		flag, wire string
	}{
		{EndpointEmbeddings, "--embedding-model", "embeddings"},
		{EndpointReranking, "--reranker-model", "rerank"},
	} {
		t.Run(string(tc.endpoint), func(t *testing.T) {
			req := EnsureRequest{Provider: "mlx", Model: "vendor/model", Endpoint: tc.endpoint, Parameters: map[string]string{"chat-template": "x", "temperature": "0.5", "top-p": "0.9", "max-tokens": "10"}}
			prepared, err := m.prepareStart(t.Context(), req)
			require.NoError(t, err)
			require.Equal(t, tc.endpoint, prepared.req.Endpoint)
			selected, err := m.resolveLaunch(&prepared.req)
			require.NoError(t, err)
			launch, err := processEngine{m}.prepare(t.Context(), svc, "mlx", prepared.req, 8090)
			require.NoError(t, err)
			require.Equal(t, []string{"-m", "mlx_vlm.server", "--host", "127.0.0.1", "--port", "8090", tc.flag, launch.launch.ModelPath}, launch.launch.Args)
			require.Equal(t, launch.launch.ModelPath, launch.launch.WireModel)
			require.Equal(t, []string{tc.wire}, launch.resolved.WireEndpoints)
			for key := range req.Parameters {
				require.NotContains(t, launch.parameters.Params, key)
			}
			run, err := m.newRun(prepared.req, selected.config, 8090)
			require.NoError(t, err)
			run.SetResolved(launch.resolved)
			run.MarkRunning(false, nil)
			require.NoError(t, m.instances.Register(run))
			defer func() { require.NoError(t, m.instances.Remove(run.ID)) }()
			chatReq := EnsureRequest{Provider: "mlx", Model: req.Model, Endpoint: EndpointChat}
			chat, err := m.newRun(chatReq, svc, 8091)
			require.NoError(t, err)
			chat.MarkRunning(false, nil)
			chat.SetResolved(instance.Resolved{WireEndpoints: []string{"chat_completions", "messages", "responses"}})
			require.NoError(t, m.instances.Register(chat))
			defer func() {
				if m.instances.Has(chat.ID) {
					require.NoError(t, m.instances.Remove(chat.ID))
				}
			}()
			require.NotEqual(t, chat.ID, run.ID)
			got, found := m.GetInstanceByCanonicalModel("mlx", req.Model, tc.endpoint)
			require.True(t, found)
			require.Same(t, run, got)
			details := m.ModelFeatureDetails("mlx", req.Model, nil)
			require.Equal(t, []string{"chat_completions", "messages", "responses"}, details["wire_endpoints"])
			states := details["features"].(map[string]string)
			require.Equal(t, "present", states["embeddings"])
			require.Equal(t, "present", states["reranking"])
			require.NoError(t, m.instances.Remove(chat.ID))
			details = m.ModelFeatureDetails("mlx", req.Model, nil)
			require.Contains(t, details["wire_endpoints"], "messages_compat")
			require.Contains(t, details["wire_endpoints"], "responses")
		})
	}
	require.NoError(t, m.EnsureModelFeatures(context.Background(), "mlx", dir, []string{"embeddings", "reranking"}, nil, nil))
}

func TestMLXVisionReadinessOverlay(t *testing.T) {
	m, _, dir := mlxEndpointFixture(t, true)
	cfg := m.AppsConfig()
	require.NoError(t, cfg.UpdateApp("mlx", func(svc *config.ServiceConfig) error { svc.Runtime.HealthCheck.Path = "/base-only"; return nil }))
	m.ReloadConfig(cfg)
	svc, _ := cfg.LookupApp("mlx")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"vision_config":{}}`), 0600))
	selected, _, excluded, err := m.featureRuntime(svc, process.ResolveModel(svc, "vendor/model"), "", "chat")
	require.NoError(t, err)
	require.Contains(t, excluded, "temperature")
	require.Contains(t, excluded, "top-p")
	req := EnsureRequest{Provider: "mlx", Model: "vendor/model", Endpoint: EndpointChat}
	selection, err := m.resolveLaunch(&req)
	require.NoError(t, err)
	run, err := m.newRun(req, selection.config, 8090)
	require.NoError(t, err)
	require.Contains(t, run.HealthURL, "/v1/models")
	probe := buildHealthCheckConfig(selected, 8090).ReadinessProbe
	require.Equal(t, 600*time.Second, probe.Timeout)
	requests := make(chan *http.Request, 1)
	monitor := health.NewMonitor(nil, health.MonitorConfig{HTTPClient: &http.Client{Transport: readinessTransport(func(req *http.Request) (*http.Response, error) {
		requests <- req
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})}})
	inst, writer, done, _ := readinessWatch(t, probe, monitor)
	_, err = writer.Write([]byte("Application startup complete\n"))
	require.NoError(t, err)
	waitReadiness(t, done, time.Second)
	require.Equal(t, instance.StatusRunning, inst.GetStatus())
	require.Equal(t, "/v1/chat/completions", (<-requests).URL.Path)
	require.Equal(t, "INFO - Starting httpd", svc.Runtime.HealthCheck.ReadinessProbe.LogPatterns.Success[0].Pattern)
	changed := selected
	runtimeCopy := *selected.Runtime
	runtimeCopy.HealthCheck.Path = "/another-health-path"
	changed.Runtime = &runtimeCopy
	require.Equal(t, executionDigest(selected, "mlx-vlm", "python3"), executionDigest(changed, "mlx-vlm", "python3"))
}

func TestSingleEndpointProviderKeepsRequestedShape(t *testing.T) {
	r := newContractRig(t, false)
	cfg := r.m.AppsConfig()
	require.NoError(t, cfg.UpdateApp("generic", func(svc *config.ServiceConfig) error {
		svc.Capabilities.WireEndpoints = []string{"embeddings"}
		return nil
	}))
	r.m.ReloadConfig(cfg)
	req := EnsureRequest{Provider: "generic", Model: "m", Endpoint: EndpointEmbeddings}
	preview, err := r.m.PreviewLaunch(t.Context(), req, 8740)
	require.NoError(t, err)
	require.Equal(t, []string{"embeddings"}, preview.Config.Capabilities.WireEndpoints)
	prepared, err := r.m.prepareStart(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, EndpointEmbeddings, prepared.req.Endpoint)
	require.Equal(t, "chat", prepared.req.Key().Endpoint)
	_, err = r.m.resolveLaunch(&prepared.req)
	require.NoError(t, err)
	result, err := r.m.EnsureRun(t.Context(), req)
	require.NoError(t, err)
	require.NotNil(t, result.Run)
	require.Equal(t, "chat", result.Run.Endpoint)
	found, ok := r.m.GetInstanceByCanonicalModel("generic", "m", EndpointEmbeddings)
	require.True(t, ok)
	require.Same(t, result.Run, found)
	// A read by the default endpoint still sees the run: lookups key runs, only launches admit endpoints.
	found, ok = r.m.GetInstanceByCanonicalModel("", "m", EndpointChat)
	require.True(t, ok)
	require.Same(t, result.Run, found)
	_, ok = r.m.GetInstanceByCanonicalModel("generic", "m", EndpointReranking)
	require.False(t, ok, "an unserved endpoint keys apart and never finds the shared run")
	require.Equal(t, EndpointEmbeddings, restartRequest(result.Run).Endpoint)
	restarted, err := r.m.RestartInstance(t.Context(), result.Run.ID)
	require.NoError(t, err)
	require.Equal(t, "chat", restarted.Endpoint)
	require.Equal(t, "embeddings", restarted.SnapshotConfig().Endpoint)
	require.Equal(t, instance.ParametersCurrent, r.m.ParametersStatus(t.Context(), restarted).State)
}
