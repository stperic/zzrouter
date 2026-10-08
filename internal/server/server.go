// package server provides HTTP handlers for the zzrouter host server.
// Server struct definition and interface delegates.

package server

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/teams"
	"github.com/stperic/zzrouter/pkg/audit"
	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	clusternode "github.com/stperic/zzrouter/pkg/cluster/node"
	"github.com/stperic/zzrouter/pkg/cluster/role"
	clustersignal "github.com/stperic/zzrouter/pkg/cluster/signal"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/connectivity"
	"github.com/stperic/zzrouter/pkg/discovery/network"
	"github.com/stperic/zzrouter/pkg/dispatch/chain"
	"github.com/stperic/zzrouter/pkg/fallback"
	"github.com/stperic/zzrouter/pkg/inferencelog"
	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/model"
	"github.com/stperic/zzrouter/pkg/model/cache"
	"github.com/stperic/zzrouter/pkg/observability"
	route_events "github.com/stperic/zzrouter/pkg/observability/route_events"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/routing"
	"github.com/stperic/zzrouter/pkg/update"
)

// Server represents the zzrouter host server
type Server struct {
	// === Identity ===

	node    *NodeIdentity     // Immutable node identity (name, bind, port, endpoints, role)
	backend *backend.Resolver // Provider key → upstream endpoint resolution
	proxy   *ProxyClient      // HTTP forwarding to upstream backends

	// === Domain groups ===

	providers struct {
		appMgr         *prov_apps.ProviderAppManager // Unified provider lifecycle manager
		cooldowns      *fallback.CooldownSet         // Deployment + provider cooldown pair
		rateTracker    *fallback.RateLimitTracker    // Last-seen rate limit headers per provider
		fallback       *chain.Proxy                  // Deployment-chain proxy with pre-stream fallback
		loadTracker    *fallback.LoadTracker         // In-flight per replica (read by /state)
		latencyTracker *fallback.LatencyTracker      // EWMA latency per replica (read by /state)
		healthChecker  *fallback.HealthChecker       // Background health probes (read by /state)
		routeEvents    *route_events.Bus             // Lifecycle event fan-out → /model-groups/events SSE
		// groupReaperDone closes when the reaper goroutine exits.
		// groupReaperCancel terminates the reaper independent of shutdownCtx
		// so role-demote (coord → worker) stops the reaper without waiting
		// for full server shutdown. A re-promote mints a fresh pair.
		groupReaperDone   <-chan struct{}
		groupReaperCancel context.CancelFunc
		// syncFanOut pushes the provider tree to every worker. Held here
		// because it fires on three triggers, not one: a config mutation,
		// a worker coming up, and the maintenance tick.
		syncFanOut *providerSyncFanOut
	}

	cluster struct {
		coordinator *mesh.Cluster                // Cluster coordinator
		router      routing.Router               // Request router handed to services; always the Swappable below
		routerSwap  *routing.Swappable           // Swap handle — flips LocalOnly ↔ ClusterAware on mode transitions
		node        *clusterResourceGroup        // Owns Start/Stop lifecycle for resource tracker + mDNS
		listener    *clusternode.Node            // mTLS cluster HTTP server — constructed here, Start/Stop wired in mTLS PR 2 step 4
		resources   *mesh.ResourceTracker        // Access handle for wiring; same object Node starts/stops
		resRouter   *routing.ResourceAwareRouter // Resource-aware model routing
		discovery   *network.NodeDiscovery       // Access handle for wiring; same object Node starts/stops
		signaler    *clustersignal.Signaler      // Worker→coord notify + goodbye
	}

	inference struct {
		logBridge   *InferenceLogBridge  // Inference log bridge (for latency tracker wiring)
		logStore    *inferencelog.Store  // In-memory ring buffer for request logging
		normalizers *ResponseNormalizers // Per-provider response shape normalizers
		affinity    *responseAffinity    // /v1/responses session affinity
		logJobID    string               // Firehose jobs.Handle ID mirroring /inference-logs
	}

	// === Cluster role ===

	role *role.Manager // Single source of truth for cluster mode; gates coordinator subsystems + per-request route access

	// === Access control ===

	access         *AccessControl       // Unified auth + quota enforcement
	spendEvents    *SpendEventBus       // Quota breach event bus → /spend/events SSE
	keyStore       *keys.FileKeyStore   // Virtual key store (accessed by keys_service CRUD)
	teamStore      *teams.FileTeamStore // Team store (accessed by teams_service CRUD)
	auditSink      audit.Sink           // Append-only audit log for key/team/quota mutations; audit.Null until wired
	auditConfigDir string               // configDir captured for lazy audit-sink open on coordinator promote

	// === Infrastructure ===

	nodeConfigStore     *pkgConfig.NodeConfigStore // Single owner of node.yaml I/O
	config              *pkgConfig.NodeConfig      // Cached pointer (same object as nodeConfigStore.Config())
	configStore         *pkgConfig.AppsConfigStore // Single owner of provider config I/O
	appsConfig          *pkgConfig.AppsConfig      // Cached pointer (same object as configStore.Config())
	engine              *gin.Engine
	httpServer          *http.Server
	httpClient          *http.Client                    // Configured HTTP client for outbound requests
	httpStreamingClient *http.Client                    // HTTP client for streaming operations (no timeout)
	clusterMTLSMu       sync.Mutex                      // Guards clusterMTLSClient lazy init
	clusterMTLSClient   *http.Client                    // mTLS client for coord→worker proxying (OU=worker)
	connManager         *connectivity.ConnectionManager // Centralized connection manager (for metrics)
	onFinalize          atomic.Pointer[finalizeHook]    // Fired after FinalizeOn/Offboarding success
	lastMutation        atomic.Int64                    // Unix nanos of most recent FinalizeOn/Offboarding; served in /health as last_config_mutation_at
	startTime           time.Time                       // Track server start time for uptime
	otelProvider        *observability.Provider         // OpenTelemetry observability provider
	model               *model.Subsystem                // Model-domain orchestrator (pricing today; cache/fallback/resolver next steps)
	jobs                *jobs.Registry                  // Per-node streaming job registry — downloads, installs, updates, inference-log
	updateScheduler     *update.Scheduler               // Auto-update scheduler
	updateRollouts      *update.Rollouts
	updateSchedulingMu  sync.Mutex
	maintenance         *maintenanceLoop // Periodic cleanup tasks
	shutdownCtx         context.Context
	shutdownCancel      context.CancelFunc
	// listenerReady closes after net.Listen succeeds but BEFORE ServeTLS
	// completes its first handshake. Safe to wait on for worker→coord
	// outbound signaling; NOT safe to wait on for any path that expects
	// the coord can immediately dial this worker's inbound TLS listener.
	listenerReady  chan struct{}
	draining       atomic.Bool    // Readiness probes return "not_ready" when true
	refreshWG      sync.WaitGroup // Drained in Stop
	refreshMu      sync.Mutex     // Guards refreshRunning/refreshQueued
	refreshRunning bool           // Coalescer: a refresh goroutine is live
	refreshQueued  bool           // Coalescer: another refresh was requested while one was running

	// Grouped service/controller/responder components
	services            Services
	controllers         Controllers
	responders          *responderSet
	auth                *AuthHandlers
	logsHandlers        *LogsHandlers
	ollamaHandlers      *OllamaHandlers
	openaiModelHandlers *OpenAIModelHandlers
	clusterHandlers     *ClusterHandlers

	// MCP stdio bridge serialization
	mcpGateway *MCPGateway

	// fsnotify-driven cache invalidator. Closes the disk-state-vs-cache
	// drift class of bug; nil when the models root doesn't exist yet
	// (pre-deploy state) or fsnotify init failed.
	modelsWatcher *modelsWatcher
}

// GetInstanceByModel returns instance info for a loaded model.
func (s *Server) GetInstanceByModel(modelName string) (*instance.Instance, bool) {
	if s.providers.appMgr == nil {
		return nil, false
	}
	return s.providers.appMgr.GetInstanceByModel(modelName)
}

// openAIModelRuntimeLookup answers /v1/models' "is this model hot?" question
// by querying the local instance registry. Returns nil when the model isn't
// running on this node — handlers downgrade to status="cold" in that case.
// Local-only by design: cluster-wide rollup would require fan-out which
// /v1/models can't afford on every call.
func (s *Server) openAIModelRuntimeLookup(model, endpoint string) *OpenAIModelRuntime {
	if s.providers.appMgr == nil {
		return nil
	}
	inst, ok := s.providers.appMgr.Instances().GetByModelEndpoint(model, endpoint)
	if !ok {
		return nil
	}
	var status string
	switch inst.GetStatus() {
	case instance.StatusRunning:
		status = "running"
	case instance.StatusStarting:
		status = "loading"
	default:
		return nil
	}
	return &OpenAIModelRuntime{
		Status: status,
		Node:   s.GetNodename(),
		Port:   inst.Port,
	}
}

// IsWorker reports whether the node is currently acting as a cluster
// worker. Reads through role.Manager so interface consumers (NodeNamer,
// mocks) observe runtime transitions (promote/demote); use
// s.node.IsWorker() only for config-source-of-truth identity checks
// that must not flip with role.
func (s *Server) IsWorker() bool {
	return s.role.Current().IsWorker()
}

// Invalidate is the single model-catalog invalidation entry point
// (satisfies ModelCacheProvider). Cascades to Registry scan cache +
// ModelCache; NodeResourceCache rides the next RefreshCacheSync.
func (s *Server) Invalidate() {
	s.model.Cache.Invalidate()
}

// ListModels returns cached models with filtering (satisfies ModelCacheProvider).
func (s *Server) ListModels(ctx context.Context, host, repo, app, model string) ([]*cache.CachedModel, error) {
	return s.model.Cache.ListModels(ctx, host, repo, app, model)
}

// LookupModel returns the cached model matching name or cache.ErrModelNotFound
// (satisfies ModelCacheProvider).
func (s *Server) LookupModel(ctx context.Context, name string) (*cache.CachedModel, error) {
	return s.model.Cache.LookupModel(ctx, name)
}

// GetCacheStats reports basic metrics about the unified model cache.
func (s *Server) GetCacheStats() map[string]any {
	return s.model.Cache.GetCacheStats()
}

// ServeHTTP makes Server an http.Handler. The implementation delegates to
// the wrapped gin engine; it exists so external packages (notably the
// integration test package) can drive the server via httptest without
// reaching into the unexported engine field.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.engine.ServeHTTP(w, r)
}

// IsAppEnabled checks if a provider is enabled in the configuration
func (s *Server) IsAppEnabled(providerType string) bool {
	if s.appsConfig == nil {
		return true
	}
	if svc, exists := s.appsConfig.LookupApp(providerType); exists {
		return svc.IsEnabled()
	}
	return true
}

// ============================================================================
// Interface Delegates — thin wrappers satisfying external contracts
// ============================================================================

// IsLocalNode delegates to NodeIdentity (satisfies ModelCacheProvider).
func (s *Server) IsLocalNode(host string) bool {
	return s.node.IsLocalNode(host)
}

// GetNodename returns the server's hostname (satisfies NodeInfoProvider).
func (s *Server) GetNodename() string {
	return s.node.Nodename()
}

// GetConnectionPoolStats returns HTTP connection pool utilization metrics.
func (s *Server) GetConnectionPoolStats() *connectivity.ConnectionPoolStats {
	if s.connManager == nil {
		return nil
	}
	stats := s.connManager.GetStats()
	return &stats
}

// Lifecycle methods: see server_lifecycle.go (Start, Stop, maintenance)
// Executor wiring: see server_wiring.go (new*Executor, buildHealthTargets)
