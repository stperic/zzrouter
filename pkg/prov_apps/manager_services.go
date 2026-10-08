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
	"sync"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/host"
	"github.com/stperic/zzrouter/pkg/host/service"
	"github.com/stperic/zzrouter/pkg/prov_apps/health"
	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
	"github.com/stperic/zzrouter/pkg/utils"
)

var (
	ErrExternalService = errors.New("provider is external; zzRouter cannot change its lifecycle")
	ErrServiceDisabled = errors.New("provider is disabled; enable it before starting")
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
	managerSetting  string
	status          ProviderServiceStatus
	observedRunning *bool
}

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

func serviceManager(ctx context.Context, name string, cfg config.ServiceConfig) (service.Manager, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if cfg.Service != nil && cfg.Service.Manager != "" && cfg.Service.Manager != "auto" {
		manager, err := service.Get(cfg.Service.Manager)
		if err != nil {
			return nil, err
		}
		probe, ok := manager.(service.Probe)
		detected := ok && probe.DetectContext(ctx, name)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !detected {
			return nil, fmt.Errorf("configured %s service is unavailable for %s; host owner must restore it", manager.Name(), name)
		}
		return manager, nil
	}
	manager := service.DetectContext(ctx, name)
	return manager, ctx.Err()
}

// Remember detected ownership when stop unloads a service's registration.
func (m *ProviderAppManager) selectedServiceManager(ctx context.Context, name string, cfg config.ServiceConfig) (service.Manager, error) {
	setting := ""
	if cfg.Service != nil {
		setting = cfg.Service.Manager
	}
	state := m.serviceState(name)
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.manager != nil && state.managerSetting == setting {
		return state.manager, nil
	}
	manager, err := serviceManager(ctx, name, cfg)
	if err == nil {
		state.manager, state.managerSetting = manager, setting
	}
	return manager, err
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
		state := m.serviceState(name)
		state.mu.Lock()
		changed := state.observedRunning != nil && *state.observedRunning != status.Running
		state.observedRunning = new(status.Running)
		if changed && !status.Running {
			state.status.LastExit = &ProviderServiceExit{At: utils.NowUTC(), Reason: manager.Name() + " reported that the provider stopped; exit details are unavailable"}
		}
		result.Desired = state.status.Desired
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
	result.Enabled, result.Managed = cfg.IsEnabled(), true
	owned := state.cancel != nil
	state.mu.Unlock()
	if !owned {
		binary, args := daemon.DaemonCommand()
		pids, err := process.DaemonPIDs(ctx, binary, args)
		if err != nil {
			return result, err
		}
		if len(pids) > 0 {
			result.Supervisor, result.Running, result.PID = "external", true, pids[0]
		}
	}
	return result, nil
}

// ControlProviderService serializes API actions with installation and activation.
func (m *ProviderAppManager) ControlProviderService(ctx context.Context, name, action string) (ProviderServiceStatus, error) {
	if action != "start" && action != "stop" && action != "restart" {
		return ProviderServiceStatus{}, fmt.Errorf("unsupported service action %q", action)
	}
	unlock, err := m.installs.acquireProvider(ctx, name)
	if err != nil {
		return ProviderServiceStatus{}, err
	}
	defer unlock()
	if err := m.controlProviderService(ctx, name, action, false); err != nil {
		return ProviderServiceStatus{}, err
	}
	m.serviceCatalogChanged()
	return m.ProviderServiceStatus(ctx, name)
}

// ApplyProviderService uses the same ownership gate as lifecycle controls.
func (m *ProviderAppManager) ApplyProviderService(ctx context.Context, name string) (*service.ApplyResult, error) {
	unlock, err := m.installs.acquireProvider(ctx, name)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if m.stopping.Load() {
		return nil, ErrShutdown
	}
	cfg, err := m.serviceConfig(name)
	if err != nil {
		return nil, err
	}
	if m.ProviderManagedBinary(name) == "" || !cfg.HasEndpoint() {
		return nil, ErrExternalService
	}
	if !cfg.IsEnabled() {
		return nil, ErrServiceDisabled
	}
	env := m.providerServiceEnv(name)
	if _, err := process.ChildEnvironment(env); err != nil {
		return nil, err
	}
	manager, err := m.selectedServiceManager(ctx, name, cfg)
	if err != nil {
		return nil, err
	}
	if manager != nil {
		controller, ok := manager.(service.EnvironmentController)
		if !ok {
			return nil, fmt.Errorf("%s has no runtime environment controller", manager.Name())
		}
		result := controller.ApplyEnvContext(ctx, name, env)
		if result.Applied || result.Restarted {
			m.serviceCatalogChanged()
		}
		return result, nil
	}
	if err := m.controlProviderService(ctx, name, "restart", false); err != nil {
		return nil, err
	}
	m.serviceCatalogChanged()
	return &service.ApplyResult{Manager: "zzRouter", Applied: true, Restarted: true, EnvVarsCount: len(env)}, nil
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
		if err := controller.Control(ctx, name, action); err != nil {
			return err
		}
		state := m.serviceState(name)
		state.mu.Lock()
		state.manualStop = action == "stop" && !installing
		state.status.Desired = action != "stop"
		state.mu.Unlock()
		return nil
	}
	daemon, ok := inst.(install.DaemonInstaller)
	if !ok {
		return ErrExternalService
	}
	return m.controlDaemon(ctx, name, action, cfg, daemon, installing)
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
		for _, pid := range pids {
			if err := process.Terminate(ctx, process.TerminateRequest{PID: pid, ProcessGroupID: pid}); err != nil {
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
			})
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
		failure, reason := instance.CaptureFailure(logPath, reason, exitCode, "")
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

func (m *ProviderAppManager) reconcileServices() {
	if !m.servicesStarted.Load() || m.stopping.Load() {
		return
	}
	names := make(map[string]bool)
	m.servicesMu.Lock()
	for name := range m.services {
		names[name] = true
	}
	m.servicesMu.Unlock()
	if cfg := m.AppsConfig(); cfg != nil {
		cfg.RangeApps(func(name string, svc config.ServiceConfig) bool {
			inst, _ := m.installs.Dispatcher().Get(name)
			if _, ok := inst.(install.DaemonInstaller); ok && svc.HasEndpoint() {
				names[name] = true
			}
			return true
		})
	}
	for name := range names {
		ctx, cancel := context.WithTimeout(m.shutdownCtx, 30*time.Second)
		if err := m.reconcileService(ctx, name); err != nil {
			slog.Warn("Managed provider reconciliation failed", "provider", name, "error", err)
		}
		cancel()
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
	}
	if m.servicesStarted.Load() && m.servicesObserved != nil {
		select {
		case <-m.servicesObserved:
		case <-ctx.Done():
			result = errors.Join(result, ctx.Err())
		}
	}
	return result
}
