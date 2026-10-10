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
	prepare(svc config.ServiceConfig, configKey string, req EnsureRequest, port int) (preparedLaunch, error)
	// upstream is where a run's requests go and what of the caller's request travels there.
	upstream(run *instance.Instance) (string, backend.Upstream, error)
	// outlivesNode reports whether a run's model stays loaded when this node stops.
	outlivesNode() bool
	// hasLog reports whether a run writes a log file of its own.
	hasLog() bool
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

// HasLog reports whether run writes a log file of its own.
func (m *ProviderAppManager) HasLog(run *instance.Instance) bool { return m.runEngine(run).hasLog() }

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

// runEndpoint is a run's key endpoint: its own only on an endpoint-aware launched provider.
func runEndpoint(svc config.ServiceConfig, endpoint Endpoint) Endpoint {
	if !svc.IsEndpointAware() || Lifecycle(svc) == LifecycleServed {
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
func (e processEngine) prepare(svc config.ServiceConfig, configKey string, req EnsureRequest, port int) (preparedLaunch, error) {
	endpoint := string(EndpointOrDefault(req.Endpoint))
	features, err := e.m.modelLaunchParameters(svc, process.ResolveModel(svc, req.Model), req, endpoint)
	if err != nil {
		return preparedLaunch{}, err
	}
	return e.m.prepareLaunchFeatures(features, configKey, endpoint, port)
}

func (processEngine) upstream(run *instance.Instance) (string, backend.Upstream, error) {
	return fmt.Sprintf("http://%s:%d", constants.Localhost, run.Port), backend.Engine(), nil
}

func (processEngine) outlivesNode() bool { return false }

func (processEngine) hasLog() bool { return true }

// servedEngine loads a run's model into its provider's daemon, which schedules its own memory.
type servedEngine struct{ m *ProviderAppManager }

// neverExpires is the keep-alive of a run zzRouter does not expire: a served run's daemon expires it.
const neverExpires time.Duration = -1

// servedRun builds a starting served run for req, and the keep-alive its daemon is given (nil: its own default).
func (e servedEngine) servedRun(ctx context.Context, req EnsureRequest, svc config.ServiceConfig, configKey string) (*instance.Instance, *time.Duration, error) {
	load, err := e.prepare(svc, configKey, req, 0)
	if err != nil {
		return nil, nil, err
	}
	run, err := e.m.newRun(ctx, req, svc, 0)
	if err != nil {
		return nil, nil, err
	}
	keepAlive, explicit, _ := runKeepAlive(svc, req) // prepare checked it
	// The daemon applies an explicit keep-alive, else its own default; zero is zzRouter's: end at release.
	var daemonKeepAlive *time.Duration
	if explicit && keepAlive != 0 {
		daemonKeepAlive = &keepAlive
	}
	if keepAlive != 0 {
		run.KeepAlive = neverExpires
	}
	run.WireModel = load.launch.WireModel
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
	// The load ends the start: Stop waits for it, as for any start, before it forgets runs.
	m.onDemandStarts.Add(1)
	run.TrackGoroutine()
	go func() {
		defer m.onDemandStarts.Done()
		defer run.GoroutineDone()
		defer utils.RecoverAndLog("prov_apps.loadServed")
		e.load(run, daemonKeepAlive, readinessBudget(selection.config))
	}()
	return run, nil
}

// load brings a run's model up within budget; only Stop cancels a load its callers share.
func (e servedEngine) load(run *instance.Instance, keepAlive *time.Duration, budget time.Duration) {
	m := e.m
	ctx, cancel := context.WithTimeout(m.startsCtx, budget)
	defer cancel()
	err := ErrProviderUnavailable
	if loader, target, ok := m.servedTarget(run.Provider); ok {
		var unreachable *url.Error
		if err = loader.LoadModel(ctx, target, run.WireModel, keepAlive); errors.As(err, &unreachable) {
			err = fmt.Errorf("%w: %w", ErrProviderUnavailable, err)
		}
	}
	if m.startsCtx.Err() != nil {
		err = ErrShutdown // a start Stop cancelled answers why, loaded or not: the node forgets it
	}
	if err != nil {
		err = fmt.Errorf("load %s: %w", run.Model, err)
		if run.Context().Err() == nil { // a stop in progress owns the run's removal
			m.releaseRun(run, err)
		}
		m.emitEvent(failedEvent(run, obsruns.FailureLaunchErr, err))
		return
	}
	if run.Context().Err() != nil { // ended or retired while loading: the node asked the model gone
		if loader, resolved, ok := m.daemonOf(run.Provider); ok {
			if err := loader.UnloadModel(m.shutdownCtx, protocol.TargetOf(resolved), run.WireModel); err != nil {
				slog.Warn("Failed to unload a model whose run ended while it loaded", "run", run.ID, "error", err)
			}
		}
		return
	}
	run.MarkRunning()
	run.SetStartedAtIfZero()
	m.emitEvent(runEvent(EventInstanceRunning, run, "Ready (loaded)"))
}

func (e servedEngine) down(ctx context.Context, run *instance.Instance) error {
	// A load in flight lands first, or the unload could arrive before it.
	if err := run.WaitForGoroutines(ctx); err != nil {
		return fmt.Errorf("drain goroutines: %w", err)
	}
	if run.GetStartedAt().IsZero() {
		return nil // the load never landed, or undid itself on seeing the stop
	}
	loader, resolved, ok := e.m.daemonOf(run.Provider)
	if !ok {
		return ErrProviderUnavailable
	}
	return loader.UnloadModel(ctx, protocol.TargetOf(resolved), run.WireModel)
}

// prepare is the one served preparation: the daemon's name and the run's keep-alive; no parameter is sent.
func (servedEngine) prepare(svc config.ServiceConfig, _ string, req EnsureRequest, _ int) (preparedLaunch, error) {
	keepAlive, explicit, err := runKeepAlive(svc, req)
	if err != nil {
		return preparedLaunch{}, err
	}
	params := map[string]string{}
	if explicit {
		params[keepAliveParameter] = instance.FormatKeepAlive(keepAlive)
	}
	return preparedLaunch{
		launch:     process.Launch{WireModel: req.Model}, // runModel named it as the daemon does
		parameters: LaunchParameters{Params: params, Config: svc},
		resolved:   instance.Resolved{Runtime: svc.Name, Parameters: params, WireEndpoints: wireEndpoints(svc)},
		config:     svc,
	}, nil
}

func (e servedEngine) upstream(run *instance.Instance) (string, backend.Upstream, error) {
	if _, resolved, ok := e.m.daemonOf(run.Provider); ok {
		return resolved.Endpoint, resolved.Upstream, nil
	}
	return "", backend.Upstream{}, ErrProviderUnavailable
}

func (servedEngine) outlivesNode() bool { return true }

func (servedEngine) hasLog() bool { return false }

// readinessBudget is how long a provider gives a run to become ready.
func readinessBudget(svc config.ServiceConfig) time.Duration {
	raw := ""
	if svc.Runtime != nil && svc.Runtime.HealthCheck.ReadinessProbe != nil {
		raw = svc.Runtime.HealthCheck.ReadinessProbe.Timeout
	}
	return readinessTimeout(raw)
}
