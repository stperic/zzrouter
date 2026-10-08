package prov_apps

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/host"
	"github.com/stperic/zzrouter/pkg/host/service"
	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
	"github.com/stretchr/testify/require"
)

type serviceProbeTransport func(*http.Request) (*http.Response, error)

func (t serviceProbeTransport) RoundTrip(req *http.Request) (*http.Response, error) { return t(req) }

type testDaemonInstaller struct {
	install.ProviderInstaller
	binary  string
	args    []string
	managed bool
}

func (i *testDaemonInstaller) ManagedBinary() string {
	if i.managed {
		return i.binary
	}
	return ""
}

func (i *testDaemonInstaller) DaemonCommand() (string, []string) { return i.binary, i.args }

func TestProviderDaemonHelper(t *testing.T) {
	if os.Getenv("PROVIDER_DAEMON_HELPER") != "1" {
		return
	}
	if os.Getenv("PROVIDER_DAEMON_CRASH") == "1" {
		fmt.Fprintln(os.Stderr, "RuntimeError: managed daemon failed; token=sk-123456789012345678901234567890123456")
		os.Exit(17)
	}
	select {}
}

func newServiceTestManager(t *testing.T, managed, crash bool) *ProviderAppManager {
	t.Helper()
	t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
	isolateProviderRoot(t)
	cfg := testAppsConfig()
	svc, _ := cfg.LookupApp("ollama")
	svc.Service = nil
	svc.Defaults = &config.AppDefaultsConfig{Environment: map[string]string{"PROVIDER_DAEMON_HELPER": "1"}}
	if crash {
		svc.Defaults.Environment["PROVIDER_DAEMON_CRASH"] = "1"
	}
	require.NoError(t, cfg.AddApp("generic-daemon", svc))
	client := &http.Client{Transport: serviceProbeTransport(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("test endpoint is not listening")
	})}
	m, err := NewProviderAppManager(cfg, WithHTTPClient(client))
	require.NoError(t, err)
	binary, err := os.Executable()
	require.NoError(t, err)
	m.installs.Dispatcher().Register("generic-daemon", &testDaemonInstaller{
		binary: binary, args: []string{"-test.run=^TestProviderDaemonHelper$"}, managed: managed,
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, m.Stop(ctx))
	})
	return m
}

func serviceStatus(t *testing.T, m *ProviderAppManager) ProviderServiceStatus {
	t.Helper()
	status, err := m.ProviderServiceStatus(context.Background(), "generic-daemon")
	require.NoError(t, err)
	return status
}

func TestManagedServiceBootStopRestartAndShutdown(t *testing.T) {
	m := newServiceTestManager(t, true, false)
	m.Start(context.Background())
	require.Eventually(t, func() bool { return serviceStatus(t, m).PID > 0 }, 5*time.Second, 10*time.Millisecond)
	first := serviceStatus(t, m)
	require.Equal(t, "zzRouter", first.Supervisor)
	require.True(t, first.Managed)
	_, err := m.ControlProviderService(context.Background(), "generic-daemon", "stop")
	require.NoError(t, err)
	require.False(t, serviceStatus(t, m).Running)
	m.reconcileServices()
	require.False(t, serviceStatus(t, m).Desired, "an unrelated config reload must preserve a manual stop")
	_, err = m.ControlProviderService(context.Background(), "generic-daemon", "start")
	require.NoError(t, err)
	require.Eventually(t, func() bool { return serviceStatus(t, m).PID > 0 }, 5*time.Second, 10*time.Millisecond)
	require.NotEqual(t, first.PID, serviceStatus(t, m).PID)
	require.NoError(t, m.Stop(context.Background()))
	require.False(t, serviceStatus(t, m).Running)
}

func TestManagedServiceCrashRecordsExitAndBacksOff(t *testing.T) {
	m := newServiceTestManager(t, true, true)
	m.Start(context.Background())
	require.Eventually(t, func() bool { return serviceStatus(t, m).LastExit != nil }, 5*time.Second, 10*time.Millisecond)
	status := serviceStatus(t, m)
	require.Equal(t, 17, *status.LastExit.Detail.ExitCode)
	require.Contains(t, status.LastExit.Reason, "managed daemon failed")
	require.NotContains(t, status.LastExit.Reason, "sk-123456")
	require.Equal(t, "1s", status.Backoff)
	require.Eventually(t, func() bool { return serviceStatus(t, m).Backoff == "2s" }, 5*time.Second, 10*time.Millisecond)
	_, err := m.ControlProviderService(context.Background(), "generic-daemon", "stop")
	require.NoError(t, err)
	require.False(t, serviceStatus(t, m).Desired)
}

func TestExternalServiceNeverStartsOrStops(t *testing.T) {
	m := newServiceTestManager(t, false, false)
	m.Start(context.Background())
	for _, action := range []string{"start", "stop", "restart"} {
		_, err := m.ControlProviderService(context.Background(), "generic-daemon", action)
		require.ErrorIs(t, err, ErrExternalService)
	}
	_, err := m.ApplyProviderService(context.Background(), "generic-daemon")
	require.ErrorIs(t, err, ErrExternalService)
	status := serviceStatus(t, m)
	require.Equal(t, "external", status.Supervisor)
	require.False(t, status.Managed)
	require.Zero(t, status.PID)
}

func TestExplicitUnavailableServiceDoesNotFallBack(t *testing.T) {
	cfg := config.ServiceConfig{Service: &config.ServiceManagement{Manager: "not-a-manager"}}
	manager, err := serviceManager(context.Background(), "generic-daemon", cfg)
	require.Error(t, err)
	require.Nil(t, manager)
}

func TestManagedServiceConfigRemovalStopsSupervision(t *testing.T) {
	for _, change := range []string{"provider", "endpoint", "nil"} {
		t.Run(change, func(t *testing.T) {
			m := newServiceTestManager(t, true, false)
			m.Start(context.Background())
			require.Eventually(t, func() bool { return serviceStatus(t, m).Running }, 5*time.Second, 10*time.Millisecond)
			fresh := testAppsConfig()
			if change == "endpoint" {
				svc, _ := fresh.LookupApp("vllm")
				require.NoError(t, fresh.AddApp("generic-daemon", svc))
			}
			if change == "nil" {
				fresh = nil
			}
			m.ReloadConfig(fresh)
			state := m.serviceState("generic-daemon")
			state.mu.Lock()
			defer state.mu.Unlock()
			require.Nil(t, state.done)
			require.False(t, state.status.Desired)
		})
	}
}

type testOSService struct {
	service.Manager
	name             string
	running          bool
	starts           int
	observationError error
}

func (s *testOSService) Name() string { return s.name }
func (s *testOSService) Status(string) service.ServiceStatus {
	return service.ServiceStatus{Manager: s.name, Running: s.running, ObservationError: s.observationError}
}

func TestFailedOSProbeDoesNotInventAStopOrOverwriteExitEvidence(t *testing.T) {
	m := newServiceTestManager(t, true, false)
	manager := &testOSService{name: "systemd", running: true}
	state := m.serviceState("generic-daemon")
	state.manager = manager
	state.status.LastExit = &ProviderServiceExit{Reason: "earlier real exit"}
	calls := 0
	m.catalogChanged = func() { calls++ }
	require.True(t, serviceStatus(t, m).Running)
	manager.running, manager.observationError = false, errors.New("service bus is not accessible")
	_, err := m.ProviderServiceStatus(t.Context(), "generic-daemon")
	require.ErrorContains(t, err, "service state is unknown")
	require.Zero(t, calls)
	require.True(t, *state.observedRunning)
	require.Equal(t, "earlier real exit", state.status.LastExit.Reason)
	manager.observationError = nil
	require.False(t, serviceStatus(t, m).Running)
	require.Equal(t, 1, calls, "a later successful stop observation remains actionable")
}
func (s *testOSService) Control(_ context.Context, _, action string) error {
	s.running = action != "stop"
	if s.running {
		s.starts++
	}
	return nil
}

func (s *testOSService) ApplyEnvContext(context.Context, string, map[string]string) *service.ApplyResult {
	return &service.ApplyResult{Manager: s.name, Applied: true, Restarted: true}
}

func TestOSServiceManualStopPreservesSupervisor(t *testing.T) {
	for _, name := range []string{"systemd", "launchd"} {
		t.Run(name, func(t *testing.T) {
			m := newServiceTestManager(t, true, false)
			manager := &testOSService{name: name, running: true}
			m.serviceState("generic-daemon").manager = manager
			m.Start(context.Background())
			require.Equal(t, 1, manager.starts)
			_, err := m.ControlProviderService(context.Background(), "generic-daemon", "stop")
			require.NoError(t, err)
			m.reconcileServices()
			require.Equal(t, 1, manager.starts)
			status := serviceStatus(t, m)
			require.Equal(t, name, status.Supervisor)
			require.False(t, status.Running)
			_, err = m.ControlProviderService(context.Background(), "generic-daemon", "start")
			require.NoError(t, err)
			require.Equal(t, 2, manager.starts)
		})
	}
}

func TestServiceControlWaitHonorsCancellation(t *testing.T) {
	m := newServiceTestManager(t, true, false)
	unlock, err := m.installs.acquireProvider(context.Background(), "generic-daemon")
	require.NoError(t, err)
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err = m.ControlProviderService(ctx, "generic-daemon", "start")
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestOccupiedExternalEndpointIsNeverTakenOver(t *testing.T) {
	m := newServiceTestManager(t, true, false)
	m.httpClient = &http.Client{Transport: serviceProbeTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("external"))}, nil
	})}
	_, err := m.ControlProviderService(context.Background(), "generic-daemon", "start")
	require.ErrorContains(t, err, "outside this supervisor")
	require.False(t, serviceStatus(t, m).Running)
	require.False(t, serviceStatus(t, m).Desired)
}

func TestCancelledEndpointProbeCannotStartPersistentDaemon(t *testing.T) {
	m := newServiceTestManager(t, true, false)
	entered := make(chan struct{})
	m.httpClient = &http.Client{Transport: serviceProbeTransport(func(req *http.Request) (*http.Response, error) {
		close(entered)
		<-req.Context().Done()
		return nil, req.Context().Err()
	})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() { _, err := m.ControlProviderService(ctx, "generic-daemon", "start"); finished <- err }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("probe did not run")
	}
	cancel()
	require.ErrorIs(t, <-finished, context.Canceled)
	require.False(t, serviceStatus(t, m).Desired)
	require.Zero(t, serviceStatus(t, m).PID)
}

func TestAutomaticReconcileChecksManualIntentInsideLifecycleGate(t *testing.T) {
	m := newServiceTestManager(t, true, false)
	m.servicesStarted.Store(true)
	unlock, err := m.installs.acquireProvider(context.Background(), "generic-daemon")
	require.NoError(t, err)
	finished := make(chan struct{})
	go func() { m.reconcileServices(); close(finished) }()
	select {
	case <-finished:
		t.Fatal("reconciliation bypassed the lifecycle gate")
	case <-time.After(50 * time.Millisecond):
	}
	state := m.serviceState("generic-daemon")
	state.mu.Lock()
	state.manualStop = true
	state.mu.Unlock()
	unlock()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("reconciliation did not finish")
	}
	require.False(t, serviceStatus(t, m).Desired)
	require.Zero(t, serviceStatus(t, m).PID)
}

func TestRestartMigratesOnlyTheIdentifiedManagedDaemon(t *testing.T) {
	m := newServiceTestManager(t, true, false)
	installer, err := m.installs.Dispatcher().Get("generic-daemon")
	require.NoError(t, err)
	binary, args := installer.(install.DaemonInstaller).DaemonCommand()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := host.CommandContext(ctx, binary, args...)
	cmd.Env, err = process.ChildEnvironment(m.providerServiceEnv("generic-daemon"))
	require.NoError(t, err)
	started := make(chan int, 1)
	done := make(chan error, 1)
	go func() { done <- process.RunOwnedCommandStarted(ctx, cmd, func(pid int) { started <- pid }) }()
	var priorPID int
	select {
	case priorPID = <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("old daemon did not start")
	}
	prior := serviceStatus(t, m)
	require.Equal(t, "external", prior.Supervisor, "a detached process has no zzRouter supervisor yet")
	require.True(t, prior.Managed)
	require.True(t, prior.Running)
	require.Equal(t, priorPID, prior.PID)
	_, err = m.ControlProviderService(context.Background(), "generic-daemon", "start")
	require.Error(t, err, "start cannot claim a detached process")
	_, err = m.ControlProviderService(context.Background(), "generic-daemon", "restart")
	require.NoError(t, err)
	require.Eventually(t, func() bool { return serviceStatus(t, m).PID > 0 }, 5*time.Second, 10*time.Millisecond)
	require.NotEqual(t, priorPID, serviceStatus(t, m).PID)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("old daemon was not reaped")
	}
}

func TestOSServiceObservationNotifiesStateTransitions(t *testing.T) {
	m := newServiceTestManager(t, true, false)
	manager := &testOSService{name: "systemd", running: true}
	m.serviceState("generic-daemon").manager = manager
	calls := 0
	m.catalogChanged = func() { calls++ }
	require.True(t, serviceStatus(t, m).Running)
	require.Zero(t, calls, "the initial observation cannot invalidate an in-progress refresh")
	manager.running = false
	stopped := serviceStatus(t, m)
	require.False(t, stopped.Running)
	require.Equal(t, 1, calls)
	require.Contains(t, stopped.LastExit.Reason, "systemd reported")
	_ = serviceStatus(t, m)
	require.Equal(t, 1, calls, "unchanged status does not invalidate")
	manager.running = true
	require.True(t, serviceStatus(t, m).Running)
	require.Equal(t, 2, calls)
}

func TestOSServiceApplyInvalidatesCatalog(t *testing.T) {
	m := newServiceTestManager(t, true, false)
	m.serviceState("generic-daemon").manager = &testOSService{name: "systemd", running: true}
	calls := 0
	m.catalogChanged = func() { calls++ }
	result, err := m.ApplyProviderService(context.Background(), "generic-daemon")
	require.NoError(t, err)
	require.True(t, result.Restarted)
	require.Equal(t, 1, calls)
}

func TestOSObservationPublishesStopAndRecoveryWithoutLifecycleActions(t *testing.T) {
	m := newServiceTestManager(t, true, false)
	manager := &testOSService{name: "systemd", running: true}
	m.serviceState("generic-daemon").manager = manager
	var calls atomic.Int32
	var published ProviderServiceStatus
	m.catalogChanged = func() {
		calls.Add(1)
		var err error
		published, err = m.ProviderServiceStatus(t.Context(), "generic-daemon")
		require.NoError(t, err, "publication can re-enter the status owner")
	}
	m.Start(t.Context())
	initial := calls.Load()
	manager.running = false
	m.observeOSServices(t.Context())
	require.False(t, published.Running)
	require.Equal(t, initial+1, calls.Load())
	manager.running = true
	m.observeOSServices(t.Context())
	require.True(t, published.Running)
	require.Equal(t, initial+2, calls.Load())
	m.observeOSServices(t.Context())
	require.Equal(t, initial+2, calls.Load())
	require.Equal(t, 1, manager.starts, "observation never controls the host supervisor")
	require.NoError(t, m.Stop(t.Context()))
	select {
	case <-m.servicesObserved:
	default:
		t.Fatal("shutdown must join the service observer")
	}
}

type blockedOSProbe struct {
	testOSService
	entered chan struct{}
}

func (s *blockedOSProbe) DetectContext(context.Context, string) bool { return true }
func (s *blockedOSProbe) StatusContext(ctx context.Context, _ string) service.ServiceStatus {
	close(s.entered)
	<-ctx.Done()
	return service.ServiceStatus{Manager: s.name}
}

func TestOSObservationCancelsAnInFlightProbe(t *testing.T) {
	m := newServiceTestManager(t, true, false)
	probe := &blockedOSProbe{testOSService: testOSService{name: "systemd"}, entered: make(chan struct{})}
	m.serviceState("generic-daemon").manager = probe
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); m.observeOSServices(ctx) }()
	select {
	case <-probe.entered:
	case <-time.After(time.Second):
		t.Fatal("service probe did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("service probe did not cancel")
	}
	require.Nil(t, m.serviceState("generic-daemon").observedRunning, "cancellation is not an observed stop")
}

type atomicOSService struct {
	testOSService
	active   atomic.Bool
	controls atomic.Int32
}

func (s *atomicOSService) Status(string) service.ServiceStatus {
	return service.ServiceStatus{Manager: s.name, Running: s.active.Load()}
}

func (s *atomicOSService) Control(_ context.Context, _, action string) error {
	s.controls.Add(1)
	s.active.Store(action != "stop")
	return nil
}

func TestOSServicePeriodicObservationDetectsStopAndRecovery(t *testing.T) {
	m := newServiceTestManager(t, true, false)
	manager := &atomicOSService{testOSService: testOSService{name: "systemd"}}
	manager.active.Store(true)
	m.serviceState("generic-daemon").manager = manager
	var published atomic.Bool
	m.catalogChanged = func() {
		status, err := m.ProviderServiceStatus(t.Context(), "generic-daemon")
		if err == nil {
			published.Store(status.Running)
		}
	}
	m.Start(t.Context())
	require.True(t, published.Load())
	manager.active.Store(false)
	require.Eventually(t, func() bool { return !published.Load() }, 15*time.Second, 10*time.Millisecond,
		"an OS-supervised exit must be detected without an API call")
	manager.active.Store(true)
	require.Eventually(t, published.Load, 15*time.Second, 10*time.Millisecond,
		"OS-supervised recovery must clear stale admission evidence")
	require.Equal(t, int32(1), manager.controls.Load(), "periodic observation must not control the host supervisor")
}

func TestProviderServiceStatusCloneDoesNotShareDiagnostics(t *testing.T) {
	code := 17
	original := ProviderServiceStatus{LastExit: &ProviderServiceExit{Reason: "failed", Detail: &instance.FailureInfo{ExitCode: &code, ErrorTail: []string{"original"}}}}
	cloned := original.Clone()
	cloned.LastExit.Reason = "changed"
	*cloned.LastExit.Detail.ExitCode = 99
	cloned.LastExit.Detail.ErrorTail[0] = "changed"
	require.Equal(t, "failed", original.LastExit.Reason)
	require.Equal(t, 17, *original.LastExit.Detail.ExitCode)
	require.Equal(t, []string{"original"}, original.LastExit.Detail.ErrorTail)
}

func TestInstallServiceStepOwnsDaemonBeforeOwnershipMarker(t *testing.T) {
	m := newServiceTestManager(t, false, false)
	ctx := m.installs.installContext(context.Background(), "generic-daemon")
	unlock, err := m.installs.acquireProvider(ctx, "generic-daemon")
	require.NoError(t, err)
	defer unlock()
	plan := &install.Plan{Steps: []install.Step{
		{Number: 1, ServiceAction: install.ServiceStart},
		{Number: 2, ServiceAction: install.ServiceStop},
	}}
	result := plan.ExecuteStep(ctx, 1)
	require.True(t, result.Passed, result.Message)
	state := m.serviceState("generic-daemon")
	require.Eventually(t, func() bool {
		state.mu.Lock()
		defer state.mu.Unlock()
		return state.status.PID > 0
	}, 5*time.Second, 10*time.Millisecond)
	result = plan.ExecuteStep(ctx, 2)
	require.True(t, result.Passed, result.Message)
	state.mu.Lock()
	defer state.mu.Unlock()
	require.Nil(t, state.done)
}

func TestInstallGatePreventsCrashRestartDuringActivation(t *testing.T) {
	m := newServiceTestManager(t, true, true)
	m.Start(context.Background())
	require.Eventually(t, func() bool { return serviceStatus(t, m).LastExit != nil }, 5*time.Second, 10*time.Millisecond)
	unlocked, err := m.installs.acquireProvider(context.Background(), "generic-daemon")
	require.NoError(t, err)
	prior := serviceStatus(t, m).LastExit.At
	require.Never(t, func() bool { return serviceStatus(t, m).LastExit.At != prior }, 1200*time.Millisecond, 10*time.Millisecond)
	unlocked()
	require.Eventually(t, func() bool { return serviceStatus(t, m).LastExit.At != prior }, 5*time.Second, 10*time.Millisecond)
}
