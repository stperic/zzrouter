package prov_apps

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/jobs"
	obsruns "github.com/stperic/zzrouter/pkg/observability/runs"
	"github.com/stperic/zzrouter/pkg/prov_apps/detect"
	"github.com/stperic/zzrouter/pkg/prov_apps/health"
	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/builtins"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/logging"
	"github.com/stperic/zzrouter/pkg/prov_apps/port"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
	"github.com/stperic/zzrouter/pkg/prov_apps/protocol"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
)

const defaultEventChannelSize = 64

// eventDrainWindow bounds the trailing drain in eventLoop after shutdownCtx
// fires. Catches emit() calls that passed the shutdown.Load() gate but had
// not yet executed the channel send when ctx cancelled. 100ms is generous
// for a buffered, non-blocking sender.
const eventDrainWindow = 100 * time.Millisecond

// configKeyNormalizer strips dots and hyphens from provider names
// to resolve display names (e.g., "llama.cpp") to config keys (e.g., "llamacpp").
var configKeyNormalizer = strings.NewReplacer(".", "", "-", "")

// ProviderAppManager is the top-level facade for all provider lifecycle operations.
// It wires together instance tracking, port allocation, protocol handlers,
// detection, installation, health monitoring, and logging.
//
// LOCK ORDERING: ProviderAppManager.mu → instance.Registry.mu → instance.Instance.mu
// versionCache.mu and InstallCoordinator.locks (per-name install serialization)
// are orthogonal to this chain.
type ProviderAppManager struct {
	appsConfig *config.AppsConfig
	instances  *instance.Registry
	ports      *port.AppPoolManager
	protocols  *protocol.Registry
	logMgr     *logging.Manager
	healthMon  *health.Monitor
	httpClient *http.Client
	launcher   *process.Launcher
	pidTracker *process.PIDTracker

	// versions is the shared cache of detected/installed provider versions.
	// Read by ProviderVersion/ProviderVersions/ProviderStatus/syncVersions on
	// the facade; written post-install/upgrade/uninstall by InstallCoordinator
	// and by the bulk detection path. The cache owns its own lock — m.mu
	// does not protect it.
	versions *versionCache

	// installs owns install/upgrade/uninstall + per-name install serialization
	// + the install progress tracker. Constructed in NewProviderAppManager
	// and exposed via the Install accessor.
	installs *InstallCoordinator

	// auditEvents carries lifecycle events to eventLoop for structured logging.
	// Buffered, non-blocking sends; events that race past the shutdown gate
	// are dropped via the default branch in emitEvent.
	auditEvents    chan Event
	catalogChanged func()

	// idleExpiry receives instance IDs when keep-alive timers fire.
	// The idleReaper goroutine listens and auto-stops expired instances.
	idleExpiry chan string

	mu             sync.RWMutex
	shutdown       atomic.Bool // true once Stop() has drained instances; gates emitEvent
	stopping       atomic.Bool // single-shot guard for concurrent Stop() callers
	startOnce      sync.Once
	shutdownCtx    context.Context
	shutdownCancel context.CancelFunc

	// parentCtx ferries WithParentContext's value across the option-
	// loop boundary; consumed by NewProviderAppManager to seed
	// shutdownCtx. Nil after construction.
	parentCtx context.Context

	// lastReloadFP is the fingerprint of the AppsConfig most recently
	// applied via ReloadConfig. Guarded by mu (same lock as appsConfig
	// so reads always see a consistent (config, fingerprint) pair).
	lastReloadFP [32]byte

	// jobsRegistry, when non-nil, drives SSE progress streams for every
	// install/upgrade/uninstall the coordinator runs. Set via
	// WithJobsRegistry. Nil disables the streaming integration and
	// preserves pre-jobs behavior.
	jobsRegistry *jobs.Registry

	// canonicalModel maps a caller-supplied model name onto the name
	// the model cache indexes that model under. A model is reachable by
	// several names -- its source repo id, its file stem -- and the
	// instance registry is keyed by whichever one the launch used, so
	// without this two callers naming the same weights differently each
	// get their own copy of it resident. Nil leaves names untouched,
	// which is right for callers with no cache (tests, workers built
	// before one exists).
	canonicalModel func(string) string

	// validateModel checks catalog identities before canonicalization loses
	// an addressed variant's name. Nil leaves unpublished weights to the cold-launch guard.
	validateModel func(context.Context, string) error

	// modelSource names the registry a model's weights came from
	// (huggingface, ollama, ...), as the model catalog records it. Nil
	// leaves a run's source unstated.
	modelSource func(ctx context.Context, model string) string

	// launchGroup coalesces concurrent LaunchInstance calls for the
	// same (provider, model) so simultaneous POST /runs or /runs/load
	// don't spawn racing instances. First caller runs the full launch
	// flow; subsequent callers wait and receive the same *Instance.
	// Key: "<provider>:<model>" (forbidden characters aren't possible
	// in either field — validated upstream).
	launchGroup singleflight.Group

	// nodename is this node's cluster name, used to resolve the node
	// tiers of the parameter tree. Optional; nil means Tier 0 only.
	nodename func() string

	// localizeParams turns configured parameter values into what this
	// node's engine is given. Optional; nil passes them through.
	localizeParams    ParamLocalizer
	runtimeChecks     install.RuntimeChecksResolver
	installPolicyFile string
	memoryBudget      func(provider string) (*schema.MemoryBudget, error)
	memoryLaunchMu    sync.Mutex
	servicesMu        sync.Mutex
	services          map[string]*managedService
	servicesStarted   atomic.Bool
	servicesObserved  chan struct{}
}

// ParamLocalizer rewrites one launch's configured parameters into the
// values this node hands its engine, such as an asset name into the path
// of that file here. An error refuses the launch. Its output is trusted:
// validation applies to what the operator configured, before it runs.
type ParamLocalizer func(provider, endpoint, model string, params map[string]string) (Localized, error)

// Localized is one launch's parameters as this node's engine is given them.
type Localized struct {
	// Sources identifies parameters supplied by model features.
	Sources map[string]string
	Params  map[string]string
	// Files maps each parameter whose value became a file here to the
	// digest of that file's content: the engine reads the bytes, so they
	// are part of what the launch was given.
	Files map[string]string
}

// Option configures a ProviderAppManager.
type Option func(*ProviderAppManager)

// WithHTTPClient sets a shared HTTP client for all provider connections.
func WithHTTPClient(client *http.Client) Option {
	return func(m *ProviderAppManager) {
		m.httpClient = client
	}
}

// WithJobsRegistry enables SSE progress streaming for every
// install/upgrade/uninstall by threading the given registry into the
// coordinator. Nil registry is a no-op (matches default).
func WithJobsRegistry(r *jobs.Registry) Option {
	return func(m *ProviderAppManager) {
		m.jobsRegistry = r
	}
}

// WithModelCanonicalizer supplies the mapping from a caller-supplied
// model name to the cache's canonical name for it. Applied before the
// launch is keyed, so every alias form of one model resolves to one
// instance.
func WithModelCanonicalizer(fn func(string) string) Option {
	return func(m *ProviderAppManager) {
		m.canonicalModel = fn
	}
}

// WithModelValidator supplies catalog admission for model names and aliases.
func WithModelValidator(fn func(context.Context, string) error) Option {
	return func(m *ProviderAppManager) { m.validateModel = fn }
}

// ValidateModel checks a name before any canonicalization or warm reuse.
func (m *ProviderAppManager) ValidateModel(ctx context.Context, model string) error {
	if m.validateModel != nil {
		if err := m.validateModel(ctx, model); err != nil {
			if errors.Is(err, config.ErrModelNameConflict) {
				return err
			}
			slog.Warn("Model admission evidence unavailable; continuing", "model", model, "error", err)
		}
	}
	return nil
}

type localModelAdmissionKey struct{}
type localModelAdmission struct {
	manager     *ProviderAppManager
	config      *config.AppsConfig
	fingerprint [32]byte
	model       string
}

// AdmitLocalModel checks unpublished weights once for a launch operation.
// The returned context carries the check through force-stop and command building.
func (m *ProviderAppManager) AdmitLocalModel(ctx context.Context, model string) (context.Context, error) {
	if err := ctx.Err(); err != nil {
		return ctx, err
	}
	m.mu.RLock()
	cfg, fp := m.appsConfig, m.lastReloadFP
	if checked, ok := ctx.Value(localModelAdmissionKey{}).(localModelAdmission); ok &&
		checked.manager == m && checked.config == cfg && checked.fingerprint == fp && checked.model == model {
		m.mu.RUnlock()
		return ctx, nil
	}
	var services []config.ServiceConfig
	if cfg != nil {
		cfg.RangeApps(func(_ string, svc config.ServiceConfig) bool {
			if svc.Mode == constants.AppModeOnDemand && len(svc.Variants()) > 0 {
				services = append(services, svc)
			}
			return true
		})
	}
	m.mu.RUnlock()
	for _, svc := range services {
		if err := ctx.Err(); err != nil {
			return ctx, err
		}
		if err := process.ValidateVariantModel(&svc, model); err != nil {
			return ctx, err
		}
	}
	return context.WithValue(ctx, localModelAdmissionKey{}, localModelAdmission{manager: m, config: cfg, fingerprint: fp, model: model}), nil
}

// WithModelSource supplies the catalog lookup a launch records its
// weights' source with, so every view of a run states where they came
// from instead of a client guessing it from the provider.
func WithModelSource(fn func(ctx context.Context, model string) string) Option {
	return func(m *ProviderAppManager) {
		m.modelSource = fn
	}
}

// WithNodename supplies this node's cluster name, which the parameter
// tiers are keyed by. Without it the manager can only see Tier 0, so a
// value configured under nodes.<this node> is invisible to it.
func WithNodename(fn func() string) Option {
	return func(m *ProviderAppManager) {
		m.nodename = fn
	}
}

// WithParamLocalizer supplies the last step between parameter
// resolution and the engine's argv; see ParamLocalizer.
func WithParamLocalizer(l ParamLocalizer) Option {
	return func(m *ProviderAppManager) {
		m.localizeParams = l
	}
}

// WithRuntimeChecks supplies this node's current schema declarations.
func WithRuntimeChecks(checks install.RuntimeChecksResolver) Option {
	return func(m *ProviderAppManager) { m.runtimeChecks = checks }
}

// WithMemoryBudget supplies release-owned engine memory-budget semantics.
func WithMemoryBudget(resolve func(provider string) (*schema.MemoryBudget, error)) Option {
	return func(m *ProviderAppManager) { m.memoryBudget = resolve }
}

// NewProviderAppManager creates a new manager from apps config.
func NewProviderAppManager(appsConfig *config.AppsConfig, opts ...Option) (*ProviderAppManager, error) {
	m := &ProviderAppManager{
		appsConfig:  appsConfig,
		instances:   instance.NewRegistry(),
		versions:    newVersionCache(),
		auditEvents: make(chan Event, defaultEventChannelSize),
		idleExpiry:  make(chan string, defaultEventChannelSize),
		services:    make(map[string]*managedService),
	}

	// Options first so WithParentContext can supply the ctx parent.
	// No existing option reads shutdownCtx — keep that invariant.
	for _, opt := range opts {
		opt(m)
	}
	parent := m.parentCtx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	m.shutdownCtx = ctx
	m.shutdownCancel = cancel

	// Defaults for anything not set by options
	if m.httpClient == nil {
		m.httpClient = &http.Client{Timeout: constants.HTTPDefaultTimeout}
	}
	if m.logMgr == nil {
		m.logMgr = logging.NewManager(logging.DefaultManagerConfig(
			config.Paths().GetLogsDir(),
		))
	}

	// Create port pool manager
	portMgr, err := port.NewAppPoolManager(appsConfig)
	if err != nil {
		cancel()
		return nil, err
	}
	m.ports = portMgr

	// Create protocol registry
	m.protocols = protocol.NewRegistry(appsConfig, m.httpClient)

	// Create health monitor (after instances registry is set)
	if m.healthMon == nil {
		m.healthMon = health.NewMonitor(m.instances, health.DefaultMonitorConfig())
	}

	// PID tracking: shared directory for both the launcher binary and the PIDTracker.
	// The launcher writes JSON PID files for signal forwarding; the PIDTracker writes
	// plain PID files for orphan detection. Both use the same directory so
	// CleanupOrphans can find processes from either source.
	pidDir := config.Paths().GetPIDDir()

	// Create process launcher (trusted = config-driven commands from provider config)
	m.launcher = process.NewLauncher(process.DefaultSecurityConfig(), true, pidDir)

	// Create PID tracker for cross-restart orphan detection.
	// On startup, any PID files left from a previous session indicate orphaned
	// processes (server crashed or was killed without graceful shutdown).
	pidTracker, err := process.NewPIDTracker(pidDir)
	if err != nil {
		slog.Warn("Failed to create PID tracker, orphan cleanup disabled", "error", err)
	} else {
		m.pidTracker = pidTracker
		// No active instances on fresh start — every tracked PID is an orphan.
		if n := pidTracker.CleanupOrphans(nil); n > 0 {
			slog.Info("Cleaned up orphaned processes from previous session", "count", n)
		}
	}

	// Construct the install coordinator last — it depends on the manager's
	// shared state (versions cache, shutdown atomic, audit channel, instance
	// registry) and a config getter that follows ReloadConfig swaps.
	m.installs = newCoordinator(
		builtins.NewDispatcher(m.providerServiceEnv, m.runtimeChecks, m.resolveRecipe),
		install.NewProgressTracker(),
		m.auditEvents,
		&m.shutdown,
		m.instances,
		m.versions,
		m.AppsConfig,
		m.jobsRegistry,
	)

	m.installs.catalogChanged = m.catalogChanged
	m.installs.serviceControl = m.controlInstallService
	m.installs.smoke = m.smokeInstalledRuntime
	return m, nil
}

// Start launches the background goroutines (event consumer, idle reaper).
// The provided ctx controls the lifetime of these goroutines; cancelling it
// is equivalent to calling Stop. Fire-and-forget — degraded-ok if not called
// (events are dropped via non-blocking sends).
//
// Refuses to spin up goroutines if Stop has already run (shutdown set).
// Without this gate, a Start-after-Stop call would spawn an idleReaper that
// reads from the closed idleExpiry channel and an eventLoop that hangs
// forever (emit() drops via the shutdown gate so nothing arrives).
func (m *ProviderAppManager) Start(ctx context.Context) {
	if m == nil {
		return
	}
	m.startOnce.Do(func() {
		if m.shutdown.Load() {
			slog.Warn("Start called after Stop; manager remains shutdown")
			return
		}
		// Replace the background context with the caller's context so that
		// the server's shutdown propagates to the manager's goroutines.
		m.shutdownCancel()
		childCtx, cancel := context.WithCancel(ctx)
		m.shutdownCtx = childCtx
		m.shutdownCancel = cancel

		go m.eventLoop()
		go m.idleReaper() //nolint:contextcheck // uses m.shutdownCtx internally; see idleReaper body
		m.servicesObserved = make(chan struct{})
		m.servicesStarted.Store(true)
		go m.observeServices(childCtx)
		m.reconcileServices()
	})
}

// eventLoop drains the audit channel and logs all provider events.
// Runs until shutdownCtx is canceled, then drains for a short trailing
// window to catch in-flight emits.
//
// The trailing window closes the race where an emit() goroutine has passed
// the shutdown.Load() gate but has not yet executed the channel send when
// Stop fires shutdownCancel. Without it, the drain's non-blocking default
// branch can return before the late send lands and the event is lost.
func (m *ProviderAppManager) eventLoop() {
	for {
		select {
		case e, ok := <-m.auditEvents:
			if !ok {
				return
			}
			m.logEvent(e)
		case <-m.shutdownCtx.Done():
			deadline := time.NewTimer(eventDrainWindow)
			defer deadline.Stop()
			for {
				select {
				case e, ok := <-m.auditEvents:
					if !ok {
						return
					}
					m.logEvent(e)
				case <-deadline.C:
					return
				}
			}
		}
	}
}

// idleReaper listens for keep-alive expiry signals and auto-stops idle instances.
// An instance is only stopped if it's still running and truly idle (no active requests,
// last activity exceeds keep-alive). This handles the case where a request arrives
// between timer fire and reaper processing.
func (m *ProviderAppManager) idleReaper() {
	for {
		select {
		case id, ok := <-m.idleExpiry:
			if !ok {
				return
			}
			inst, exists := m.instances.Get(id)
			if !exists {
				continue
			}
			// Only stop if still running and idle (re-check in case activity arrived)
			if !inst.IsRunning() || inst.GetActiveRequests() > 0 || !inst.ShouldUnload() {
				continue
			}
			slog.Info("auto-stopping idle instance", "instance", id, "provider", inst.Provider, "model", inst.Model, "keep_alive", inst.GetKeepAlive())
			// Bound the StopInstance call: a hung subprocess shouldn't
			// wedge the reaper. Derive from shutdownCtx so a manager
			// Stop also unblocks (the previous Background ctx couldn't
			// be cancelled by anything).
			stopCtx, stopCancel := context.WithTimeout(m.shutdownCtx, constants.ClusterActionTimeout)
			if err := m.StopInstance(stopCtx, id); err != nil {
				slog.Warn("failed to auto-stop idle instance", "instance", id, "error", err)
			}
			stopCancel()
		case <-m.shutdownCtx.Done():
			return
		}
	}
}

// WithCatalogChanged invalidates serving metadata after lifecycle changes.
func WithCatalogChanged(fn func()) Option {
	return func(m *ProviderAppManager) { m.catalogChanged = fn }
}

func (m *ProviderAppManager) logEvent(e Event) {
	attrs := []any{
		"type", e.Type.String(),
		"provider", e.Provider,
	}
	if e.Instance != "" {
		attrs = append(attrs, "instance", e.Instance)
	}
	if e.Model != "" {
		attrs = append(attrs, "model", e.Model)
	}
	if e.Message != "" {
		attrs = append(attrs, "message", e.Message)
	}
	if e.Error != nil {
		attrs = append(attrs, "error", e.Error.Error())
	}
	slog.Info("[ProviderEvent]", attrs...)
	m.emitLifecycleMetric(e)
}

// emitLifecycleMetric maps a lifecycle Event onto the
// pkg/observability/runs deployment counters. Runs on the eventLoop
// goroutine — single-threaded, so cardinality bookkeeping doesn't
// race. Reads typed Reason / Cause fields off the Event so the
// closed-enum classification stays pinned at the emit site (no
// fragile substring matching of free-form Message).
//
// server.address is looked up from the live instance registry by
// Event.Instance ID; the lookup is O(1) on a sync.Map and runs
// uncontended on this goroutine. An instance that's already been
// removed (StopInstance's Remove fires before the Stopped event)
// surfaces an empty server.address — acceptable trade-off; for the
// stopped/failed events the address is dashboard noise anyway.
func (m *ProviderAppManager) emitLifecycleMetric(e Event) {
	addr := m.lookupServerAddress(e.Instance)
	switch e.Type {
	case EventInstanceRunning:
		obsruns.RecordStart(context.Background(), e.Provider, e.Model, addr)
	case EventInstanceStopped:
		reason := e.Reason
		if reason == "" {
			reason = obsruns.StopReasonUser
		}
		obsruns.RecordStop(context.Background(), e.Provider, e.Model, addr, reason)
	case EventInstanceFailed:
		cause := e.Cause
		if cause == "" {
			cause = obsruns.FailureOther
		}
		obsruns.RecordFailure(context.Background(), e.Provider, e.Model, addr, cause)
	}
}

// lookupServerAddress resolves an instance ID to its host portion
// (parsed from the recorded HealthURL) for the deployment-lifecycle
// metrics. Returns "" when the instance is gone (already removed),
// when no HealthURL was recorded, or when the URL has no host —
// local instances often bind to localhost only, surfacing an empty
// or "127.0.0.1" address; either is honest.
func (m *ProviderAppManager) lookupServerAddress(instanceID string) string {
	if m == nil || instanceID == "" {
		return ""
	}
	inst, ok := m.instances.Get(instanceID)
	if !ok || inst == nil || inst.HealthURL == "" {
		return ""
	}
	u, err := url.Parse(inst.HealthURL)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Hostname()
}

// --- Accessor methods for subpackages ---

// Instances returns the instance registry.
func (m *ProviderAppManager) Instances() *instance.Registry {
	return m.instances
}

// Protocols returns the protocol registry.
func (m *ProviderAppManager) Protocols() *protocol.Registry {
	return m.protocols
}

// Install returns the install coordinator (install/upgrade/uninstall +
// per-name serialization + progress tracking).
func (m *ProviderAppManager) Install() *InstallCoordinator {
	return m.installs
}

// ResolveVersion returns the pinned version for a provider if no explicit version is given.
func (m *ProviderAppManager) ResolveVersion(provider, version string) string {
	if version != "" {
		return version
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.appsConfig != nil {
		// Kind-agnostic on purpose. AppsConfigStore.SetProviderPinnedVersion
		// writes through LookupApp, which accepts any kind, so reading
		// through GetOnDemand meant the write and the read disagreed about
		// where a pin lives: PATCH /providers/ollama {"pinned_version":...}
		// answered "pinned to X" and persisted it, then every install kept
		// resolving the newest upstream release, because ollama is an
		// external provider. zzRouter does install and version ollama, so
		// "external" says nothing about whether a pin should apply.
		if svc, ok := m.appsConfig.LookupApp(provider); ok && svc.PinnedVersion != "" {
			return svc.PinnedVersion
		}
	}
	return version
}

// Variant reports whether model is a variant some provider defines, with
// that provider and the base whose weights it runs. With provider set,
// only that provider is asked.
func (m *ProviderAppManager) Variant(provider, model string) (owner, base string, ok bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.appsConfig == nil {
		return "", "", false
	}
	if provider == "" {
		return m.appsConfig.LookupVariant(model)
	}
	if svc, found := m.appsConfig.LookupApp(provider); found {
		if _, b, isVariant := svc.Variant(model); isVariant {
			return provider, b, true
		}
	}
	return "", "", false
}

// Ports returns the port pool manager.
func (m *ProviderAppManager) Ports() *port.AppPoolManager {
	return m.ports
}

// LogManager returns the log manager.
func (m *ProviderAppManager) LogManager() *logging.Manager {
	return m.logMgr
}

// AppsConfig returns the apps configuration.
func (m *ProviderAppManager) AppsConfig() *config.AppsConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.appsConfig
}

// ProviderManagedBinary reports the binary a MANAGED install of a provider
// runs its daemon from, or "" when this node has no managed install of it,
// or its installer cannot name one.
//
// "" is the answer to two different questions at once -- not ours, and not
// nameable -- and callers want the same thing in both cases: do not claim
// zzRouter starts this daemon. Anything more specific belongs on
// ProviderStatus, which reports Managed separately.
func (m *ProviderAppManager) ProviderManagedBinary(name string) string {
	inst, err := m.installs.Dispatcher().Get(name)
	if err != nil {
		return ""
	}
	return managedBinaryOf(inst)
}

// managedBinaryOf asks an installer for its managed daemon binary. No
// IsInstalled gate: BinaryLocator's contract is already "empty when there
// is no managed install to name", and re-checking costs a second marker
// stat to learn what the answer alone says.
func managedBinaryOf(inst install.ProviderInstaller) string {
	locator, ok := inst.(install.BinaryLocator)
	if !ok {
		return ""
	}
	return locator.ManagedBinary()
}

// providerServiceEnv implements install.ServiceEnvFunc: the environment a
// provider's config declares for the daemon its install plan starts.
//
// Resolves the tier tree rather than reading Defaults.Environment, because
// nodes.<node>.environment is the tier an operator reaches for first when
// tuning ONE worker -- and a defaults-only read would leave exactly that
// value out of the start command. Same call the on-demand launch path
// makes (model_instance_runtime.go), so both ways of starting a provider
// answer "what environment does it get" identically.
//
// Model tier is deliberately not resolved: a daemon outlives any one
// model, so there is no model to resolve against when it starts.
//
// Reads through AppsConfig on every call so a plan built after an edit
// carries the edited values; the installers hold this func, never a
// snapshot of the map.
func (m *ProviderAppManager) providerServiceEnv(provider string) map[string]string {
	cfg := m.AppsConfig()
	if cfg == nil {
		return nil
	}
	svc, ok := cfg.LookupApp(provider)
	if !ok {
		return nil
	}
	return config.FlattenEnvironment(svc.Resolve(m.localNodename(), "").Environment)
}

// localNodename is "" when no name was wired, which Resolve reads as
// "skip the node tiers" -- the same answer as before this was threaded,
// rather than a panic or a wrong node's values.
func (m *ProviderAppManager) localNodename() string {
	if m.nodename == nil {
		return ""
	}
	return m.nodename()
}

// ReloadConfig replaces the manager's apps configuration and reconciles all
// internal subsystems: port pools, protocol registry, and version cache.
// This is the SINGLE method the server calls after any config mutation
// (install, uninstall, enable, disable). The server should not iterate
// providers or manage manager internals directly.
func (m *ProviderAppManager) ReloadConfig(fresh *config.AppsConfig) config.ReloadDisposition {
	// nil is a documented reset: manager forgets its config (see
	// TestProviderAppManager_ReloadConfig_NilSafe). Always material.
	fp := fresh.Fingerprint()
	m.mu.Lock()
	unchanged := fresh != nil && m.lastReloadFP == fp
	m.appsConfig = fresh
	m.lastReloadFP = fp
	m.mu.Unlock()

	if fresh == nil {
		m.reconcileServices()
		return config.DispositionApplied
	}
	if unchanged {
		return config.DispositionIgnored
	}

	// 1. Port pools: ensure newly enabled providers have ports allocated
	fresh.RangeApps(func(name string, svc config.ServiceConfig) bool {
		if svc.IsEnabled() && !svc.IsModelHubRegistry() {
			if err := m.ports.EnsurePool(name, svc); err != nil {
				slog.Warn("Failed to register port pool", "provider", name, "error", err)
			}
		}
		return true
	})

	// 2. Protocol registry: add enabled, remove disabled/registry
	m.protocols.Sync(fresh)

	// 3. Version cache: detect versions for newly enabled providers
	m.syncVersions(fresh)
	m.reconcileServices()

	return config.DispositionApplied
}

// syncVersions detects versions for enabled providers that don't have one cached yet.
// Lifetime is bounded by m.shutdownCtx so a racing Stop cancels
// in-flight probes instead of letting them write to a dead manager.
func (m *ProviderAppManager) syncVersions(cfg *config.AppsConfig) {
	if m.shutdown.Load() {
		return
	}
	ctx, cancel := context.WithTimeout(m.shutdownCtx, constants.ClusterActionTimeout)
	defer cancel()

	cfg.RangeApps(func(name string, svc config.ServiceConfig) bool {
		if !svc.IsEnabled() || svc.IsModelHubRegistry() {
			return true
		}

		// Skip if already cached with a real version
		if cached := m.ProviderVersion(name); cached != "" && cached != "unknown" {
			return true
		}

		version := m.detectProviderVersion(ctx, name, &svc)
		m.SetProviderVersion(name, version)
		if version != "" && version != "unknown" && version != "cloud" {
			slog.Info("Provider detected", "name", name, "version", version)
		}
		return true
	})
}

// detectProviderVersion is the single version lookup path. In priority order:
//
//  1. Cloud providers → "cloud" if credentials are available, else "unknown".
//  2. Managed installs → the version file written at install time
//     (install.ReadInstalledVersion). No subprocess.
//  3. Ollama specifically → a one-shot /api/version probe. This is the
//     only non-managed provider zzRouter integrates with; generic
//     detection has been removed.
//
// Anything else returns "unknown". Runtime shell-outs, python module
// imports, and executable version parsing used to live here and were
// cut with the discovery subsystem — managed installs cache their
// version on disk, the user picks the provider at download time, and
// anything we didn't install is not ours to fingerprint.
func (m *ProviderAppManager) detectProviderVersion(ctx context.Context, name string, svc *config.ServiceConfig) string {
	if svc.IsCloudProvider() {
		if svc.IsCloudAvailable() {
			return "cloud"
		}
		return "unknown"
	}
	if v := install.ReadInstalledVersion(name); v != "" {
		return v
	}
	if name == "ollama" {
		if v, ok := detect.ProbeOllama(ctx, svc); ok {
			return v
		}
	}
	return "unknown"
}

// --- Protocol convenience methods ---

// Protocol returns a provider by name.
func (m *ProviderAppManager) Protocol(name string) (protocol.FullProvider, bool) {
	p, err := m.protocols.Get(name)
	if err != nil {
		return nil, false
	}
	return p, true
}

// --- Version cache ---

// ProviderVersion returns the cached version for a provider, or "unknown".
func (m *ProviderAppManager) ProviderVersion(name string) string {
	return m.versions.Get(name)
}

// SetProviderVersion sets the cached version for a provider.
// Used by server discovery to populate version info detected at startup.
func (m *ProviderAppManager) SetProviderVersion(name, version string) {
	m.versions.Set(name, version)
}

// ProviderRequirements returns the requirements from provider config for a provider.
// Returns nil if the provider or its requirements are not configured.
func (m *ProviderAppManager) ProviderRequirements(name string) *config.AppRequirements {
	_, svc, ok := m.resolveConfigKey(name)
	if !ok {
		return nil
	}
	return svc.Requirements
}

// ClearProviderVersion removes the cached version for a provider.
func (m *ProviderAppManager) ClearProviderVersion(name string) {
	m.versions.Clear(name)
}

// ProviderVersions returns a copy of all cached versions.
func (m *ProviderAppManager) ProviderVersions() map[string]string {
	return m.versions.Snapshot()
}

// --- Instance convenience methods ---

// GetInstance returns an instance by ID.
func (m *ProviderAppManager) GetInstance(id string) (*instance.Instance, bool) {
	return m.instances.Get(id)
}

// GetInstanceByModel returns the instance serving a model. The name is
// canonicalized the same way a launch canonicalizes it, so a model looked
// up by the alias it was launched with resolves to the instance that
// launch created. Without that, a caller who launches by repo id can
// never find the run again: the registry holds it under the local name.
func (m *ProviderAppManager) GetInstanceByModel(model string) (*instance.Instance, bool) {
	return m.instances.GetByModel(m.CanonicalModelName(model))
}

// CanonicalModelName maps a caller-supplied model name onto the name this
// node knows the model by. Callers reach a model by whatever name they
// hold — a registry id, a repo path, a file stem — and every one of them
// has to land on the same instance, so anything keyed by model name
// (registry lookups, launch keys) resolves through here first.
func (m *ProviderAppManager) CanonicalModelName(model string) string {
	if m.canonicalModel == nil {
		return model
	}
	if canonical := m.canonicalModel(model); canonical != "" {
		return canonical
	}
	return model
}

// ListInstances returns read-only snapshots of all instances.
func (m *ProviderAppManager) ListInstances(hostname string) []instance.InstanceInfo {
	all := m.instances.List()
	infos := make([]instance.InstanceInfo, 0, len(all))
	for _, inst := range all {
		infos = append(infos, inst.ToInfo(hostname))
	}
	return infos
}

// resolveConfigKey maps a provider name (e.g., "llama.cpp") to its provider config
// config key (e.g., "llamacpp"). Returns the key and config if found.
// Acquires a read lock to safely access appsConfig (which may be swapped by ReloadConfig).
func (m *ProviderAppManager) resolveConfigKey(name string) (string, config.ServiceConfig, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.resolveConfigKeyLocked(name)
}

// resolveConfigKeyLocked performs name resolution without acquiring any locks.
// Safe to call from methods that already hold m.mu.
func (m *ProviderAppManager) resolveConfigKeyLocked(name string) (string, config.ServiceConfig, bool) {
	if m.appsConfig == nil {
		return "", config.ServiceConfig{}, false
	}
	if svc, ok := m.appsConfig.LookupApp(name); ok {
		return name, svc, true
	}
	normalized := configKeyNormalizer.Replace(name)
	if svc, ok := m.appsConfig.LookupApp(normalized); ok {
		return normalized, svc, true
	}
	return "", config.ServiceConfig{}, false
}

// ProviderConfig returns a provider's config by any name a launch accepts
// for it: its config key or a display spelling ("llama.cpp"), which is
// what an instance's Provider may hold.
func (m *ProviderAppManager) ProviderConfig(name string) (config.ServiceConfig, bool) {
	_, svc, ok := m.resolveConfigKey(name)
	return svc, ok
}

// ProviderStatus returns the current status of a provider.
func (m *ProviderAppManager) ProviderStatus(name string) (*ProviderStatus, error) {
	configKey, svcCfg, ok := m.resolveConfigKey(name)
	if !ok {
		return nil, ErrProviderNotFound
	}

	managed := false
	var managedProcess process.Presence
	// Version cache is keyed by config key (e.g., "llamacpp"), installer by display name (e.g., "llama.cpp").
	version := m.ProviderVersion(configKey)
	if inst, err := m.installs.Dispatcher().Get(name); err == nil {
		managed = inst.IsInstalled()
		if v, err := inst.InstalledVersion(); err == nil && v != "" {
			version = v
		}
		// Only ask about a process when there is a managed install to ask
		// about. An empty path is "no install of ours", which is already
		// said by Managed -- answering "unknown" as well would invite a
		// reader to think we tried and failed.
		if bin := managedBinaryOf(inst); bin != "" {
			managedProcess = process.BinaryPresence(bin)
		}
	}

	// Provider is "installed" if either managed by zzRouter or detected externally.
	// "unknown" means detection ran but couldn't determine the version — not a real install.
	installed := managed || (version != "" && version != versionUnknown)

	instances := m.instances.ListByProvider(configKey)
	infos := make([]instance.InstanceInfo, 0, len(instances))
	for _, inst := range instances {
		infos = append(infos, inst.ToInfo(""))
	}

	return &ProviderStatus{
		Name:           svcCfg.Name,
		Installed:      installed,
		Managed:        managed,
		ManagedProcess: managedProcess,
		Version:        version,
		Mode:           svcCfg.Mode,
		Enabled:        svcCfg.IsEnabled(),
		Instances:      infos,
	}, nil
}

// --- Catalog convenience methods ---

// IsProviderSupported returns true if the provider is defined in apps config.
func (m *ProviderAppManager) IsProviderSupported(name string) bool {
	m.mu.RLock()
	cfg := m.appsConfig
	m.mu.RUnlock()
	return cfg.Find(name) != nil
}

// SupportedProviders returns the names of all configured providers.
func (m *ProviderAppManager) SupportedProviders() []string {
	m.mu.RLock()
	cfg := m.appsConfig
	m.mu.RUnlock()
	if cfg == nil {
		return nil
	}
	return cfg.AppNames()
}

// --- Config accessors ---

// HealthCheckConfig returns the health monitor configuration.
func (m *ProviderAppManager) HealthCheckConfig() health.MonitorConfig {
	return m.healthMon.Config()
}

// --- Event emission ---

func (m *ProviderAppManager) emitEvent(e Event) {
	if m.shutdown.Load() {
		return
	}
	switch e.Type {
	case EventInstanceRunning, EventInstanceStopped, EventInstanceFailed:
		if m.catalogChanged != nil {
			m.catalogChanged()
		}
	}
	select {
	case m.auditEvents <- e:
	default:
	}
}

// --- Shutdown ---

// IsShutdown returns true if the manager has been shut down.
func (m *ProviderAppManager) IsShutdown() bool {
	return m.shutdown.Load()
}

// WithInstallPolicy supplies a startup-only path, outside provider sync and mutation.
func WithInstallPolicy(path string) Option {
	return func(m *ProviderAppManager) { m.installPolicyFile = path }
}

func (m *ProviderAppManager) resolveRecipe(provider, runtime string) (install.RecipeSnapshot, error) {
	cfg := m.AppsConfig()
	if cfg == nil {
		return install.RecipeSnapshot{}, ErrProviderNotFound
	}
	sc, ok := cfg.LookupApp(provider)
	if !ok {
		return install.RecipeSnapshot{}, ErrProviderNotFound
	}
	return install.ResolveRecipe(sc, m.localNodename(), runtime, m.installPolicyFile)
}

// CheckInstallAuthority checks proposed provider data against this node's protected authority.
func (m *ProviderAppManager) CheckInstallAuthority(sc config.ServiceConfig, runtime string) (install.RecipeSnapshot, error) {
	return install.ResolveRecipe(sc, m.localNodename(), runtime, m.installPolicyFile)
}

// InstallPolicyCapabilities publishes this node's non-secret authority for a declared runtime.
func (m *ProviderAppManager) InstallPolicyCapabilities(sc config.ServiceConfig, runtime string) install.PolicyCapabilities {
	result := install.PolicyCapabilities{}
	if snapshot, err := install.ResolveRecipe(sc, m.localNodename(), runtime, ""); err == nil {
		result.ReleaseRecipe = snapshot.Authority == "release"
	}
	policy, fp, err := install.ReadInstallPolicy(m.installPolicyFile)
	if err != nil {
		result.Reason = err.Error()
		return result
	}
	grant, ok := policy.Runtimes[sc.Name+"/"+runtime]
	if !ok {
		result.Reason = "protected policy has no runtime grant"
		return result
	}
	result.Grant = &grant
	result.PolicyFingerprint = fp
	resolved, err := sc.ResolveInstall(m.localNodename(), runtime)
	if err != nil {
		result.Reason = err.Error()
		return result
	}
	if err := grant.Authorize(resolved.Recipe); err != nil {
		result.Reason = err.Error()
		return result
	}
	result.OverrideApproved = true
	return result
}
