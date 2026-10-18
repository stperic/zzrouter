// Package cache is the cluster-wide model cache. It layers over
// modelregistry with an EventCache semantics: invalidated only by
// mutations (pull completion, apps-config change, cluster handlers,
// etc.), never by TTL. Reads return whatever state the last populate
// left behind; freshness is a function of mutation coverage, not
// wall-clock time.
//
// This package is the step-3 move of the model consolidation plan;
// see docs/plan_model_subsystem_consolidation.md.
package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/modelregistry"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stperic/zzrouter/pkg/utils"
)

// AutoRouter is the minimal surface the cache needs from the auto-route
// manager — reconcile after every cache refresh. Defined here (rather
// than importing pkg/model/autoroute) because autoroute imports cache
// for CachedModel; the reverse direction would be an import cycle.
// Implemented by AutoRouter.
type AutoRouter interface {
	SyncAllFromCache()
}

// NodeInfo is the minimal local-node metadata the cache needs. Implemented
// by the internal/server NodeIdentity; passed in as an interface so the
// cache package stays decoupled from internal/server types.
type NodeInfo interface {
	Nodename() string
}

// Cache owns the cluster-wide model cache. EventCache: invalidated by
// mutation events, never by TTL. Every node runs its own Cache; on
// coordinators the index aggregates local + peers, on workers (and
// standalone / unclaimed) the index is local-only — the lookup API
// is unified regardless of role.
type Cache struct {
	modelDetails    func(string, string, map[string]any) map[string]any
	inventoryStatus func(context.Context, map[string]string) []InventoryUnavailable
	// Injected dependencies
	node                      NodeInfo
	registry                  func() *modelregistry.Registry
	appsConfig                func() *pkgConfig.AppsConfig
	clusterClient             func() mesh.ClusterClient
	nodeCache                 *NodeResourceCache
	resources                 *mesh.ResourceTracker
	autoRoute                 AutoRouter
	config                    *pkgConfig.NodeConfig
	resolveEndpointToNodename func(string) string

	// Lifecycle: Start installs lifeCtx / lifeCancel; Stop cancels and
	// waits for any in-flight async refresh. Start→Stop→Start cycles
	// safely — required for coordinator promote/demote (mTLS PR 4).
	lifecycleMu sync.Mutex
	lifeCtx     context.Context
	lifeCancel  context.CancelFunc

	// Owned cache state.
	// valid means models/indexes were populated since the last Invalidate.
	// lastUpdated is retained for /stats observability only; reads never
	// consult it for validity decisions.
	admissionModels            []*CachedModel
	localSnapshot              localModelSnapshot
	modelRefusals              map[string]error
	generation                 uint64
	admissionUnavailableLogged bool
	models                     []*CachedModel
	indexByTarget              map[modelTarget]*CachedModel
	indexByNode                map[modelNode]*CachedModel
	inventoryUnavailable       map[providerNode]InventoryUnavailable
	indexByName                map[string]*CachedModel
	indexBySuffix              map[string]*CachedModel
	valid                      bool
	lastUpdated                time.Time
	lastAttempt                time.Time
	populateErr                error
	mu                         sync.RWMutex
	// refreshMu coalesces blocking discovery without holding the catalog read lock.
	refreshMu sync.Mutex
	// refreshing gates the fire-and-forget RefreshAsync path so
	// concurrent triggers collapse to one background goroutine.
	refreshing int32

	// reloadMu guards lastReloadFP. ReloadConfig may run concurrently
	// with reads, but the fingerprint write itself is cheap and the
	// critical section is two scalars.
	reloadMu     sync.Mutex
	lastReloadFP [32]byte
}

type localModelSnapshot struct {
	models      []map[string]any
	unavailable []InventoryUnavailable
}

// Config bundles Cache dependencies. Named fields beat a 10-arg
// constructor: callers pass what they have by name, missing fields
// land as nil zero-values that downstream methods already nil-guard.
type Config struct {
	// InventoryStatus adds observed daemon state to an atomic registry scan snapshot.
	InventoryStatus func(context.Context, map[string]string) []InventoryUnavailable
	// ModelDetails enriches local entries using evidence on this node.
	ModelDetails func(string, string, map[string]any) map[string]any
	// Node identifies the local node — used for hostname tagging on
	// cached entries and master-endpoint discovery in
	// NotifyMasterCacheRefresh.
	Node NodeInfo
	// Registry returns the current model registry (lazy accessor so the
	// cache can be constructed before the registry is fully wired).
	Registry func() *modelregistry.Registry
	// AppsConfig returns the current providers config (lazy).
	AppsConfig func() *pkgConfig.AppsConfig
	// ClusterClient lazily supplies worker inventory reads; nil means no cluster.
	ClusterClient func() mesh.ClusterClient
	// NodeCache receives metrics from the same accepted catalog publication.
	NodeCache *NodeResourceCache
	// Resources is the local node's resource tracker. Its metrics are
	// written into NodeCache under the local hostname on each refresh.
	Resources *mesh.ResourceTracker
	// AutoRoute, if non-nil, is reconciled after every successful refresh.
	// Set post-construction via SetAutoRoute when the autoroute manager
	// is wired.
	AutoRoute AutoRouter
	// NodeConfig drives cluster-mode branching in Start.
	NodeConfig *pkgConfig.NodeConfig
	// ResolveEndpointToNodename maps a host:port back to a node name
	// (used to normalize the "node" field on worker-reported models).
	ResolveEndpointToNodename func(string) string
}

// New constructs a Cache from Config. Accessors (Registry, AppsConfig,
// ClusterClient) are called lazily so the Cache can be wired before
// those dependencies are fully ready.
func New(cfg Config) *Cache {
	return &Cache{
		modelDetails:              cfg.ModelDetails,
		inventoryStatus:           cfg.InventoryStatus,
		node:                      cfg.Node,
		registry:                  cfg.Registry,
		appsConfig:                cfg.AppsConfig,
		clusterClient:             cfg.ClusterClient,
		nodeCache:                 cfg.NodeCache,
		resources:                 cfg.Resources,
		autoRoute:                 cfg.AutoRoute,
		config:                    cfg.NodeConfig,
		resolveEndpointToNodename: cfg.ResolveEndpointToNodename,
		indexByTarget:             make(map[modelTarget]*CachedModel),
		indexByName:               make(map[string]*CachedModel),
		indexBySuffix:             make(map[string]*CachedModel),
	}
}

// Start pre-populates the cache before the server accepts requests.
// Every node warms its own local catalog — workers included, because
// every node is a producer (see pkg/cluster/role docs). The only
// role-dependent choice is whether to also kick async cluster-wide
// aggregation: coord-with-peers does, everyone else does not.
//
//   - Coordinator with worker endpoints: sync local warm, async aggregate
//     from peers. Snappy boot; cluster-wide catalog fills in seconds later.
//   - Everyone else (worker, standalone coord, unclaimed): sync full
//     refresh. RefreshCacheSync falls through to local-only when the
//     cluster client has no peers to broadcast to.
//
// Start is fire-and-forget (degraded-ok): a failed warm logs and
// continues; the next read retries via ensurePopulated. Safe to call
// after a prior Stop.
func (mc *Cache) Start(ctx context.Context) {
	mc.lifecycleMu.Lock()
	if mc.lifeCancel != nil {
		mc.lifecycleMu.Unlock()
		return // already started
	}
	mc.lifeCtx, mc.lifeCancel = context.WithCancel(ctx)
	lifeCtx := mc.lifeCtx
	mc.lifecycleMu.Unlock()

	if mc.config.Cluster.IsMaster() && len(mc.config.Cluster.Endpoints) > 0 {
		mc.warmLocalCache()
		mc.RefreshAsync()
		return
	}
	if err := mc.RefreshCacheSync(lifeCtx); err != nil {
		slog.Warn("Cache warm on startup failed (will retry on first request)", "error", err)
	}
}

// Stop cancels the lifecycle context (aborting any in-flight worker
// fetch) and waits for the background async refresh goroutine to exit,
// bounded by ctx. Safe to call when Start was never invoked. Safe to
// call multiple times.
//
// On ctx expiry before the refresh goroutine exits, Stop logs a
// structured warning and returns — the goroutine finishes its own
// teardown independently (defer on its CAS gate). The caller already
// owns ctx and knows it expired; a propagated error would be
// redundant with the log.
//
// Stop does NOT clear the cached models or indexes. Reads continue to
// return the pre-Stop state between Stop and the next Start — this is
// deliberate EventCache semantics (freshness is driven by mutation
// events, never by lifecycle transitions). To force re-population,
// call Invalidate.
func (mc *Cache) Stop(ctx context.Context) {
	mc.lifecycleMu.Lock()
	cancel := mc.lifeCancel
	mc.lifeCtx = nil
	mc.lifeCancel = nil
	mc.lifecycleMu.Unlock()

	if cancel == nil {
		return
	}
	cancel()

	// Spin until the async goroutine clears the refreshing gate, or ctx expires.
	// Background refresh is bounded by ClusterQueryTimeout so this wait is
	// short in practice.
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for atomic.LoadInt32(&mc.refreshing) == 1 {
		select {
		case <-ctx.Done():
			slog.Warn("cache stop deadline exceeded",
				"subsystem", "model.cache",
				"reason", "background refresh goroutine did not exit in time",
				"err", ctx.Err())
			return
		case <-ticker.C:
		}
	}

}

// currentCtx returns the active lifecycle context, or Background if Start
// has not been called. Callers capture this for bounded I/O so Stop can
// cancel in-flight work.
func (mc *Cache) currentCtx() context.Context {
	mc.lifecycleMu.Lock()
	defer mc.lifecycleMu.Unlock()
	if mc.lifeCtx != nil {
		return mc.lifeCtx
	}
	return context.Background()
}

// SetAutoRoute injects the auto-route manager after construction.
// autoroute.Manager depends on cache accessors (GetAllModels) and
// would form a construction cycle if passed to New; this setter
// breaks the cycle.
func (mc *Cache) SetAutoRoute(ar AutoRouter) {
	mc.autoRoute = ar
}

// GetAllModels returns the current cached models, or nil if the cache
// is not populated (pre-first-refresh or post-Invalidate). Exposed so
// autoroute.New can wire its getAllModels callback.
func (mc *Cache) GetAllModels() []*CachedModel {
	return mc.getAllModelsFromCache()
}

// LookupByName returns a cached model by exact name (thread-safe).
func (mc *Cache) LookupByName(name string) (*CachedModel, bool) {
	mc.mu.RLock()
	defer mc.mu.RUnlock()
	m, ok := mc.indexByName[name]
	return m, ok
}

type modelTarget struct{ model, provider, node string }

// LookupTarget returns an exact replica by model name or source alias.
// Unlike name-only lookups, it never substitutes a different provider or node.
func (mc *Cache) LookupTarget(ctx context.Context, model, provider, node string) (*CachedModel, error) {
	if err := mc.ensurePopulated(ctx); err != nil {
		return nil, err
	}
	mc.mu.RLock()
	defer mc.mu.RUnlock()
	return mc.indexByTarget[modelTarget{strings.ToLower(model), provider, node}], nil
}

// HasModel returns true if the model exists in the cache index.
func (mc *Cache) HasModel(name string) bool {
	mc.mu.RLock()
	_, ok := mc.indexByName[name]
	mc.mu.RUnlock()
	return ok
}

// ---------------------------------------------------------------------------
// Cache lifecycle
// ---------------------------------------------------------------------------

func (mc *Cache) updateModelCache(models []*CachedModel) {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	mc.updateModelCacheLocked(models)
}

func (mc *Cache) updateModelCacheLocked(models []*CachedModel) {
	var cfg *pkgConfig.AppsConfig
	if mc.appsConfig != nil {
		cfg = mc.appsConfig()
	}
	models = append(models, expandVariants(models, cfg)...)
	mc.admissionModels = models
	mc.modelRefusals = buildModelRefusals(models, cfg)
	mc.models = models
	mc.indexByTarget = make(map[modelTarget]*CachedModel, len(models))
	mc.indexByNode = make(map[modelNode]*CachedModel, len(models))
	mc.indexByName = make(map[string]*CachedModel, len(models))
	mc.indexBySuffix = make(map[string]*CachedModel, len(models))
	for _, m := range models {
		if m.Incomplete {
			continue
		}
		for _, name := range []string{m.Name, m.SourceID} {
			if name == "" {
				continue
			}
			nodeKey := modelNode{strings.ToLower(name), strings.ToLower(m.Node)}
			if _, exists := mc.indexByNode[nodeKey]; !exists {
				mc.indexByNode[nodeKey] = m
			}
			key := modelTarget{strings.ToLower(name), m.Provider, m.Node}
			if _, exists := mc.indexByTarget[key]; !exists {
				mc.indexByTarget[key] = m
			}
		}
		mc.indexByName[m.Name] = m
		shortName := utils.FormatModelName(m.Name)
		if shortName != m.Name {
			if _, exists := mc.indexBySuffix[shortName]; !exists {
				mc.indexBySuffix[shortName] = m
			}
		}
		// SourceID alias (e.g. HF repo path) — first-write wins so a
		// repo with multiple GGUF variants resolves to whichever the
		// scanner saw first; clients that need a specific quant pass
		// the file-stem alias via indexByName.
		if m.SourceID != "" && m.SourceID != m.Name {
			if _, exists := mc.indexByName[m.SourceID]; !exists {
				mc.indexByName[m.SourceID] = m
			}
		}
	}
	mc.valid = true
	mc.lastUpdated = utils.Now()
	mc.lastAttempt, mc.populateErr = mc.lastUpdated, nil
	utils.LogDebugf("Unified model cache updated: %d models", len(models))
}

// Invalidate is THE single entry point for model-catalog invalidation.
// Call this from every mutation that can change what models exist, where,
// or how they are assigned to providers: pull completion, model delete,
// apps-config change, cluster-join notifications.
//
// Cascades:
//   - Registry scan cache (pkg/modelregistry): invalidated first so the
//     next RefreshCacheSync re-scans fresh.
//   - Cache itself: indexes cleared, valid flag flipped; last models retained as failure evidence.
//
// NodeResourceCache follows accepted catalog publications without separate invalidation.
//
// Registry.InvalidateScanCache is called outside mc.mu to preserve the
// lock order: RefreshCacheSync calls Registry.ListAllModels without
// holding mc.mu, so taking Registry's lock under mc.mu would invert.
func (mc *Cache) Invalidate() {
	if reg := mc.registry(); reg != nil {
		reg.InvalidateScanCache()
	}
	mc.mu.Lock()
	defer mc.mu.Unlock()
	mc.invalidateLocked()
}

// ReloadConfig implements configReloader. Apps-config mutations can shift
// auto-assignment outcomes, so we invalidate on any material change.
// Fingerprint check avoids the full catalog rebuild when the on-disk YAML
// churned without a semantic change. nil config is always treated as a
// material reset (matches ProviderAppManager's nil-reset contract).
func (mc *Cache) ReloadConfig(fresh *pkgConfig.AppsConfig) pkgConfig.ReloadDisposition {
	if fresh == nil {
		mc.Invalidate()
		return pkgConfig.DispositionApplied
	}
	fp := fresh.Fingerprint()
	mc.reloadMu.Lock()
	unchanged := mc.lastReloadFP == fp
	mc.lastReloadFP = fp
	mc.reloadMu.Unlock()
	if unchanged {
		return pkgConfig.DispositionIgnored
	}
	if mc.registry != nil {
		if reg := mc.registry(); reg != nil {
			reg.InvalidateScanCache()
		}
	}
	mc.mu.Lock()
	evidence := mc.admissionModels
	mc.invalidateLocked()
	mc.admissionModels = evidence
	mc.modelRefusals = buildModelRefusals(evidence, fresh)
	mc.mu.Unlock()
	return pkgConfig.DispositionApplied
}

func (mc *Cache) invalidateLocked() {
	mc.generation++
	mc.admissionUnavailableLogged = false
	mc.admissionModels = nil
	mc.localSnapshot = localModelSnapshot{}
	clear(mc.modelRefusals)
	// Retain the last snapshot only as evidence for failed nodes on the next publish.
	clear(mc.indexByTarget)
	clear(mc.indexByNode)
	clear(mc.inventoryUnavailable)
	clear(mc.indexByName)
	clear(mc.indexBySuffix)
	mc.valid = false
	mc.lastAttempt = time.Time{}
}

// ---------------------------------------------------------------------------
// Public query methods
// ---------------------------------------------------------------------------

// GetCacheStats reports basic metrics about the unified model cache.
// lastUpdated is informational — validity is driven by the valid flag.
func (mc *Cache) GetCacheStats() map[string]any {
	mc.mu.RLock()
	defer mc.mu.RUnlock()

	var ageSeconds float64
	if !mc.lastUpdated.IsZero() {
		ageSeconds = time.Since(mc.lastUpdated).Seconds()
	}
	size := len(mc.models)
	if !mc.valid {
		size = 0
	}
	return map[string]any{
		"size":            size,
		"indexed_by_name": len(mc.indexByName),
		"suffix_entries":  len(mc.indexBySuffix),
		"last_updated":    mc.lastUpdated,
		"age_seconds":     ageSeconds,
		"valid":           mc.valid,
	}
}

// LookupModel returns the cached model matching name (exact, then suffix).
// Consults the unified index; if invalid (first read or post-invalidate),
// blocks on a singleflight-guarded RefreshCacheSync. Returns
// ErrModelNotFound{ModelName: name} if the model is absent after populate
// — callers use errors.As to distinguish "not here" from "lookup failed."
//
// Role-agnostic: every node runs its own Cache. On workers the index
// is local-only (cluster client is nil so RefreshCacheSync walks just
// the local registry); on coords the index aggregates local + peers.
// Suffix matching is a no-op on worker-flat names ("llama2") and kicks
// in on coord-prefixed cluster-wide entries ("gpu-1/ollama/llama2").
//
// This is the blocking read API — use it when you need the answer to
// proceed (inference routing, deploy gating). For hot-path non-blocking
// probes that tolerate "not here right now" (e.g. fast-path routing
// before falling back to broadcast), use LookupByName / HasModel.
func (mc *Cache) LookupModel(ctx context.Context, name string) (*CachedModel, error) {
	if err := mc.ensurePopulated(ctx); err != nil {
		return nil, err
	}

	mc.mu.RLock()
	defer mc.mu.RUnlock()
	if m, ok := mc.indexByName[name]; ok {
		return m, nil
	}
	if m, ok := mc.indexBySuffix[name]; ok {
		return m, nil
	}
	return nil, ErrModelNotFound{ModelName: name}
}

// HasModelExact reports whether `name` matches a cached model by exact
// canonical name only — no suffix-index fallback. Used by gates that
// must distinguish "this exact repo+variant is in the pool" from "some
// model with a similar suffix is in the pool". The auto_deploy gate is
// the canonical caller: a suffix-index hit on an unrelated model would
// skip the deploy and launch against a missing file. Returns false on
// any populate error — gates should fail-open toward "deploy" rather
// than refuse to deploy because the cache had a transient issue.
func (mc *Cache) HasModelExact(ctx context.Context, name string) bool {
	if err := mc.ensurePopulated(ctx); err != nil {
		return false
	}
	mc.mu.RLock()
	defer mc.mu.RUnlock()
	_, ok := mc.indexByName[name]
	return ok
}

// inventoryRetryInterval prevents failed discovery from being retried on every request.
const inventoryRetryInterval = 30 * time.Second

func (mc *Cache) populationCurrent() (bool, error) {
	mc.mu.RLock()
	defer mc.mu.RUnlock()
	if mc.valid && len(mc.inventoryUnavailable) == 0 {
		return true, nil
	}
	return time.Since(mc.lastAttempt) < inventoryRetryInterval, mc.populateErr
}

// errCatalogChanged means an invalidation landed while a refresh was
// gathering, so the refresh's result was not published.
var errCatalogChanged = errors.New("model catalog changed during refresh")

// ensurePopulated serializes initial population and retries of degraded catalogs.
func (mc *Cache) ensurePopulated(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if current, err := mc.populationCurrent(); current {
		return err
	}
	mc.refreshMu.Lock()
	defer mc.refreshMu.Unlock()
	if current, err := mc.populationCurrent(); current {
		return err
	}
	attempted := utils.Now()
	err := mc.RefreshCacheSync(ctx)
	if err != nil {
		err = fmt.Errorf("cache miss and refresh failed: %w", err)
		mc.mu.Lock()
		if mc.lastAttempt.Before(attempted) {
			mc.lastAttempt, mc.populateErr = attempted, err
		}
		mc.mu.Unlock()
	}
	return err
}

func (mc *Cache) getAllModelsFromCache() []*CachedModel {
	mc.mu.RLock()
	defer mc.mu.RUnlock()
	if !mc.valid {
		return nil
	}
	return mc.models
}

// ListModels returns all cached models matching the given filters (empty
// string on a filter means "don't filter on this dimension"). Blocks on
// a singleflight-guarded RefreshCacheSync when the cache is invalid;
// subsequent reads are lock-free until the next invalidation event.
//
// This is the blocking listing API. For single-model lookups use
// LookupModel; for non-blocking index probes use LookupByName / HasModel.
// Role-agnostic: see LookupModel for the unified-index rationale.
func (mc *Cache) ListModels(ctx context.Context, host, repo, app, model string) ([]*CachedModel, error) {
	if err := mc.ensurePopulated(ctx); err != nil {
		return nil, err
	}

	mc.mu.RLock()
	allModels, valid := mc.models, mc.valid
	mc.mu.RUnlock()
	if !valid {
		return nil, fmt.Errorf("no models available (cache empty, refresh in progress or failed)")
	}

	filtered := filterCachedModels(allModels, host, repo, app, model)
	utils.LogDebugf("[Cache] Returning %d filtered models (from %d total)", len(filtered), len(allModels))
	return filtered, nil
}

// LocalModelSnapshot preserves the worker wire body from the same publication as admission.
func (mc *Cache) LocalModelSnapshot(ctx context.Context) ([]map[string]any, []InventoryUnavailable, error) {
	if err := mc.ensurePopulated(ctx); err != nil {
		return nil, nil, err
	}
	mc.mu.RLock()
	defer mc.mu.RUnlock()
	if !mc.valid {
		return nil, nil, errCatalogChanged
	}
	return mc.localSnapshot.models, mc.localSnapshot.unavailable, nil
}

// ---------------------------------------------------------------------------
// Refresh / population
// ---------------------------------------------------------------------------

// RefreshAsync triggers a non-blocking background refresh. Concurrent
// triggers collapse to one goroutine (atomic CAS gate). Callers that
// want to block on completion should use RefreshCacheSync directly.
//
// Exposed so server-side callbacks (cluster reconnect, download
// completion) can drive eager refreshes without waiting.
func (mc *Cache) RefreshAsync() {
	if atomic.CompareAndSwapInt32(&mc.refreshing, 0, 1) {
		ctx := mc.currentCtx()
		go func() {
			defer atomic.StoreInt32(&mc.refreshing, 0)
			defer utils.RecoverAndLog("cache.RefreshAsync")
			if err := mc.RefreshCacheSync(ctx); err != nil {
				slog.Warn("Background cache refresh failed", "error", err)
			}
		}()
	}
}

// RefreshCacheSync publishes the catalog. A refresh an invalidation overtakes is stale, not wrong, so it
// refreshes again from the newer state until one publishes, a build fails, or ctx ends.
// ctx bounds the worker broadcast: the hot read path passes the request ctx, background callers
// (RefreshAsync, Start) the lifecycle ctx so Stop cancels them.
func (mc *Cache) RefreshCacheSync(ctx context.Context) error {
	return mc.refresh(ctx, "")
}

// RefreshNode publishes only the notifying worker without waiting for other nodes.
func (mc *Cache) RefreshNode(ctx context.Context, node string) error {
	mc.mu.Lock()
	mc.generation++
	mc.mu.Unlock()
	return mc.refresh(ctx, node)
}

func (mc *Cache) refresh(ctx context.Context, node string) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if published, err := mc.refreshOnce(ctx, node); err != nil || published {
			return err
		}
	}
}

func (mc *Cache) refreshOnce(ctx context.Context, node string) (bool, error) {
	mc.mu.Lock()
	if node != "" && !mc.valid {
		mc.invalidateLocked()
		mc.mu.Unlock()
		return true, nil
	}
	generation := mc.generation
	mc.mu.Unlock()
	var localItems []map[string]any
	var local localModelSnapshot
	if node == "" {
		var err error
		localItems, local, err = mc.buildLocalAndCloudItems(ctx)
		if err != nil {
			return false, err
		}
	}
	workerItems, workerResources, workerUnavailable := mc.fetchWorkerData(ctx, node)
	allItems := make([]map[string]any, 0, len(localItems)+len(workerItems))
	allItems = append(allItems, localItems...)
	allItems = append(allItems, workerItems...)
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if !mc.publishModelItems(allItems, generation, append(local.unavailable, workerUnavailable...), local, node, workerResources) {
		return false, nil
	}
	if mc.autoRoute != nil {
		mc.autoRoute.SyncAllFromCache()
	}
	for _, failure := range workerUnavailable {
		if node != "" && failure.Provider == "" {
			return true, fmt.Errorf("%s: %s", node, failure.Reason)
		}
	}
	return true, nil
}

func (mc *Cache) warmLocalCache() {
	mc.mu.RLock()
	generation := mc.generation
	mc.mu.RUnlock()
	localItems, local, err := mc.buildLocalAndCloudItems(mc.currentCtx())
	if err != nil {
		slog.Warn("Cache warm failed", "error", err)
		return
	}
	if !mc.publishModelItems(localItems, generation, local.unavailable, local, "", nil) {
		return
	}
	slog.Info("Local model cache warmed (worker models loading async)", "local_models", len(localItems))
}

func (mc *Cache) buildLocalAndCloudItems(ctx context.Context) ([]map[string]any, localModelSnapshot, error) {
	reg := mc.registry()
	var localModels []*metadata.ModelMetadata
	var failures map[string]string
	if reg != nil {
		var err error
		localModels, failures, err = reg.ModelSnapshot(ctx)
		if err != nil {
			return nil, localModelSnapshot{}, fmt.Errorf("failed to list local models: %w", err)
		}
	}

	hostname := mc.node.Nodename()
	if hostname == "" {
		return nil, localModelSnapshot{}, fmt.Errorf("hostname not configured - cannot populate cache (set Node.Name in config)")
	}

	items := make([]map[string]any, 0, len(localModels)+8)
	for _, m := range localModels {
		items = append(items, m.ToOllamaFormat(hostname))
	}
	cfg := mc.appsConfig()
	items = EnrichLocalItems(items, cfg, mc.modelDetails)
	local := localModelSnapshot{models: items, unavailable: mc.localUnavailable(ctx, failures)}
	items = append(items, EnrichLocalItems(mc.getCloudAppModels(), cfg, mc.modelDetails)...)
	return items, local, nil
}

func (mc *Cache) getCloudAppModels() []map[string]any {
	cfg := mc.appsConfig()
	if cfg == nil {
		return nil
	}

	hostname := mc.node.Nodename()
	var items []map[string]any

	cfg.RangeApps(func(appKey string, appCfg pkgConfig.ServiceConfig) bool {
		if !appCfg.IsCloudProvider() || !appCfg.IsEnabled() {
			return true
		}
		if appCfg.Capabilities == nil || len(appCfg.Capabilities.Models) == 0 {
			return true
		}
		for _, modelID := range appCfg.Capabilities.Models {
			items = append(items, map[string]any{
				"name":         modelID,
				"node":         hostname,
				"is_cloud":     true,
				"assigned_app": appKey,
				"source_repo":  appKey,
				"details":      map[string]any{"format": "cloud"},
			})
		}
		return true
	})
	return items
}

func (mc *Cache) fetchWorkerData(parent context.Context, node string) ([]map[string]any, map[string]*mesh.ResourceMetrics, []InventoryUnavailable) {
	workerResources := make(map[string]*mesh.ResourceMetrics)
	cc := mc.clusterClient()
	if cc == nil {
		return nil, workerResources, nil
	}

	ctx, cancel := context.WithTimeout(parent, constants.ClusterQueryTimeout)
	defer cancel()

	var responses []*mesh.NodeResponse
	if node != "" {
		resp, err := cc.Unicast(ctx, node, "/zzrouter/v1/internal/models", &mesh.QueryParams{Method: "GET"})
		responses = []*mesh.NodeResponse{{NodeName: node, Response: resp, Error: err}}
	} else {
		broadcastResp, err := cc.Broadcast(ctx, "/zzrouter/v1/internal/models", &mesh.QueryParams{Method: "GET"})
		if err != nil {
			slog.Warn("[Cache] Failed to fetch worker data", "error", err)
		}
		if broadcastResp == nil {
			// A top-level broadcast failure identifies no node; unknown names remain fail-open.
			return nil, workerResources, []InventoryUnavailable{{Reason: "cluster model inventory could not be read"}}
		}
		responses = broadcastResp.Responses
	}

	var allModels []map[string]any
	var unavailable []InventoryUnavailable

	for _, nodeResp := range responses {
		nodeName := nodeResp.NodeName
		if nodeName == "" {
			nodeName = nodeResp.Node
		}
		if node == "" && mc.resolveEndpointToNodename != nil {
			if resolved := mc.resolveEndpointToNodename(nodeName); resolved != "" {
				nodeName = resolved
			}
		}
		if nodeResp.Error != nil || nodeResp.Response == nil || nodeResp.Response.StatusCode != 200 {
			unavailable = append(unavailable, InventoryUnavailable{Node: nodeName, Reason: "node model inventory could not be read"})
			continue
		}
		var body struct {
			Unavailable []InventoryUnavailable `json:"inventory_unavailable,omitempty"`
			Models      []map[string]any       `json:"models"`
			Resources   *mesh.ResourceMetrics  `json:"resources,omitempty"`
		}
		if err := json.Unmarshal(nodeResp.Response.Body, &body); err != nil {
			unavailable = append(unavailable, InventoryUnavailable{Node: nodeName, Reason: "node returned an invalid model inventory"})
			continue
		}
		for _, failure := range body.Unavailable {
			failure.Node = nodeName
			unavailable = append(unavailable, failure)
		}
		for _, item := range body.Models {
			if node != "" {
				if host := extractStringField(item, "node"); host != "" && host != constants.Localhost && !strings.EqualFold(host, node) {
					item["ip_address"] = host
				}
				item["node"] = node
			} else if host := extractStringField(item, "node"); host != "" && host != constants.Localhost && mc.resolveEndpointToNodename != nil {
				if hostname := mc.resolveEndpointToNodename(host); hostname != "" {
					item["node"] = hostname
					item["ip_address"] = host
				}
			}
		}
		allModels = append(allModels, body.Models...)
		if body.Resources != nil {
			if node != "" {
				workerResources[node] = body.Resources
				continue
			}
			nodeName := body.Resources.NodeName
			if nodeName == "" {
				nodeName = nodeResp.NodeName
			}
			if nodeName == "" {
				nodeName = nodeResp.Node
			}
			if mc.resolveEndpointToNodename != nil {
				if resolved := mc.resolveEndpointToNodename(nodeName); resolved != "" {
					nodeName = resolved
				}
			}
			if nodeName != "" {
				workerResources[nodeName] = body.Resources
			}
		}
	}
	return allModels, workerResources, unavailable
}

// populateModelCacheFromItems converts raw map items to CachedModel structs.
func (mc *Cache) modelItems(items []map[string]any) []*CachedModel {
	cachedModels := make([]*CachedModel, 0, len(items))
	for _, item := range items {
		name := extractStringField(item, "name")
		cm := &CachedModel{
			VariantOf:  extractStringField(item, "variant_of"),
			Name:       name,
			Model:      name,
			SourceID:   extractStringField(item, "source_id"),
			SourceRepo: extractStringField(item, "source_repo"),
			Node:       extractStringField(item, "node"),
			Provider:   extractStringField(item, "assigned_app"),
			Size:       extractInt64Field(item, "size"),
			Digest:     extractStringField(item, "digest"),
		}
		cm.Incomplete, _ = item["incomplete"].(bool)
		if isCloud, ok := item["is_cloud"].(bool); ok {
			cm.IsCloud = isCloud
		}
		if details, ok := item["details"].(map[string]any); ok {
			cm.Format = extractStringField(details, "format")
			cm.Details = maps.Clone(details)
			// A worker on an older release reports declared endpoints; derive the shims once, here.
			if eps, known := cm.WireEndpoints(); known {
				cm.Details["wire_endpoints"] = pkgConfig.EffectiveWireEndpoints(eps)
			}
		}
		if ipAddr, ok := item["ip_address"].(string); ok && ipAddr != "" {
			cm.Extra = make(map[string]any)
			cm.Extra["ip_address"] = ipAddr
		}
		if modifiedAt := extractStringField(item, "modified_at"); modifiedAt != "" {
			if t, err := time.Parse(time.RFC3339, modifiedAt); err == nil {
				cm.Modified = t
			}
		}
		cachedModels = append(cachedModels, cm)
	}
	return cachedModels
}

func (mc *Cache) populateModelCacheFromItems(items []map[string]any) {
	mc.updateModelCache(mc.modelItems(items))
}

func (mc *Cache) publishModelItems(items []map[string]any, generation uint64, unavailable []InventoryUnavailable, local localModelSnapshot, node string, resources map[string]*mesh.ResourceMetrics) bool {
	models := mc.modelItems(items)
	mc.mu.Lock()
	defer mc.mu.Unlock()
	if generation != mc.generation {
		return false
	}
	if node != "" {
		for _, failure := range mc.inventoryUnavailable {
			if !strings.EqualFold(failure.Node, node) {
				unavailable = append(unavailable, failure)
			}
		}
	}
	known := make(map[modelTarget]bool, len(models))
	for _, current := range models {
		known[modelTarget{strings.ToLower(current.Name), current.Provider, current.Node}] = true
	}
	var retained []*CachedModel
	for _, previous := range mc.models {
		if known[modelTarget{strings.ToLower(previous.Name), previous.Provider, previous.Node}] {
			continue
		}
		if previous.VariantOf != "" {
			continue
		}
		if node != "" && !strings.EqualFold(previous.Node, node) {
			retained = append(retained, previous)
			continue
		}
		for _, failure := range unavailable {
			if failure.Node != "" && strings.EqualFold(previous.Node, failure.Node) && (failure.Provider == "" || strings.EqualFold(previous.Provider, failure.Provider)) {
				models = append(models, previous)
				break
			}
		}
	}
	lastAttempt, populateErr := mc.lastAttempt, mc.populateErr
	mc.updateModelCacheLocked(append(retained, models...))
	if node == "" {
		mc.localSnapshot = local
	} else {
		mc.lastAttempt, mc.populateErr = lastAttempt, populateErr
	}
	if mc.nodeCache != nil {
		if node == "" && resources != nil {
			mc.nodeCache.Update(mc.resources, mc.node.Nodename(), resources)
		} else if node != "" {
			mc.nodeCache.Put(node, resources[node])
		}
	}
	mc.inventoryUnavailable = make(map[providerNode]InventoryUnavailable, len(unavailable))
	for _, failure := range unavailable {
		mc.inventoryUnavailable[providerNode{failure.Node, failure.Provider}] = cloneInventory(failure)
		if node != "" && strings.EqualFold(failure.Node, node) {
			mc.lastAttempt = mc.lastUpdated
		}
	}
	return true
}
