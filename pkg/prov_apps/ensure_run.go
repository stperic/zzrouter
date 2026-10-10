package prov_apps

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"

	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/utils"
	"golang.org/x/sync/singleflight"
)

// decide observes this node for prepared and asks the policy; the run returned is the one Existing describes.
func (m *ProviderAppManager) decide(ctx context.Context, prepared preparedStart) (Decision, *instance.Instance) {
	req := prepared.req
	n := Node{Lifecycle: prepared.lifecycle, Absent: prepared.absent, Runtime: req.selectedRuntime}
	var existing *instance.Instance
	switch n.Lifecycle {
	case LifecycleServed:
		_, _, n.Reachable = m.Loader(req.Provider)
	case LifecycleNone:
		var listed bool
		listed, n.Reachable = m.providerLists(ctx, req.Provider, req.Model)
		n.Absent = !listed
	default:
		if run, ok := m.instances.GetByModelEndpoint(req.Model, string(req.Endpoint)); ok {
			existing, n.Existing = run, runView(run)
		}
	}
	return Decide(req.want(), n), existing
}

func runView(run *instance.Instance) *RunView {
	cfg := run.SnapshotConfig()
	return &RunView{ID: run.ID, Provider: run.Provider, Runtime: cfg.Runtime, Plan: cfg.DisposablePlanID, Status: run.GetStatus(), Unconfirmed: run.StopUnconfirmed.Load()}
}

type preparedStart struct {
	req       EnsureRequest
	selection launchSelection
	lifecycle RunLifecycle
	absent    bool // catalog evidence, gathered once the start is cold
	decision  Decision
	existing  *instance.Instance // the run decision.Run or decision.Replace names
}

// answer is the result of a decision that starts nothing: the reused run or the refusal.
func (p preparedStart) answer() (EnsureResult, error) {
	if p.decision.Act == ActReuse {
		return EnsureResult{Run: p.existing}, nil
	}
	if p.decision.Err == nil {
		return EnsureResult{}, errUndecided
	}
	return EnsureResult{}, p.decision.Err
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
	// A launch checks its catalog only when cold; other lifecycles gather evidence in decide.
	return preparedStart{req: req, selection: launchSelection{key: key, config: cfg}, lifecycle: Lifecycle(cfg)}, ctx.Err()
}

func (m *ProviderAppManager) prepareColdStart(ctx context.Context, prepared preparedStart) (context.Context, preparedStart, error) {
	req := prepared.req
	// Decide ignores the catalog for a smoke, so a smoke does not read it.
	if m.localCatalog != nil && req.Origin != OriginInstall && prepared.lifecycle == LifecycleProcess {
		local, err := m.localCatalog(ctx, m.CanonicalModelName(prepared.selection.config.WeightsOf(req.Model)))
		if err != nil {
			slog.Warn("Model catalog evidence unavailable; continuing", "model", req.Model, "error", err)
		} else {
			prepared.absent = !local
		}
	}
	prepared.decision, prepared.existing = m.decide(ctx, prepared)
	if prepared.decision.Act != ActStart {
		return ctx, prepared, prepared.decision.Err
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
	if prepared.req.Origin == OriginInstall {
		return m.runSmokeStart(ctx, prepared)
	}
	if prepared.lifecycle != LifecycleProcess {
		ctx, prepared, err = m.prepareColdStart(ctx, prepared)
		if err != nil {
			return EnsureResult{}, err
		}
		return m.executeStart(ctx, prepared)
	}
	if prepared.decision, prepared.existing = m.decide(ctx, prepared); prepared.decision.Act != ActStart {
		return prepared.answer()
	}
	if req.Detached {
		ctx, prepared, err = m.prepareColdStart(ctx, prepared)
		if err != nil {
			return EnsureResult{}, err
		}
		if prepared.decision.Act != ActStart {
			return prepared.answer()
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
		run := runView(result.Run)
		if run.Status.IsTerminal() {
			if cause := result.Run.FailureCause(); cause != nil {
				return EnsureResult{}, cause
			}
			return EnsureResult{}, fmt.Errorf("run %s is %s: %s", result.Run.ID, run.Status, result.Run.GetErrorMessage())
		}
		if req.Force && !result.Started {
			return EnsureResult{}, ErrInstancesRunning
		}
		req.Force = false // A successful shared start already performed the replacement.
		if decision := Decide(req.want(), Node{Lifecycle: LifecycleProcess, Existing: run}); decision.Act != ActReuse {
			return EnsureResult{}, decision.Err
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
	if prepared.decision.Act == ActReuse {
		return prepared.answer()
	}
	if prepared.req.Origin != OriginInstall {
		unlock, err := m.installs.acquireProvider(ctx, prepared.req.selectedRuntime)
		if err != nil {
			return EnsureResult{}, err
		}
		defer unlock()
	}
	prepared.decision, prepared.existing = m.decide(ctx, prepared)
	return m.executeStart(ctx, prepared)
}

func (m *ProviderAppManager) executeStart(ctx context.Context, prepared preparedStart) (result EnsureResult, err error) {
	if err = ctx.Err(); err != nil {
		return result, err
	}
	switch prepared.decision.Act {
	case ActStart:
	case ActReady:
		return EnsureResult{Served: true}, nil
	case ActLoad:
		return m.loadServed(ctx, prepared.req)
	default:
		return prepared.answer()
	}
	req := prepared.req
	if m.observeStart != nil && req.Origin != OriginInstall {
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
	if replace := prepared.decision.Replace; replace != "" {
		if err = m.StopInstance(ctx, replace); err != nil && !errors.Is(err, ErrInstanceNotFound) {
			return result, err
		}
	}
	run, err := m.launchInstanceLocked(ctx, req, prepared.selection)
	return EnsureResult{Run: run, Started: err == nil}, err
}

func (m *ProviderAppManager) loadServed(ctx context.Context, req EnsureRequest) (EnsureResult, error) {
	loader, target, ok := m.Loader(req.Provider)
	if !ok {
		return EnsureResult{}, ErrProviderUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, constants.ClusterActionTimeout)
	defer cancel()
	if err := loader.LoadModel(ctx, target, req.Model, req.Parameters); err != nil {
		return EnsureResult{}, fmt.Errorf("failed to load model via provider API: %w", err)
	}
	_, portText, _ := net.SplitHostPort(target.Host())
	port, _ := strconv.Atoi(portText)
	return EnsureResult{Served: true, Started: true, Port: port}, nil
}
