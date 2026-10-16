package prov_apps

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/jobs"
	obsruns "github.com/stperic/zzrouter/pkg/observability/runs"
	"github.com/stperic/zzrouter/pkg/prov_apps/health"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stperic/zzrouter/pkg/utils"
)

func (m *ProviderAppManager) launchInstanceLocked(ctx context.Context, req EnsureRequest, selection launchSelection) (*instance.Instance, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	declared, err := m.launchSchema(selection.key)
	if err != nil {
		return nil, err
	}
	inst, err := m.registerRun(ctx, req, selection)
	if err != nil {
		return nil, err
	}
	if launchBudget(declared) != nil {
		// Admission measures free memory, so admissions run one at a time.
		node := Node{ShuttingDown: m.shuttingDown(), Lifecycle: LifecycleProcess, Runtime: req.selectedRuntime}
		select {
		case m.memoryAdmission <- struct{}{}:
		default:
			node.AdmissionHeld = true
			return m.queueRun(inst, req, node)
		}
		defer func() { <-m.memoryAdmission }()
		blocker, err := m.startingBlocker(inst.ID, selection.key, declared)
		if err != nil {
			m.releaseRun(inst, err)
			return nil, err
		}
		if blocker != nil {
			node.Blocker = runView(blocker)
			return m.queueRun(inst, req, node)
		}
	}
	run, err := m.admitRun(ctx, inst, req, selection, declared)
	if err != nil {
		m.releaseRun(inst, err)
		return nil, err
	}
	m.watchRun(inst, req)
	m.startRun(inst, run)
	return inst, nil
}

type launchSelection struct {
	key    string
	config config.ServiceConfig
	model  process.ResolvedModel
}

// resolveLaunch resolves the provider config and freezes the runtime a
// launch selects; toolkit reads happen under that runtime's gate.
func (m *ProviderAppManager) resolveLaunch(req *EnsureRequest) (launchSelection, error) {
	configKey, svcCfg, ok := m.resolveConfigKey(req.Provider)
	if !ok {
		return launchSelection{}, fmt.Errorf("%w: %q not found in config", ErrProviderNotFound, req.Provider)
	}
	model := process.ResolveModel(svcCfg, req.Model)
	svcCfg, selected, _, err := m.featureRuntime(svcCfg, model, req.Runtime, string(EndpointOrDefault(req.Endpoint)))
	if err != nil {
		return launchSelection{}, err
	}
	if selected == "" {
		selected = svcCfg.Name
	}
	req.selectedRuntime = selected
	return launchSelection{key: configKey, config: svcCfg, model: model}, nil
}

// registerRun allocates a port and registers a starting, not yet admitted run.
func (m *ProviderAppManager) registerRun(ctx context.Context, req EnsureRequest, selection launchSelection) (*instance.Instance, error) {
	allocatedPort, err := m.ports.AllocateForProvider(req.Provider, req.Port)
	if err != nil {
		return nil, fmt.Errorf("provider %s port allocation failed: %w", req.Provider, err)
	}
	inst, err := m.newRun(req, selection.config, allocatedPort)
	if err == nil {
		weights := m.reserveWeights(ctx, req, selection)
		inst.SetWeights(weights)
		inst.SourceRepo = weights.Source
	}
	if err == nil {
		err = m.instances.Register(inst)
	}
	if err != nil {
		m.ports.ReleaseForProvider(req.Provider, allocatedPort)
		return nil, err
	}
	return inst, nil
}

// newRun configures a starting run and its immutable log before publication; port is 0 for none.
func (m *ProviderAppManager) newRun(req EnsureRequest, svcCfg config.ServiceConfig, port int) (*instance.Instance, error) {
	settings, err := m.runSettings(svcCfg, req)
	if err != nil {
		return nil, err
	}
	endpoint := req.Key().Endpoint
	id := instance.GenerateID(req.Provider, req.Model, endpoint)
	inst := instance.NewInstance(id, req.Provider, req.Model, port, settings.KeepAlive, 0)
	inst.Endpoint = endpoint
	inst.StartedBy = instance.StartedByAPI
	if req.Origin == OriginRequest {
		inst.StartedBy = instance.StartedByRequest
	}
	if svcCfg.Runtime != nil {
		inst.FailureClassifier = &svcCfg.Runtime.FailurePatterns
	}
	inst.StopUnconfirmed.Store(req.Origin == OriginInstall) // Smoke retains ownership until strict process drain.
	inst.Config = instance.Config{
		Endpoint: string(req.Endpoint),
		Runtime:  req.Runtime, DisposablePlanID: req.DisposablePlanID,
		Provider:   req.Provider,
		LaunchMode: instance.LaunchModeNative,
		Model:      req.Model,
		Port:       port,
		EnvVars:    req.EnvVars,
		Parameters: req.Parameters,
	}
	if !m.engine(Lifecycle(svcCfg)).outlivesNode() && req.DisposablePlanID == "" && req.Origin != OriginInstall {
		saved := restartRequest(inst)
		saved.keyEndpoint = Endpoint(inst.Endpoint)
		// Every transition passes setStatusLocked; the store writes only when a pin is added or dropped.
		inst.CommitStatus = func(status instance.Status, pinned bool) {
			if status == instance.StatusRunning || status.IsTerminal() && !m.stopping.Load() {
				m.pins.update(saved, status == instance.StatusRunning && pinned)
			}
		}
	}

	// Set HealthURL from provider config (not hardcoded /health)
	if port > 0 {
		host := constants.Localhost
		if m.selfHost != nil {
			host = m.selfHost()
		}
		inst.BaseURL = "http://" + net.JoinHostPort(host, strconv.Itoa(port))
		healthPath := "/health"
		if svcCfg.Runtime != nil && svcCfg.Runtime.HealthCheck.Path != "" {
			healthPath = svcCfg.Runtime.HealthCheck.Path
		}
		inst.HealthURL = fmt.Sprintf("http://localhost:%d%s", port, healthPath)
	}

	// Initialize concurrency limiter from provider config
	if svcCfg.Runtime != nil && svcCfg.Runtime.MaxConcurrentRequests > 0 {
		inst.InitConcurrencyLimit(instance.ConcurrencyConfig{
			MaxConcurrentRequests: svcCfg.Runtime.MaxConcurrentRequests,
			MaxQueuedRequests:     svcCfg.Runtime.QueueDepth(),
			QueueTimeout:          svcCfg.Runtime.GetQueueTimeout(),
		})
	}

	logFile, logPath, err := m.runLogFile(inst)
	if err != nil {
		return nil, err
	}
	inst.SetLogFilePath(logPath)
	return inst, logFile.Close()
}

const keepAliveParameter = config.KeepAliveParameter

func (m *ProviderAppManager) runSettings(svc config.ServiceConfig, req EnsureRequest) (config.RunSettings, error) {
	params := mergeStringMaps(config.FlattenParameters(svc.ResolveEndpoint(m.localNodename(), req.Model, string(EndpointOrDefault(req.Endpoint))).Parameters), req.Parameters)
	settings, err := config.ParseRunSettings(params, m.idleEvictionDefault)
	if err != nil {
		return config.RunSettings{}, fmt.Errorf("%w: %w", ErrParameterValidation, err)
	}
	return settings, nil
}

// releaseRun undoes registerRun for a run no process was started for. It
// fails the run with cause first, so a caller already holding it gets that
// cause. The port goes back only with the registry entry, so a racing stop
// cannot return it twice.
func (m *ProviderAppManager) releaseRun(inst *instance.Instance, cause error) {
	inst.MarkFailedError(cause)
	if err := m.instances.Remove(inst.ID); err != nil {
		slog.Warn("failed to remove instance during cleanup", "id", inst.ID, "error", err)
		return
	}
	m.ports.ReleaseForProvider(inst.Provider, inst.Port)
}

// admittedRun is what an admitted run's process is started with.
type admittedRun struct {
	cmd       string
	args      []string
	vars      map[string]string
	logFile   *os.File
	health    health.CheckConfig
	transform process.EnvironmentTransform
	placement []*regexp.Regexp
}

// runLogFile opens run's log file for appending; it outlives the run on this node.
func (m *ProviderAppManager) runLogFile(run *instance.Instance) (*os.File, string, error) {
	// Sanitized so a hostile model id cannot escape the log directory.
	f, path, err := m.logMgr.CreateLogFile(run.ID, run.Provider, sanitizeLogFragment(run.Model))
	if err != nil {
		return nil, "", fmt.Errorf("failed to create log file: %w", err)
	}
	return f, path, nil
}

// admitRun measures and prepares a registered run, then marks it admitted.
// Callers hold memoryAdmission when the provider has a memory budget.
func (m *ProviderAppManager) admitRun(ctx context.Context, inst *instance.Instance, req EnsureRequest, selection launchSelection, declared *schema.ProviderSchema) (admittedRun, error) {
	logFile, _, err := m.runLogFile(inst)
	if err != nil {
		return admittedRun{}, err
	}
	run, err := m.prepareRun(ctx, inst, req, selection, declared)
	if err == nil && !inst.Admit() {
		err = fmt.Errorf("run %s stopped before it was admitted", inst.ID)
	}
	if err != nil {
		logFile.Close()
		return admittedRun{}, err
	}
	run.logFile = logFile
	if m.seams.admitted != nil {
		m.seams.admitted()
	}
	m.emitEvent(runEvent(EventInstanceStarting, inst, fmt.Sprintf("Starting %s on port %d", req.Model, inst.Port)))
	return run, nil
}

// prepareRun resolves a run's parameters, checks its memory fit and builds argv.
func (m *ProviderAppManager) prepareRun(ctx context.Context, inst *instance.Instance, req EnsureRequest, selection launchSelection, declared *schema.ProviderSchema) (admittedRun, error) {
	endpoint := inst.Endpoint
	features, err := m.modelLaunchParameters(ctx, selection.config, selection.model, req, endpoint, declared)
	if err != nil {
		return admittedRun{}, err
	}
	var featurePaths []string
	for _, feature := range features.Config.Features {
		if path := features.Params[feature.Flag]; len(feature.Files) > 0 && filepath.IsAbs(path) {
			featurePaths = append(featurePaths, path)
		}
	}
	argvFeatures := features
	argvFeatures.Params = maps.Clone(features.Params)
	evicted, err := m.admitMemory(ctx, argvFeatures)
	inst.SetEvictedRuns(evicted)
	var prepared preparedLaunch
	if err == nil {
		prepared, err = m.prepareLaunchFeatures(features, argvFeatures.Params, selection.key, endpoint, inst.Port)
	}
	if err != nil {
		return admittedRun{}, err
	}
	settings, err := config.ParseRunSettings(prepared.parameters.Params, m.idleEvictionDefault)
	if err != nil {
		return admittedRun{}, fmt.Errorf("%w: %w", ErrParameterValidation, err)
	}
	inst.SetKeepAlive(settings.KeepAlive)
	// An incomplete download reaches the engine as a corrupt-tensor crash
	// tens of seconds in; catching it here names the actual problem.
	if err := process.VerifyLocalWeights(ctx, prepared.launch.ModelPath, featurePaths, selection.model.SourcePath()); err != nil {
		return admittedRun{}, err
	}
	if err := ctx.Err(); err != nil {
		return admittedRun{}, err
	}
	if len(featurePaths) > 0 {
		weights := inst.Weights()
		weights.Paths = append(weights.Paths, featurePaths...)
		inst.SetWeights(weights)
	}
	inst.SetResolved(prepared.resolved)
	inst.WireModel = prepared.launch.WireModel
	var placement []*regexp.Regexp
	if budget := launchBudget(declared); budget != nil && budget.Kind == schema.MemoryLayers {
		patterns := declared.ForEndpoint(endpoint)[budget.Parameter].PlacementLog
		if len(patterns) > 0 {
			placement = []*regexp.Regexp{
				placementLayers:       patterns["layers"],
				placementCPUOverrides: patterns["cpu_overrides"],
				placementWeights:      patterns["weights"],
			}
		}
	}
	return admittedRun{
		cmd: prepared.launch.Command, args: prepared.launch.Args, vars: prepared.resolved.Environment,
		health: buildHealthCheckConfig(prepared.config, inst.Port), transform: prepared.executionEnvironment, placement: placement,
	}, nil
}

// queueRun accepts a run that waits for another launch to claim its GPU
// memory, when the policy lets it wait on what node observed.
func (m *ProviderAppManager) queueRun(inst *instance.Instance, req EnsureRequest, node Node) (*instance.Instance, error) {
	decision := Decide(req.want(), node)
	if decision.Act != ActQueue {
		err := decision.Err
		if err == nil {
			err = errUndecided
		}
		m.releaseRun(inst, err)
		return nil, err
	}
	inst.SetWaitingOn(decision.Run)
	m.watchRun(inst, req)
	inst.TrackGoroutine()
	go func() {
		defer inst.GoroutineDone()
		defer utils.RecoverAndLog("prov_apps.awaitAdmission")
		m.awaitAdmission(inst, req)
	}()
	return inst, nil
}

// awaitAdmission admits a queued run once no admitted run is still
// starting, then starts it. Stopping the run ends the wait; StopInstance
// owns its cleanup.
func (m *ProviderAppManager) awaitAdmission(inst *instance.Instance, req EnsureRequest) {
	started := false
	defer func() {
		if !started {
			inst.ReleaseMemory()
		}
	}()
	ctx := inst.Context()
	for {
		run, blocker, err := m.tryAdmit(ctx, inst, req)
		switch {
		case ctx.Err() != nil:
			if run.logFile != nil { // stopped after admission, before its process
				run.logFile.Close()
			}
			return
		case err != nil:
			inst.MarkFailedError(err)
			m.emitEvent(failedEvent(inst, obsruns.FailureLaunchErr, err))
			return
		case blocker == nil:
			started = true
			m.startRun(inst, run)
			return
		}
		inst.SetWaitingOn(blocker.ID)
		select {
		case <-blocker.LeftStarting():
		case <-ctx.Done():
			return
		}
	}
}

// tryAdmit admits a queued run unless an admitted run is still starting.
// Config is resolved again: it may have changed while the run waited.
func (m *ProviderAppManager) tryAdmit(ctx context.Context, inst *instance.Instance, req EnsureRequest) (admittedRun, *instance.Instance, error) {
	selection, err := m.resolveLaunch(&req)
	if err != nil {
		return admittedRun{}, nil, err
	}
	declared, err := m.launchSchema(selection.key)
	if err != nil {
		return admittedRun{}, nil, err
	}
	unlock, err := m.installs.acquireProvider(ctx, req.selectedRuntime)
	if err != nil {
		return admittedRun{}, nil, err
	}
	defer unlock()
	ctx, release, err := m.catalogPermit(ctx, 1)
	if err != nil {
		return admittedRun{}, nil, err
	}
	defer release()
	inst.SetWeights(m.reserveWeights(ctx, req, selection))
	select {
	case m.memoryAdmission <- struct{}{}:
	case <-ctx.Done():
		return admittedRun{}, nil, ctx.Err()
	}
	defer func() { <-m.memoryAdmission }()
	if blocker, err := m.startingBlocker(inst.ID, selection.key, declared); err != nil || blocker != nil {
		return admittedRun{}, blocker, err
	}
	run, err := m.admitRun(ctx, inst, req, selection, declared)
	return run, nil, err
}

// watchRun records a detached launch job before callers can reuse the run.
func (m *ProviderAppManager) watchRun(inst *instance.Instance, req EnsureRequest) {
	if m.jobsRegistry == nil {
		return
	}
	h, err := m.jobsRegistry.StartDetached(jobs.KindRun, "", jobs.Meta{
		"run_id":   inst.ID,
		"origin":   req.Origin,
		"provider": req.Provider,
		"model":    req.Model,
		"port":     inst.Port,
	})
	if err != nil {
		slog.Warn("failed to open run job handle; continuing without SSE", "instance", inst.ID, "error", err)
		return
	}
	inst.StreamJobID = h.ID()
	inst.TrackGoroutine()
	go func() {
		defer inst.GoroutineDone()
		defer utils.RecoverAndLog("prov_apps.watchInstanceReadiness")
		m.watchInstanceReadiness(inst, h)
	}()
}

// startRun spawns an admitted run's lifecycle: launch, reaper, health monitor.
func (m *ProviderAppManager) startRun(inst *instance.Instance, run admittedRun) {
	inst.TrackGoroutine()
	go func() {
		defer inst.GoroutineDone()
		defer utils.RecoverAndLog("prov_apps.runInstanceLifecycle")
		m.runInstanceLifecycle(inst, run)
	}()
}

// launchBudget returns the memory budget a launch is measured against: nil
// when the provider declares none, or an external one nothing here measures.
func launchBudget(declared *schema.ProviderSchema) *schema.MemoryBudget {
	if declared == nil || declared.Diagnostics == nil {
		return nil
	}
	budget := declared.Diagnostics.Memory
	if budget == nil || budget.Kind == schema.MemoryExternal {
		return nil
	}
	return budget
}

// startingBlocker returns an admitted run still starting under a measured
// budget: its memory claim is not visible yet, so nothing else is measured
// until it leaves starting.
func (m *ProviderAppManager) startingBlocker(id, provider string, declared *schema.ProviderSchema) (*instance.Instance, error) {
	for _, run := range m.instances.List() {
		if run.ID == id || run.GetStatus() != instance.StatusStarting || !run.Admitted() {
			continue
		}
		key, _, ok := m.resolveConfigKey(run.Provider)
		if !ok {
			continue
		}
		pending := declared
		if key != provider {
			var err error
			pending, err = m.launchSchema(key)
			if err != nil {
				return nil, err
			}
		}
		if launchBudget(pending) != nil {
			return run, nil
		}
	}
	return nil, nil //nolint:nilnil // no blocker is not an error
}

func visibleDeviceEnvironment(environment map[string]string) (map[string]string, error) {
	env, err := process.ChildEnvironment(environment)
	if err != nil {
		return nil, err
	}
	visible := make(map[string]string)
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		if key == "CUDA_VISIBLE_DEVICES" {
			visible[key] = value
		}
	}
	return visible, nil
}

// admitMemory checks a launch's GPU memory budget, evicting idle runs when
// the node allows it, and returns the runs it evicted.
func (m *ProviderAppManager) admitMemory(ctx context.Context, features LaunchParameters) ([]instance.EvictedRun, error) {
	_, err := m.checkMemory(ctx, features, false)
	var fit *process.MemoryFitError
	settings, settingsErr := config.ParseRunSettings(features.Params, m.idleEvictionDefault)
	if settingsErr != nil {
		return nil, fmt.Errorf("%w: %w", ErrParameterValidation, settingsErr)
	}
	if !settings.EvictIdle || !errors.As(err, &fit) {
		return nil, err
	}
	evicted, skipped := m.reclaimFor(ctx, fit)
	if len(evicted) > 0 {
		_, err = m.checkMemory(ctx, features, false)
	}
	if errors.As(err, &fit) {
		for n, holder := range fit.Holders {
			fit.Holders[n].ReclaimSkippedReason = skipped[holder.Run]
		}
	}
	return evicted, err
}

func (m *ProviderAppManager) memoryRuns() []process.RunRef {
	var runs []process.RunRef
	for _, run := range m.instances.List() {
		if run.GetStatus().IsTerminal() && !run.StopUnconfirmed.Load() && len(run.Resolved().Commitments) == 0 {
			continue
		}
		runs = append(runs, process.RunRef{ID: run.ID, Model: run.Model, Provider: run.Key().Provider, PID: run.GetProcessID(), Commitments: run.Resolved().Commitments})
	}
	return runs
}

// runInstanceLifecycle manages the launched process lifecycle in a goroutine.
// It launches the process, starts health monitoring, and waits for process exit.
func (m *ProviderAppManager) runInstanceLifecycle(inst *instance.Instance, run admittedRun) {
	defer inst.ReleaseMemory()
	// Wrap log file with readiness writer if probe is configured.
	// This scans process output for patterns like "INFO - Starting httpd"
	// to detect readiness faster than HTTP polling.
	var logWriter io.Writer = run.logFile
	var readinessWriter *health.ReadinessWriter
	if run.health.ReadinessProbe != nil {
		readinessWriter = health.NewReadinessWriter(run.logFile, run.health.ReadinessProbe, run.placement, func(index int, values []string) { recordPlacement(inst, index, values) })
		logWriter = readinessWriter
	}

	if m.seams.beforeLaunch != nil {
		m.seams.beforeLaunch()
	}
	if inst.Context().Err() != nil { // stopped before its process: spawn nothing, fail nothing
		run.logFile.Close()
		return
	}
	// Launch process (exec.Command, not CommandContext — process lifetime managed by Terminate)
	result, err := m.launcher.Launch(m.shutdownCtx, inst, run.cmd, run.args, run.vars, logWriter, run.transform)
	if err != nil {
		run.logFile.Close()
		inst.MarkFailedError(err)
		m.emitEvent(failedEvent(inst, obsruns.FailureLaunchErr, err))
		return
	}

	if m.seams.processStarted != nil {
		m.seams.processStarted(result)
	}
	inst.SetJobHandle(result.JobHandle)
	inst.SetStdinPipe(result.StdinPipe)
	inst.SetProcessInfo(result.PID, result.ProcessGroupID, result.StartToken, result.Command) // last: a stop claims the recorded identity
	// A stop that ran before the pid was recorded saw no process; this goroutine signals it instead.
	if inst.Context().Err() != nil {
		if pid, startToken := inst.ClaimTermination(false); pid > 0 {
			inst.TrackGoroutine()
			go func() {
				defer inst.GoroutineDone()
				if err := m.terminateProcess(context.Background(), inst, pid, startToken); err != nil {
					slog.Warn("failed to stop a process launched during its stop", "instance", inst.ID, "pid", pid, "error", err)
				}
			}()
		}
	}

	// Track PID on disk so orphan cleanup can find it after a crash
	if m.pidTracker != nil {
		if trackErr := m.pidTracker.Track(inst.ID, result.PID, result.StartToken); trackErr != nil {
			slog.Warn("failed to track PID", "instance", inst.ID, "pid", result.PID, "error", trackErr)
		}
	}

	// Monitor readiness probe in a sub-goroutine (if configured)
	if readinessWriter != nil {
		inst.TrackGoroutine()
		go func() {
			defer inst.GoroutineDone()
			defer utils.RecoverAndLog("prov_apps.watchReadiness")
			m.watchReadiness(inst, readinessWriter, run.health.ReadinessProbe)
		}()
	}

	// Start HTTP health monitor in a sub-goroutine (uses inst context, cancelled on stop)
	inst.TrackGoroutine()
	go func() {
		defer inst.GoroutineDone()
		defer utils.RecoverAndLog("prov_apps.healthMonitor")
		m.healthMon.Run(inst.Context(), inst, run.health, func(run *instance.Instance) {
			m.publishRunning(run, true, "healthy")
		})
	}()

	// Reaper — blocks until process exits. This is the ONLY place that waits on the process.
	state, waitErr := result.Wait()
	if waitErr != nil {
		slog.Debug("process exited with error", "instance", inst.ID, "pid", result.PID, "error", waitErr)
	} else if state != nil && !state.Success() {
		slog.Debug("process exited with non-zero status", "instance", inst.ID, "pid", result.PID, "exit_code", state.ExitCode())
	}

	// Process has exited — remove PID file regardless of how it exited.
	// StopInstance also calls Untrack, but if the process crashed or was OOM-killed
	// without going through StopInstance, this is the only cleanup path.
	if m.pidTracker != nil {
		m.pidTracker.Untrack(inst.ID)
	}

	// Process has exited — safe to close the log file
	run.logFile.Close()

	// Mark failed if not already stopped/failed by StopInstance or health monitor.
	// A running instance whose process exits without going through
	// StopInstance is conceptually a stop with reason=crash for the
	// dashboard view; emit Stopped (not Failed) so the lifecycle
	// metric tracks "this deployment is no longer up" cleanly.
	// Failed remains for pre-running launch errors.
	status := inst.GetStatus()
	if status != instance.StatusStopping && status != instance.StatusStopped {
		var exitCode *int
		signal := ""
		if state != nil {
			if code := state.ExitCode(); code >= 0 {
				exitCode = &code
			} else {
				signal, _ = strings.CutPrefix(state.String(), "signal: ")
			}
		}
		inst.MarkFailedWithExit("process exited unexpectedly", exitCode, signal)
		if status == instance.StatusFailed {
			return
		}
		m.emitEvent(stoppedEvent(inst, obsruns.StopReasonCrash, "process exited unexpectedly"))
	}
}

// watchReadiness monitors the readiness writer signal channel for log-based
// readiness or failure detection. On success, transitions to running immediately.
// On failure, marks the instance as failed. Times out after probe.Timeout.
// defaultReadinessTimeout applies when a provider declares a readiness
// probe without a timeout of its own.
const defaultReadinessTimeout = 120 * time.Second

// serveCheckPollInterval paces serve-check retries while an instance is
// still loading. Short enough that a fast local engine is marked ready
// promptly, long enough that a slow one isn't hammered with generations.
const serveCheckPollInterval = 2 * time.Second

func (m *ProviderAppManager) watchReadiness(inst *instance.Instance, rw *health.ReadinessWriter, probe *health.ReadinessProbe) {
	absoluteDeadline := utils.Now().Add(probe.Timeout)
	timer := time.NewTimer(probe.Timeout)
	defer timer.Stop()
	poll := time.NewTimer(serveCheckPollInterval)
	defer poll.Stop()
	poll.Stop()
	var pollC <-chan time.Time
	if probe.Serve != nil && len(probe.LogPatterns.Success) == 0 {
		poll.Reset(0)
		pollC = poll.C
	}
	readyDetail := ""

	for {
		if inst.GetStatus() != instance.StatusStarting {
			return
		}
		now := utils.Now()
		deadline := absoluteDeadline
		if probe.StallTimeout > 0 {
			stallDeadline := rw.LastOutputAt().Add(probe.StallTimeout)
			if stallDeadline.Before(deadline) {
				deadline = stallDeadline
			}
		}
		if !now.Before(deadline) {
			reason := fmt.Sprintf("not ready within %s", probe.Timeout)
			if deadline.Before(absoluteDeadline) {
				reason = fmt.Sprintf("no output for %s while starting; last line: %s", probe.StallTimeout, rw.LastLine())
			}
			m.failNotReady(inst, reason)
			return
		}
		if readyDetail != "" {
			m.publishRunning(inst, true, fmt.Sprintf("Ready (%s)", readyDetail))
			return
		}
		timer.Reset(deadline.Sub(now))
		select {
		case sig := <-rw.Signal:
			if sig.Failed {
				m.failReadiness(inst, sig)
				return
			}
			if sig.Ready {
				slog.Info("readiness probe matched", "instance", inst.ID, "pattern", sig.Pattern)
				if probe.Serve == nil {
					readyDetail = fmt.Sprintf("log: %s", sig.Pattern)
				} else {
					poll.Reset(0)
					pollC = poll.C
				}
			}
		case <-pollC:
			// A serve request must not outlive the current startup window.
			ctx, cancel := context.WithDeadline(inst.Context(), deadline)
			if m.healthMon.Serves(ctx, inst, probe.Serve) {
				readyDetail = fmt.Sprintf("served %s", probe.Serve.Path)
			}
			cancel()
			poll.Reset(serveCheckPollInterval)
		case <-timer.C:
		case <-inst.LeftStarting():
			return
		case <-inst.Context().Done():
			return
		}
	}
}

func (m *ProviderAppManager) failReadiness(inst *instance.Instance, sig health.ReadinessSignal) {
	slog.Warn("readiness probe failure matched", "instance", inst.ID, "pattern", sig.Pattern)
	if sig.Cause != nil {
		inst.MarkFailedError(sig.Cause)
	} else {
		inst.MarkFailed(fmt.Sprintf("readiness probe failure: %s", sig.Pattern))
	}
	m.emitEvent(failedEvent(inst, obsruns.FailureReadiness, fmt.Errorf("readiness failure (log: %s)", sig.Pattern)))
}

// failNotReady closes the startup window on an instance that never came
// up. Only readiness can do this: the health monitor ignores failures
// while an instance is starting, precisely so a slow load isn't mistaken
// for a broken one, which leaves nobody else to notice a launch that
// simply never arrives. A no-op once the instance moved on.
func (m *ProviderAppManager) failNotReady(inst *instance.Instance, reason string) {
	if inst.GetStatus() != instance.StatusStarting {
		return
	}
	slog.Warn("instance never became ready", "instance", inst.ID, "reason", reason)
	inst.MarkFailed(reason)
	m.emitEvent(failedEvent(inst, obsruns.FailureReadiness, errors.New(reason)))
}

// StopInstance gracefully stops an instance and removes it from the registry.
// Sequence: cancel monitor and keep-alive → terminate process → drain → cleanup.
func (m *ProviderAppManager) StopInstance(ctx context.Context, id string) error {
	return m.stopInstance(ctx, id, obsruns.StopReasonUser)
}

// stopInstance is StopInstance with the reason its stopped event carries.
func (m *ProviderAppManager) stopInstance(ctx context.Context, id string, reason obsruns.StopReason) error {
	inst, ok := m.instances.Get(id)
	if !ok {
		return ErrInstanceNotFound
	}
	return m.endRun(ctx, inst, reason, m.runEngine(inst).down)
}

// endRun stops a run: down ends its model (nil leaves it loaded), then the run drains and leaves the registry.
func (m *ProviderAppManager) endRun(ctx context.Context, inst *instance.Instance, reason obsruns.StopReason, down func(context.Context, *instance.Instance) error) (result error) {
	id := inst.ID
	if !inst.ClaimEnd() {
		return &instance.AdmissionError{Reason: instance.ErrStopping, RetryAfter: instance.AdmissionRetryAfter}
	}
	inst.StopUnconfirmed.Store(true)
	defer func() {
		if inst.StopUnconfirmed.Store(result != nil); result != nil {
			inst.ReleaseEnd()
		}
	}()

	// Mark stopping
	inst.SetStatus(instance.StatusStopping)
	m.emitEvent(runEvent(EventInstanceStopping, inst, "Stopping instance"))

	// Stop background activity even if the engine cannot end the model.
	inst.Cancel()
	if down != nil {
		if err := down(ctx, inst); err != nil {
			return err
		}
	}

	// Drain the sole process reaper before releasing runtime ownership or files.
	if err := inst.WaitForGoroutines(ctx); err != nil {
		return fmt.Errorf("drain goroutines: %w", err)
	}
	inst.SetStatus(instance.StatusStopped)

	// Release Windows Job Object handle (no-op if zero / non-Windows)
	if jh := inst.GetJobHandle(); jh != 0 {
		process.CloseJobHandle(jh)
		inst.SetJobHandle(0)
	}

	// Cleanup
	if m.pidTracker != nil {
		m.pidTracker.Untrack(id)
	}
	inst.CloseLogs()
	if err := m.instances.Remove(id); err != nil {
		slog.Warn("failed to remove instance from registry", "id", id, "error", err)
	} else {
		m.ports.ReleaseForProvider(inst.Provider, inst.Port)
	}

	// Emit event
	m.record(inst, stoppedEvent(inst, reason, fmt.Sprintf("stopped (%s)", reason)))

	return nil
}

// terminateProcess ends a run's process tree, gracefully first, within the
// provider's shutdown_timeout.
func (m *ProviderAppManager) terminateProcess(ctx context.Context, inst *instance.Instance, pid int, startToken int64) error {
	req := process.TerminateRequest{
		PID:            pid,
		StartToken:     startToken,
		Inspector:      m.seams.inspector,
		ProcessGroupID: inst.GetProcessGroupID(),
		Graceful:       m.resolveShutdownTimeout(inst.Provider),
		JobHandle:      inst.GetJobHandle(),
	}
	inst.CloseStdinPipe() // idempotent; the request leaves StdinPipe nil so it is not closed twice
	return process.Terminate(ctx, req)
}

// RestartInstance stops a running instance and launches a new one with the
// same provider, model, parameters, and environment. The port is intentionally
// not preserved — the new instance receives a freshly allocated port — and
// the new instance ID is different from the old one. Returns the new
// instance, or an error if either the stop or the relaunch fails.
func (m *ProviderAppManager) RestartInstance(ctx context.Context, id string) (*instance.Instance, error) {
	if m.shutdown.Load() {
		return nil, ErrShutdown
	}

	inst, ok := m.instances.Get(id)
	if !ok {
		return nil, ErrInstanceNotFound
	}

	// Snapshot launch parameters under the instance lock before stopping —
	// SnapshotConfig clones the maps, so the relaunch is unaffected if the
	// stop path mutates or clears the source instance's Config.
	req := restartRequest(inst)
	if req.Provider == "" {
		return nil, fmt.Errorf("instance %q has incomplete launch config (provider=%q model=%q); cannot restart", id, req.Provider, req.Model)
	}

	req.Force = true
	result, err := m.EnsureRun(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("relaunch %s/%s: %w", req.Provider, req.Model, err)
	}
	return result.Run, nil
}

// shuttingDown reports that Stop has begun or finished. Stop sets stopping
// before it ends any run, so a caller that sees a run stopping for shutdown
// also sees this.
func (m *ProviderAppManager) shuttingDown() bool {
	return m.stopping.Load() || m.shutdown.Load()
}

// Stop gracefully stops all instances and releases resources.
//
// Sequence:
//  1. Set shutdown flag
//  2. Cancel shutdownCtx (stops keepalive, health monitors)
//  3. Stop all running instances
//  4. Close log manager
//  5. Close event channel
func (m *ProviderAppManager) Stop(ctx context.Context) error {
	if m == nil {
		return nil
	}
	// `stopping` gates concurrent Stop() callers. `shutdown` flips
	// only AFTER the per-instance stop loop so emitEvent keeps accepting
	// each StopInstance's EventStopped — otherwise the audit trail loses
	// every shutdown-time termination.
	// Serialize feature-install admission with the shutdown drain.
	m.mu.Lock()
	beginStop := m.stopping.CompareAndSwap(false, true)
	m.mu.Unlock()
	if !beginStop {
		return nil
	}

	// Background starts end before the instance drain, so none registers a run after it.
	m.startsCancel()
	var stopErr error
	if m.reaperDone != nil {
		select {
		case <-m.reaperDone:
		case <-ctx.Done():
			stopErr = ctx.Err()
		}
	}
	done := make(chan struct{})
	go func() {
		m.onDemandStarts.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		stopErr = ctx.Err()
	}
	for _, inst := range m.instances.List() {
		if err := ctx.Err(); err != nil {
			slog.Warn("provider manager shutdown interrupted by caller deadline",
				"instances_remaining", len(m.instances.List()),
				"error", err)
			stopErr = err
			break
		}
		eng := m.runEngine(inst)
		down := eng.down
		if eng.outlivesNode() {
			down = nil
		}
		// A stop already in progress is its caller's to finish.
		if err := m.endRun(ctx, inst, obsruns.StopReasonUser, down); err != nil && !errors.Is(err, instance.ErrStopping) {
			stopErr = err
			slog.Warn("failed to stop instance during shutdown",
				"id", inst.ID, "error", err)
		}
	}

	// Drain any in-flight async installs BEFORE flipping the shutdown
	// gate. On ctx timeout we warn + orphan — force-killing a pip
	// install halfway leaves a dirty venv; pkg/jobs' inactivity-gated
	// TTL reaps the stale handle on next startup.
	if m.installs != nil {
		if err := m.installs.WaitForInstalls(ctx); err != nil {
			slog.Warn("async install goroutines did not drain before shutdown deadline",
				"error", err)
			if stopErr == nil {
				stopErr = err
			}
		}
	}

	// Gate further event emission. Any emitEvent calls that raced past
	// the .Load() check fall through the non-blocking default branch;
	// we deliberately do NOT close m.auditEvents — the Load→send
	// sequence is non-atomic and closing between the check and the
	// send would panic. The channel is GC'd when the manager goes
	// out of scope.
	m.shutdown.Store(true)

	// Cancel shutdownCtx so eventLoop, idleReaper, and any in-flight
	// detection exit promptly.
	m.shutdownCancel()

	if err := m.stopServices(ctx); err != nil && stopErr == nil {
		stopErr = err
	}

	return stopErr
}

// --- Helpers ---

// sanitizeLogFragment strips every character outside the allowlist
// [A-Za-z0-9._-] so a hostile or quirky model/provider id (slashes,
// backslashes, NULs, dotdot) cannot escape the log directory or create
// a filename the OS rejects. Replaces banned characters with '_';
// collapses runs of '_' to a single one; trims leading/trailing dots
// (which would create hidden files on *nix and be rejected on
// Windows).
func sanitizeLogFragment(s string) string {
	if s == "" {
		return "unknown"
	}
	out := make([]byte, 0, len(s))
	lastUnderscore := false
	for i := 0; i < len(s); i++ {
		b := s[i]
		ok := (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') ||
			(b >= '0' && b <= '9') || b == '.' || b == '-'
		if ok {
			out = append(out, b)
			lastUnderscore = false
			continue
		}
		if !lastUnderscore {
			out = append(out, '_')
			lastUnderscore = true
		}
	}
	// Trim leading / trailing dots and underscores.
	start, end := 0, len(out)
	for start < end && (out[start] == '.' || out[start] == '_') {
		start++
	}
	for end > start && (out[end-1] == '.' || out[end-1] == '_') {
		end--
	}
	if start == end {
		return "unknown"
	}
	return string(out[start:end])
}

// buildHealthCheckConfig maps provider config to a health.CheckConfig.
// port is accepted for symmetry with providers that embed the port in a
// non-default URL template; the default /health probe uses the instance's
// bound port at dial time, not a template substitution.
func buildHealthCheckConfig(svcCfg config.ServiceConfig, _ int) health.CheckConfig {
	hc := health.CheckConfig{
		HTTPHealthPath: "/health",
		ExpectedStatus: 200,
	}

	if svcCfg.Runtime == nil {
		return hc
	}

	hc.ActivityProbe = svcCfg.Runtime.ActivityProbe
	rc := svcCfg.Runtime.HealthCheck
	if rc.Path != "" {
		hc.HTTPHealthPath = rc.Path
	}
	if rc.Interval != "" {
		if d, err := time.ParseDuration(rc.Interval); err == nil {
			hc.Interval = d
		}
	}
	if rc.Timeout != "" {
		if d, err := time.ParseDuration(rc.Timeout); err == nil {
			hc.Timeout = d
		}
	}
	if rc.Retries > 0 {
		hc.MaxRetries = rc.Retries
	}

	// Map readiness probe from config
	if rp := rc.ReadinessProbe; rp != nil && (len(rp.LogPatterns.Success)+len(rp.LogPatterns.Failure) > 0 || rp.Serve != nil) {
		stall, err := rp.StallDuration()
		if err != nil {
			slog.Warn("invalid readiness probe config, skipping", "error", err)
			return hc
		}
		probe := &health.ReadinessProbe{Timeout: readinessTimeout(rp.Timeout), StallTimeout: stall, FailureClassifier: &svcCfg.Runtime.FailurePatterns}
		for _, p := range rp.LogPatterns.Success {
			probe.LogPatterns.Success = append(probe.LogPatterns.Success, health.PatternMatcher{
				Pattern: p.Pattern,
				IsRegex: p.IsRegex,
			})
		}
		for _, p := range rp.LogPatterns.Failure {
			probe.LogPatterns.Failure = append(probe.LogPatterns.Failure, health.PatternMatcher{
				Pattern: p.Pattern,
				IsRegex: p.IsRegex,
			})
		}
		if sc := rp.Serve; sc != nil {
			probe.Serve = &health.ServeCheck{Path: sc.Path, Method: sc.Method, Body: sc.Body}
		}
		if err := probe.Validate(); err == nil {
			hc.ReadinessProbe = probe
		} else {
			slog.Warn("invalid readiness probe config, skipping", "error", err)
		}
	}

	return hc
}

// restartRequest replays the requested shape and overrides while re-reading current provider tiers.
func restartRequest(inst *instance.Instance) EnsureRequest {
	cfg := inst.SnapshotConfig()
	req := EnsureRequest{
		Runtime: cfg.Runtime, DisposablePlanID: cfg.DisposablePlanID,
		Provider:   cfg.Provider,
		Model:      cfg.Model,
		Endpoint:   Endpoint(cfg.Endpoint),
		Parameters: cfg.Parameters,
		EnvVars:    cfg.EnvVars,
	}
	if req.Endpoint == "" {
		req.Endpoint = Endpoint(inst.Endpoint)
	}
	if inst.StartedBy == instance.StartedByRequest {
		req.Origin = OriginRequest
	}
	return req
}

func (m *ProviderAppManager) resolveRawLaunchParams(svcCfg config.ServiceConfig, req EnsureRequest, endpoint string) (params, env, sources map[string]string) {
	resolved := svcCfg.ResolveEndpoint(m.localNodename(), req.Model, endpoint)
	env = mergeStringMaps(config.FlattenEnvironment(resolved.Environment), req.EnvVars)
	params = mergeStringMaps(config.FlattenParameters(resolved.Parameters), req.Parameters)
	sources = make(map[string]string, len(params))
	for name, value := range resolved.Parameters {
		sources[name] = value.Tier.String()
	}
	for name := range req.Parameters {
		sources[name] = config.TierRequest.String()
	}
	return params, env, sources
}

// buildLaunch turns resolved parameters into the engine's command: it
// validates what the operator configured, has the node's ParamLocalizer
// rewrite it into node-local values (such as asset paths), spells auto
// from the endpoint schema, then builds argv. Validation checks the configured
// values, not what they become here. It also returns the digests of the
// files the localizer resolved.
//
// Split out from EnsureRun so the parameters-to-argv contract can be
// tested without starting a process.
func (m *ProviderAppManager) buildLaunch(svcCfg config.ServiceConfig, provider, endpoint string, model process.ResolvedModel, port int, params map[string]string, declared *schema.ProviderSchema) (process.Launch, map[string]string, error) {
	if _, err := config.ParseRunSettings(params, m.idleEvictionDefault); err != nil {
		return process.Launch{}, nil, fmt.Errorf("%w: %w", ErrParameterValidation, err)
	}
	if err := process.ValidateParameters(params); err != nil {
		return process.Launch{}, nil, fmt.Errorf("%w: %w", ErrParameterValidation, err)
	}
	var files map[string]string
	shapes := declared.ForEndpoint(endpoint)
	if m.localizeParams != nil {
		local, err := m.localizeParams(provider, endpoint, model.Name, params, shapes)
		if err != nil {
			return process.Launch{}, nil, fmt.Errorf("%w: on node %q: %w", ErrParameterValidation, m.localNodename(), err)
		}
		params, files = local.Params, local.Files
	}
	params = maps.Clone(params)
	for key, shape := range shapes {
		if config.IsAuto(params[key]) && shape.Auto != "" {
			params[key] = shape.Auto
		}
	}
	launch, err := process.NewCommandBuilder(&svcCfg).Build(model, port, params)
	if err != nil {
		return process.Launch{}, nil, fmt.Errorf("failed to build command: %w", err)
	}
	return launch, files, nil
}

// preparedLaunch is a launch worked out from config but not started.
type preparedLaunch struct {
	parameters           LaunchParameters
	executionEnvironment process.EnvironmentTransform
	launch               process.Launch
	resolved             instance.Resolved
	config               config.ServiceConfig
}

// LaunchPreview carries argv and metadata from one launch preparation.
type LaunchPreview struct {
	process.Launch
	LaunchParameters
	Model      string
	Assessment process.MemoryAssessment
}

// PreviewLaunch renders the preparation a start would make, without starting anything.
func (m *ProviderAppManager) PreviewLaunch(ctx context.Context, req EnsureRequest, port int) (LaunchPreview, error) {
	key, svc, ok := m.resolveConfigKey(req.Provider)
	if !ok {
		return LaunchPreview{}, ErrProviderNotFound
	}
	if err := svc.ValidateRunTarget(req.Model, string(req.Endpoint)); err != nil {
		return LaunchPreview{}, fmt.Errorf("%w: %w", ErrParameterValidation, err)
	}
	req.Model = m.runModel(key, svc, req.Model)
	prepared, err := m.engine(Lifecycle(svc)).prepare(ctx, svc, key, req, port)
	preview := LaunchPreview{Launch: prepared.launch, LaunchParameters: prepared.parameters, Model: req.Model}
	if err == nil {
		preview.Assessment, _ = m.checkMemory(ctx, prepared.parameters, true)
	}
	return preview, err
}

func (m *ProviderAppManager) prepareLaunchFeatures(features LaunchParameters, argvParams map[string]string, configKey, endpoint string, port int) (preparedLaunch, error) {
	params, env, svcCfg := features.Params, features.Environment, features.Config
	budget := launchBudget(features.declared)
	if budget != nil && budget.Kind == schema.MemoryReserveMiB {
		argvParams = maps.Clone(argvParams)
		delete(argvParams, budget.Parameter)
	}
	launch, files, err := m.buildLaunch(svcCfg, configKey, endpoint, features.model, port, argvParams, features.declared)
	if err != nil {
		return preparedLaunch{}, err
	}
	launch.Command = features.ExecutionCommand(configKey)
	runtime := features.RuntimeKey
	if runtime == "" {
		runtime = svcCfg.Name
	}
	states, probes := m.launchFeatureEvidence(configKey, svcCfg.Features, params, features.declared.ForEndpoint(endpoint))
	return preparedLaunch{
		launch: launch, config: svcCfg, parameters: features, executionEnvironment: features.ExecutionEnvironment,
		resolved: instance.Resolved{Features: states, FeatureHTTP: probes, WorkingDir: launch.WorkingDir, Commitments: features.Commitments, Runtime: runtime, Parameters: params, Environment: env, Files: mergeStringMaps(files, features.Files), Execution: executionDigest(svcCfg, features.RuntimeKey, launch.Command), WireEndpoints: wireEndpoints(svcCfg)},
	}, nil
}

// mergeStringMaps merges two string maps, with overrides taking precedence.
// Returns a new map — neither input is modified.
func mergeStringMaps(defaults, overrides map[string]string) map[string]string {
	if len(defaults) == 0 && len(overrides) == 0 {
		return nil
	}

	result := make(map[string]string, len(defaults)+len(overrides))
	maps.Copy(result, defaults)
	maps.Copy(result, overrides)
	return result
}

// resolveShutdownTimeout returns the graceful shutdown timeout for a provider.
// Reads shutdown_timeout from provider config defaults. Supports:
//   - "gpu" → 30s (GPU providers need time to flush memory)
//   - Duration string (e.g., "15s", "1m") → parsed duration
//   - Empty → process.GracefulTimeout (10s default)
func (m *ProviderAppManager) resolveShutdownTimeout(provider string) time.Duration {
	_, svcCfg, ok := m.resolveConfigKey(provider)
	if !ok {
		return process.GracefulTimeout
	}

	hint := svcCfg.GetShutdownTimeout()
	switch hint {
	case "":
		return process.GracefulTimeout
	case "gpu":
		return 30 * time.Second
	default:
		if d, err := time.ParseDuration(hint); err == nil {
			return d
		}
		return process.GracefulTimeout
	}
}

// readinessTimeout parses a probe timeout, falling back to a window that
// covers a cold engine start without stranding a launch forever.
func readinessTimeout(raw string) time.Duration {
	if d, err := time.ParseDuration(raw); err == nil && d > 0 {
		return d
	}
	return defaultReadinessTimeout
}
