// package server provides HTTP handlers for the zzrouter host server.
// Routes - Main HTTP route orchestration
//
// ============================================================================
// ROUTE FILE ORGANIZATION
// ============================================================================
//
// routes.go (this file)
//   - Main entry point for all route registration
//   - Health/liveness endpoints (infrastructure)
//   - Calls domain-specific route files
//
// routes_public.go
//   - Client-facing API (/zzrouter/*)
//   - Orchestration, routing, caching
//   - Auth: ADMIN KEY
//
// routes_internal.go
//   - Cluster-internal API (/zzrouter/internal/*)
//   - Direct provider execution
//   - Auth: CLUSTER KEY
//
// routes_compat.go
//   - Compatibility layer for existing tools
//   - OpenAI API (/v1/*)
//   - Ollama API (/api/*)
//   - Auth: NONE (for compatibility)
//
// ============================================================================

package server

import (
	"context"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	"github.com/stperic/zzrouter/pkg/utils"
)

// proxyMetrics caps the per-request budget for cluster-wide /metrics scrapes.
// Prometheus' default scrape_timeout is 10s; we leave headroom for the
// inbound→coord→worker hop. maxMetricsBytes bounds memory exposure to a
// misbehaving worker emitting an unbounded body — 16 MiB is ~50× a typical
// node_exporter dump.
const (
	metricsProxyTimeout = 15 * time.Second
	maxMetricsBytes     = 16 << 20
)

// setupRoutes configures all HTTP routes for the server
func (s *Server) setupRoutes() {
	utils.LogDebugf("[Routes] Setting up HTTP routes")
	s.engine.Use(modelAdmissionMiddleware())

	// ========================================================================
	// HEALTH ENDPOINTS (Infrastructure)
	// ========================================================================
	// No auth required - used by load balancers, orchestrators, monitoring
	s.registerHealthRoutes()

	// ========================================================================
	// OPENAPI SPEC (Discovery)
	// ========================================================================
	// No auth — exposed so AI agents can fetch the schema before authenticating.
	s.registerOpenAPIRoutes()

	// ========================================================================
	// ZZROUTER API ROUTES
	// ========================================================================
	if s.cluster.router == nil {
		panic("setupRoutes called before routing layer was initialized")
	}

	// Logs handlers — needed by both coordinator (public routes) and worker (internal RunsExecutor)
	s.logsHandlers = &LogsHandlers{
		node:                s.node,
		appMgr:              s.providers.appMgr,
		httpClient:          s.httpClient,
		httpStreamingClient: s.httpStreamingClient,
		getClusterClient:    s.getClusterClient,
		routeToClusterNode:  s.routeToClusterNode,
		getClusterNodeURL:   s.getClusterNodeURL,
		isLocalNode:         s.node.IsLocalNode,
		handleClusterAction: s.HandleClusterAction,
	}

	// Route groups by cluster mode:
	//
	// All modes:        /health/*, /metrics, /zzrouter/v1/internal/*,
	//                   /zzrouter/v1/cluster/{join,leave}
	// Coordinator only: /zzrouter/v1/* (public + access control),
	//                   /v1/* (OpenAI compat), /api/* (Ollama compat)
	isWorker := s.node.IsWorker()

	// CLUSTER MEMBERSHIP
	// Built before registerPublicRoutes because routes_public.go captures
	// s.clusterHandlers as the receiver for /cluster/endpoints routes; a nil
	// receiver there panics on the first request.
	s.clusterHandlers = &ClusterHandlers{
		node:            s.node,
		config:          s.config,
		nodeConfigStore: s.nodeConfigStore,
		coordinator:     s.cluster.coordinator,
		listener:        s.cluster.listener,
		httpClient:      s.httpClient,
		clusterScheme:   func() string { return mesh.Scheme(s.config.Node.IsTLSEnabled()) },
		invalidateCache: s.model.Cache.Invalidate,
		advertiseURL:    s.config.Cluster.AdvertiseURL,
	}

	if !isWorker {
		// Compat surface (OpenAI /v1/*, Ollama /api/*, NativeWire mounts)
		// is mounted on the admin port for COORDINATORS only — that's the
		// surface external clients hit. Workers expose compat exclusively
		// on their cluster mTLS port (installed via SetWorkerCompatHandler
		// in server_factory); never on the admin port. External clients
		// dialing worker:9090/v1/* get a clean route-not-found 404 from
		// gin's normal route table — no middleware gate needed.
		s.registerCompatibilityRoutes()
	}

	if !isWorker {
		// PUBLIC API — general management (models, runs, providers, pricing, …).
		// Security: admin key (ZZROUTER_ADMIN_API_KEY).
		publicAPI := s.engine.Group("/zzrouter/v1", RequestIDMiddleware(), APIVersionMiddleware(), s.auth.adminAuthMiddleware())
		s.registerPublicRoutes(publicAPI)

		// ACCESS CONTROL API — teams, virtual keys, spend reports.
		// Separate group so operators can later swap in a dedicated
		// ZZROUTER_ACCESS_CONTROL_API_KEY via accessControlAuthMiddleware
		// without touching any URL, handler, or client. Paths still live
		// under /zzrouter/v1 — the split is by middleware, not by URL.
		accessAPI := s.engine.Group("/zzrouter/v1", RequestIDMiddleware(), APIVersionMiddleware(), s.auth.accessControlAuthMiddleware())
		s.registerAccessControlRoutes(accessAPI)
	}
	// Worker-read fallback (plan §5.3) deleted: workers bind admin port
	// to localhost only. Coord is the single ingress for any state read.

	// Coordinator-only operator endpoints for the cluster lifecycle:
	//
	//   /pairing/accept      — admin paste of worker-printed code →
	//                          signs CSR → wakes worker's long-poll
	//   /pairing/pending     — list pending pairing requests (code redacted)
	//   /decommission-worker — Worker → Unclaimed + drop from registry
	//   /revoke-worker       — append worker SPKI fingerprint to deny list
	//
	// Admin-keyed. These call in/out of the mTLS layer and are not
	// served by mTLS themselves — the pairing accept produces the
	// credentials via the coord's pending-request store; the
	// decommission endpoint is an operator action independent of the
	// worker's own auth state.
	if !isWorker {
		clusterOps := s.engine.Group("/zzrouter/v1/cluster",
			RequestIDMiddleware(), APIVersionMiddleware(),
			s.auth.adminAuthMiddleware())
		clusterOps.POST("/pairing/accept", s.clusterHandlers.handleClusterPairingAccept)
		clusterOps.GET("/pairing/pending", s.clusterHandlers.handleClusterPairingList)
		clusterOps.POST("/decommission-worker", s.clusterHandlers.handleClusterDecommissionWorker)
		clusterOps.POST("/revoke-worker", s.clusterHandlers.handleClusterRevokeWorker)
	}

	// Worker-side pairing lifecycle — available on Unclaimed workers
	// (which configure themselves as cluster.mode=worker in node.yaml
	// and therefore appear as isWorker=true here). Mounted in its own
	// group so it survives the !isWorker gate above. Loopback-exempt
	// admin middleware: the CLI on the same machine always bypasses
	// the admin key check (avoids env-var coordination friction between
	// the daemon's startup env and the operator's shell). Non-loopback
	// callers still need admin auth — defense-in-depth for multi-user
	// hosts and accidental external exposure.
	pairOps := s.engine.Group("/zzrouter/v1/cluster",
		RequestIDMiddleware(), APIVersionMiddleware(),
		loopbackOrAdmin(s.auth.adminAuthMiddleware()))
	pairOps.POST("/pair", s.clusterHandlers.handleClusterPair)
	pairOps.POST("/reset", s.clusterHandlers.handleClusterReset)

	// INTERNAL API — in-process dispatch ONLY on the public engine.
	// Remote coordinator→worker /internal/* is served on the mTLS
	// cluster-port listener (pkg/clusternode). This public-engine
	// mount is retained only so Server.ServeClusterRequest can
	// route local dispatch through s.engine.ServeHTTP without a
	// dedicated engine; internalRequestOnlyMiddleware 404s any
	// caller that doesn't carry the in-process trust marker.
	internalAPI := s.engine.Group("/zzrouter/v1/internal",
		RequestIDMiddleware(), APIVersionMiddleware(),
		internalRequestOnlyMiddleware())
	s.registerInternalRoutes(internalAPI)

	utils.LogDebugf("[Routes] Route setup complete")
}

// ============================================================================
// HEALTH ROUTES
// ============================================================================

// registerHealthRoutes registers health check endpoints for infrastructure
func (s *Server) registerHealthRoutes() {
	health := s.engine.Group("/health")
	{
		// Pass draining check function so readiness probe can report "draining" during graceful shutdown
		healthHandler := NewHealthHandler(
			[]string{"cluster", "admin_api", "providers", "openai_api"},
			func() bool { return s.draining.Load() },
			s.nodeIdentityReport,
		)
		health.GET("", healthHandler.GetHealth)
		health.GET("/live", healthHandler.GetHealthLive)
		health.GET("/ready", healthHandler.GetHealthReady)

		// Long-form probe aliases. Some orchestrators and probe tools expect
		// /health/liveliness and /health/readiness (with the -ness suffix)
		// instead of /live and /ready; these are thin wrappers around the
		// same handlers so a single health-check subsystem serves both
		// naming conventions.
		health.GET("/liveliness", healthHandler.GetHealthLive)
		health.GET("/readiness", healthHandler.GetHealthReady)

		// /health/services reports the state of cross-component integrations
		// zzrouter talks to (observability exporters, key stores, cluster
		// registry). Payload is intentionally minimal until the
		// per-integration probes are wired; the route exists so operators
		// get a structured 200 instead of a NoRoute 404.
		health.GET("/services", healthHandler.GetHealthServices)

		// /health/shared-status reports multi-pod coordination state. Single-
		// node zzrouter returns a stable "standalone" payload; cluster mode
		// will fill in peer info when the cluster registry is wired to this
		// endpoint.
		health.GET("/shared-status", healthHandler.GetHealthSharedStatus)
	}
	utils.LogDebugf("[Routes] Registered health endpoints: 7")

	// Prometheus metrics endpoint. Always registered so disabled-feature probes
	// receive a 503 Problem Details instead of a silent 404. Handler resolution
	// is lazy: the otel provider may be nil when observability is off, and
	// even when present the Prometheus reader is only built if metrics are on.
	// Both cases collapse to the same remediation hint.
	//
	// On coord: /metrics returns coord's own metrics; /metrics?node=<name>
	// proxies to that worker over cluster mTLS so Prometheus can scrape
	// cluster-wide without needing per-worker network access.
	s.engine.GET("/metrics", func(c *gin.Context) {
		if node := c.Query("node"); node != "" {
			s.proxyMetricsToNode(c, node)
			return
		}
		if s.otelProvider == nil {
			ServiceUnavailable(c, "metrics are disabled; set observability.enabled: true and observability.metrics.enabled: true in node.yaml")
			return
		}
		promHandler := s.otelProvider.PrometheusHandler()
		if promHandler == nil {
			ServiceUnavailable(c, "metrics are disabled; set observability.metrics.enabled: true in node.yaml")
			return
		}
		promHandler.ServeHTTP(c.Writer, c.Request)
	})
	utils.LogDebugf("[Routes] Registered Prometheus /metrics endpoint (lazy)")
}

// proxyMetricsToNode dispatches a /metrics request to the named worker
// over the cluster mTLS port. Coordinator-only; on a worker the cluster
// listener isn't connected to peer registry so the call would 502.
func (s *Server) proxyMetricsToNode(c *gin.Context, node string) {
	clusterURL := s.resolveNodeToClusterURL(node)
	if clusterURL == "" {
		ServiceUnavailable(c, "node "+node+" not found in cluster registry")
		return
	}
	client := s.coordWorkerMTLSClient()
	if client == nil {
		ServiceUnavailable(c, "coord mTLS dispatch unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), metricsProxyTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, clusterURL+"/metrics", nil)
	if err != nil {
		ServiceUnavailable(c, "build proxy request: "+err.Error())
		return
	}
	resp, err := client.Do(req)
	if err != nil {
		ServiceUnavailable(c, "proxy /metrics: "+err.Error())
		return
	}
	defer func() { _ = resp.Body.Close() }()
	mergeUpstreamHeaders(c.Writer.Header(), resp.Header)
	c.Writer.WriteHeader(resp.StatusCode)
	_, _ = io.CopyN(c.Writer, resp.Body, maxMetricsBytes)
}

// ============================================================================
// PUBLIC ROUTES
// ============================================================================

// registerPublicRoutes registers public API routes (/zzrouter/*)
// These are client-facing endpoints for orchestration and routing
func (s *Server) registerPublicRoutes(router *gin.RouterGroup) {
	utils.LogDebugf("[Routes] Registering public API routes (/zzrouter/*)")

	router.GET("/server/version", handleGetVersionSimple)               // Local version identity
	router.GET("/server/version/compatibility", handleGetCompatibility) // Local compatibility

	// Domain endpoints (require routing layer)
	RegisterPublicAPIRoutes(router, s, s.cluster.router)

	// Cluster system endpoints (registered after RegisterPublicAPIRoutes populates services.Nodes)
	clusterSystem := &ClusterSystemHandlers{nodes: s.services.Nodes}
	router.GET("/health", clusterSystem.HandleHealthFromNodes) // Cluster-wide health (reads from nodes registry)
}

// registerAccessControlRoutes mounts the teams / keys / spend surface on a
// dedicated group so it can be gated with a separate middleware (and, later,
// a separate API key) without touching URLs or handlers.
func (s *Server) registerAccessControlRoutes(router *gin.RouterGroup) {
	utils.LogDebugf("[Routes] Registering access control routes (/zzrouter/v1 teams+keys+spend)")
	RegisterAccessControlAPIRoutes(router, s)
}

// ============================================================================
// INTERNAL ROUTES
// ============================================================================

// registerInternalRoutes registers internal API routes (/zzrouter/internal/*)
// These are cluster-facing endpoints for direct provider execution
func (s *Server) registerInternalRoutes(router *gin.RouterGroup) {
	utils.LogDebugf("[Routes] Registering internal API routes (/zzrouter/internal/*)")

	RegisterInternalAPIRoutes(router, s)
}
