package prov_apps

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/utils"
)

// versionCache is a thread-safe map of provider config-key → installed version.
// Owned by ProviderAppManager and shared by reference with InstallCoordinator
// so post-install/upgrade/uninstall mutations land in the same store the
// facade reads from.
type versionCache struct {
	mu sync.RWMutex
	m  map[string]string
}

func newVersionCache() *versionCache {
	return &versionCache{m: make(map[string]string)}
}

// Get returns the cached version, or "unknown" if absent.
func (c *versionCache) Get(name string) string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if v, ok := c.m[name]; ok {
		return v
	}
	return "unknown"
}

// Lookup returns the raw entry (empty string + false if absent), preserving
// the "set but empty" vs "never set" distinction used by managed-install
// guards.
func (c *versionCache) Lookup(name string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.m[name]
	return v, ok
}

// Set stores a version for a provider.
func (c *versionCache) Set(name, version string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[name] = version
}

// Clear removes a cached version (called post-uninstall).
func (c *versionCache) Clear(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, name)
}

// MergeNonEmpty copies entries with non-empty values into the cache. Used by
// the bulk version-detection path during DiscoverAndRegister.
func (c *versionCache) MergeNonEmpty(in map[string]string) {
	if len(in) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, v := range in {
		if v != "" {
			c.m[k] = v
		}
	}
}

// Snapshot returns a copy of all cached entries.
func (c *versionCache) Snapshot() map[string]string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]string, len(c.m))
	for k, v := range c.m {
		out[k] = v
	}
	return out
}

// InstallCoordinator owns the install/upgrade/uninstall surface and per-name
// install serialization. Self-contained: dependencies passed at construction;
// no back-pointer to ProviderAppManager. State shared with the manager
// (versions cache, shutdown gate, instances registry) is shared by pointer
// to a value type, not via interface dispatch.
type InstallCoordinator struct {
	dispatcher     *install.Dispatcher
	progress       *install.ProgressTracker
	catalogChanged func()
	audit          chan<- Event       // write-only view of manager.auditEvents
	shutdown       *atomic.Bool       // shared with manager — single source of truth
	instances      *instance.Registry // for "refuse upgrade/uninstall if instances running"
	versions       *versionCache      // shared with manager by pointer
	appsConfig     func() *config.AppsConfig
	jobs           *jobs.Registry // optional — nil disables SSE integration
	serviceControl func(context.Context, string, install.ServiceAction) error
	smoke          func(context.Context, string, string, string) error

	locks sync.Map // map[string]chan struct{} serializing provider lifecycle

	// wg tracks every detached goroutine spawned by {Install,Upgrade,Uninstall}Async.
	// WaitForInstalls drains it on shutdown.
	wg sync.WaitGroup

	// liveMu guards liveJobs, the provider→jobID reservation index used
	// to dedupe concurrent async requests for the same provider. A second
	// POST while a job is in flight returns the SAME jobID with 202
	// instead of blocking on the per-name install mutex.
	liveMu   sync.Mutex
	liveJobs map[string]string
}

func (c *InstallCoordinator) installContext(ctx context.Context, provider string) context.Context {
	if c.serviceControl == nil {
		return ctx
	}
	return install.WithServiceControl(ctx, func(ctx context.Context, action install.ServiceAction) error {
		return c.serviceControl(ctx, provider, action)
	})
}

func newCoordinator(
	dispatcher *install.Dispatcher,
	progress *install.ProgressTracker,
	audit chan<- Event,
	shutdown *atomic.Bool,
	instances *instance.Registry,
	versions *versionCache,
	appsConfig func() *config.AppsConfig,
	jobsReg *jobs.Registry,
) *InstallCoordinator {
	return &InstallCoordinator{
		dispatcher: dispatcher,
		progress:   progress,
		audit:      audit,
		shutdown:   shutdown,
		instances:  instances,
		versions:   versions,
		appsConfig: appsConfig,
		jobs:       jobsReg,
		liveJobs:   make(map[string]string),
	}
}

// openJob is the shared path for opening a jobs.Handle. Returns nil if
// the registry is unavailable or Start fails (both non-fatal — the
// producer runs without SSE integration). All four async-202 paths
// (install/upgrade/uninstall/execute-step) use StartDetached so the job
// outlives the HTTP request that accepted it.
func (c *InstallCoordinator) openJob(action, provider string) jobs.Handle {
	if c.jobs == nil {
		return nil
	}
	h, err := c.jobs.StartDetached(jobs.KindInstall, "", jobs.Meta{
		"provider": provider,
		"action":   action,
	})
	if err != nil {
		return nil
	}
	return h
}

// Dispatcher returns the install.Dispatcher for provider-installer lookup.
func (c *InstallCoordinator) Dispatcher() *install.Dispatcher {
	return c.dispatcher
}

// Installer is shorthand for c.Dispatcher().Get(name) — the dominant call
// pattern in handlers that need to invoke Preflight, InstallPlan, etc.
func (c *InstallCoordinator) Installer(name string) (install.ProviderInstaller, error) {
	return c.dispatcher.Get(name)
}

// Progress returns the current install progress for a provider, or nil.
func (c *InstallCoordinator) Progress(provider string) *install.InstallProgress {
	return c.progress.Get(provider)
}

// StartProgress creates a fresh progress handle for a provider install. Used
// by the single-step executor so /install/status polls return byte-level
// download telemetry the same way the run-all path does.
func (c *InstallCoordinator) StartProgress(provider string) *install.InstallProgress {
	return c.progress.Start(provider, "install", 0)
}

// acquireProvider acquires the per-name gate for install/upgrade/uninstall.
// Closes the race where two goroutines both pass the IsInstalled() gate
// before either calls Install. Different providers run in parallel;
// cross-process protection is handled by the install-package LockFile.
func (c *InstallCoordinator) acquireProvider(ctx context.Context, name string) (func(), error) {
	gate, _ := c.locks.LoadOrStore(name, make(chan struct{}, 1))
	semaphore, _ := gate.(chan struct{})
	select {
	case semaphore <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-semaphore
			return nil, err
		}
		return func() { <-semaphore }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// emit sends an event to the audit channel. Non-blocking; events are dropped
// once shutdown is set.
func (c *InstallCoordinator) emit(e Event) {
	if c.shutdown.Load() {
		return
	}
	switch e.Type {
	case EventInstallCompleted, EventUpgradeCompleted, EventUninstallCompleted:
		if c.catalogChanged != nil {
			c.catalogChanged()
		}
	}
	select {
	case c.audit <- e:
	default:
	}
}

// checkPlatform enforces ServiceConfig.Platforms against the running host.
// Empty/missing list = unconstrained. Wraps ErrUnsupportedPlatform so HTTP
// mappers return 400. Returns nil when the platform is allowed or when no
// config is loaded for the provider (treat unknown as unconstrained — the
// dispatcher.Get above already screens out truly missing providers).
func (c *InstallCoordinator) checkPlatform(name string) error {
	cfg := c.appsConfig()
	if cfg == nil {
		return nil
	}
	svc, ok := cfg.LookupApp(name)
	if !ok || len(svc.Platforms) == 0 {
		return nil
	}
	cur := fsroot.CurrentPlatform()
	for _, p := range svc.Platforms {
		if p.OS == cur.OS && (p.Arch == "" || p.Arch == cur.Arch) {
			return nil
		}
	}
	return fmt.Errorf("%w: %s is not supported on %s", install.ErrUnsupportedPlatform, name, cur.String())
}

// resolvePinned returns the configured pinned version for a provider, or the
// requested version if non-empty.
//
// The pin seeds a fresh install: it is what an operator wants a NEW node to
// start on. It is deliberately NOT consulted by upgrade — see
// resolveUpgradeTarget for why.
//
// Both on-demand AND external kinds carry pinned_version (Ollama is external
// but ships a release tag in config). The on-demand-only lookup that
// previously lived here silently fell through to "" for externals, which
// pushed the resolution onto each builtin's own upstream call — a network
// round trip that times out on slow links.
func (c *InstallCoordinator) resolvePinned(provider, requested string) string {
	if requested != "" {
		return requested
	}
	// The accessor itself can be unset on a partially-wired coordinator;
	// calling a nil func field panics rather than returning nil.
	if c.appsConfig == nil {
		return ""
	}
	cfg := c.appsConfig()
	if cfg == nil {
		return ""
	}
	if od := cfg.GetOnDemand(provider); od != nil && od.PinnedVersion != "" {
		return od.PinnedVersion
	}
	if ep := cfg.GetExternal(provider); ep != nil && ep.PinnedVersion != "" {
		return ep.PinnedVersion
	}
	return ""
}

// resolveUpgradeTarget returns the version an upgrade should install.
//
// An explicit request wins. Otherwise upgrade resolves the newest release
// upstream publishes — NOT the config pin.
//
// Resolving to the pin would make "upgrade" mean "reinstall the version new
// nodes start on", which is a no-op whenever the node already matches, and a
// silent downgrade after an operator has upgraded a node past the pin once.
// The pin governs install; upgrade means move forward. An operator who wants
// the pin can still pass it explicitly.
//
// A failed upstream lookup falls back to the pin rather than erroring: the
// old behaviour is a worse answer, but it is better than refusing to upgrade
// at all when the network is down.
func (c *InstallCoordinator) resolveUpgradeTarget(ctx context.Context, provider, requested string) string {
	if requested != "" {
		return requested
	}
	// Installable, not merely published: on a node whose provider comes
	// through a packager, the project's newest tag is a target the installer
	// cannot reach.
	if tag, err := install.LatestInstallableRelease(ctx, provider); err == nil && tag.Tag != "" {
		return tag.Tag
	}
	return c.resolvePinned(provider, "")
}

// resolveConfigKeyAndVersion returns the canonical config key (after
// dot/hyphen normalization) and the cached version. Used by the managed-
// install guards in Upgrade/Uninstall — distinguishes "not in config"
// (ErrProviderNotFound) from "in config but version was detected externally"
// (ErrProviderNotManaged).
//
//nolint:unparam // configKey is the public contract; callers today only consume version but the normalized key is the documented resolution output
func (c *InstallCoordinator) resolveConfigKeyAndVersion(name string) (configKey, version string, ok bool) {
	cfg := c.appsConfig()
	if cfg == nil {
		return "", "", false
	}
	if _, found := cfg.LookupApp(name); found {
		configKey = name
	} else {
		normalized := configKeyNormalizer.Replace(name)
		if _, found := cfg.LookupApp(normalized); !found {
			return "", "", false
		}
		configKey = normalized
	}
	version, _ = c.versions.Lookup(configKey)
	return configKey, version, true
}

// progressOnlyHandle wraps a jobs.Handle and swallows terminal calls
// (Done/Fail). Used to attach progress-only mirroring onto
// InstallProgress without letting the progress tracker's automatic
// terminal emission fire BEFORE the coordinator has run its finalize
// callback. The real handle lives on the coord goroutine and decides
// when to terminate — only that code has the full success contract
// (install succeeded AND finalize flipped config).
type progressOnlyHandle struct{ inner jobs.Handle }

func (p progressOnlyHandle) Progress(percent int, step string, bytes jobs.Bytes) {
	p.inner.Progress(percent, step, bytes)
}
func (p progressOnlyHandle) Meta(m jobs.Meta)         { p.inner.Meta(m) }
func (p progressOnlyHandle) Done()                    {} // swallow; coord fires
func (p progressOnlyHandle) Fail(err error)           {} // swallow; coord fires
func (p progressOnlyHandle) Context() context.Context { return p.inner.Context() }
func (p progressOnlyHandle) ID() string               { return p.inner.ID() }
func (p progressOnlyHandle) Epoch() string            { return p.inner.Epoch() }

// ErrAsyncDisabled indicates the async entry points were called with no jobs
// registry wired. This is a wiring / configuration bug, not a shutdown state —
// kept as a distinct sentinel so errors.Is(err, ErrShutdown) does NOT match
// and HTTP error mapping surfaces 500-misconfig instead of 503-retry-later.
var ErrAsyncDisabled = errors.New("async install requires jobs registry")

// reserveJob attempts to claim an in-flight slot for provider. Returns the
// existing live jobID (+ true) if one is already running so the caller can
// short-circuit to an idempotent 202 response; returns ("", false) when the
// slot was successfully reserved.
func (c *InstallCoordinator) reserveJob(provider, jobID string) (string, bool) {
	c.liveMu.Lock()
	defer c.liveMu.Unlock()
	if existing, ok := c.liveJobs[provider]; ok {
		return existing, true
	}
	c.liveJobs[provider] = jobID
	return "", false
}

// releaseJob clears the live-slot for provider once the goroutine exits.
// Safe to call with an id that has since been overwritten by another claim
// — the compare-and-delete pattern prevents ABA.
func (c *InstallCoordinator) releaseJob(provider, jobID string) {
	c.liveMu.Lock()
	defer c.liveMu.Unlock()
	if c.liveJobs[provider] == jobID {
		delete(c.liveJobs, provider)
	}
}

// WaitForInstalls blocks until every detached install/upgrade/uninstall
// goroutine exits, or ctx expires. Called from ProviderAppManager.Stop so
// graceful shutdown drains the installs. On ctx timeout, returns ctx.Err()
// and logs a warn (callers are expected to force-exit; orphaned installs
// will be reaped by pkg/jobs' inactivity-gated TTL on next startup).
func (c *InstallCoordinator) WaitForInstalls(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		c.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// InstallAsync opens a jobs handle, returns its ID synchronously, and runs
// the install body on a tracked detached goroutine. Cancel via
// DELETE /zzrouter/v1/jobs/:id propagates to the installer through the
// handle's ctx. finalize runs inside the goroutine after a successful
// install and before the terminal Done event — if finalize fails the
// handle emits Fail with that error so subscribers see the drift (binary
// on disk, config never flipped) instead of a false success.
//
// Returns the live jobID + nil when a concurrent request is already
// installing the same provider; callers should respond with 202 and that
// ID (idempotent retry).
//
// Synchronous errors: ErrShutdown, ErrProviderNotFound,
// ErrProviderAlreadyInstalled. These carry their usual HTTP mapping —
// no job is opened, no goroutine spawned.
func (c *InstallCoordinator) InstallAsync(ctx context.Context, req InstallRequest, finalize func() error) (string, error) {
	if c.shutdown.Load() {
		return "", ErrShutdown
	}
	// Wiring / infra check FIRST so a missing jobs registry fails loud
	// with a programming-error sentinel instead of masquerading as a
	// plausible-looking domain error (AlreadyInstalled, NotFound).
	if c.jobs == nil {
		return "", ErrAsyncDisabled
	}
	inst, err := c.dispatcher.Get(req.Provider)
	if err != nil {
		return "", ErrProviderNotFound
	}
	if err := c.checkPlatform(req.Provider); err != nil {
		return "", err
	}
	if inst.IsInstalled() && !req.Force {
		return "", ErrProviderAlreadyInstalled
	}

	// Open the handle BEFORE reserving the slot so we have an ID to
	// register; close on conflict.
	h := c.openJob("install", req.Provider)
	if h == nil {
		return "", ErrAsyncDisabled
	}
	if existing, busy := c.reserveJob(req.Provider, h.ID()); busy {
		// Another install is already live; abandon our handle and
		// return the live one. Mark ours Done immediately so the
		// transient ID doesn't leak until TTL — subscribers on the
		// live ID see the real progress.
		h.Done()
		return existing, nil
	}

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer c.releaseJob(req.Provider, h.ID())
		defer func() {
			if r := recover(); r != nil {
				slog.Error("install goroutine panicked",
					"panic", r, "stack", string(debug.Stack()),
					"provider", req.Provider, "job_id", h.ID())
				h.Fail(fmt.Errorf("install panic: %v", r))
			}
		}()

		if err := c.runInstall(h.Context(), h, req, inst, nil); err != nil {
			h.Fail(err)
			return
		}
		if finalize != nil {
			if err := finalize(); err != nil {
				h.Fail(fmt.Errorf("finalize: %w", err))
				return
			}
		}
		h.Done()
	}()
	return h.ID(), nil
}

// runInstall is the goroutine body. Uses h.Context() so
// DELETE /jobs/:id propagates to installer.
func (c *InstallCoordinator) runInstall(ctx context.Context, h jobs.Handle, req InstallRequest, inst install.ProviderInstaller, reportProgress func(string)) error {
	unlock, err := c.acquireProvider(ctx, req.Provider)
	if err != nil {
		return err
	}
	defer unlock()
	ctx = c.installContext(ctx, req.Provider)
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.shutdown.Load() {
		return ErrShutdown
	}
	if c.instances.CountActiveByProvider(c.runtimeProvider(req.Provider)) > 0 {
		return ErrInstancesRunning
	}

	if inst.IsInstalled() {
		if !req.Force {
			return ErrProviderAlreadyInstalled
		}
		typed := false
		if c.appsConfig != nil && c.appsConfig() != nil {
			cfg, ok := c.appsConfig().LookupApp(req.Provider)
			typed = ok && cfg.Install != nil
		}
		if !typed {
			if err := inst.Uninstall(ctx); err != nil {
				return fmt.Errorf("force-reinstall: uninstall failed: %w", err)
			}
		}
	}

	version := c.resolvePinned(req.Provider, req.Version)
	progress := c.progress.Start(req.Provider, "install", 0)
	// Attach a progress-only wrapper so byte-level download updates
	// mirror onto the jobs stream but the tracker's auto-terminal
	// (SetCompleted → h.Done) does NOT fire — this goroutine's caller
	// owns the terminal emission AFTER finalize runs.
	if h != nil {
		progress.SetJobHandle(progressOnlyHandle{inner: h})
	}

	if pr, ok := inst.(install.ProgressReporter); ok {
		pr.SetProgress(progress)
		defer pr.SetProgress(nil)
	}

	c.emit(Event{
		Type: EventInstallStarted, Provider: req.Provider,
		Message: "Installing " + req.Provider, Time: utils.Now(),
	})

	if err := inst.Install(ctx, version, func(msg string) {
		if reportProgress != nil {
			reportProgress(msg)
		}
		if n, total, desc, ok := parseStepProgress(msg); ok {
			progress.SetStep(n, total, desc)
		}
		c.emit(Event{
			Type: EventInstallProgress, Provider: req.Provider,
			Message: msg, Time: utils.Now(),
		})
	}); err != nil {
		progress.SetFailed(err.Error())
		c.emit(Event{
			Type: EventInstallFailed, Provider: req.Provider,
			Error: err, Message: err.Error(), Time: utils.Now(),
		})
		return err
	}

	progress.SetCompleted()
	c.emit(Event{
		Type: EventInstallCompleted, Provider: req.Provider,
		Message: "Installation complete", Time: utils.Now(),
	})

	if v, err := inst.InstalledVersion(); err == nil && v != "" {
		c.versions.Set(req.Provider, v)
	}
	return nil
}

// UpgradeAsync mirrors InstallAsync shape. Upgrade has no percent
// telemetry; subscribers see a single pending event then Done/Fail.
// toVersion pins the upgrade target; empty means "the newest release this
// node can install, else the config pin" — see resolveUpgradeTarget.
//
// The context is accepted for symmetry with InstallAsync and deliberately
// unused: the work outlives the request that started it, so everything past
// admission runs on the job's own context.
func (c *InstallCoordinator) UpgradeAsync(_ context.Context, name, toVersion string, finalize func() error) (string, error) {
	if c.shutdown.Load() {
		return "", ErrShutdown
	}
	if c.jobs == nil {
		return "", ErrAsyncDisabled
	}
	if c.instances.CountActiveByProvider(name) > 0 {
		return "", ErrInstancesRunning
	}
	inst, err := c.dispatcher.Get(name)
	if err != nil {
		return "", ErrProviderNotFound
	}
	if err := c.checkPlatform(name); err != nil {
		return "", err
	}
	if !inst.IsInstalled() {
		if _, version, ok := c.resolveConfigKeyAndVersion(name); ok && version != "" && version != versionUnknown {
			return "", ErrProviderNotManaged
		}
		return "", ErrProviderNotFound
	}

	h := c.openJob("upgrade", name)
	if h == nil {
		return "", ErrAsyncDisabled
	}
	if existing, busy := c.reserveJob(name, h.ID()); busy {
		h.Done()
		return existing, nil
	}

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer c.releaseJob(name, h.ID())
		defer func() {
			if r := recover(); r != nil {
				slog.Error("upgrade goroutine panicked",
					"panic", r, "stack", string(debug.Stack()),
					"provider", name, "job_id", h.ID())
				h.Fail(fmt.Errorf("upgrade panic: %v", r))
			}
		}()

		// Resolution happens inside runUpgrade, on the job's context. The
		// ctx passed to UpgradeAsync belongs to the HTTP request that
		// accepted the job, and gin cancels it the moment that handler
		// returns 202 — well before this goroutine reaches the network.
		if err := c.runUpgrade(h, name, toVersion, inst); err != nil {
			h.Fail(err)
			return
		}
		if finalize != nil {
			if err := finalize(); err != nil {
				h.Fail(fmt.Errorf("finalize: %w", err))
				return
			}
		}
		h.Done()
	}()
	return h.ID(), nil
}

// runUpgrade resolves the target and performs the upgrade. Both run on the
// job's context: an upgrade with no explicit target has to reach the network
// to learn what "latest" means, and doing that on the request context made
// every such lookup fail instantly and fall back to the pin — which is the
// one answer resolveUpgradeTarget documents as wrong.
func (c *InstallCoordinator) runUpgrade(h jobs.Handle, name, requested string, inst install.ProviderInstaller) error {
	ctx := h.Context()
	unlock, err := c.acquireProvider(ctx, name)
	if err != nil {
		return err
	}
	defer unlock()
	if c.instances.CountActiveByProvider(c.runtimeProvider(name)) > 0 {
		return ErrInstancesRunning
	}
	ctx = c.installContext(ctx, name)
	toVersion := c.resolveUpgradeTarget(ctx, name, requested)

	if err := inst.Upgrade(ctx, toVersion, func(msg string) {
		c.emit(Event{
			Type: EventUpgradeStarted, Provider: name,
			Message: msg, Time: utils.Now(),
		})
		// Meta only patches the sidecar payload — it is carried by the NEXT
		// emitted event, so on its own it publishes nothing and a subscriber
		// sees one pending frame then the terminal one. Progress is what
		// emits, which is why an upgrade showed no steps while an install
		// did. Keep the Meta patch so the terminal frame still names the
		// last step.
		h.Meta(jobs.Meta{"step": msg})
		h.Progress(upgradeStepPercent(msg), msg, jobs.Bytes{})
	}); err != nil {
		return err
	}
	if v, err := inst.InstalledVersion(); err == nil && v != "" {
		c.versions.Set(name, v)
	}
	c.emit(Event{Type: EventUpgradeCompleted, Provider: name, Time: utils.Now()})
	return nil
}

// upgradeStepRe matches the "[n/m]" counter the installers prefix their step
// descriptions with.
var upgradeStepRe = regexp.MustCompile(`^\[(\d+)/(\d+)\]`)

// upgradeStepPercent derives a percentage from a step description.
//
// Returns 0 when the message carries no counter, which the emit path treats
// as "no percentage known" rather than as zero progress — an upgrade whose
// installer words its steps differently still streams the text.
func upgradeStepPercent(msg string) int {
	m := upgradeStepRe.FindStringSubmatch(msg)
	if m == nil {
		return 0
	}
	cur, err1 := strconv.Atoi(m[1])
	total, err2 := strconv.Atoi(m[2])
	if err1 != nil || err2 != nil || total <= 0 || cur < 0 {
		return 0
	}
	if cur > total {
		cur = total
	}
	return cur * 100 / total
}

// UninstallAsync mirrors InstallAsync shape. finalize (disable config)
// runs inside the goroutine before Done.
func (c *InstallCoordinator) UninstallAsync(ctx context.Context, name string, finalize func() error) (string, error) {
	if c.shutdown.Load() {
		return "", ErrShutdown
	}
	if c.jobs == nil {
		return "", ErrAsyncDisabled
	}
	if c.instances.CountActiveByProvider(name) > 0 {
		return "", ErrInstancesRunning
	}
	inst, err := c.dispatcher.Get(name)
	if err != nil {
		return "", ErrProviderNotFound
	}
	if !inst.IsInstalled() {
		if _, version, ok := c.resolveConfigKeyAndVersion(name); ok && version != "" && version != versionUnknown {
			return "", ErrProviderNotManaged
		}
		return "", ErrProviderNotFound
	}

	h := c.openJob("uninstall", name)
	if h == nil {
		return "", ErrAsyncDisabled
	}
	if existing, busy := c.reserveJob(name, h.ID()); busy {
		h.Done()
		return existing, nil
	}

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer c.releaseJob(name, h.ID())
		defer func() {
			if r := recover(); r != nil {
				slog.Error("uninstall goroutine panicked",
					"panic", r, "stack", string(debug.Stack()),
					"provider", name, "job_id", h.ID())
				h.Fail(fmt.Errorf("uninstall panic: %v", r))
			}
		}()

		if err := c.runUninstall(h, name, inst); err != nil {
			h.Fail(err)
			return
		}
		if finalize != nil {
			if err := finalize(); err != nil {
				h.Fail(fmt.Errorf("finalize: %w", err))
				return
			}
		}
		h.Done()
	}()
	return h.ID(), nil
}

func (c *InstallCoordinator) runUninstall(h jobs.Handle, name string, inst install.ProviderInstaller) error {
	ctx := h.Context()
	unlock, err := c.acquireProvider(ctx, name)
	if err != nil {
		return err
	}
	defer unlock()
	if c.instances.CountActiveByProvider(c.runtimeProvider(name)) > 0 {
		return ErrInstancesRunning
	}
	ctx = c.installContext(ctx, name)

	if err := inst.Uninstall(ctx); err != nil {
		return err
	}
	c.versions.Clear(name)
	c.emit(Event{Type: EventUninstallCompleted, Provider: name, Time: utils.Now()})
	return nil
}

// ExecuteStepAsync runs a single install plan step on a goroutine and
// returns the job_id for subscribers. Mirrors InstallAsync's shape:
// progress is mirrored onto the jobs stream via progressOnlyHandle so
// byte-level download telemetry flows, and terminal Done/Fail is
// emitted exclusively by this method's goroutine.
//
// finalize is invoked after the step passes AND VerifyAll reports all
// steps now pass — wizard-terminal equivalent to the Run All's
// finalizeOn callback. nil-safe.
func (c *InstallCoordinator) ExecuteStepAsync(ctx context.Context, provider, version string, step int, finalize func() error) (string, error) {
	if c.shutdown.Load() {
		return "", ErrShutdown
	}
	if c.jobs == nil {
		return "", ErrAsyncDisabled
	}
	inst, err := c.dispatcher.Get(provider)
	if err != nil {
		return "", ErrProviderNotFound
	}

	h := c.openJob("install-step", provider)
	if h == nil {
		return "", ErrAsyncDisabled
	}
	// No reserveJob here — multiple single-step runs are legitimate
	// (user can re-run a failed step while an unrelated install job is
	// tracked). The per-provider acquireProvider() inside runInstallStep
	// still serializes mutations.

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer func() {
			if r := recover(); r != nil {
				slog.Error("execute-step goroutine panicked",
					"panic", r, "stack", string(debug.Stack()),
					"provider", provider, "step", step, "job_id", h.ID())
				h.Fail(fmt.Errorf("execute-step panic: %v", r))
			}
		}()

		result, err := c.runInstallStep(h, provider, version, step, inst)
		if err != nil {
			h.Fail(err)
			return
		}
		if !result.Passed {
			h.Fail(fmt.Errorf("%s", result.Message))
			return
		}
		if finalize != nil {
			if err := finalize(); err != nil {
				h.Fail(fmt.Errorf("finalize: %w", err))
				return
			}
		}
		h.Meta(jobs.Meta{"result": result})
		h.Done()
	}()
	return h.ID(), nil
}

// runInstallStep is the goroutine body for ExecuteStepAsync. Mirrors
// the body of HandleInternalExecuteStep (the sync executor) but routes
// progress through the jobs handle. Returns the StepResult for the
// caller to inspect and encode onto the terminal event.
func (c *InstallCoordinator) runInstallStep(h jobs.Handle, provider, version string, step int, inst install.ProviderInstaller) (*install.StepResult, error) {
	ctx := h.Context()
	unlock, err := c.acquireProvider(ctx, provider)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if c.instances.CountActiveByProvider(c.runtimeProvider(provider)) > 0 {
		return nil, ErrInstancesRunning
	}
	ctx = c.installContext(ctx, provider)

	progress := c.progress.Start(provider, "install", 0)
	progress.SetJobHandle(progressOnlyHandle{inner: h})
	if pr, ok := inst.(install.ProgressReporter); ok {
		pr.SetProgress(progress)
		defer pr.SetProgress(nil)
	}

	resolved := version
	if version == "" {
		resolved = c.resolvePinned(provider, "")
	}
	plan, err := inst.InstallPlan(ctx, resolved)
	if err != nil {
		progress.SetFailed(err.Error())
		return nil, err
	}

	// Skip-if-already-passed mirrors the sync handler. Emit a single
	// meta frame so SSE subscribers see the reason and the terminal
	// arrives cleanly via the caller's h.Done().
	for _, s := range plan.Steps {
		if s.Number != step {
			continue
		}
		if s.Verify.Type != "" {
			if vr := install.VerifyStepContext(ctx, s); vr.Passed {
				progress.SetCompleted()
				return &vr, nil
			}
		}
		progress.SetStep(s.Number, len(plan.Steps), s.Description)
		break
	}

	result := plan.ExecuteStep(ctx, step)
	if result.Passed {
		progress.SetCompleted()
	} else {
		progress.SetFailed(result.Message)
	}
	return result, nil
}

// parseStepProgress extracts step number, total, and description from
// "[N/M] desc" format emitted by the install runner.
func parseStepProgress(msg string) (step, total int, desc string, ok bool) {
	n, err := fmt.Sscanf(msg, "[%d/%d]", &step, &total)
	if err != nil || n != 2 {
		return 0, 0, "", false
	}
	idx := strings.Index(msg, "] ")
	if idx < 0 {
		return step, total, "", true
	}
	return step, total, msg[idx+2:], true
}
