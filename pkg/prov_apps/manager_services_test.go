package prov_apps

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stperic/zzrouter/internal/testproc"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/host"
	"github.com/stperic/zzrouter/pkg/host/service"
	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stperic/zzrouter/pkg/utils/clock"
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
	testproc.AwaitOwner()
}

func newServiceTestManager(t *testing.T, managed, crash bool) *ProviderAppManager {
	t.Helper()
	t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
	isolateProviderRoot(t)
	cfg := testAppsConfig()
	svc, _ := cfg.LookupApp("ollama")
	svc.Service = nil
	owner, pid := testproc.OwnerEnv()
	svc.Defaults = &config.AppDefaultsConfig{Environment: map[string]string{"PROVIDER_DAEMON_HELPER": "1", owner: pid}}
	if crash {
		svc.Defaults.Environment["PROVIDER_DAEMON_CRASH"] = "1"
	}
	require.NoError(t, cfg.AddApp("generic-daemon", svc))
	client := &http.Client{Transport: serviceProbeTransport(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("test endpoint is not listening")
	})}
	registry, err := jobs.NewRegistry(jobs.Config{NodeName: "test", Clock: clock.System()})
	require.NoError(t, err)
	t.Cleanup(registry.Stop) // after the manager's cleanup below, as the server orders them
	m, err := NewProviderAppManager(cfg, WithHTTPClient(client), WithJobsRegistry(registry))
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
	m.reconcileServices(false)
	require.False(t, serviceStatus(t, m).Desired, "an unrelated config reload must preserve a manual stop")
	_, err = m.ControlProviderService(context.Background(), "generic-daemon", "start")
	require.NoError(t, err)
	require.Eventually(t, func() bool { return serviceStatus(t, m).PID > 0 }, 5*time.Second, 10*time.Millisecond)
	require.NotEqual(t, first.PID, serviceStatus(t, m).PID)
	require.NoError(t, m.Stop(context.Background()))
	require.False(t, serviceStatus(t, m).Running)
}

// A host supervisor probe that stalls past the old 3 s cut (as on a loaded
// Windows runner) still lets boot start the daemon, and a resolved "no OS
// supervisor" is not re-probed by later status reads.
func TestStalledSupervisorProbeStillStartsManagedDaemon(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix process fixture")
	}
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	probe := "#!/bin/sh\necho \"$*\" >> " + calls + "\n" +
		"if [ ! -e " + dir + "/stalled ]; then : > " + dir + "/stalled; /bin/sleep 3.5; fi\nexit 1\n"
	for _, name := range []string{"launchctl", "systemctl", "nssm"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(probe), 0o700))
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	m := newServiceTestManager(t, true, false)
	m.Start(context.Background())
	require.Eventually(t, func() bool { return serviceStatus(t, m).PID > 0 }, 5*time.Second, 10*time.Millisecond)
	before, err := os.ReadFile(calls)
	require.NoError(t, err)
	for range 20 {
		serviceStatus(t, m)
	}
	after, err := os.ReadFile(calls)
	require.NoError(t, err)
	require.Equal(t, string(before), string(after), "a resolved supervisor is not re-probed per status read")
}

// A stalled host probe must not hold the service lock: status reads, control
// actions and host listings take it without a deadline.
func TestStalledSupervisorProbeDoesNotHoldServiceLock(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix process fixture")
	}
	dir := t.TempDir()
	entered, release := filepath.Join(dir, "entered"), filepath.Join(dir, "release")
	probe := testproc.Script(": > " + entered + "\nwhile kill -0 $test_owner && [ ! -e " + release + " ]; do /bin/sleep 0.01; done\nexit 1\n")
	for _, name := range []string{"launchctl", "systemctl", "nssm"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(probe), 0o700))
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	m := newServiceTestManager(t, true, false)
	// Release before the manager's cleanup stops it, so a failure cannot deadlock.
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0o600) })
	cfg, err := m.serviceConfig("generic-daemon")
	require.NoError(t, err)
	resolved := make(chan error, 1)
	go func() {
		_, err := m.selectedServiceManager(context.Background(), "generic-daemon", cfg)
		resolved <- err
	}()
	require.Eventually(t, func() bool { _, err := os.Stat(entered); return err == nil }, 5*time.Second, 5*time.Millisecond)
	state := m.serviceState("generic-daemon")
	require.Eventually(t, func() bool {
		if !state.mu.TryLock() {
			return false
		}
		state.mu.Unlock()
		return true
	}, 2*time.Second, 5*time.Millisecond, "a stalled probe holds the service lock")
	require.NoError(t, os.WriteFile(release, nil, 0o600))
	require.NoError(t, <-resolved)
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
	_, _, err := m.ApplyProviderService(context.Background(), "generic-daemon")
	require.ErrorIs(t, err, ErrExternalService)
	require.Empty(t, m.jobsRegistry.List(jobs.KindService), "a refusal opens no readiness job")
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

func TestServiceManagerNoneIsRefused(t *testing.T) {
	cfg := config.ServiceConfig{Service: &config.ServiceManagement{Manager: "none"}}
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
			m.reconcileServices(false)
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
	go func() { m.reconcileServices(false); close(finished) }()
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

func TestBootReconcileBlockedByAnExitingDaemonIsRetried(t *testing.T) {
	m := newServiceTestManager(t, true, false)
	m.servicesStarted.Store(true)
	installer, err := m.installs.Dispatcher().Get("generic-daemon")
	require.NoError(t, err)
	binary, args := installer.(install.DaemonInstaller).DaemonCommand()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := host.CommandContext(ctx, binary, args...)
	cmd.Env, err = process.ChildEnvironment(m.providerServiceEnv("generic-daemon"))
	require.NoError(t, err)
	started, done := make(chan int, 1), make(chan error, 1)
	go func() {
		done <- process.RunOwnedCommandStarted(ctx, cmd, func(pid int) { started <- pid }, constants.NoGrace)
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the previous node's daemon did not start")
	}
	m.reconcileServices(false)
	require.False(t, serviceStatus(t, m).Desired, "a daemon this node cannot identify is never claimed")
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the previous node's daemon did not exit")
	}
	m.reconcileServices(true)
	require.Eventually(t, func() bool { return serviceStatus(t, m).PID > 0 }, 5*time.Second, 10*time.Millisecond)
	require.True(t, serviceStatus(t, m).Desired)
}

func TestRestartMigratesOnlyTheIdentifiedManagedDaemon(t *testing.T) {
	m := newServiceTestManager(t, true, false)
	installer, err := m.installs.Dispatcher().Get("generic-daemon")
	require.NoError(t, err)
	binary, args := installer.(install.DaemonInstaller).DaemonCommand()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := host.CommandContext(ctx, binary, args...)
	owner, pid := testproc.OwnerEnv()
	cmd.Env = []string{"PROVIDER_DAEMON_HELPER=1", owner + "=" + pid}
	started := make(chan int, 1)
	done := make(chan error, 1)
	go func() {
		done <- process.RunOwnedCommandStarted(ctx, cmd, func(pid int) { started <- pid }, constants.NoGrace)
	}()
	var priorPID int
	select {
	case priorPID = <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("old daemon did not start")
	}
	require.False(t, (process.Inspector{}).HasMarker(int32(priorPID)), "migration must also identify an unmarked daemon")
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
	result, jobID, err := m.ApplyProviderService(context.Background(), "generic-daemon")
	require.NoError(t, err)
	require.True(t, result.Restarted)
	require.Equal(t, 1, calls)
	require.Equal(t, jobID, serviceStatus(t, m).JobID, "a restarting apply answers with the readiness job")
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

func (s *blockedOSProbe) DetectContext(context.Context, string) (bool, error) { return true, nil }
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
	midway   func() // observes halfway through a restart, as the services ticker can
}

func (s *atomicOSService) Status(string) service.ServiceStatus {
	return service.ServiceStatus{Manager: s.name, Running: s.active.Load()}
}

func (s *atomicOSService) Control(_ context.Context, _, action string) error {
	s.controls.Add(1)
	if action == "restart" && s.midway != nil {
		s.active.Store(false)
		s.midway()
	}
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
	// The same bound as the OS-supervised exit test: a slow Windows runner took over 5 s to observe the crash.
	require.Eventually(t, func() bool { return serviceStatus(t, m).LastExit != nil }, 15*time.Second, 10*time.Millisecond)
	unlocked, err := m.installs.acquireProvider(context.Background(), "generic-daemon")
	require.NoError(t, err)
	prior := serviceStatus(t, m).LastExit.At
	require.Never(t, func() bool { return serviceStatus(t, m).LastExit.At != prior }, 1200*time.Millisecond, 10*time.Millisecond)
	unlocked()
	require.Eventually(t, func() bool { return serviceStatus(t, m).LastExit.At != prior }, 5*time.Second, 10*time.Millisecond)
}

// serviceJobEnd waits for readiness job id to end and returns its final event.
func serviceJobEnd(t *testing.T, m *ProviderAppManager, id string) jobs.Event {
	t.Helper()
	var end jobs.Event
	require.Eventually(t, func() bool {
		var err error
		end, err = m.jobsRegistry.Get(id)
		return err == nil && end.Phase.IsTerminal()
	}, 10*time.Second, 10*time.Millisecond)
	return end
}

// answerProbes makes the endpoint answer 200 once the returned flag is set.
func answerProbes(m *ProviderAppManager) *atomic.Bool {
	ready := new(atomic.Bool)
	m.httpClient = &http.Client{Transport: serviceProbeTransport(func(*http.Request) (*http.Response, error) {
		if !ready.Load() {
			return nil, errors.New("test endpoint is not listening")
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ready"))}, nil
	})}
	return ready
}

func TestServiceStartReadinessJobEndsOnTheEndpointsFirstAnswer(t *testing.T) {
	m := newServiceTestManager(t, true, false)
	ready := answerProbes(m)
	m.serviceState("generic-daemon").status.LastExit = &ProviderServiceExit{At: utils.NowUTC().Add(-time.Minute), Reason: "an earlier start's exit"}
	status, err := m.ControlProviderService(t.Context(), "generic-daemon", "start")
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(status.JobID, "svc_"), status.JobID)
	require.Never(t, func() bool {
		event, err := m.jobsRegistry.Get(status.JobID)
		return err != nil || event.Phase.IsTerminal()
	}, serviceReadyPoll+500*time.Millisecond, 50*time.Millisecond, "the job waits while the endpoint is silent, and an earlier exit is not this start's")
	require.Equal(t, status.JobID, serviceStatus(t, m).JobID, "status names the job to wait on")
	ready.Store(true)
	require.Equal(t, jobs.PhaseDone, serviceJobEnd(t, m, status.JobID).Phase)
	require.Empty(t, serviceStatus(t, m).JobID)
}

func TestServiceReadinessJobFailsWithTheDaemonsExit(t *testing.T) {
	m := newServiceTestManager(t, true, true)
	status, err := m.ControlProviderService(t.Context(), "generic-daemon", "start")
	require.NoError(t, err)
	end := serviceJobEnd(t, m, status.JobID)
	require.Equal(t, jobs.PhaseFailed, end.Phase)
	require.Contains(t, end.Err, "managed daemon failed")
	exit, ok := end.Meta["last_exit"].(*ProviderServiceExit)
	require.True(t, ok, "the job carries the exit evidence")
	require.Equal(t, 17, *exit.Detail.ExitCode)
}

// A stop ends the job itself; the job never stops the service a second time.
func TestServiceStopEndsTheReadinessJob(t *testing.T) {
	m := newServiceTestManager(t, true, false)
	var published atomic.Int32 // every API control publishes once, so a second stop shows here
	m.catalogChanged = func() { published.Add(1) }
	status, err := m.ControlProviderService(t.Context(), "generic-daemon", "start")
	require.NoError(t, err)
	require.Eventually(t, func() bool { return serviceStatus(t, m).PID > 0 }, 5*time.Second, 10*time.Millisecond)
	_, err = m.ControlProviderService(t.Context(), "generic-daemon", "stop")
	require.NoError(t, err)
	end := serviceJobEnd(t, m, status.JobID)
	require.Equal(t, jobs.PhaseFailed, end.Phase)
	require.Equal(t, errReadyStopped.Error(), end.Err)
	stopped := serviceStatus(t, m)
	require.False(t, stopped.Desired)
	require.Zero(t, stopped.PID)
	after := published.Load()
	require.Never(t, func() bool { return published.Load() != after }, time.Second, 10*time.Millisecond,
		"the job does not stop the service again")
}

// A start or restart while a job is live reuses it, and the restart's own stop is
// not the provider exiting, whichever supervisor runs it.
func TestServiceRestartReusesTheJobAndItsStopIsNoExit(t *testing.T) {
	for _, supervisor := range []string{"zzRouter", "systemd"} {
		t.Run(supervisor, func(t *testing.T) {
			m := newServiceTestManager(t, true, false)
			state := m.serviceState("generic-daemon")
			if supervisor == "systemd" {
				manager := &atomicOSService{testOSService: testOSService{name: "systemd"}}
				manager.midway = func() {
					_, err := m.ProviderServiceStatus(context.Background(), "generic-daemon")
					require.NoError(t, err)
				}
				state.manager = manager
			}
			first, err := m.ControlProviderService(t.Context(), "generic-daemon", "start")
			require.NoError(t, err)
			require.Eventually(t, func() bool { return serviceStatus(t, m).Running }, 5*time.Second, 10*time.Millisecond)
			for _, action := range []string{"start", "restart"} {
				again, err := m.ControlProviderService(t.Context(), "generic-daemon", action)
				require.NoError(t, err)
				require.Equal(t, first.JobID, again.JobID, action)
			}
			require.Never(t, func() bool {
				event, err := m.jobsRegistry.Get(first.JobID)
				return err != nil || event.Phase.IsTerminal()
			}, serviceReadyPoll+500*time.Millisecond, 50*time.Millisecond, "the job outlives the restart")
			require.Nil(t, serviceStatus(t, m).LastExit)
			require.Len(t, m.jobsRegistry.List(jobs.KindService), 1)
		})
	}
}

// A stop through the API is not the provider exiting; an exit after a start still is.
func TestOSServiceAPIStopRecordsNoExitButACrashDoes(t *testing.T) {
	m := newServiceTestManager(t, true, false)
	manager := &atomicOSService{testOSService: testOSService{name: "systemd"}}
	m.serviceState("generic-daemon").manager = manager
	_, err := m.ControlProviderService(t.Context(), "generic-daemon", "start")
	require.NoError(t, err)
	_, err = m.ControlProviderService(t.Context(), "generic-daemon", "stop")
	require.NoError(t, err)
	require.Nil(t, serviceStatus(t, m).LastExit)
	_, err = m.ControlProviderService(t.Context(), "generic-daemon", "start")
	require.NoError(t, err)
	manager.active.Store(false)
	require.Contains(t, serviceStatus(t, m).LastExit.Reason, "systemd reported that the provider stopped")
}

// A stop leaves the service known stopped even when nothing reads it after, as an
// install step's stop does, so a start from outside zzRouter republishes the catalog.
func TestStopBaselineMakesAnOutsideStartAChange(t *testing.T) {
	m := newServiceTestManager(t, true, false)
	manager := &atomicOSService{testOSService: testOSService{name: "systemd"}}
	manager.active.Store(true)
	m.serviceState("generic-daemon").manager = manager
	require.True(t, serviceStatus(t, m).Running)
	require.NoError(t, m.controlInstallService(t.Context(), "generic-daemon", install.ServiceStop))
	var published atomic.Int32
	m.catalogChanged = func() { published.Add(1) }
	manager.active.Store(true)
	require.True(t, serviceStatus(t, m).Running)
	require.Equal(t, int32(1), published.Load())
}

// lateSpawnOSService spawns after its start returns, as launchd can: the first
// read after a start finds no process.
type lateSpawnOSService struct {
	atomicOSService
	spawning atomic.Bool
}

func (s *lateSpawnOSService) Control(ctx context.Context, name, action string) error {
	s.spawning.Store(action != "stop")
	return s.atomicOSService.Control(ctx, name, action)
}

func (s *lateSpawnOSService) Status(name string) service.ServiceStatus {
	if s.spawning.CompareAndSwap(true, false) {
		return service.ServiceStatus{Manager: s.name}
	}
	return s.atomicOSService.Status(name)
}

// After a start zzRouter does not know the state until something observes it, so
// a process not spawned yet is no exit and the job keeps waiting.
func TestNotYetSpawnedServiceIsNoExit(t *testing.T) {
	m := newServiceTestManager(t, true, false)
	manager := &lateSpawnOSService{atomicOSService: atomicOSService{testOSService: testOSService{name: "launchd"}}}
	m.serviceState("generic-daemon").manager = manager
	status, err := m.ControlProviderService(t.Context(), "generic-daemon", "start")
	require.NoError(t, err)
	require.False(t, status.Running, "the start's own read found no process yet")
	require.True(t, serviceStatus(t, m).Running)
	require.Never(t, func() bool {
		event, err := m.jobsRegistry.Get(status.JobID)
		return err != nil || event.Phase.IsTerminal()
	}, serviceReadyPoll+500*time.Millisecond, 50*time.Millisecond, "a late spawn does not fail the job")
	require.Nil(t, serviceStatus(t, m).LastExit)
}

// The window opens when the control begins, so an exit recorded before the job
// exists, as a daemon that dies at spawn can be, fails it.
func TestReadinessWindowCountsExitsBeforeTheJobOpens(t *testing.T) {
	m := newServiceTestManager(t, true, false)
	since := utils.NowUTC().Add(-time.Second)
	state := m.serviceState("generic-daemon")
	state.mu.Lock()
	state.status.LastExit = &ProviderServiceExit{At: utils.NowUTC(), Reason: "exited at spawn"}
	state.mu.Unlock()
	end := serviceJobEnd(t, m, m.awaitReady("generic-daemon", since))
	require.Equal(t, jobs.PhaseFailed, end.Phase)
	require.Contains(t, end.Err, "exited at spawn")
}

// An endpoint that already answers ends the job at once; the start still names it.
func TestFastReadyStartStillNamesItsJob(t *testing.T) {
	m := newServiceTestManager(t, true, false)
	m.serviceState("generic-daemon").manager = &atomicOSService{testOSService: testOSService{name: "systemd"}}
	answerProbes(m).Store(true)
	m.catalogChanged = func() { // the start's status read follows the job's end
		require.Eventually(t, func() bool {
			listed := m.jobsRegistry.List(jobs.KindService)
			return len(listed) == 1 && listed[0].Phase.IsTerminal()
		}, 5*time.Second, 10*time.Millisecond)
	}
	status, err := m.ControlProviderService(t.Context(), "generic-daemon", "start")
	require.NoError(t, err)
	require.NotEmpty(t, status.JobID)
	require.Equal(t, jobs.PhaseDone, serviceJobEnd(t, m, status.JobID).Phase)
}

// A job that ends by any path but a cancel, a reap for one, leaves the service running.
func TestEndedReadinessJobLeavesTheServiceRunning(t *testing.T) {
	m := newServiceTestManager(t, true, false)
	var published atomic.Int32
	m.catalogChanged = func() { published.Add(1) }
	status, err := m.ControlProviderService(t.Context(), "generic-daemon", "start")
	require.NoError(t, err)
	require.Eventually(t, func() bool { return serviceStatus(t, m).PID > 0 }, 5*time.Second, 10*time.Millisecond)
	state := m.serviceState("generic-daemon")
	state.mu.Lock()
	h := state.ready
	state.mu.Unlock()
	after := published.Load()
	h.Fail(errors.New("job reaped: inactivity timeout exceeded"))
	require.Eventually(t, func() bool { return serviceStatus(t, m).JobID == "" }, 5*time.Second, 10*time.Millisecond,
		"an ended job is no longer the one to wait on")
	require.Never(t, func() bool { return published.Load() != after }, time.Second, 10*time.Millisecond,
		"the service is not stopped")
	running := serviceStatus(t, m)
	require.True(t, running.Desired)
	require.NotZero(t, running.PID)
	require.Equal(t, "job reaped: inactivity timeout exceeded", serviceJobEnd(t, m, status.JobID).Err)
}

// Shutdown ends a live job and waits for its watcher, so nothing reports after Stop.
func TestStopEndsReadinessJobsAndWaitsForTheirWatchers(t *testing.T) {
	m := newServiceTestManager(t, true, false)
	m.serviceState("generic-daemon").manager = &atomicOSService{testOSService: testOSService{name: "systemd"}}
	probing, release := make(chan struct{}, 1), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	m.httpClient = &http.Client{Transport: serviceProbeTransport(func(*http.Request) (*http.Response, error) {
		select {
		case probing <- struct{}{}:
		default:
		}
		<-release // a probe that ignores its deadline holds the watcher
		return nil, errors.New("test endpoint is not listening")
	})}
	status, err := m.ControlProviderService(t.Context(), "generic-daemon", "start")
	require.NoError(t, err)
	select {
	case <-probing:
	case <-time.After(5 * time.Second):
		t.Fatal("the watcher never probed")
	}
	stopped := make(chan error, 1)
	go func() { stopped <- m.Stop(context.Background()) }()
	select {
	case <-stopped:
		t.Fatal("Stop returned while the watcher was still running")
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-stopped:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return once the watcher could")
	}
	end, err := m.jobsRegistry.Get(status.JobID)
	require.NoError(t, err)
	require.Equal(t, "manager shutdown", end.Err)
}

// Cancelling the job stops the service, as a run job's cancel stops its run.
func TestServiceReadinessJobCancelStopsTheService(t *testing.T) {
	m := newServiceTestManager(t, true, false)
	status, err := m.ControlProviderService(t.Context(), "generic-daemon", "start")
	require.NoError(t, err)
	require.Eventually(t, func() bool { return serviceStatus(t, m).PID > 0 }, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, m.jobsRegistry.Cancel(status.JobID))
	end := serviceJobEnd(t, m, status.JobID)
	require.Equal(t, "cancelled", end.Err)
	stopped := serviceStatus(t, m)
	require.False(t, stopped.Desired)
	require.Zero(t, stopped.PID)
	require.Empty(t, stopped.JobID)
}

// Disabling a provider stops its daemon, so the wait it leaves behind ends too;
// a disabled provider refuses a start without opening another job.
func TestDisabledProviderEndsAndRefusesReadinessJobs(t *testing.T) {
	m := newServiceTestManager(t, true, false)
	m.Start(t.Context())
	status, err := m.ControlProviderService(t.Context(), "generic-daemon", "start")
	require.NoError(t, err)
	cfg := testAppsConfig()
	svc, _ := cfg.LookupApp("ollama")
	svc.Service, svc.Enabled = nil, new(false)
	require.NoError(t, cfg.AddApp("generic-daemon", svc))
	m.ReloadConfig(cfg)
	require.Equal(t, errReadyStopped.Error(), serviceJobEnd(t, m, status.JobID).Err)
	for _, action := range []string{"start", "restart"} {
		_, err := m.ControlProviderService(t.Context(), "generic-daemon", action)
		require.ErrorIs(t, err, ErrServiceDisabled)
	}
	_, _, err = m.ApplyProviderService(t.Context(), "generic-daemon")
	require.ErrorIs(t, err, ErrServiceDisabled)
	require.Len(t, m.jobsRegistry.List(jobs.KindService), 1, "a refusal opens no readiness job")
}

// The probe reaches the provider's own endpoint, so it carries the credential the provider declares.
func TestProviderEndpointProbeSendsTheDeclaredCredential(t *testing.T) {
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer nas-token" {
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer daemon.Close()
	nas := config.NewOllamaConnectProvider("ollama-nas", daemon.URL, "nas-token")
	cfg := testAppsConfig()
	require.NoError(t, cfg.AddApp("ollama-nas", config.ServiceConfig{
		Enabled: new(true), Name: "ollama-nas", Mode: "external", Protocol: nas.Protocol,
		Runtime:      &config.AppRuntimeConfig{Endpoint: daemon.URL, API: nas.Runtime.API},
		Capabilities: nas.Capabilities,
	}))
	m, err := NewProviderAppManager(cfg, WithHTTPClient(daemon.Client()))
	require.NoError(t, err)
	cleanupProviderManager(t, m)
	require.True(t, m.ProviderEndpointHealthy(t.Context(), "ollama-nas"))
}
