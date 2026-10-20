package prov_apps

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/host"
	"github.com/stperic/zzrouter/pkg/host/service"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/jobs"
	obsruns "github.com/stperic/zzrouter/pkg/observability/runs"
	"github.com/stperic/zzrouter/pkg/prov_apps/health"
	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
	"github.com/stperic/zzrouter/pkg/prov_apps/protocol"
	"github.com/stperic/zzrouter/pkg/utils"
)

// ProviderServiceStatus describes daemon ownership separately from endpoint health.
type ProviderServiceStatus struct {
	Provider   string               `json:"provider"`
	Supervisor string               `json:"supervisor"`
	Managed    bool                 `json:"managed"`
	Enabled    bool                 `json:"enabled"`
	Desired    bool                 `json:"desired_running"`
	Running    bool                 `json:"running"`
	PID        int                  `json:"pid,omitempty"`
	Backoff    string               `json:"backoff,omitempty"`
	LastExit   *ProviderServiceExit `json:"last_exit,omitempty"`
	JobID      string               `json:"job_id,omitempty"` // the readiness job while it waits on the endpoint
}

// ProviderServiceExit retains bounded diagnostics for the last unexpected exit.
type ProviderServiceExit struct {
	At     time.Time             `json:"at"`
	Reason string                `json:"reason"`
	Detail *instance.FailureInfo `json:"detail"`
}

// Clone returns an independent diagnostic snapshot.
func (s ProviderServiceStatus) Clone() ProviderServiceStatus {
	if s.LastExit != nil {
		exit := *s.LastExit
		s.LastExit = &exit
		if exit.Detail != nil {
			detail := *exit.Detail
			detail.ErrorTail = append([]string(nil), detail.ErrorTail...)
			if detail.ExitCode != nil {
				code := *detail.ExitCode
				detail.ExitCode = &code
			}
			exit.Detail = &detail
		}
	}
	return s
}

type managedService struct {
	mu              sync.Mutex
	cancel          context.CancelFunc
	done            chan struct{}
	manualStop      bool
	manager         service.Manager
	noOSManager     bool // resolved: no host service manager owns it, so zzRouter supervises
	managerSetting  string
	status          ProviderServiceStatus
	observedRunning *bool
	unreconciled    bool        // the last reconcile failed; the services ticker retries it
	controls        uint64      // bumped as each host supervisor control begins and ends
	controlling     bool        // a host supervisor control is in flight
	ready           jobs.Handle // the live readiness job, nil once it ends; status reads name it
	readySince      time.Time   // an exit before it belongs to an earlier start
}

const (
	// serviceReadyPoll spaces a readiness job's endpoint probes.
	serviceReadyPoll = 2 * time.Second
	// endpointProbeTimeout bounds one probe of a provider's endpoint.
	endpointProbeTimeout = 3 * time.Second
)

// errReadyStopped ends a readiness job whose service a stop took down.
var errReadyStopped = errors.New("stopped before its endpoint answered")

func (m *ProviderAppManager) serviceState(name string) *managedService {
	m.servicesMu.Lock()
	defer m.servicesMu.Unlock()
	if m.services == nil {
		m.services = make(map[string]*managedService)
	}
	state := m.services[name]
	if state == nil {
		state = &managedService{status: ProviderServiceStatus{Provider: name, Supervisor: "zzRouter"}}
		m.services[name] = state
	}
	return state
}

func (m *ProviderAppManager) serviceConfig(name string) (config.ServiceConfig, error) {
	cfg := m.AppsConfig()
	if cfg != nil {
		if svc, ok := cfg.LookupApp(name); ok {
			return svc, nil
		}
	}
	return config.ServiceConfig{}, fmt.Errorf("provider %q is not configured", name)
}

// serviceManager resolves the host supervisor within the caller's budget; a
// probe that cannot answer is an error, never a missing service.
func serviceManager(ctx context.Context, name string, cfg config.ServiceConfig) (service.Manager, error) {
	if cfg.Service != nil && cfg.Service.Manager != "" && cfg.Service.Manager != "auto" {
		manager, err := service.Get(cfg.Service.Manager)
		if err != nil {
			return nil, err
		}
		detected := false
		if probe, ok := manager.(service.Probe); ok {
			if detected, err = probe.DetectContext(ctx, name); err != nil {
				return nil, fmt.Errorf("cannot tell whether %s supervises %s: %w", manager.Name(), name, err)
			}
		}
		if !detected {
			return nil, fmt.Errorf("configured %s service is unavailable for %s; host owner must restore it", manager.Name(), name)
		}
		return manager, nil
	}
	manager, err := service.DetectContext(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("cannot tell whether a host service manager supervises %s: %w", name, err)
	}
	return manager, nil
}

// resolvedManager returns the supervisor remembered for setting; the caller holds mu.
func (state *managedService) resolvedManager(setting string) (service.Manager, bool) {
	return state.manager, (state.manager != nil || state.noOSManager) && state.managerSetting == setting
}

// Remember the resolved supervisor, including none: stop may unload a
// service's registration, and re-probing would spawn host commands per read.
// The probe runs outside mu so a slow host cannot stall status reads.
func (m *ProviderAppManager) selectedServiceManager(ctx context.Context, name string, cfg config.ServiceConfig) (service.Manager, error) {
	setting := ""
	if cfg.Service != nil {
		setting = cfg.Service.Manager
	}
	state := m.serviceState(name)
	state.mu.Lock()
	manager, resolved := state.resolvedManager(setting)
	state.mu.Unlock()
	if resolved {
		return manager, nil
	}
	manager, err := serviceManager(ctx, name, cfg)
	if err != nil {
		return nil, err
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if first, resolved := state.resolvedManager(setting); resolved {
		return first, nil
	}
	state.manager, state.noOSManager, state.managerSetting = manager, manager == nil, setting
	return manager, nil
}

// ProviderServiceStatus observes a provider; observation never starts it.
func (m *ProviderAppManager) ProviderServiceStatus(ctx context.Context, name string) (ProviderServiceStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return ProviderServiceStatus{}, err
	}
	cfg, err := m.serviceConfig(name)
	if err != nil {
		return ProviderServiceStatus{}, err
	}
	result := ProviderServiceStatus{Provider: name, Supervisor: "external", Enabled: cfg.IsEnabled()}
	// Missing installer capability describes a connected external provider.
	inst, _ := m.installs.Dispatcher().Get(name)
	if inst == nil {
		return result, nil
	}
	result.Managed = managedBinaryOf(inst) != ""
	if !result.Managed || !cfg.HasEndpoint() {
		return result, nil
	}
	manager, err := m.selectedServiceManager(ctx, name, cfg)
	if err != nil {
		return result, err
	}
	if manager != nil {
		state := m.serviceState(name)
		state.mu.Lock()
		controls, settled := state.controls, !state.controlling
		state.mu.Unlock()
		var status service.ServiceStatus
		if probe, ok := manager.(service.Probe); ok {
			status = probe.StatusContext(ctx, name)
		} else {
			status = manager.Status(name)
		}
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if status.ObservationError != nil {
			return result, fmt.Errorf("%s service state is unknown: %w", manager.Name(), status.ObservationError)
		}
		result.Supervisor, result.Running, result.PID = manager.Name(), status.Running, status.PID
		state.mu.Lock()
		changed := false
		if settled && controls == state.controls { // a read overlapping a control saw the commanded transition
			changed = state.observedRunning != nil && *state.observedRunning != status.Running
			state.observedRunning = new(status.Running)
		}
		if changed && !status.Running {
			state.status.LastExit = &ProviderServiceExit{At: utils.NowUTC(), Reason: manager.Name() + " reported that the provider stopped; exit details are unavailable"}
		}
		result.Desired, result.JobID = state.status.Desired, state.readyID(m.jobsRegistry)
		result.LastExit = state.status.Clone().LastExit
		state.mu.Unlock()
		if changed {
			m.serviceCatalogChanged()
		}
		return result, nil
	}
	daemon, ok := inst.(install.DaemonInstaller)
	if !ok {
		return result, nil
	}
	state := m.serviceState(name)
	state.mu.Lock()
	result = state.status.Clone()
	result.Enabled, result.Managed, result.JobID = cfg.IsEnabled(), true, state.readyID(m.jobsRegistry)
	owned := state.cancel != nil
	state.mu.Unlock()
	if !owned {
		binary, args := daemon.DaemonCommand()
		pids, err := process.DaemonPIDs(ctx, binary, args)
		if err != nil {
			return result, err
		}
		if len(pids) > 0 {
			result.Supervisor, result.Running, result.PID = "external", true, pids[0].PID
		}
	}
	return result, nil
}

// ControlProviderService serializes API actions with installation and activation.
// A start or restart returns once the control does; status.JobID names the
// readiness job it opened or reused, "" when none could be opened.
func (m *ProviderAppManager) ControlProviderService(ctx context.Context, name, action string) (ProviderServiceStatus, error) {
	if action != "start" && action != "stop" && action != "restart" {
		return ProviderServiceStatus{}, fmt.Errorf("unsupported service action %q", action)
	}
	unlock, err := m.installs.acquireProvider(ctx, name)
	if err != nil {
		return ProviderServiceStatus{}, err
	}
	defer unlock()
	since := utils.NowUTC() // exits count from before the control: its own transitions record none
	if err := m.controlProviderService(ctx, name, action, false); err != nil {
		return ProviderServiceStatus{}, err
	}
	jobID := ""
	if action != "stop" {
		jobID = m.awaitReady(name, since)
	}
	m.serviceCatalogChanged()
	status, err := m.ProviderServiceStatus(ctx, name)
	status.JobID = jobID // the watcher may already have ended it
	return status, err
}

// ApplyProviderService uses the same ownership gate as lifecycle controls.
// After a restart it also names the readiness job, "" when none could be opened.
func (m *ProviderAppManager) ApplyProviderService(ctx context.Context, name string) (*service.ApplyResult, string, error) {
	unlock, err := m.installs.acquireProvider(ctx, name)
	if err != nil {
		return nil, "", err
	}
	defer unlock()
	if m.stopping.Load() {
		return nil, "", ErrShutdown
	}
	cfg, err := m.serviceConfig(name)
	if err != nil {
		return nil, "", err
	}
	if m.ProviderManagedBinary(name) == "" || !cfg.HasEndpoint() {
		return nil, "", ErrExternalService
	}
	if !cfg.IsEnabled() {
		return nil, "", ErrServiceDisabled
	}
	env := m.providerServiceEnv(name)
	if _, err := process.ChildEnvironment(env); err != nil {
		return nil, "", err
	}
	manager, err := m.selectedServiceManager(ctx, name, cfg)
	if err != nil {
		return nil, "", err
	}
	since := utils.NowUTC() // exits count from before the control: its own transitions record none
	if manager != nil {
		controller, ok := manager.(service.EnvironmentController)
		if !ok {
			return nil, "", fmt.Errorf("%s has no runtime environment controller", manager.Name())
		}
		state := m.serviceState(name)
		state.beginControl()
		defer state.endControl(false)
		result := controller.ApplyEnvContext(ctx, name, env)
		jobID := ""
		if result.Restarted {
			m.daemonRestarted(name)
			jobID = m.awaitReady(name, since)
		}
		if result.Applied || result.Restarted {
			m.serviceCatalogChanged()
		}
		return result, jobID, nil
	}
	if err := m.controlProviderService(ctx, name, "restart", false); err != nil {
		return nil, "", err
	}
	jobID := m.awaitReady(name, since)
	m.serviceCatalogChanged()
	return &service.ApplyResult{Manager: "zzRouter", Applied: true, Restarted: true, EnvVarsCount: len(env)}, jobID, nil
}

// ProviderEndpointHealthy reports whether name's own endpoint answers 200 to the credential it declares.
func (m *ProviderAppManager) ProviderEndpointHealthy(ctx context.Context, name string) bool {
	cfg, err := m.serviceConfig(name)
	if err != nil {
		return false
	}
	target, ok := backend.Target(&cfg)
	if !ok {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, endpointProbeTimeout)
	defer cancel()
	req, err := target.NewRequest(ctx, http.MethodGet, "", nil)
	if err != nil {
		return false
	}
	response, err := m.httpClient.Do(req)
	if err != nil {
		return false
	}
	_ = response.Body.Close()
	return response.StatusCode == http.StatusOK
}

// awaitReady opens name's readiness job after a start or restart, or reuses the
// live one, counting exits from since; "" when none can be opened.
func (m *ProviderAppManager) awaitReady(name string, since time.Time) string {
	state := m.serviceState(name)
	state.mu.Lock()
	defer state.mu.Unlock()
	if m.jobsRegistry == nil || m.stopping.Load() {
		return "" // once Stop begins, stopServices may already have waited on the watchers
	}
	state.readySince = since
	if id := state.readyID(m.jobsRegistry); id != "" {
		return id
	}
	h, err := m.jobsRegistry.StartDetached(jobs.KindService, "", jobs.Meta{"provider": name})
	if err != nil {
		slog.Warn("Failed to open a service readiness job", "provider", name, "error", err)
		return ""
	}
	state.ready = h
	m.serviceWatchers.Go(func() { m.watchServiceReady(name, state, h) })
	return h.ID()
}

// watchServiceReady ends h: done on the endpoint's first 200; failed on an exit
// after readySince, or on a cancel, which stops the service as a run job's cancel
// stops its run. A stop, a disable or shutdown ends h where it happens. It sets no deadline.
func (m *ProviderAppManager) watchServiceReady(name string, state *managedService, h jobs.Handle) {
	defer utils.RecoverAndLog("prov_apps.watchServiceReady")
	started := utils.Now()
	h.Progress(0, "waiting-for-health", jobs.Bytes{})
	poll := time.NewTicker(serviceReadyPoll)
	defer poll.Stop()
	heartbeat := time.NewTicker(watcherHeartbeatInterval)
	defer heartbeat.Stop()
	for {
		if m.ProviderEndpointHealthy(h.Context(), name) {
			state.releaseReady(h)
			h.Done()
			return
		}
		if exit := state.exitSinceReady(); exit != nil {
			state.releaseReady(h)
			h.Meta(jobs.Meta{"last_exit": exit})
			h.Fail(fmt.Errorf("provider exited before its endpoint answered: %s", exit.Reason))
			return
		}
		select {
		case <-h.Context().Done():
			if state.releaseReady(h) && errors.Is(context.Cause(h.Context()), jobs.ErrCanceled) {
				h.Fail(m.cancelServiceReady(name))
			}
			return
		case <-heartbeat.C:
			meta := jobs.Meta{"elapsed_ms": utils.Now().Sub(started).Milliseconds()}
			if status, err := m.ProviderServiceStatus(h.Context(), name); err == nil {
				meta["running"], meta["pid"] = status.Running, status.PID
			}
			h.Meta(meta)
			h.Progress(0, "waiting-for-health", jobs.Bytes{})
		case <-poll.C:
		}
	}
}

// cancelServiceReady stops a service whose readiness job was cancelled.
func (m *ProviderAppManager) cancelServiceReady(name string) error {
	ctx, cancel := context.WithTimeout(m.shutdownCtx, m.stopBudget(name))
	defer cancel()
	if _, err := m.ControlProviderService(ctx, name, "stop"); err != nil {
		return fmt.Errorf("cancelled; stopping the service failed: %w", err)
	}
	return errors.New("cancelled")
}

// releaseReady detaches h while it is the live readiness job; false means another path ended it.
func (state *managedService) releaseReady(h jobs.Handle) bool {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.ready != h {
		return false
	}
	state.ready = nil
	return true
}

// endReady fails the live readiness job, if any, for the stop that made it moot.
func (state *managedService) endReady(why error) {
	state.mu.Lock()
	h := state.ready
	state.ready = nil
	state.mu.Unlock()
	if h != nil {
		h.Fail(why)
	}
}

// readyID names the live readiness job, "" when none; the caller holds mu.
func (state *managedService) readyID(registry *jobs.Registry) string {
	if state.ready == nil {
		return ""
	}
	return registry.HandOut(state.ready.ID())
}

// beginControl marks a host supervisor control in flight; until endControl, an
// observation attributes no transition.
func (state *managedService) beginControl() {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.controls++
	state.controlling = true
}

// endControl ends it. A stop that took effect leaves the baseline stopped; after
// any other control the state is unknown until an observation sees it.
// Cost: an OS-managed service that dies before any observation saw it running records no exit, and its job keeps waiting while showing running:false.
func (state *managedService) endControl(stopped bool) {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.controls++
	state.controlling = false
	state.observedRunning = nil
	if stopped {
		state.observedRunning = new(false)
	}
}

// exitSinceReady is the exit recorded after the readiness window opened, if any.
func (state *managedService) exitSinceReady() *ProviderServiceExit {
	state.mu.Lock()
	defer state.mu.Unlock()
	if exit := state.status.LastExit; exit == nil || !exit.At.After(state.readySince) {
		return nil
	}
	return state.status.Clone().LastExit
}

func (m *ProviderAppManager) controlProviderService(ctx context.Context, name, action string, installing bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.stopping.Load() {
		return ErrShutdown
	}
	cfg, err := m.serviceConfig(name)
	if err != nil {
		return err
	}
	inst, err := m.installs.Dispatcher().Get(name)
	if err != nil || (!installing && managedBinaryOf(inst) == "") || !cfg.HasEndpoint() {
		return ErrExternalService
	}
	if action != "stop" && !installing && !cfg.IsEnabled() {
		return ErrServiceDisabled
	}
	manager, err := m.selectedServiceManager(ctx, name, cfg)
	if err != nil {
		return err
	}
	if manager != nil {
		controller, ok := manager.(service.Controller)
		if !ok {
			return fmt.Errorf("%s has no lifecycle controller", manager.Name())
		}
		state := m.serviceState(name)
		state.beginControl()
		stopped := false // set once the stop took effect
		defer func() { state.endControl(stopped) }()
		if err := controller.Control(ctx, name, action); err != nil {
			return err
		}
		stopped = action == "stop"
		state.mu.Lock()
		state.manualStop = action == "stop" && !installing
		state.status.Desired = action != "stop"
		state.mu.Unlock()
	} else if daemon, ok := inst.(install.DaemonInstaller); !ok {
		return ErrExternalService
	} else if err := m.controlDaemon(ctx, name, action, cfg, daemon, installing); err != nil {
		return err
	}
	if action == "stop" {
		m.serviceState(name).endReady(errReadyStopped)
	}
	if action != "start" {
		m.daemonRestarted(name)
	}
	return nil
}

// daemonRestarted retires provider's ready runs: a stopped or restarted daemon dropped their models.
func (m *ProviderAppManager) daemonRestarted(provider string) {
	for _, run := range m.runsIn(provider, instance.StatusRunning) {
		m.retireRun(run, fmt.Sprintf("retired: %s's daemon stopped or restarted", provider), obsruns.StopReasonUser)
	}
}

// The install coordinator holds the lifecycle gate. Only a newly activated
// binary may start before its ownership marker is written.
func (m *ProviderAppManager) controlInstallService(ctx context.Context, name string, action install.ServiceAction) error {
	if action == install.ServiceStop && m.ProviderManagedBinary(name) == "" {
		return m.serviceState(name).stop(ctx, false)
	}
	return m.controlProviderService(ctx, name, string(action), true)
}

func (m *ProviderAppManager) controlDaemon(ctx context.Context, name, action string, cfg config.ServiceConfig, daemon install.DaemonInstaller, installing bool) error {
	state := m.serviceState(name)
	if action != "start" {
		if err := state.stop(ctx, !installing); err != nil {
			return err
		}
		binary, args := daemon.DaemonCommand()
		pids, err := process.DaemonPIDs(ctx, binary, args)
		if err != nil {
			return err
		}
		for _, identified := range pids {
			if err := process.Terminate(ctx, identified); err != nil {
				return err
			}
		}
	}
	if action == "stop" {
		return nil
	}
	binary, args := daemon.DaemonCommand()
	if binary == "" {
		return fmt.Errorf("managed provider binary is missing; reinstall %s through the API", name)
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	state.manualStop = false
	if state.done != nil {
		return nil
	}
	// A pre-existing process is not ours to reap. Explicit restart can migrate
	// a positively identified managed daemon; endpoint health alone cannot.
	pids, err := process.DaemonPIDs(ctx, binary, args)
	if err != nil {
		return fmt.Errorf("identify managed daemon: %w", err)
	}
	if len(pids) != 0 {
		return fmt.Errorf("managed binary is already running or unreadable outside this supervisor; restart requires a positively identified daemon")
	}
	if cfg.Runtime != nil && cfg.Runtime.Endpoint != "" {
		probeCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		target, ok := backend.Target(&cfg)
		if !ok {
			return ErrExternalService
		}
		req, err := target.NewRequest(probeCtx, http.MethodGet, "", nil)
		if err != nil {
			return err
		}
		if response, err := m.httpClient.Do(req); err == nil {
			_ = response.Body.Close()
			return fmt.Errorf("provider endpoint is occupied by a process outside this supervisor; host owner must identify it before recovery")
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	env, err := process.ChildEnvironment(m.providerServiceEnv(name))
	if err != nil {
		return err
	}
	serviceCtx, cancel := context.WithCancel(m.shutdownCtx)
	state.cancel, state.done = cancel, make(chan struct{})
	state.status.Managed, state.status.Enabled, state.status.Desired = true, cfg.IsEnabled(), true
	go m.superviseService(serviceCtx, name, binary, args, env, state)
	return nil
}

func (state *managedService) stop(ctx context.Context, manual bool) error {
	state.mu.Lock()
	if manual {
		state.manualStop = true
	}
	state.status.Desired = false
	done := state.done
	if state.cancel != nil {
		state.cancel()
	}
	state.mu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (m *ProviderAppManager) superviseService(ctx context.Context, name, binary string, args, env []string, state *managedService) {
	defer func() {
		state.mu.Lock()
		state.status.Running, state.status.PID, state.status.Desired = false, 0, false
		state.status.Backoff = ""
		close(state.done)
		state.done, state.cancel = nil, nil
		state.mu.Unlock()
	}()
	delay := time.Second
	first := true // The initial start was authorized inside the lifecycle gate.
	for ctx.Err() == nil {
		unlock := func() {}
		if !first {
			var err error
			unlock, err = m.installs.acquireProvider(ctx, name)
			if err != nil {
				return
			}
		}
		first = false
		var releaseOnce sync.Once
		release := func() { releaseOnce.Do(unlock) }
		logPath := filepath.Join(config.Paths().GetLogsDir(), "provider-"+sanitizeLogFragment(name)+".log")
		err := os.MkdirAll(filepath.Dir(logPath), 0700)
		var logFile *os.File
		if err == nil {
			logFile, err = os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
		}
		if err == nil {
			cmd := host.CommandContext(ctx, binary, args...)
			cmd.Env, cmd.Stdout, cmd.Stderr = env, logFile, logFile
			err = process.RunOwnedCommandStarted(ctx, cmd, func(pid int) {
				release()
				state.mu.Lock()
				state.status.Running, state.status.PID, state.status.Backoff = true, pid, ""
				state.mu.Unlock()
				m.serviceCatalogChanged()
			}, process.GracefulTimeout)
			_ = logFile.Close()
		}
		release()
		if ctx.Err() != nil {
			return
		}
		reason := "provider process exited successfully while still enabled"
		var exitCode *int
		if err != nil {
			reason = err.Error()
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				exitCode = new(exit.ExitCode())
			}
		}
		var classifier *httperr.Classifier
		if cfg, err := m.serviceConfig(name); err == nil && cfg.Runtime != nil {
			classifier = &cfg.Runtime.FailurePatterns
		}
		failure, reason := instance.CaptureFailure(logPath, reason, exitCode, "", classifier)
		state.mu.Lock()
		state.status.Running, state.status.PID, state.status.Backoff = false, 0, delay.String()
		state.status.LastExit = &ProviderServiceExit{At: utils.NowUTC(), Reason: reason, Detail: failure}
		state.mu.Unlock()
		m.serviceCatalogChanged()
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		delay = min(30*time.Second, delay*2)
	}
}

func (m *ProviderAppManager) serviceCatalogChanged() {
	if m.catalogChanged != nil {
		m.catalogChanged()
	}
}

// OS supervisors do not emit owned-process exits, so observe their existing state.
func (m *ProviderAppManager) observeServices(ctx context.Context) {
	defer close(m.servicesObserved)
	ticker := time.NewTicker(health.DefaultInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.observeOSServices(ctx)
			m.reconcileServices(true) // e.g. at boot, while the old daemon exits
			m.reconcileServedRuns(ctx)
		}
	}
}

func (m *ProviderAppManager) observeOSServices(ctx context.Context) {
	m.servicesMu.Lock()
	var names []string
	for name, state := range m.services {
		state.mu.Lock()
		if state.manager != nil {
			names = append(names, name)
		}
		state.mu.Unlock()
	}
	m.servicesMu.Unlock()
	for _, name := range names {
		if ctx.Err() != nil {
			return
		}
		if _, err := m.ProviderServiceStatus(ctx, name); err != nil {
			slog.Debug("Provider service observation failed", "provider", name, "error", err)
		}
	}
}

// reconcileServices reconciles every service, or with retry only those whose last reconcile failed.
func (m *ProviderAppManager) reconcileServices(retry bool) {
	if !m.servicesStarted.Load() || m.stopping.Load() {
		return
	}
	names := make(map[string]bool)
	m.servicesMu.Lock()
	for name := range m.services {
		names[name] = true
	}
	m.servicesMu.Unlock()
	if cfg := m.AppsConfig(); cfg != nil && !retry {
		cfg.RangeApps(func(name string, svc config.ServiceConfig) bool {
			inst, _ := m.installs.Dispatcher().Get(name)
			if _, ok := inst.(install.DaemonInstaller); ok && svc.HasEndpoint() {
				names[name] = true
			}
			return true
		})
	}
	for name := range names {
		state := m.serviceState(name)
		state.mu.Lock()
		skip := retry && !state.unreconciled
		state.mu.Unlock()
		if skip {
			continue
		}
		ctx, cancel := context.WithTimeout(m.shutdownCtx, 30*time.Second)
		err := m.reconcileService(ctx, name)
		cancel()
		if err != nil {
			slog.Warn("Managed provider reconciliation failed; retrying on the next services tick", "provider", name, "error", err)
		}
		state.mu.Lock()
		state.unreconciled = err != nil
		state.mu.Unlock()
	}
}

func (m *ProviderAppManager) reconcileService(ctx context.Context, name string) error {
	unlock, err := m.installs.acquireProvider(ctx, name)
	if err != nil {
		return err
	}
	defer unlock()
	state := m.serviceState(name)
	cfg, cfgErr := m.serviceConfig(name)
	inst, _ := m.installs.Dispatcher().Get(name)
	if cfgErr != nil || !cfg.IsEnabled() || !cfg.HasEndpoint() || inst == nil || managedBinaryOf(inst) == "" {
		defer state.endReady(errReadyStopped)
		return state.stop(ctx, false)
	}
	state.mu.Lock()
	stopped := state.manualStop
	state.mu.Unlock()
	if stopped {
		return nil
	}
	if err := m.controlProviderService(ctx, name, "start", false); err != nil {
		return err
	}
	m.serviceCatalogChanged()
	return nil
}

func (m *ProviderAppManager) stopServices(ctx context.Context) error {
	m.servicesMu.Lock()
	states := make([]*managedService, 0, len(m.services))
	for _, state := range m.services {
		states = append(states, state)
	}
	m.servicesMu.Unlock()
	var result error
	for _, state := range states {
		result = errors.Join(result, state.stop(ctx, false))
		state.endReady(errors.New("manager shutdown"))
	}
	watched := make(chan struct{})
	go func() {
		m.serviceWatchers.Wait()
		close(watched)
	}()
	waits := []<-chan struct{}{watched}
	if m.servicesStarted.Load() && m.servicesObserved != nil {
		waits = append(waits, m.servicesObserved)
	}
	for _, done := range waits {
		select {
		case <-done:
		case <-ctx.Done():
			return errors.Join(result, ctx.Err())
		}
	}
	return result
}

// RunningModels names the models provider's live runs serve.
func (m *ProviderAppManager) RunningModels(provider string) []string {
	var names []string
	for _, run := range m.instances.ListByProvider(provider) {
		if !run.GetStatus().IsTerminal() {
			names = append(names, run.Model)
		}
	}
	slices.Sort(names)
	return names
}

// retireRun ends a run whose daemon no longer holds its model, for why; nothing is asked of the daemon.
func (m *ProviderAppManager) retireRun(run *instance.Instance, why string, reason obsruns.StopReason) {
	if !run.ClaimEnd() {
		return // a stop in progress ends it
	}
	defer run.FinishEnd(nil)
	run.Cancel()
	m.releaseRun(run, fmt.Errorf("%w: %s", ErrModelDropped, why))
	m.record(run, stoppedEvent(run, reason, why))
}

// retireUnserved retires the runs of a served provider config no longer enables: it no longer exists here.
func (m *ProviderAppManager) retireUnserved(before *config.AppsConfig) {
	for _, run := range m.instances.List() {
		_, svc, found := m.resolveConfigKey(run.Provider)
		if found && svc.IsEnabled() {
			continue
		}
		if !found && before != nil {
			svc, found = before.LookupApp(run.Provider)
		}
		if found && Lifecycle(svc) == LifecycleServed {
			m.retireRun(run, fmt.Sprintf("retired: its provider %s was disabled or removed", run.Provider), obsruns.StopReasonUser)
		}
	}
}

// reconcileServedRuns makes each enabled served provider's runs match what its daemon reports loaded.
func (m *ProviderAppManager) reconcileServedRuns(ctx context.Context) {
	if m.stopping.Load() {
		return
	}
	m.AppsConfig().RangeApps(func(name string, svc config.ServiceConfig) bool {
		if ctx.Err() == nil && svc.IsEnabled() && Lifecycle(svc) == LifecycleServed {
			m.reconcileServed(ctx, name)
		}
		return true
	})
}

// reconcileServed retires runs the daemon dropped and adopts models no run names; a failed listing changes nothing.
func (m *ProviderAppManager) reconcileServed(ctx context.Context, provider string) {
	ctx, release, err := m.catalogPermit(ctx, 1)
	if err != nil {
		return
	}
	defer release()
	var ready []*instance.Instance // none while a load is in flight: the daemon may be making room for it
	if len(m.runsIn(provider, instance.StatusStarting)) == 0 {
		ready = m.runsIn(provider, instance.StatusRunning)
	}
	failed := m.runsIn(provider, instance.StatusStopping)
	held, observed, ok := m.daemonHolds(ctx, provider)
	if !ok {
		return
	}
	ready = slices.DeleteFunc(ready, func(run *instance.Instance) bool {
		return run.Weights().Backend != "" && !backend.SameEndpoint(run.Weights().Backend, observed)
	})
	failed = slices.DeleteFunc(failed, func(run *instance.Instance) bool {
		return run.Weights().Backend != "" && !backend.SameEndpoint(run.Weights().Backend, observed)
	})
	m.settle(ready, held, obsruns.StopReasonCrash, func(run *instance.Instance) string {
		return fmt.Sprintf("dropped by the daemon: %s unloaded %s to fit another model or at its own expiry (either is possible); a request to the model starts it again", provider, run.Model)
	})
	// A failed stop is retried while the daemon holds the model, else released; a stop in progress keeps its claim.
	for _, run := range failed {
		if !run.StopUnconfirmed.Load() {
			continue
		}
		var down func(context.Context, *instance.Instance) error
		if _, ok := held[strings.ToLower(run.Model)]; ok {
			down = m.runEngine(run).down
		}
		if err := m.endRun(ctx, run, obsruns.StopReasonUser, down); err != nil && !errors.Is(err, instance.ErrStopping) {
			slog.Warn("Failed to stop a served run", "run", run.ID, "error", err)
		}
	}
	m.adoptServed(ctx, provider, held, observed)
}

// runsIn is provider's runs in status, read before a listing judges them: a later one may not be in it.
func (m *ProviderAppManager) runsIn(provider string, status instance.Status) []*instance.Instance {
	var runs []*instance.Instance
	for _, run := range m.instances.ListByProvider(provider) {
		if run.GetStatus() == status {
			runs = append(runs, run)
		}
	}
	return runs
}

// daemonHolds is what provider's daemon lists loaded, by name in lower case as it matches names; false if it cannot say.
func (m *ProviderAppManager) daemonHolds(ctx context.Context, provider string) (map[string]protocol.RunningModelInfo, string, bool) {
	loader, target, ok := m.servedTarget(provider)
	if !ok {
		return nil, "", false
	}
	listed, err := loader.ListRunningModels(ctx, target)
	if err != nil {
		slog.Debug("Served models unavailable; runs unchanged", "provider", provider, "error", err)
		return nil, "", false
	}
	held := make(map[string]protocol.RunningModelInfo, len(listed))
	for _, model := range listed {
		held[strings.ToLower(model.Name)] = model
	}
	return held, target.Endpoint(), true
}

// settle records what the daemon holds of each run and retires, for why, the ready ones it no longer holds.
func (m *ProviderAppManager) settle(runs []*instance.Instance, held map[string]protocol.RunningModelInfo, reason obsruns.StopReason, why func(*instance.Instance) string) (retired []*instance.Instance) {
	contextChanged := false
	for _, run := range runs {
		if model, ok := held[strings.ToLower(run.Model)]; ok {
			contextChanged = run.SetLoaded(model.Loaded) || contextChanged
		} else if run.GetStatus() == instance.StatusRunning {
			m.retireRun(run, why(run), reason)
			retired = append(retired, run)
		}
	}
	if contextChanged { // a daemon reload can change it on a run already published
		m.refreshCatalog()
	}
	return retired
}

// retireDisplaced settles, once run's load lands, the runs ready before it: the ones the daemon dropped, run displaced.
func (m *ProviderAppManager) retireDisplaced(ctx context.Context, run *instance.Instance) []instance.EvictedRun {
	ready := append(m.runsIn(run.Provider, instance.StatusRunning), run) // run itself is still starting
	held, observed, ok := m.daemonHolds(ctx, run.Provider)
	if !ok || !backend.SameEndpoint(run.Weights().Backend, observed) {
		return nil // the reconcile retires them on its next listing
	}
	ready = slices.DeleteFunc(ready, func(other *instance.Instance) bool {
		return other.Weights().Backend != "" && !backend.SameEndpoint(other.Weights().Backend, observed)
	})
	why := fmt.Sprintf("displaced by run %s (%s): the daemon unloaded it to fit that model; a request to it starts it again", run.ID, run.Model)
	var evicted []instance.EvictedRun
	for _, other := range m.settle(ready, held, obsruns.StopReasonMemoryPressure, func(*instance.Instance) string { return why }) {
		evicted = append(evicted, instance.EvictedRun{RunID: other.ID, Model: other.Model, Provider: other.Provider, FreedMiB: other.Loaded().VRAM / constants.BytesPerMB})
	}
	return evicted
}

// adoptServed adopts on a second listing in a row: one alone may predate the stop that unloaded the model.
func (m *ProviderAppManager) adoptServed(ctx context.Context, provider string, held map[string]protocol.RunningModelInfo, observed string) {
	m.sightingsMu.Lock()
	var candidates []protocol.RunningModelInfo
	seen, sighted := m.sightings[provider], make(map[string]bool)
	for _, model := range held {
		if m.holds(model.Name) {
			continue
		}
		if !seen[model.Name] {
			sighted[model.Name] = true
			continue
		}
		candidates = append(candidates, model)
	}
	m.sightings[provider] = sighted
	m.sightingsMu.Unlock()
	for _, model := range candidates {
		m.adoptRun(ctx, provider, model, observed)
	}
}

// adoptRun registers a run for a model another client loaded, unless a start or a run holds its key.
func (m *ProviderAppManager) adoptRun(ctx context.Context, provider string, model protocol.RunningModelInfo, observed string) {
	key, svc, ok := m.resolveConfigKey(provider)
	if !ok {
		return
	}
	run, _, err := servedEngine{m}.servedRun(ctx, EnsureRequest{Provider: provider, Model: model.Name, Endpoint: EndpointChat}, svc, key, observed)
	if err != nil {
		slog.Warn("Failed to adopt a served model", "provider", provider, "model", model.Name, "error", err)
		return
	}
	run.StartedBy = instance.StartedByExternal
	run.KeepAlive = neverExpires // only a client's hint arms zzRouter's timer on a model it did not load
	run.SetLoaded(model.Loaded)
	// As joinFlight decides: under m.mu against Stop, under flightsMu against a start of this key.
	registered := false
	m.mu.Lock()
	m.flightsMu.Lock()
	if !m.holds(model.Name) && !m.flying(model.Name) && !m.stopping.Load() {
		registered = m.instances.Register(run) == nil
	}
	m.flightsMu.Unlock()
	m.mu.Unlock()
	if !registered {
		_ = os.Remove(run.LogFilePath)
		return
	}
	m.publishRunning(run, false, "adopted (loaded by another client)")
}

// holds reports a run of any provider and endpoint for a name the daemon lists, case aside.
func (m *ProviderAppManager) holds(name string) bool {
	return slices.ContainsFunc(m.instances.List(), func(run *instance.Instance) bool { return strings.EqualFold(run.Model, name) })
}
