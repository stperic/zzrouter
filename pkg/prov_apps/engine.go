package prov_apps

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/constants"
	obsruns "github.com/stperic/zzrouter/pkg/observability/runs"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
	"github.com/stperic/zzrouter/pkg/prov_apps/protocol"
	"github.com/stperic/zzrouter/pkg/utils"
)

// engine is how a run's model comes up and goes down; a provider's lifecycle chooses it.
type engine interface {
	// up registers a run for req and hands it off; the run leaves starting when its model is ready or failed.
	up(ctx context.Context, req EnsureRequest, selection launchSelection) (*instance.Instance, error)
	// down ends a run's model; the run drains after it.
	down(ctx context.Context, run *instance.Instance) error
	// prepare works out what up would give the engine for req, without starting anything.
	prepare(ctx context.Context, svc config.ServiceConfig, configKey string, req EnsureRequest, port int) (preparedLaunch, error)
	// upstream is where a run's requests go and what of the caller's request travels there.
	upstream(run *instance.Instance) (string, backend.Upstream, error)
	// outlivesNode reports whether a run's model stays loaded when this node stops.
	outlivesNode() bool
}

func (m *ProviderAppManager) engine(lifecycle RunLifecycle) engine {
	if lifecycle == LifecycleServed {
		return servedEngine{m}
	}
	return processEngine{m}
}

// runEngine is the engine run's provider uses.
func (m *ProviderAppManager) runEngine(run *instance.Instance) engine {
	_, svc, _ := m.resolveConfigKey(run.Provider)
	return m.engine(Lifecycle(svc))
}

// Upstream returns where a run's requests go and what of the caller's request travels there.
func (m *ProviderAppManager) Upstream(run *instance.Instance) (string, backend.Upstream, error) {
	return m.runEngine(run).upstream(run)
}

// runModel is the name a run of model is registered under: canonical, as a served provider's daemon names it.
func (m *ProviderAppManager) runModel(configKey string, svc config.ServiceConfig, model string) string {
	return m.daemonName(configKey, svc, m.CanonicalModelName(model))
}

// daemonName is model as a served provider's daemon lists it; any other provider keys on model itself.
func (m *ProviderAppManager) daemonName(configKey string, svc config.ServiceConfig, model string) string {
	if model == "" {
		return ""
	}
	p, _ := m.Protocol(configKey)
	if loader, ok := p.(protocol.ModelLoader); ok && Lifecycle(svc) == LifecycleServed {
		return loader.Identity(model)
	}
	return model
}

// RequestModel is the model a request naming model sends run's engine: that name when the engine resolves it.
func (m *ProviderAppManager) RequestModel(run *instance.Instance, model string) string {
	key, svc, _ := m.resolveConfigKey(run.Provider)
	if run.WireModel == "" || m.daemonName(key, svc, model) == run.WireModel {
		return model
	}
	return run.WireModel
}

// runEndpoint is a run's key endpoint: a shared engine folds the endpoints it serves into chat,
// so an unserved endpoint keys a run no launch admits and never finds a shared run.
func runEndpoint(svc config.ServiceConfig, endpoint Endpoint) Endpoint {
	if svc.RunScope == config.RunScopeNode {
		return ""
	}
	if (!svc.IsEndpointAware() || Lifecycle(svc) == LifecycleServed) && svc.ServesEndpoint(string(endpoint)) == nil {
		return EndpointChat
	}
	return EndpointOrDefault(endpoint)
}

// processEngine launches a process per run.
type processEngine struct{ m *ProviderAppManager }

func (e processEngine) up(ctx context.Context, req EnsureRequest, selection launchSelection) (*instance.Instance, error) {
	return e.m.launchInstanceLocked(ctx, req, selection)
}

func (e processEngine) down(ctx context.Context, run *instance.Instance) error {
	// No pid yet: the lifecycle goroutine sees the cancelled context and signals it.
	if pid := run.ClaimTermination(true); pid > 0 {
		return e.m.terminateProcess(ctx, run, pid)
	}
	return nil
}

// prepare is what launching req on port would give the engine; a launch and a parameters-status check share it.
func (e processEngine) prepare(ctx context.Context, svc config.ServiceConfig, configKey string, req EnsureRequest, port int) (preparedLaunch, error) {
	endpoint := string(EndpointOrDefault(req.Endpoint))
	declared, err := e.m.launchSchema(configKey)
	if err != nil {
		return preparedLaunch{}, err
	}
	features, err := e.m.modelLaunchParameters(ctx, svc, process.ResolveModel(svc, req.Model), req, endpoint, declared)
	if err != nil {
		return preparedLaunch{}, err
	}
	return e.m.prepareLaunchFeatures(features, features.Params, configKey, endpoint, port)
}

func (processEngine) upstream(run *instance.Instance) (string, backend.Upstream, error) {
	return fmt.Sprintf("http://%s:%d", constants.Localhost, run.Port), backend.Engine(), nil
}

func (processEngine) outlivesNode() bool { return false }

// servedEngine loads a run's model into its provider's daemon, which schedules its own memory.
type servedEngine struct{ m *ProviderAppManager }

// neverExpires is the keep-alive of a run zzRouter does not expire: a served run's daemon expires it.
const neverExpires time.Duration = -1

// servedRun builds a starting served run for req, and the keep-alive its daemon is given (nil: its own default).
func (e servedEngine) servedRun(ctx context.Context, req EnsureRequest, svc config.ServiceConfig, configKey string, observed ...string) (*instance.Instance, *time.Duration, error) {
	load, err := e.prepare(ctx, svc, configKey, req, 0)
	if err != nil {
		return nil, nil, err
	}
	_, daemon, ok := e.m.daemonOf(req.Provider)
	if !ok || (len(observed) > 0 && !backend.SameEndpoint(daemon.Endpoint, observed[0])) {
		return nil, nil, ErrProviderUnavailable
	}
	run, err := e.m.newRun(req, svc, 0)
	if err != nil {
		return nil, nil, err
	}
	settings, _ := e.m.runSettings(svc, req) // prepare checked it
	keepAlive, explicit := settings.KeepAlive, settings.KeepAliveExplicit
	// The daemon applies an explicit keep-alive, else its own default; zero is zzRouter's: end at release.
	var daemonKeepAlive *time.Duration
	if explicit && keepAlive != 0 {
		daemonKeepAlive = &keepAlive
	}
	if keepAlive != 0 {
		run.KeepAlive = neverExpires
	}
	run.WireModel = load.launch.WireModel
	weights := e.m.reserveWeights(ctx, req, launchSelection{key: configKey, config: svc})
	weights.Backend = daemon.Endpoint
	weights.Model = run.WireModel
	run.SetWeights(weights)
	run.SourceRepo = weights.Source
	run.SetResolved(load.resolved)
	run.Admit() // the daemon admits its own memory
	return run, daemonKeepAlive, nil
}

func (e servedEngine) up(ctx context.Context, req EnsureRequest, selection launchSelection) (*instance.Instance, error) {
	m := e.m
	run, daemonKeepAlive, err := e.servedRun(ctx, req, selection.config, selection.key)
	if err != nil {
		return nil, err
	}
	if err := m.instances.Register(run); err != nil {
		return nil, err
	}
	m.watchRun(run, req)
	sent := "daemon default"
	if daemonKeepAlive != nil {
		sent = instance.FormatKeepAlive(*daemonKeepAlive)
	}
	m.record(run, runEvent(EventInstanceStarting, run, "load requested, keep-alive "+sent))
	budget := "" // the provider's readiness window
	if rc := selection.config.Runtime; rc != nil && rc.HealthCheck.ReadinessProbe != nil {
		budget = rc.HealthCheck.ReadinessProbe.Timeout
	}
	// The load ends the start: Stop waits for it, as for any start, before it forgets runs.
	m.onDemandStarts.Add(1)
	run.TrackGoroutine()
	go func() {
		defer m.onDemandStarts.Done()
		defer run.GoroutineDone()
		defer utils.RecoverAndLog("prov_apps.loadServed")
		e.load(run, daemonKeepAlive, readinessTimeout(budget))
	}()
	return run, nil
}

// load brings a run's model up within budget; only Stop cancels a load its callers share.
func (e servedEngine) load(run *instance.Instance, keepAlive *time.Duration, budget time.Duration) {
	m := e.m
	ctx, cancel := context.WithTimeout(m.startsCtx, budget)
	defer cancel()
	err := ErrProviderUnavailable
	if loader, target, ok := m.servedRunTarget(run); ok {
		var unreachable *url.Error
		if err = loader.LoadModel(ctx, target, run.WireModel, keepAlive); errors.As(err, &unreachable) {
			err = fmt.Errorf("%w: %w", ErrProviderUnavailable, err)
		}
	}
	if m.startsCtx.Err() != nil {
		err = ErrShutdown // a start Stop cancelled answers why, loaded or not: the node forgets it
	}
	if err != nil {
		err = fmt.Errorf("load %s failed: %w", run.Model, err)
		if run.Context().Err() == nil { // a stop in progress owns the run's removal
			m.releaseRun(run, err)
		}
		m.record(run, failedEvent(run, obsruns.FailureLaunchErr, err))
		return
	}
	if run.Context().Err() == nil {
		run.SetEvictedRuns(m.retireDisplaced(ctx, run))
		if m.publishRunning(run, false, "loaded") {
			return
		}
	}
	// A load that landed after retirement uses the same cleanup as cancellation.
	if loader, resolved, ok := m.daemonForRun(run); ok {
		if err := loader.UnloadModel(m.shutdownCtx, protocol.TargetOf(resolved), run.WireModel); err != nil {
			slog.Warn("Failed to unload a model whose run ended while it loaded", "run", run.ID, "error", err)
		}
	}

}

func (e servedEngine) down(ctx context.Context, run *instance.Instance) error {
	// A load in flight lands first, or the unload could arrive before it.
	if err := run.WaitForGoroutines(ctx); err != nil {
		return fmt.Errorf("drain goroutines: %w", err)
	}
	if run.GetStartedAt().IsZero() && run.StartedBy != instance.StartedByExternal {
		return nil // the load never landed, or undid itself on seeing the stop
	}
	loader, resolved, ok := e.m.daemonForRun(run)
	if !ok {
		return ErrProviderUnavailable
	}
	return loader.UnloadModel(ctx, protocol.TargetOf(resolved), run.WireModel)
}

// prepare is the one served preparation: the daemon's name and the run's keep-alive; no parameter is sent.
func (e servedEngine) prepare(ctx context.Context, svc config.ServiceConfig, _ string, req EnsureRequest, _ int) (preparedLaunch, error) {
	settings, err := e.m.runSettings(svc, req)
	if err != nil {
		return preparedLaunch{}, err
	}
	params := map[string]string{}
	if settings.KeepAliveExplicit {
		params[keepAliveParameter] = instance.FormatKeepAlive(settings.KeepAlive)
	}
	if settings.EvictIdleExplicit {
		params[config.EvictIdleParameter] = fmt.Sprint(settings.EvictIdle)
	}
	return preparedLaunch{
		launch:     process.Launch{WireModel: req.Model}, // runModel named it as the daemon does
		parameters: LaunchParameters{Params: params, Config: svc},
		resolved:   instance.Resolved{Runtime: svc.Name, Parameters: params, WireEndpoints: wireEndpoints(svc)},
		config:     svc,
	}, nil
}

func (e servedEngine) upstream(run *instance.Instance) (string, backend.Upstream, error) {
	if _, resolved, ok := e.m.daemonForRun(run); ok {
		return resolved.Endpoint, resolved.Upstream, nil
	}
	return "", backend.Upstream{}, ErrProviderUnavailable
}

func (servedEngine) outlivesNode() bool { return true }

// daemonForRun preserves physical endpoint identity while accepting its current credentials.
func (m *ProviderAppManager) daemonForRun(run *instance.Instance) (protocol.ModelLoader, *backend.Resolved, bool) {
	loader, target, ok := m.daemonOf(run.Provider)
	if ok && run.Weights().Backend != "" && !backend.SameEndpoint(target.Endpoint, run.Weights().Backend) {
		return nil, nil, false
	}
	return loader, target, ok
}
func (m *ProviderAppManager) servedRunTarget(run *instance.Instance) (protocol.ModelLoader, protocol.Target, bool) {
	loader, target, ok := m.daemonForRun(run)
	if !ok {
		return nil, protocol.Target{}, false
	}
	return loader, protocol.TargetOf(target), true
}
