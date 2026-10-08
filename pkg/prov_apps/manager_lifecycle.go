package prov_apps

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"strings"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/discovery/gpu"
	"github.com/stperic/zzrouter/pkg/jobs"
	obsruns "github.com/stperic/zzrouter/pkg/observability/runs"
	"github.com/stperic/zzrouter/pkg/prov_apps/health"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
	"github.com/stperic/zzrouter/pkg/utils"
)

// LaunchInstance creates and starts a new provider instance.
// It allocates a port, builds the command from config, launches the process,
// and starts health monitoring. The process runs until explicitly stopped.
//
// Concurrent callers for the same (provider, model) are coalesced through
// launchGroup: the first caller runs the full launch flow, and subsequent
// callers wait on the singleflight and receive the same *Instance back.
// Before entering the launch flow, GetByModel is re-checked so a caller
// that arrives after a terminal launch completes gets the live instance
// without paying for another launch round-trip.
func (m *ProviderAppManager) LaunchInstance(ctx context.Context, req LaunchRequest) (*instance.Instance, error) {
	if m.shutdown.Load() {
		return nil, ErrShutdown
	}

	if err := m.ValidateModel(ctx, req.Model); err != nil {
		return nil, err
	}

	// Resolve the caller's name to the one the cache indexes this model
	// under, BEFORE the singleflight key and the existing-instance
	// check below are computed from it. Both are keyed by model name,
	// so canonicalizing afterwards would leave every alias form with
	// its own key -- which is how one model ended up loaded twice on
	// one node, once as its repo id and once as its file stem.
	requestedModel := req.Model
	req.Model = m.CanonicalModelName(req.Model)

	// Collapse endpoint to "chat" when the provider's config declares no
	// endpoints overlays — single instance per (provider, model) for
	// vLLM / MLX / cloud providers. Providers that DO declare overlays
	// keep their per-endpoint key, spawning separate instances.
	endpoint := string(EndpointOrDefault(req.Endpoint))
	if _, svcCfg, ok := m.resolveConfigKey(req.Provider); ok && !svcCfg.IsEndpointAware() {
		endpoint = string(EndpointChat)
	}
	req.Endpoint = Endpoint(endpoint)
	if req.installSmoke {
		// The install owns its runtime gate; joining a launch waiting on it deadlocks.
		if run, ok := m.instances.GetByModelEndpoint(req.Model, endpoint); ok && !run.GetStatus().IsTerminal() {
			return nil, fmt.Errorf("%w: smoke cannot reuse an existing run", ErrInstancesRunning)
		}
		admitted, err := m.AdmitLocalModel(ctx, requestedModel)
		if err != nil {
			return nil, err
		}
		return m.launchInstanceLocked(admitted, req)
	}
	key := req.Provider + ":" + req.Model + ":" + endpoint
	result, err, _ := m.launchGroup.Do(key, func() (any, error) {
		// Fast path: a previous caller may have completed their launch
		// while we waited on the singleflight. Endpoint must match —
		// returning a chat instance to an embeddings caller would route
		// /v1/embeddings to a server that 501s.
		if existing, found := m.instances.GetByModelEndpoint(req.Model, endpoint); found {
			if req.Provider == "" || existing.Provider == req.Provider {
				status := existing.GetStatus()
				if status != instance.StatusFailed && status != instance.StatusStopped {
					config := existing.SnapshotConfig()
					if config.DisposablePlanID != req.DisposablePlanID || config.Runtime != req.Runtime {
						return nil, fmt.Errorf("%w: stop the existing run before changing its runtime", ErrInstancesRunning)
					}
					return existing, nil
				}
			}
		}
		admitted, err := m.AdmitLocalModel(ctx, requestedModel)
		if err != nil {
			return nil, err
		}
		return m.launchInstanceLocked(admitted, req)
	})
	if err != nil {
		return nil, err
	}
	inst := result.(*instance.Instance) //nolint:errcheck // singleflight returns the exact type the fn returned
	launchConfig := inst.SnapshotConfig()
	if launchConfig.Runtime != req.Runtime || launchConfig.DisposablePlanID != req.DisposablePlanID {
		return nil, fmt.Errorf("%w: concurrent launch selected a different runtime; stop it before retrying", ErrInstancesRunning)
	}
	return inst, nil
}

// launchInstanceLocked is the inner body of LaunchInstance, held under
// the launchGroup singleflight so only one per (provider, model) key
// runs at a time. Split out so LaunchInstance stays focused on the
// coalescing contract.
func (m *ProviderAppManager) launchInstanceLocked(ctx context.Context, req LaunchRequest) (*instance.Instance, error) { //nolint:gocyclo,cyclop // Launch admission, resource allocation and failure cleanup share one ordered flow.
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Refuse a busy admission before config or feature work, without holding it across an install gate.
	if m.memoryBudget != nil {
		if !m.memoryLaunchMu.TryLock() {
			return nil, ErrAtCapacity
		}
		m.memoryLaunchMu.Unlock()
	}

	// Resolve provider config
	configKey, svcCfg, ok := m.resolveConfigKey(req.Provider)
	if !ok {
		return nil, fmt.Errorf("%w: %q not found in config", ErrProviderNotFound, req.Provider)
	}
	// Freeze the selected runtime before locking; toolkit reads happen under its gate.
	_, selected, _, err := m.featureRuntime(svcCfg, req.Model, req.Runtime)
	if err != nil {
		return nil, err
	}
	if selected == "" {
		selected = svcCfg.Name
	}
	req.selectedRuntime = selected
	if req.installSmoke && selected != req.Runtime {
		return nil, fmt.Errorf("smoke model does not use the runtime being installed")
	}
	if !req.installSmoke {
		unlock, err := m.installs.acquireProvider(ctx, selected)
		if err != nil {
			return nil, err
		}
		defer unlock()
	}

	if m.memoryBudget != nil {
		// Register and admit together; a pending GPU allocation remains visible to the next launch.
		if !m.memoryLaunchMu.TryLock() {
			return nil, fmt.Errorf("%w: another launch is being admitted; retry when it becomes ready or stops", ErrAtCapacity)
		}
		defer m.memoryLaunchMu.Unlock()
	}
	// Allocate port
	allocatedPort, err := m.ports.AllocateForProvider(req.Provider, req.Port)
	if err != nil {
		return nil, fmt.Errorf("port allocation failed: %w", err)
	}

	// Determine keep-alive from config (default: 5 minutes)
	keepAlive := 5 * time.Minute
	if svcCfg.Runtime != nil && svcCfg.Runtime.KeepAlive != "" {
		if d, err := time.ParseDuration(svcCfg.Runtime.KeepAlive); err == nil {
			keepAlive = d
		}
	}

	// Create instance
	endpoint := string(EndpointOrDefault(req.Endpoint))
	id := instance.GenerateID(req.Provider, req.Model, endpoint)
	inst := instance.NewInstance(id, req.Provider, req.Model, allocatedPort, keepAlive, 0)
	inst.Endpoint = endpoint
	inst.StopUnconfirmed.Store(req.installSmoke) // Smoke retains ownership until strict process drain.
	inst.Config = instance.Config{
		Runtime: req.Runtime, DisposablePlanID: req.DisposablePlanID,
		Provider:   req.Provider,
		LaunchMode: instance.LaunchModeNative,
		Model:      req.Model,
		Port:       allocatedPort,
		EnvVars:    req.EnvVars,
		Parameters: req.Parameters,
	}

	// Set HealthURL from provider config (not hardcoded /health)
	healthPath := "/health"
	if svcCfg.Runtime != nil && svcCfg.Runtime.HealthCheck.Path != "" {
		healthPath = svcCfg.Runtime.HealthCheck.Path
	}
	inst.HealthURL = fmt.Sprintf("http://localhost:%d%s", allocatedPort, healthPath)
	if m.modelSource != nil {
		weights, _, _ := strings.Cut(svcCfg.WeightsOf(req.Model), "#")
		inst.SourceRepo = m.modelSource(ctx, weights)
	}

	// Initialize concurrency limiter from provider config
	if svcCfg.Runtime != nil && svcCfg.Runtime.MaxConcurrentRequests > 0 {
		inst.InitConcurrencyLimit(svcCfg.Runtime.MaxConcurrentRequests, svcCfg.Runtime.GetQueueTimeout())
	}

	// Wire keep-alive expiry to the manager's idle reaper
	inst.IdleExpiry = m.idleExpiry

	// Register in registry
	if err := m.instances.Register(inst); err != nil {
		m.ports.ReleaseForProvider(req.Provider, allocatedPort)
		return nil, err
	}

	// From here, errors must clean up: remove from registry + release port
	cleanup := func() {
		if removeErr := m.instances.Remove(id); removeErr != nil {
			slog.Warn("failed to remove instance during cleanup", "id", id, "error", removeErr)
		}
		m.ports.ReleaseForProvider(req.Provider, allocatedPort)
	}

	// Create log file. Sanitize model name for filesystem safety —
	// strip every character outside [A-Za-z0-9._-] so a hostile or
	// quirky model id (slashes, backslashes, ..) cannot escape the
	// log directory.
	safeModel := sanitizeLogFragment(req.Model)
	logFile, logPath, err := m.logMgr.CreateLogFile(id, req.Provider, safeModel)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("failed to create log file: %w", err)
	}
	inst.SetLogFilePath(logPath)

	features, err := m.modelLaunchParameters(svcCfg, req, endpoint)
	if err == nil {
		err = m.admitMemory(ctx, configKey, id, instance.Resolved{Parameters: features.Params, Environment: features.Environment})
	}
	var prepared preparedLaunch
	if err == nil {
		prepared, err = m.prepareLaunchFeatures(features, configKey, endpoint, req, allocatedPort)
	}
	if err != nil {
		logFile.Close()
		cleanup()
		return nil, err
	}
	svcCfg = prepared.config
	inst.SetResolved(prepared.resolved)
	launch, mergedEnv := prepared.launch, prepared.resolved.Environment

	// An incomplete download reaches the engine as a corrupt-tensor crash
	// tens of seconds in; catching it here names the actual problem.
	if err := process.VerifyLocalWeights(ctx, launch.ModelPath); err != nil {
		logFile.Close()
		cleanup()
		return nil, err
	}

	cmd, args := launch.Command, launch.Args
	if err := ctx.Err(); err != nil {
		logFile.Close()
		cleanup()
		return nil, err
	}
	inst.WireModel = launch.WireModel

	// Build health check config from provider config
	hcConfig := buildHealthCheckConfig(svcCfg, allocatedPort)

	// Emit starting event
	m.emitEvent(Event{
		Type: EventInstanceStarting, Provider: req.Provider,
		Instance: id, Model: req.Model,
		Message: fmt.Sprintf("Starting %s on port %d", req.Model, allocatedPort),
		Time:    utils.Now(),
	})

	// Open a jobs.Handle for launch-readiness streaming before the
	// lifecycle goroutine starts so the handle's ID lands on the
	// instance struct atomically — LoadModel's 409 fast-path reads it
	// back to return the SAME job_id on retries. Handle lifecycle is
	// owned by watchInstanceReadiness below; StartDetached so the
	// async-202 launch outlives the HTTP accept path (commit fa91f367).
	if m.jobsRegistry != nil {
		h, jerr := m.jobsRegistry.StartDetached(jobs.KindRun, "", jobs.Meta{
			"run_id":   inst.ID,
			"provider": req.Provider,
			"model":    req.Model,
			"port":     allocatedPort,
		})
		if jerr == nil {
			inst.StreamJobID = h.ID()
			inst.TrackGoroutine()
			go func() {
				defer inst.GoroutineDone()
				defer utils.RecoverAndLog("prov_apps.watchInstanceReadiness")
				m.watchInstanceReadiness(inst, h)
			}()
		} else {
			slog.Warn("failed to open run job handle; continuing without SSE", "instance", id, "error", jerr)
		}
	}

	// Spawn lifecycle goroutine: launch → reaper → health monitor
	inst.TrackGoroutine()
	go func() {
		defer inst.GoroutineDone()
		defer utils.RecoverAndLog("prov_apps.runInstanceLifecycle")
		m.runInstanceLifecycle(inst, cmd, args, mergedEnv, logFile, hcConfig, prepared.executionEnvironment) //nolint:contextcheck // uses m.shutdownCtx via instance fields
	}()

	return inst, nil
}

func (m *ProviderAppManager) admitMemory(ctx context.Context, provider, id string, resolved instance.Resolved) error {
	if m.memoryBudget == nil {
		return nil
	}
	budget, err := m.memoryBudget(provider)
	if err != nil {
		return fmt.Errorf("memory budget declaration: %w", err)
	}
	if budget == nil || budget.Kind == "external" {
		return nil
	}
	for _, run := range m.instances.List() {
		if run.ID == id || run.GetStatus() != instance.StatusStarting {
			continue
		}
		key, _, ok := m.resolveConfigKey(run.Provider)
		if !ok {
			continue
		}
		pending, err := m.memoryBudget(key)
		if err != nil {
			return err
		}
		if pending == nil || pending.Kind != "external" {
			return fmt.Errorf("%w: run %s has a pending startup memory allocation; wait for readiness or stop it before another GPU launch", ErrAtCapacity, run.ID)
		}
	}
	if budget.Kind != "fraction_total" {
		return nil
	}
	env, err := process.ChildEnvironment(resolved.Environment)
	if err != nil {
		return err
	}
	visible := make(map[string]string)
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		if key == "CUDA_VISIBLE_DEVICES" {
			visible[key] = value
		}
	}
	devices, err := gpu.MemorySnapshotContext(ctx)
	if err != nil {
		return fmt.Errorf("%w: %w", process.ErrMemoryObservation, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	owners := make(map[int]string)
	for _, run := range m.instances.List() {
		if pid := run.GetProcessID(); pid > 0 {
			owners[pid] = run.ID
		}
	}
	if err := process.ResolveAutoMemory(*budget, resolved.Parameters, visible, devices); err != nil {
		return err
	}
	return process.CheckMemoryBudget(*budget, resolved.Parameters, visible, devices, owners)
}

// runInstanceLifecycle manages the launched process lifecycle in a goroutine.
// It launches the process, starts health monitoring, and waits for process exit.
func (m *ProviderAppManager) runInstanceLifecycle(
	inst *instance.Instance,
	cmd string, args []string,
	envVars map[string]string,
	logFile *os.File,
	hcConfig health.CheckConfig,
	executionEnvironment process.EnvironmentTransform,
) {
	// Wrap log file with readiness writer if probe is configured.
	// This scans process output for patterns like "INFO - Starting httpd"
	// to detect readiness faster than HTTP polling.
	var logWriter io.Writer = logFile
	var readinessWriter *health.ReadinessWriter
	if hcConfig.ReadinessProbe != nil {
		readinessWriter = health.NewReadinessWriter(logFile, hcConfig.ReadinessProbe)
		logWriter = readinessWriter
	}

	// Launch process (exec.Command, not CommandContext — process lifetime managed by Terminate)
	result, err := m.launcher.Launch(m.shutdownCtx, inst, cmd, args, envVars, logWriter, executionEnvironment)
	if err != nil {
		logFile.Close()
		inst.MarkFailed(err.Error())
		m.emitEvent(Event{
			Type: EventInstanceFailed, Provider: inst.Provider,
			Instance: inst.ID, Model: inst.Model,
			Error: err, Message: err.Error(), Cause: obsruns.FailureLaunchErr,
			Time: utils.Now(),
		})
		return
	}

	// Track PID on disk so orphan cleanup can find it after a crash
	if m.pidTracker != nil {
		if trackErr := m.pidTracker.Track(inst.ID, result.PID); trackErr != nil {
			slog.Warn("failed to track PID", "instance", inst.ID, "pid", result.PID, "error", trackErr)
		}
	}

	// Monitor readiness probe in a sub-goroutine (if configured)
	if readinessWriter != nil {
		inst.TrackGoroutine()
		go func() {
			defer inst.GoroutineDone()
			defer utils.RecoverAndLog("prov_apps.watchReadiness")
			m.watchReadiness(inst, readinessWriter, hcConfig.ReadinessProbe)
		}()
	}

	// Start HTTP health monitor in a sub-goroutine (uses inst context, cancelled on stop)
	inst.TrackGoroutine()
	go func() {
		defer inst.GoroutineDone()
		defer utils.RecoverAndLog("prov_apps.healthMonitor")
		m.healthMon.Run(inst.Context(), inst, hcConfig)
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
	logFile.Close()

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
		m.emitEvent(Event{
			Type: EventInstanceStopped, Provider: inst.Provider,
			Instance: inst.ID, Model: inst.Model,
			Message: "process exited unexpectedly", Reason: obsruns.StopReasonCrash,
			Time: utils.Now(),
		})
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
	timer := time.NewTimer(probe.Timeout)
	defer timer.Stop()

	select {
	case sig := <-rw.Signal:
		if sig.Ready {
			slog.Info("readiness probe matched", "instance", inst.ID, "pattern", sig.Pattern)
			// The log says the process started. For engines that listen
			// before loading weights, only the serve check knows whether
			// that means anything yet.
			ready, detail := m.awaitServes(inst, probe.Serve, timer.C)
			if !ready {
				m.failNotReady(inst, probe.Timeout)
				return
			}
			if detail == "" {
				detail = fmt.Sprintf("log: %s", sig.Pattern)
			}
			if inst.GetStatus() == instance.StatusStarting {
				inst.MarkRunning()
				inst.SetStartedAtIfZero()
				inst.UpdateLastUsed()
				m.emitEvent(Event{
					Type: EventInstanceRunning, Provider: inst.Provider,
					Instance: inst.ID, Model: inst.Model,
					Message: fmt.Sprintf("Ready (%s)", detail), Time: utils.Now(),
				})
			}
		} else if sig.Failed {
			slog.Warn("readiness probe failure matched", "instance", inst.ID, "pattern", sig.Pattern)
			inst.MarkFailed(fmt.Sprintf("readiness probe failure: %s", sig.Pattern))
			m.emitEvent(Event{
				Type: EventInstanceFailed, Provider: inst.Provider,
				Instance: inst.ID, Model: inst.Model,
				Message: fmt.Sprintf("Readiness failure (log: %s)", sig.Pattern),
				Cause:   obsruns.FailureReadiness, Time: utils.Now(),
			})
		}
	case <-timer.C:
		// Readiness owns the startup window, so it is also what ends it.
		// The health monitor deliberately ignores failures while an
		// instance is starting; if nobody failed it here, a launch that
		// never came up would sit in `starting` forever.
		m.failNotReady(inst, probe.Timeout)
	case <-inst.Context().Done():
		// Instance stopped
	}
}

// failNotReady closes the startup window on an instance that never came
// up. Only readiness can do this: the health monitor ignores failures
// while an instance is starting, precisely so a slow load isn't mistaken
// for a broken one, which leaves nobody else to notice a launch that
// simply never arrives. A no-op once the instance moved on.
func (m *ProviderAppManager) failNotReady(inst *instance.Instance, timeout time.Duration) {
	if inst.GetStatus() != instance.StatusStarting {
		return
	}
	slog.Warn("instance never became ready", "instance", inst.ID, "timeout", timeout)
	inst.MarkFailed(fmt.Sprintf("not ready within %s", timeout))
	m.emitEvent(Event{
		Type: EventInstanceFailed, Provider: inst.Provider,
		Instance: inst.ID, Model: inst.Model,
		Message: fmt.Sprintf("Not ready within %s", timeout),
		Cause:   obsruns.FailureReadiness, Time: utils.Now(),
	})
}

// awaitServes polls the provider's serve check until the engine answers,
// the readiness deadline fires, or the instance goes away. A nil check
// means the provider offers no proof beyond its log line, so the caller's
// signal stands.
func (m *ProviderAppManager) awaitServes(inst *instance.Instance, check *health.ServeCheck, deadline <-chan time.Time) (ready bool, detail string) {
	if check == nil {
		return true, ""
	}

	ticker := time.NewTicker(serveCheckPollInterval)
	defer ticker.Stop()

	for {
		if m.healthMon.Serves(inst.Context(), inst, check) {
			return true, fmt.Sprintf("served %s", check.Path)
		}
		select {
		case <-ticker.C:
		case <-deadline:
			slog.Warn("engine never served before readiness deadline",
				"instance", inst.ID, "path", check.Path)
			return false, ""
		case <-inst.Context().Done():
			return false, ""
		}
	}
}

// StopInstance gracefully stops an instance and removes it from the registry.
// Sequence: cancel monitor and keep-alive → terminate process → drain → cleanup.
func (m *ProviderAppManager) StopInstance(ctx context.Context, id string) (result error) {
	inst, ok := m.instances.Get(id)
	if !ok {
		return ErrInstanceNotFound
	}
	inst.StopUnconfirmed.Store(true)
	defer func() { inst.StopUnconfirmed.Store(result != nil) }()

	// Mark stopping
	inst.SetStatus(instance.StatusStopping)
	m.emitEvent(Event{
		Type: EventInstanceStopping, Provider: inst.Provider,
		Instance: id, Model: inst.Model,
		Message: "Stopping instance", Time: utils.Now(),
	})

	// Stop background activity even if process termination fails.
	inst.Cancel()
	inst.StopKeepAliveTimer()

	// Terminate process (graceful SIGTERM → SIGKILL)
	//    Use shutdown_timeout from provider config if available.
	pid := inst.GetProcessID()
	pgid := inst.GetProcessGroupID()
	if pid > 0 && process.IsSafeToKill(int32(pid)) {
		if process.ProcessHasMarker(int32(pid)) {
			timeout := m.resolveShutdownTimeout(inst.Provider)
			req := process.TerminateRequest{
				PID:            pid,
				ProcessGroupID: pgid,
				Graceful:       timeout,
				JobHandle:      inst.GetJobHandle(),
			}
			if inst.HasStdinPipe() {
				inst.CloseStdinPipe()
				req.StdinPipe = nil // already closed above, terminateProcessTree will skip
			}
			if err := process.Terminate(ctx, req); err != nil {
				return err
			}
		} else {
			slog.Warn("skipping process termination: PID may have been recycled", "instance", id, "pid", pid)
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
	m.ports.ReleaseForProvider(inst.Provider, inst.Port)
	if err := m.instances.Remove(id); err != nil {
		slog.Warn("failed to remove instance from registry", "id", id, "error", err)
	}

	// Emit event
	m.emitEvent(Event{
		Type: EventInstanceStopped, Provider: inst.Provider,
		Instance: id, Model: inst.Model,
		Message: "Instance stopped", Reason: obsruns.StopReasonUser,
		Time: utils.Now(),
	})

	return nil
}

// RestartInstance stops a running instance and launches a new one with the
// same provider, model, parameters, and environment. The port is intentionally
// not preserved — the new instance receives a freshly allocated port — and
// the new instance ID is different from the old one. Returns the new
// instance, or an error if either the stop or the relaunch fails.
//
// Note: there is a small window between StopInstance returning and
// LaunchInstance starting during which the model is not addressable. Callers
// that need zero-downtime should run two instances behind a router instead
// of restarting one.
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
	if req.Provider == "" || req.Model == "" {
		return nil, fmt.Errorf("instance %q has incomplete launch config (provider=%q model=%q); cannot restart", id, req.Provider, req.Model)
	}

	if err := m.StopInstance(ctx, id); err != nil {
		return nil, fmt.Errorf("stop %s: %w", id, err)
	}

	newInst, err := m.LaunchInstance(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("relaunch %s/%s: %w", req.Provider, req.Model, err)
	}
	return newInst, nil
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

	var stopErr error
	for _, inst := range m.instances.ListAll() {
		if err := ctx.Err(); err != nil {
			slog.Warn("provider manager shutdown interrupted by caller deadline",
				"instances_remaining", len(m.instances.ListAll()),
				"error", err)
			stopErr = err
			break
		}
		if err := m.StopInstance(ctx, inst.ID); err != nil {
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

	// idleExpiry is closed because the idleReaper ranges over it and
	// needs the close-signal to exit; no external senders.
	close(m.idleExpiry)

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
		probe := &health.ReadinessProbe{Timeout: readinessTimeout(rp.Timeout)}
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

// restartRequest rebuilds the launch request for a relaunch.
//
// Two things here are load-bearing and neither is obvious from the
// field list:
//
//   - instance.Config carries no endpoint, so it must come off the
//     instance itself. Dropping it relaunched an embeddings instance as
//     a chat one, and now that launch resolves the endpoints overlay it
//     would silently pick the wrong parameters too.
//   - Config.Parameters holds the ORIGINAL request tier, not the merged
//     result. Replaying it preserves the caller's explicit overrides
//     while tiers 0-3 are re-read from current config, which is what
//     lets a restart pick up a parameter change at all.
//
// Port is intentionally omitted so the pool reallocates.
func restartRequest(inst *instance.Instance) LaunchRequest {
	cfg := inst.SnapshotConfig()
	return LaunchRequest{
		Runtime: cfg.Runtime, DisposablePlanID: cfg.DisposablePlanID,
		Provider:   cfg.Provider,
		Model:      cfg.Model,
		Endpoint:   Endpoint(inst.Endpoint),
		Parameters: cfg.Parameters,
		EnvVars:    cfg.EnvVars,
	}
}

// resolveLaunchParams produces the parameters and environment a launched
// process actually receives.
//
// This is the ONLY place that answer is computed for a local launch, so
// it has to do the whole walk: defaults -> model -> node -> node x model
// -> endpoint overlay, with the request tier as the last word. Reading
// only Defaults here was how a models[X] or nodes[N] value could
// persist, peer-sync, and read back correctly from /resolved while
// having no effect on the process that was launched.
//
// A caller that resolves the tree itself and passes the result as
// req.Parameters does not get a second opinion, it gets the last word —
// and the whole resolved set is then stored as the request tier, which
// a restart replays over the config it came from. Only a coordinator
// resolving for a remote node has any business doing that.
//
// Split out from LaunchInstance so the precedence contract can be
// tested without starting a process.
func (m *ProviderAppManager) resolveLaunchParams(svcCfg config.ServiceConfig, req LaunchRequest, endpoint string) (params, env map[string]string) {
	params, env = m.resolveRawLaunchParams(svcCfg, req, endpoint)
	return config.FilterAutoValues(params), env
}

func (m *ProviderAppManager) resolveRawLaunchParams(svcCfg config.ServiceConfig, req LaunchRequest, endpoint string) (params, env map[string]string) {
	resolved := svcCfg.ResolveEndpoint(m.localNodename(), req.Model, endpoint)
	env = mergeStringMaps(config.FlattenEnvironment(resolved.Environment), req.EnvVars)
	params = mergeStringMaps(config.FlattenParameters(resolved.Parameters), req.Parameters)
	return params, env
}

// buildLaunch turns resolved parameters into the engine's command: it
// validates what the operator configured, has the node's ParamLocalizer
// rewrite it into node-local values (such as asset paths), then builds
// the argv. Validation comes first because it checks the configured
// values, not what they become here. It also returns the digests of the
// files the localizer resolved.
//
// Split out from LaunchInstance so the parameters-to-argv contract can be
// tested without starting a process.
func (m *ProviderAppManager) buildLaunch(svcCfg config.ServiceConfig, provider, endpoint, model string, port int, params map[string]string) (process.Launch, map[string]string, error) {
	if err := process.ValidateParameters(params); err != nil {
		return process.Launch{}, nil, fmt.Errorf("%w: %w", ErrParameterValidation, err)
	}
	var files map[string]string
	if m.localizeParams != nil {
		local, err := m.localizeParams(provider, endpoint, model, params)
		if err != nil {
			return process.Launch{}, nil, fmt.Errorf("%w: on node %q: %w", ErrParameterValidation, m.localNodename(), err)
		}
		params, files = local.Params, local.Files
	}
	launch, err := process.NewCommandBuilder(&svcCfg).Build(model, port, params)
	if err != nil {
		return process.Launch{}, nil, fmt.Errorf("failed to build command: %w", err)
	}
	return launch, files, nil
}

// preparedLaunch is a launch worked out from config but not started.
type preparedLaunch struct {
	executionEnvironment process.EnvironmentTransform
	launch               process.Launch
	resolved             instance.Resolved
	config               config.ServiceConfig
}

// prepareLaunch works out what launching req on port would give the
// engine. A launch and a parameters-status check both go through it, so
// "would a restart launch differently" asks the same question a launch
// answers.
func (m *ProviderAppManager) prepareLaunch(svcCfg config.ServiceConfig, configKey, endpoint string, req LaunchRequest, port int) (preparedLaunch, error) {
	features, err := m.modelLaunchParameters(svcCfg, req, endpoint)
	if err != nil {
		return preparedLaunch{}, err
	}
	return m.prepareLaunchFeatures(features, configKey, endpoint, req, port)
}

func (m *ProviderAppManager) prepareLaunchFeatures(features LaunchParameters, configKey, endpoint string, req LaunchRequest, port int) (preparedLaunch, error) {
	params, env, svcCfg := features.Params, features.Environment, features.Config
	launch, files, err := m.buildLaunch(svcCfg, configKey, endpoint, req.Model, port, params)
	if err != nil {
		return preparedLaunch{}, err
	}
	launch.Command = features.ExecutionCommand(configKey)
	runtime := features.RuntimeKey
	if runtime == "" {
		runtime = svcCfg.Name
	}
	return preparedLaunch{
		launch: launch, config: svcCfg, executionEnvironment: features.ExecutionEnvironment,
		resolved: instance.Resolved{AutoMemory: features.autoMemory, Runtime: runtime, Parameters: params, Environment: env, Files: mergeStringMaps(files, features.Files), Execution: executionDigest(svcCfg, features.RuntimeKey, launch.Command), WireEndpoints: wireEndpoints(svcCfg)},
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
