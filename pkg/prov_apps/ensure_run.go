package prov_apps

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"

	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/protocol"
	"github.com/stperic/zzrouter/pkg/utils"
	"golang.org/x/sync/singleflight"
)

type runSnapshot struct {
	run                               *instance.Instance
	provider, endpoint, runtime, plan string
	status                            instance.Status
	unconfirmed                       bool
}
type startSnapshot struct {
	local bool
	run   *runSnapshot
}
type startDecision struct {
	reuse, stop *instance.Instance
	err         error
}

func decideStart(req EnsureRequest, snapshot startSnapshot) startDecision {
	var stop *instance.Instance
	if run := snapshot.run; run != nil && run.endpoint == string(req.Endpoint) {
		if run.status == instance.StatusStopping {
			// Drain may finish between retries; use the request-slot backoff.
			return startDecision{err: &instance.AdmissionError{Reason: instance.ErrStopping, RetryAfter: instance.AdmissionRetryAfter}}
		}
		if req.installSmoke && (!run.status.IsTerminal() || run.unconfirmed) {
			return startDecision{err: ErrInstancesRunning}
		}
		if req.Force || run.status.IsTerminal() {
			stop = run.run
		} else {
			if run.provider != req.Provider {
				return startDecision{err: fmt.Errorf("%w: %s is held by provider %s; requested %s", ErrRunHeldByOtherProvider, req.Model, run.provider, req.Provider)}
			}
			if run.runtime != req.Runtime || run.plan != req.DisposablePlanID {
				return startDecision{err: ErrInstancesRunning}
			}
			return startDecision{reuse: run.run}
		}
	}
	if !snapshot.local {
		return startDecision{err: fmt.Errorf("%w: %s; download it with auto_deploy before starting", ErrModelNotLocal, req.Model)}
	}
	return startDecision{stop: stop}
}

func (m *ProviderAppManager) startSnapshot(req EnsureRequest, local bool) startSnapshot {
	snapshot := startSnapshot{local: local}
	if run, ok := m.instances.GetByModelEndpoint(req.Model, string(req.Endpoint)); ok {
		snapshot.run = snapshotRun(run)
	}
	return snapshot
}

func snapshotRun(run *instance.Instance) *runSnapshot {
	cfg := run.SnapshotConfig()
	return &runSnapshot{run, run.Provider, string(EndpointOrDefault(Endpoint(run.Endpoint))), cfg.Runtime, cfg.DisposablePlanID, run.GetStatus(), run.StopUnconfirmed.Load()}
}

type preparedStart struct {
	req       EnsureRequest
	selection launchSelection
	local     bool
	decision  startDecision
}

func (m *ProviderAppManager) prepareStart(ctx context.Context, req EnsureRequest) (preparedStart, error) {
	if m.stopping.Load() || m.shutdown.Load() {
		return preparedStart{}, ErrShutdown
	}
	if err := m.ValidateModel(ctx, req.Model); err != nil {
		return preparedStart{}, err
	}
	req.requestedModel = req.Model
	req.Model = m.CanonicalModelName(req.Model)
	req.Provider = utils.NormalizeAppType(req.Provider)
	key, cfg, ok := m.resolveConfigKey(req.Provider)
	if !ok {
		return preparedStart{}, fmt.Errorf("%w: %q not found in config", ErrProviderNotFound, req.Provider)
	}
	if !cfg.IsEnabled() {
		return preparedStart{}, ErrServiceDisabled
	}
	req.Endpoint = EndpointOrDefault(req.Endpoint)
	if !cfg.IsEndpointAware() {
		req.Endpoint = EndpointChat
	}
	return preparedStart{req: req, selection: launchSelection{key: key, config: cfg}, local: true}, ctx.Err()
}

func (m *ProviderAppManager) prepareColdStart(ctx context.Context, prepared preparedStart) (context.Context, preparedStart, error) {
	req := prepared.req
	if m.localCatalog != nil && !req.installSmoke {
		local, err := m.localCatalog(ctx, m.CanonicalModelName(prepared.selection.config.WeightsOf(req.Model)))
		if err != nil {
			slog.Warn("Model catalog evidence unavailable; continuing", "model", req.Model, "error", err)
		} else {
			prepared.local = local
		}
	}
	prepared.decision = decideStart(req, m.startSnapshot(req, prepared.local))
	if prepared.decision.err != nil || prepared.decision.reuse != nil {
		return ctx, prepared, prepared.decision.err
	}
	selection, err := m.resolveLaunch(&req)
	if err != nil {
		return ctx, prepared, err
	}
	prepared.req, prepared.selection = req, selection
	if req.Detached {
		ctx, err = m.AdmitLocalModel(ctx, req.requestedModel)
	}
	return ctx, prepared, err
}

// EnsureRun returns a run without waiting for readiness; Detached accepts a background start.
func (m *ProviderAppManager) EnsureRun(ctx context.Context, req EnsureRequest) (EnsureResult, error) {
	prepared, err := m.prepareStart(ctx, req)
	if err != nil {
		return EnsureResult{}, err
	}
	if prepared.req.installSmoke {
		return m.runSmokeStart(ctx, prepared)
	}
	decision := decideStart(prepared.req, m.startSnapshot(prepared.req, true))
	if decision.err != nil || decision.reuse != nil {
		return EnsureResult{Run: decision.reuse}, decision.err
	}
	if req.Detached {
		ctx, prepared, err = m.prepareColdStart(ctx, prepared)
		if err != nil {
			return EnsureResult{}, err
		}
		if prepared.decision.reuse != nil {
			return EnsureResult{Run: prepared.decision.reuse}, nil
		}
	}
	performed := false
	flight, err := m.startFlight(ctx, prepared, &performed)
	if err != nil {
		return EnsureResult{}, err
	}
	if req.Detached {
		return EnsureResult{}, nil
	}
	select {
	case result := <-flight:
		return m.joinStart(prepared.req, result, performed)
	case <-ctx.Done():
		return EnsureResult{}, ctx.Err()
	}
}

// The installer keeps ownership until smoke preparation has returned.
func (m *ProviderAppManager) runSmokeStart(ctx context.Context, prepared preparedStart) (EnsureResult, error) {
	m.mu.Lock()
	if m.stopping.Load() || m.shutdown.Load() {
		m.mu.Unlock()
		return EnsureResult{}, ErrShutdown
	}
	m.onDemandStarts.Add(1)
	m.mu.Unlock()
	defer m.onDemandStarts.Done()
	ctx, cancel := context.WithCancel(ctx)
	end := context.AfterFunc(m.startsCtx, cancel)
	defer cancel()
	defer end()
	return m.decideAndExecuteStart(ctx, prepared)
}

func (m *ProviderAppManager) startFlight(ctx context.Context, prepared preparedStart, performed *bool) (<-chan singleflight.Result, error) {
	m.mu.Lock()
	if m.stopping.Load() || m.shutdown.Load() {
		m.mu.Unlock()
		return nil, ErrShutdown
	}
	m.onDemandStarts.Add(1)
	m.mu.Unlock()
	start := func() (any, error) {
		*performed = true
		flightCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		end := context.AfterFunc(m.startsCtx, cancel)
		defer cancel()
		defer end()
		return m.decideAndExecuteStart(flightCtx, prepared)
	}
	flight := m.launchGroup.DoChan(prepared.req.Model+":"+string(prepared.req.Endpoint), start)
	result := make(chan singleflight.Result, 1)
	go func() {
		defer m.onDemandStarts.Done()
		completed := <-flight
		if prepared.req.Detached {
			if _, err := m.joinStart(prepared.req, completed, *performed); err != nil {
				slog.Error("Failed to start on-demand model", "model", prepared.req.Model, "provider", prepared.req.Provider, "error", err)
			}
		}
		result <- completed
	}()
	return result, nil
}

func (m *ProviderAppManager) joinStart(req EnsureRequest, flight singleflight.Result, performed bool) (EnsureResult, error) {
	if flight.Err != nil {
		return EnsureResult{}, flight.Err
	}
	result, ok := flight.Val.(EnsureResult)
	if !ok {
		return EnsureResult{}, fmt.Errorf("invalid start result %T", flight.Val)
	}
	if !performed {
		if result.Run == nil {
			return EnsureResult{}, ErrInstanceNotFound
		}
		run := snapshotRun(result.Run)
		if run.status.IsTerminal() {
			if cause := result.Run.FailureCause(); cause != nil {
				return EnsureResult{}, cause
			}
			return EnsureResult{}, fmt.Errorf("run %s is %s: %s", result.Run.ID, run.status, result.Run.GetErrorMessage())
		}
		if req.Force && !result.Started {
			return EnsureResult{}, ErrInstancesRunning
		}
		req.Force = false // A successful shared start already performed the replacement.
		decision := decideStart(req, startSnapshot{local: true, run: run})
		if decision.err != nil {
			return EnsureResult{}, decision.err
		}
	}
	result.Started = result.Started && performed
	return result, nil
}

func (m *ProviderAppManager) decideAndExecuteStart(ctx context.Context, prepared preparedStart) (EnsureResult, error) {
	if err := ctx.Err(); err != nil {
		return EnsureResult{}, err
	}
	if !prepared.req.Detached {
		var err error
		ctx, prepared, err = m.prepareColdStart(ctx, prepared)
		if err != nil {
			return EnsureResult{}, err
		}
	}
	if prepared.decision.reuse != nil {
		return EnsureResult{Run: prepared.decision.reuse}, nil
	}
	if !prepared.req.installSmoke {
		unlock, err := m.installs.acquireProvider(ctx, prepared.req.selectedRuntime)
		if err != nil {
			return EnsureResult{}, err
		}
		defer unlock()
	}
	return m.executeStart(ctx, prepared, decideStart(prepared.req, m.startSnapshot(prepared.req, prepared.local)))
}

func (m *ProviderAppManager) executeStart(ctx context.Context, prepared preparedStart, decision startDecision) (result EnsureResult, err error) {
	if err = ctx.Err(); err != nil {
		return result, err
	}
	if decision.err != nil {
		return result, decision.err
	}
	if decision.reuse != nil {
		return EnsureResult{Run: decision.reuse}, nil
	}
	req := prepared.req
	if m.observeStart != nil && !req.installSmoke {
		var finish func(EnsureResult, error)
		ctx, finish = m.observeStart(ctx, req)
		defer func() { finish(result, err) }()
	}
	if !req.Detached {
		ctx, err = m.AdmitLocalModel(ctx, req.requestedModel)
		if err != nil {
			return result, err
		}
	}
	if decision.stop != nil {
		if err = m.StopInstance(ctx, decision.stop.ID); err != nil && !errors.Is(err, ErrInstanceNotFound) {
			return result, err
		}
	}
	if !req.installSmoke && prepared.selection.config.HasEndpoint() && m.resolveBackend != nil {
		if resolved, ok := m.resolveBackend(req.Provider); ok {
			return m.loadServed(ctx, req, resolved)
		}
	}
	run, err := m.launchInstanceLocked(ctx, req, prepared.selection)
	return EnsureResult{Run: run, Started: err == nil}, err
}

func (m *ProviderAppManager) loadServed(ctx context.Context, req EnsureRequest, resolved *backend.Resolved) (EnsureResult, error) {
	loader, ok := m.Protocol(req.Provider)
	if !ok {
		return EnsureResult{}, ErrProviderNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, constants.ClusterActionTimeout)
	defer cancel()
	if err := loader.LoadModel(ctx, protocol.TargetOf(resolved), req.Model, req.Parameters); err != nil {
		return EnsureResult{}, fmt.Errorf("failed to load model via provider API: %w", err)
	}
	endpoint, _ := url.Parse(resolved.Endpoint)
	port := 0
	if endpoint != nil {
		port, _ = strconv.Atoi(endpoint.Port())
	}
	return EnsureResult{Served: true, Started: true, Port: port}, nil
}
