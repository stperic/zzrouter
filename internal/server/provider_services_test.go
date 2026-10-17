package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	status  prov_apps.ProviderServiceStatus
	actions []string
	refuse  error
	healthy bool
	jobID   string // the readiness job a start, restart or apply opens; "" when none could be
}

func (f *fakeProviderServices) ProviderManagedBinary(string) string { return "" }
func (f *fakeProviderServices) RunningModels(string) []string       { return nil }
func (f *fakeProviderServices) ProviderEndpointHealthy(context.Context, string) bool {
	return f.healthy
}
func (f *fakeProviderServices) ProviderServiceStatus(context.Context, string) (prov_apps.ProviderServiceStatus, error) {
	return f.status, nil
}
func (f *fakeProviderServices) ControlProviderService(_ context.Context, _, action string) (prov_apps.ProviderServiceStatus, error) {
	if err := f.refusal(); err != nil {
		return f.status, err
	}
	f.actions = append(f.actions, action)
	f.status.Running, f.status.Desired, f.status.JobID = action != "stop", action != "stop", ""
	if action != "stop" {
		f.status.JobID = f.jobID
	}
	return f.status, nil
}
func (f *fakeProviderServices) ApplyProviderService(context.Context, string) (*service.ApplyResult, string, error) {
	if err := f.refusal(); err != nil {
		return nil, "", err
	}
	f.actions = append(f.actions, "apply")
	return &service.ApplyResult{Manager: "zzRouter", Applied: true, Restarted: true}, f.jobID, nil
}

func (f *fakeProviderServices) refusal() error {
	if !f.status.Managed {
		return prov_apps.ErrExternalService
	}
	return f.refuse
}

func serviceExecutor(t *testing.T, managed bool) (*ParamsExecutor, *fakeProviderServices) {
	t.Helper()
	cfg := &pkgConfig.AppsConfig{}
	require.NoError(t, cfg.AddApp("generic-daemon", pkgConfig.ServiceConfig{
		Name: "generic-daemon", Protocol: pkgConfig.ProtocolOpenAI, Mode: "external",
		Runtime:      &pkgConfig.AppRuntimeConfig{Endpoint: "http://127.0.0.1:12345"},
		Capabilities: &pkgConfig.AppCapabilities{WireEndpoints: []string{"chat_completions"}},
	}))
	owner := &fakeProviderServices{status: prov_apps.ProviderServiceStatus{Provider: "generic-daemon", Managed: managed, Enabled: true, Supervisor: "external"}, healthy: true, jobID: "svc_0123456789abcdef"}
	if managed {
		owner.status.Supervisor = "zzRouter"
	}
	return NewParamsExecutor(func() *pkgConfig.AppsConfig { return cfg }, nil, func() string { return "worker" }, nil, nil, owner, nil, nil), owner
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

// Start and restart answer with the readiness job; stop and the status read
// answer with supervision, including the last exit.
func TestManagedProviderServiceAPIDelegatesAllActions(t *testing.T) {
	executor, owner := serviceExecutor(t, true)
	code := 17
	owner.status.LastExit = &prov_apps.ProviderServiceExit{Reason: "provider exited", Detail: &instance.FailureInfo{ExitCode: &code, ErrorTail: []string{"RuntimeError: startup failed"}}}
	for _, action := range []string{"start", "restart"} {
		status, body := callService(t, executor.HandleInternalServiceControl(action))
		require.Equal(t, http.StatusAccepted, status)
		require.Equal(t, "svc_0123456789abcdef", body["job_id"])
		require.Equal(t, "/zzrouter/v1/jobs/svc_0123456789abcdef", body["job_url"])
		require.Equal(t, "worker", body["node"])
	}
	status, body := callService(t, executor.HandleInternalServiceControl("stop"))
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, "zzRouter", body["supervisor"])
	require.Equal(t, false, body["running"])
	require.Nil(t, body["job_id"])
	status, body = callService(t, executor.HandleInternalGetServiceStatus)
	require.Equal(t, http.StatusOK, status)
	exit := body["last_exit"].(map[string]any)
	require.Equal(t, "provider exited", exit["reason"])
	require.Equal(t, float64(17), exit["detail"].(map[string]any)["exit_code"])
	status, body = callService(t, executor.HandleInternalApplyService)
	require.Equal(t, http.StatusAccepted, status)
	require.Equal(t, true, body["applied"])
	require.Equal(t, "svc_0123456789abcdef", body["job_id"])
	require.Equal(t, []string{"start", "restart", "stop", "apply"}, owner.actions)
}

// A start answers once the control returns: readiness is the job's, so the
// coordinator's 30 s forward deadline never races an endpoint wait.
func TestServiceStartAnswersBeforeTheEndpointIsReady(t *testing.T) {
	executor, owner := serviceExecutor(t, true)
	owner.healthy = false
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/providers/generic-daemon/service/start", nil)
	c.Params = gin.Params{{Key: "name", Value: "generic-daemon"}}
	answered := make(chan struct{})
	go func() { executor.HandleInternalServiceControl("start")(c); close(answered) }()
	select {
	case <-answered:
	case <-time.After(2 * time.Second):
		t.Fatal("start waited for an endpoint that never answers")
	}
	require.Equal(t, http.StatusAccepted, recorder.Code)
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

// A node that could open no readiness job answers with what it observes, not a
// 202 whose job URLs lead nowhere.
func TestServiceControlWithoutAJobAnswersWithTheStatus(t *testing.T) {
	executor, owner := serviceExecutor(t, true)
	owner.jobID = ""
	for _, handler := range []gin.HandlerFunc{executor.HandleInternalServiceControl("start"), executor.HandleInternalServiceControl("restart")} {
		status, body := callService(t, handler)
		require.Equal(t, http.StatusOK, status)
		require.Equal(t, "zzRouter", body["supervisor"])
		require.Equal(t, true, body["healthy"])
		require.Nil(t, body["job_url"])
	}
	status, body := callService(t, executor.HandleInternalApplyService)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, true, body["restarted"])
	require.Nil(t, body["job_url"])
}

func TestDisabledProviderServiceRefusesStartWithoutAJob(t *testing.T) {
	executor, owner := serviceExecutor(t, true)
	owner.refuse = prov_apps.ErrServiceDisabled
	for _, handler := range []gin.HandlerFunc{executor.HandleInternalServiceControl("start"), executor.HandleInternalServiceControl("restart"), executor.HandleInternalApplyService} {
		status, body := callService(t, handler)
		require.Equal(t, http.StatusConflict, status)
		require.Nil(t, body["job_id"])
	}
}

func TestServiceEnvResolvesTheNodeTier(t *testing.T) {
	executor, _ := serviceExecutor(t, true)
	cfg := &pkgConfig.ServiceConfig{
		Defaults: &pkgConfig.AppDefaultsConfig{Environment: map[string]string{"RUNTIME_THREADS": "2", "RUNTIME_CACHE": "1"}},
		Nodes:    map[string]pkgConfig.NodeSpec{"worker": {Environment: map[string]string{"RUNTIME_THREADS": "8"}}, "other": {Environment: map[string]string{"RUNTIME_THREADS": "99"}}},
	}
	require.Equal(t, map[string]string{"RUNTIME_THREADS": "8", "RUNTIME_CACHE": "1"}, executor.serviceEnv(cfg))
}
