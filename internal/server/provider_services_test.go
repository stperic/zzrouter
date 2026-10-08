package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/host/service"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stretchr/testify/require"
)

type fakeProviderServices struct {
	status     prov_apps.ProviderServiceStatus
	actions    []string
	afterApply func()
}

func (f *fakeProviderServices) ProviderManagedBinary(string) string { return "" }
func (f *fakeProviderServices) ProviderServiceStatus(context.Context, string) (prov_apps.ProviderServiceStatus, error) {
	return f.status, nil
}
func (f *fakeProviderServices) ControlProviderService(_ context.Context, _, action string) (prov_apps.ProviderServiceStatus, error) {
	if !f.status.Managed {
		return f.status, prov_apps.ErrExternalService
	}
	f.actions = append(f.actions, action)
	f.status.Running, f.status.Desired = action != "stop", action != "stop"
	return f.status, nil
}
func (f *fakeProviderServices) ApplyProviderService(context.Context, string) (*service.ApplyResult, error) {
	if !f.status.Managed {
		return nil, prov_apps.ErrExternalService
	}
	f.actions = append(f.actions, "apply")
	if f.afterApply != nil {
		f.afterApply()
	}
	return &service.ApplyResult{Manager: "zzRouter", Applied: true, Restarted: true}, nil
}

type serviceReadyTransport struct{}

func (serviceReadyTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ready"))}, nil
}

func serviceExecutor(t *testing.T, managed bool) (*ParamsExecutor, *fakeProviderServices) {
	t.Helper()
	cfg := &pkgConfig.AppsConfig{}
	require.NoError(t, cfg.AddApp("generic-daemon", pkgConfig.ServiceConfig{
		Name: "generic-daemon", Protocol: pkgConfig.ProtocolOpenAI, Mode: "external",
		Runtime:      &pkgConfig.AppRuntimeConfig{Endpoint: "http://127.0.0.1:12345"},
		Capabilities: &pkgConfig.AppCapabilities{WireEndpoints: []string{"chat_completions"}},
	}))
	owner := &fakeProviderServices{status: prov_apps.ProviderServiceStatus{Provider: "generic-daemon", Managed: managed, Enabled: true, Supervisor: "external"}}
	if managed {
		owner.status.Supervisor = "zzRouter"
	}
	return NewParamsExecutor(func() *pkgConfig.AppsConfig { return cfg }, nil, func() string { return "worker" }, nil, nil, owner, nil,
		&http.Client{Transport: serviceReadyTransport{}}, nil, nil), owner
}

func callService(t *testing.T, handler gin.HandlerFunc) (int, map[string]any) {
	t.Helper()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/providers/generic-daemon/service/start", nil)
	c.Params = gin.Params{{Key: "name", Value: "generic-daemon"}}
	handler(c)
	var body map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	return recorder.Code, body
}

func TestManagedProviderServiceAPIDelegatesAllActions(t *testing.T) {
	executor, owner := serviceExecutor(t, true)
	code := 17
	owner.status.LastExit = &prov_apps.ProviderServiceExit{Reason: "provider exited", Detail: &instance.FailureInfo{ExitCode: &code, ErrorTail: []string{"RuntimeError: startup failed"}}}
	for _, action := range []string{"start", "stop", "restart"} {
		status, body := callService(t, executor.HandleInternalServiceControl(action))
		require.Equal(t, http.StatusOK, status)
		require.Equal(t, "zzRouter", body["supervisor"])
		require.Equal(t, "worker", body["node"])
		require.Equal(t, action != "stop", body["running"])
		exit := body["last_exit"].(map[string]any)
		require.Equal(t, "provider exited", exit["reason"])
		require.Equal(t, float64(17), exit["detail"].(map[string]any)["exit_code"])
	}
	status, body := callService(t, executor.HandleInternalApplyService)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, true, body["applied"])
	require.Equal(t, []string{"start", "stop", "restart", "apply"}, owner.actions)
}

type serviceFailingTransport struct{}

func (serviceFailingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(strings.NewReader("down"))}, nil
}

func TestServiceReadinessBoundsTheWholeWait(t *testing.T) {
	executor, _ := serviceExecutor(t, true)
	executor.httpClient = &http.Client{Transport: serviceFailingTransport{}}
	cfg, err := getAppConfig(executor.appsConfig(), "generic-daemon")
	require.NoError(t, err)
	finished := make(chan bool, 1)
	go func() { finished <- executor.waitForHealth(context.Background(), cfg, 10*time.Millisecond) }()
	select {
	case healthy := <-finished:
		require.False(t, healthy)
	case <-time.After(time.Second):
		t.Fatal("readiness wait exceeded its own deadline")
	}
}

func TestExternalProviderServiceAPIObservesButRefusesMutation(t *testing.T) {
	executor, owner := serviceExecutor(t, false)
	status, body := callService(t, executor.HandleInternalGetServiceStatus)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, "external", body["supervisor"])
	require.Equal(t, true, body["running"], "connected external daemons remain observable")
	require.Nil(t, body["manager"])
	for _, action := range []string{"start", "stop", "restart"} {
		status, body = callService(t, executor.HandleInternalServiceControl(action))
		require.Equal(t, http.StatusForbidden, status)
		require.Contains(t, body["detail"], "external")
	}
	status, body = callService(t, executor.HandleInternalApplyService)
	require.Equal(t, http.StatusForbidden, status)
	require.Contains(t, body["detail"], "external")
	require.Empty(t, owner.actions)
}

func TestServiceEnvResolvesTheNodeTier(t *testing.T) {
	executor, _ := serviceExecutor(t, true)
	cfg := &pkgConfig.ServiceConfig{
		Defaults: &pkgConfig.AppDefaultsConfig{Environment: map[string]string{"RUNTIME_THREADS": "2", "RUNTIME_CACHE": "1"}},
		Nodes:    map[string]pkgConfig.NodeSpec{"worker": {Environment: map[string]string{"RUNTIME_THREADS": "8"}}, "other": {Environment: map[string]string{"RUNTIME_THREADS": "99"}}},
	}
	require.Equal(t, map[string]string{"RUNTIME_THREADS": "8", "RUNTIME_CACHE": "1"}, executor.serviceEnv(cfg))
}

func TestServiceApplyReportsCancellationAfterMutation(t *testing.T) {
	executor, owner := serviceExecutor(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	owner.afterApply = cancel
	response := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(response)
	c.Params = gin.Params{{Key: "name", Value: "generic-daemon"}}
	c.Request = httptest.NewRequest(http.MethodPost, "/service/apply", nil).WithContext(ctx)
	executor.HandleInternalApplyService(c)
	require.Equal(t, http.StatusRequestTimeout, response.Code)
	require.Equal(t, []string{"apply"}, owner.actions)
}
