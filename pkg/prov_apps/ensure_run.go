package prov_apps

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	obsruns "github.com/stperic/zzrouter/pkg/observability/runs"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/utils"
)

// decide observes this node for prepared and asks the policy; the run returned is the one Existing describes.
func (m *ProviderAppManager) decide(ctx context.Context, prepared preparedStart) (Decision, *instance.Instance) {
	req := prepared.req
	n := Node{ShuttingDown: m.shuttingDown(), Lifecycle: prepared.lifecycle, Absent: prepared.absent, Runtime: req.selectedRuntime}
	switch n.Lifecycle {
	case LifecycleNone:
		var listed bool
		listed, n.Reachable = m.providerLists(ctx, req.Provider, req.Model)
		n.Absent = !listed
		return Decide(req.want(), n), nil
	case LifecycleServed:
		_, _, n.Reachable = m.servedTarget(req.Provider)
	}
	var existing *instance.Instance
	if run, ok := m.instances.GetByKey(req.Key()); ok {
		existing, n.Existing = run, runView(run)
	}
	return Decide(req.want(), n), existing
}

func runView(run *instance.Instance) *RunView {
	cfg := run.SnapshotConfig()
	view := &RunView{ID: run.ID, Provider: run.Provider, Runtime: cfg.Runtime, Plan: cfg.DisposablePlanID, Status: run.GetStatus(), Unconfirmed: run.StopUnconfirmed.Load(), Failure: run.FailureCause()}
	if view.Failure == nil && view.Status.IsTerminal() {
		view.Failure = fmt.Errorf("run %s is %s: %s", run.ID, view.Status, run.GetErrorMessage())
	}
	return view
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
	switch p.decision.Act {
	case ActReuse:
		if err := p.existing.RefreshActivity(); err != nil {
			return EnsureResult{}, err
		}
		return EnsureResult{Run: p.existing}, nil
	case ActReady:
		return EnsureResult{}, nil
	}
	if p.decision.Err == nil {
		return EnsureResult{}, errUndecided
	}
	return EnsureResult{}, p.decision.Err
}

func (m *ProviderAppManager) prepareStart(ctx context.Context, req EnsureRequest) (preparedStart, error) {
	if m.shuttingDown() {
		return preparedStart{}, ErrShutdown
	}
	if err := m.ValidateModel(ctx, req.Model); err != nil {
		return preparedStart{}, err
	}
	req.requestedModel = req.Model
	req.Provider = utils.NormalizeAppType(req.Provider)
	key, cfg, ok := m.resolveConfigKey(req.Provider)
	if !ok {
		return preparedStart{}, fmt.Errorf("%w: %q not found in config", ErrProviderNotFound, req.Provider)
	}
	if err := cfg.ValidateRunTarget(req.Model, string(req.Endpoint)); err != nil {
		return preparedStart{}, fmt.Errorf("%w: %w", ErrParameterValidation, err)
	}
	req.Model = m.runModel(key, cfg, req.Model)
	if !cfg.IsEnabled() {
		return preparedStart{}, ErrServiceDisabled
	}
	req.keyEndpoint = runEndpoint(cfg, req.Endpoint)
	// A launch checks its catalog only when cold; a provider without runs gathers evidence in decide.
	return preparedStart{req: req, selection: launchSelection{key: key, config: cfg}, lifecycle: Lifecycle(cfg)}, ctx.Err()
}

func (m *ProviderAppManager) prepareColdStart(ctx context.Context, prepared preparedStart) (context.Context, preparedStart, error) {
	req := prepared.req
	// Decide ignores the catalog for a smoke, so a smoke does not read it.
	if req.Model != "" && m.localCatalog != nil && req.Origin != OriginInstall && prepared.lifecycle == LifecycleProcess {
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
	selection := prepared.selection
	var err error
	if req.selectedRuntime == "" {
		selection, err = m.resolveLaunch(&req)
	}
	if err != nil {
		return ctx, prepared, err
	}
	prepared.req, prepared.selection = req, selection
	if req.Detached {
		ctx, err = m.AdmitLocalModel(ctx, req.requestedModel)
	}
	return ctx, prepared, err
}

// flight is one start in progress for a run key. Its leader records the outcome, removes the
// flight from m.flights, then closes done: a caller that finds no flight observes the registry.
type flight struct {
	done    chan struct{}
	run     *instance.Instance
	started bool
	err     error
}

// flying reports a start in flight for model on any endpoint; callers hold flightsMu.
func (m *ProviderAppManager) flying(model string) bool {
	for key := range m.flights {
		if key.Model != "" && strings.EqualFold(key.Model, model) {
			return true
		}
	}
	return false
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
	if prepared.lifecycle == LifecycleNone {
		// No run to share: the provider's own catalog answers, for every caller alike.
		_, _, err = m.prepareColdStart(ctx, prepared)
		return EnsureResult{}, err
	}
	// A fast path only: the flight decides again against the registry it starts on.
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
	f, leads, err := m.joinFlight(ctx, prepared)
	if err != nil || req.Detached {
		return EnsureResult{}, err
	}
	select {
	case <-f.done:
	case <-ctx.Done():
		return EnsureResult{}, ctx.Err()
	}
	if f.err != nil || leads {
		return EnsureResult{Run: f.run, Started: f.started}, f.err
	}
	prepared.decision = Decide(prepared.req.want(), Node{ShuttingDown: m.shuttingDown(), Lifecycle: LifecycleProcess, Existing: runView(f.run), Joined: true})
	prepared.existing = f.run
	return prepared.answer()
}

// joinFlight joins the start in flight for prepared's run key, or creates it and starts it.
func (m *ProviderAppManager) joinFlight(ctx context.Context, prepared preparedStart) (*flight, bool, error) {
	key := prepared.req.Key()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.shuttingDown() {
		return nil, false, ErrShutdown
	}
	m.flightsMu.Lock()
	defer m.flightsMu.Unlock()
	if f, ok := m.flights[key]; ok {
		return f, false, nil
	}
	f := &flight{done: make(chan struct{})}
	m.flights[key] = f
	m.onDemandStarts.Add(1)
	go m.fly(ctx, key, prepared, f)
	return f, true, nil
}

// fly runs a flight's start to handoff on its leader's context values, cancelled only by Stop.
func (m *ProviderAppManager) fly(ctx context.Context, key instance.Key, prepared preparedStart, f *flight) {
	defer m.onDemandStarts.Done()
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer context.AfterFunc(m.startsCtx, cancel)()
	defer cancel()
	result, err := m.decideAndExecuteStart(ctx, prepared)
	if m.startsCtx.Err() != nil && err != nil {
		err = ErrShutdown // whatever a start cancelled by Stop returned, its callers are told why
	} else if err == nil && result.Run == nil {
		err = errUndecided
	}
	if err != nil && prepared.req.Detached && !errors.Is(err, ErrShutdown) {
		slog.Error("Failed to start on-demand model", "model", prepared.req.Model, "provider", prepared.req.Provider, "error", err)
	}
	f.run, f.started, f.err = result.Run, result.Started, err
	m.flightsMu.Lock()
	delete(m.flights, key)
	m.flightsMu.Unlock()
	if m.seams.flightRemoved != nil {
		m.seams.flightRemoved()
	}
	close(f.done)
}

// The installer keeps ownership until smoke preparation has returned. A smoke never joins a
// flight: the flight's leader may wait on the install gate this smoke's installer holds.
func (m *ProviderAppManager) runSmokeStart(ctx context.Context, prepared preparedStart) (EnsureResult, error) {
	m.mu.Lock()
	if m.shuttingDown() {
		m.mu.Unlock()
		return EnsureResult{}, ErrShutdown
	}
	// A smoke never joins a flight: a flight for its model waits on the gate its installer holds.
	m.onDemandStarts.Add(1)
	m.mu.Unlock()
	defer m.onDemandStarts.Done()
	ctx, cancel := context.WithCancel(ctx)
	end := context.AfterFunc(m.startsCtx, cancel)
	defer cancel()
	defer end()
	return m.decideAndExecuteStart(ctx, prepared)
}

func (m *ProviderAppManager) decideAndExecuteStart(ctx context.Context, prepared preparedStart) (EnsureResult, error) {
	if err := ctx.Err(); err != nil {
		return EnsureResult{}, err
	}
	selection, err := m.resolveLaunch(&prepared.req)
	if err != nil {
		return EnsureResult{}, err
	}
	prepared.selection = selection
	if prepared.req.Origin != OriginInstall {
		unlock, err := m.installs.acquireProvider(ctx, prepared.req.selectedRuntime)
		if err != nil {
			return EnsureResult{}, err
		}
		defer unlock()
	}
	ctx, release, err := m.catalogPermit(ctx, 1)
	if err != nil {
		return EnsureResult{}, err
	}
	defer release()
	ctx, prepared, err = m.prepareColdStart(ctx, prepared)
	if err != nil {
		return EnsureResult{}, err
	}

	prepared.decision, prepared.existing = m.decide(ctx, prepared)
	return m.executeStart(ctx, prepared)
}

func (m *ProviderAppManager) executeStart(ctx context.Context, prepared preparedStart) (result EnsureResult, err error) {
	if err = ctx.Err(); err != nil {
		return result, err
	}
	if prepared.decision.Act != ActStart {
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
		if err = m.stopInstance(ctx, replace, obsruns.StopReasonUser); err != nil && !errors.Is(err, ErrInstanceNotFound) { // a replacement keeps the key's pin
			return result, err
		}
	}
	run, err := m.engine(prepared.lifecycle).up(ctx, req, prepared.selection)
	return EnsureResult{Run: run, Started: err == nil}, err
}
