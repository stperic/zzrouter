// package server provides HTTP handlers for the zzrouter host server.
// Public Routes - Client-facing API endpoints

package server

import (
	"context"

	"github.com/gin-gonic/gin"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stperic/zzrouter/pkg/model/cache"
	"github.com/stperic/zzrouter/pkg/modelregistry"
	"github.com/stperic/zzrouter/pkg/prov_apps/logging"
	"github.com/stperic/zzrouter/pkg/prov_apps/upstream"
	"github.com/stperic/zzrouter/pkg/routing"
	"github.com/stperic/zzrouter/pkg/update"
	"github.com/stperic/zzrouter/pkg/utils"
)

// RegisterPublicAPIRoutes registers all public API endpoints
// These endpoints handle orchestration and routing
func RegisterPublicAPIRoutes(router *gin.RouterGroup, s *Server, routingRouter routing.Router) {
	utils.LogDebugf("[PublicAPI] Registering /zzrouter/* routes")

	// API discovery: GET /zzrouter/v1 — returns endpoint groups and
	// enabled features so AI agents can bootstrap without guessing paths.
	registerAPIDiscoveryRoute(router, s)

	// Initialize services (inject routing layer). The compat surfaces need
	// a subset of these and are registered on their own engine on a worker,
	// so that subset is built by a function both roles call.
	s.bindCompatServices(routingRouter)
	s.services.Model.SetOnModelDelete(func(modelName string) {
		s.model.AutoRoute.SyncAfterDelete(modelName)
	})
	s.services.Search = NewModelSearchService(func() *pkgConfig.AppsConfig { return s.appsConfig }, s.model.Pricing)
	s.services.Runs = NewRunsService(routingRouter).WithModelValidator(s.validateModel)
	s.services.Load = NewLoadService(s, routingRouter).
		WithTierResolution(func() *pkgConfig.AppsConfig { return s.appsConfig })
	s.services.Deployments = NewDeploymentsService(routingRouter, s.node, func() *pkgConfig.AppsConfig { return s.appsConfig }).
		WithNodeURLResolver(s.resolveNodePublicURL)
	s.services.Deployments.repoFiles = s.model.Registry.GetHuggingFaceConnector().GetRepoFiles
	s.services.Deployments.refresher = s.runsRefresher()
	// Wire public-side auto_deploy on /zzrouter/v1/runs so non-coord
	// targets get the deploy step. Workers route here first; the chain
	// runs at coord and forwards the launch back to the target.
	if s.jobs != nil {
		s.services.Runs.WithAutoDeploy(
			s.services.Deployments,
			func(name string) bool {
				return s.model.Cache.HasModelExact(context.Background(), name)
			},
			func() *pkgConfig.AppsConfig { return s.appsConfig },
			s.jobs,
		)
	}
	// EnsureRun resolves a bare model name → CachedModel via the cache;
	// the cache already handles SourceID alias resolution so the agent
	// can pass either form of the name.
	s.services.Runs.WithModelLookup(func(ctx context.Context, name string) *cache.CachedModel {
		cm, err := s.model.Cache.LookupModel(ctx, name)
		if err != nil {
			return nil
		}
		return cm
	})
	s.services.Apps = NewAppsService(s, routingRouter)
	s.services.Nodes = NewNodesService(s, routingRouter, func(address string) string {
		return s.config.Cluster.Endpoints.NameFor(address)
	})
	s.services.System = NewSystemService(routingRouter)
	// Initialize controllers
	nodeValidator := &MeshAwareNodeValidator{NodeIdentity: s.node, EndpointsFn: s.GetClusterEndpoints}
	s.controllers.Models = NewModelsController(s.services.Model, s.services.Search, nodeValidator)
	appsConfigFn := func() *pkgConfig.AppsConfig { return s.appsConfig }
	s.services.RunsUtility = NewRunsUtilityService(s.providers.appMgr, appsConfigFn)
	s.controllers.Runs = NewRunsController(
		s.services.Runs, s.services.Load, s.services.RunsUtility,
		nodeValidator,  // NodeValidator
		s.logsHandlers, // RunLogHandler
		appsConfigFn,   // for provider lookups on preview
	)
	s.ensureDiscoveryController()
	s.controllers.Deployments = NewDeploymentsController(s.services.Deployments)
	s.controllers.Nodes = NewNodesController(s.services.Nodes)
	var logMgr *logging.Manager
	if s.providers.appMgr != nil {
		logMgr = s.providers.appMgr.LogManager()
	}
	s.controllers.System = NewSystemController(s.services.System, s.config, logMgr)
	s.controllers.Params = NewParamsController(s.newParamsExecutor().WithRunsRefresher(s.runsRefresher()))
	s.controllers.Assets = NewProviderAssetsController(s.configStore, templates.IsShippedAsset).
		WithSyncWait(s.awaitProviderSync).
		WithRunsRefresher(s.runsRefresher())

	// ========================================================================
	// BROADCAST & ACTION ROUTES (Delegated to Controllers)
	// ========================================================================

	// Models: List, show, delete, stats, rescans, card, defaults
	s.controllers.Models.RegisterPublicRoutes(router)

	// Runs: List, launch, stop, load, preview, restart, metrics, config, ports, batch
	s.controllers.Runs.RegisterPublicRoutes(router)

	// Params: Schema, get/set/delete parameters, ignore
	s.controllers.Params.RegisterPublicRoutes(router)

	// Provider assets: files asset-typed parameters name.
	s.controllers.Assets.RegisterPublicRoutes(router)

	// Nodes: List, list compatible
	s.controllers.Nodes.RegisterPublicRoutes(router)

	// System: Cluster-wide and local system info, show, logs
	s.controllers.System.RegisterPublicRoutes(router)

	// Deployments: List, deploy, stop
	s.controllers.Deployments.RegisterPublicRoutes(router)

	// Registries: agent-friendly pre-pull discovery (e.g., GGUF variants in a HF repo).
	NewRegistriesController(func() *modelregistry.Registry { return s.model.Registry }).RegisterPublicRoutes(router)

	// Discovery: Discover resources across cluster
	s.controllers.Discovery.RegisterPublicRoutes(router)
	// Cluster-aggregated GPU view; reaches peers via cluster mTLS
	// (s.cluster.router), not the worker admin port.
	router.GET("/cluster/hardware/gpus", s.handleClusterHardwareGPUs)

	// Model Groups: List and get model groups (with cooldown status)
	if s.model.Groups != nil {
		s.controllers.ModelGroups = NewModelGroupsController(
			s.model.Groups,
			s.providers.cooldowns.Deployments(),
			s.providers.loadTracker,
			s.providers.latencyTracker,
			s.providers.healthChecker,
			s.inference.logStore,
			s.providers.routeEvents,
			s.model.Pricing,
			s.config.Coordinator.Routing.GetRoutePrefix(),
		)
		s.controllers.ModelGroups.RegisterPublicRoutes(router)
	}

	// Provider Status: Provider-level health/cooldown and rate limit status
	s.controllers.ProviderStatus = NewProviderStatusController(func() *pkgConfig.AppsConfig { return s.appsConfig }, s.providers.cooldowns.Providers(), s.providers.rateTracker)
	s.controllers.ProviderStatus.RegisterPublicRoutes(router)

	// Teams and Keys controllers are constructed here (the general public
	// API depends on them being wired up for the rest of startup), but their
	// HTTP routes are registered under a separate group — see
	// RegisterAccessControlAPIRoutes below.
	if s.teamStore != nil && s.access != nil {
		s.services.Teams = NewTeamsService(s.teamStore, s.keyStore, s.access)
		s.controllers.Teams = NewTeamsController(s.services.Teams)

		// Sweep orphan personal teams — any kind=personal team with zero
		// referencing keys is the result of a crash mid-create-key and
		// should be garbage-collected before serving traffic. Audit
		// sink is still audit.Null here (opened later in
		// startCoordinatorSubsystems); the reaper's audit events are
		// captured by a follow-up sweep on coordinator start.
		s.services.Teams.ReconcilePersonalTeams()
	}

	if s.keyStore != nil && s.access != nil {
		s.services.Keys = NewKeysService(s.keyStore, s.services.Teams, s.access)
		s.controllers.Keys = NewKeysController(s.services.Keys)
	}

	// Config reload: POST /config/reload re-reads providers/ from disk,
	// runs every listener, returns the per-listener ReloadReport.
	// Route is registered even when s.configStore is nil so
	// disabled-feature probes get 503 rather than a silent 404.
	registerConfigReloadRoute(router, s.configStore)

	// Update: Auto-update management
	// Lazy getter: setupRoutes runs before the scheduler is constructed
	// in server_lifecycle.Start, so capturing the pointer here would
	// always yield nil — the controller would 503 even when
	// update.enabled: true.
	updateController := NewUpdateController(func() *update.Scheduler { return s.updateScheduler })
	updateController.server = s
	updateController.RegisterRoutes(router)

	// Pricing: token-cost pricing data lookup.
	// Routes are registered even when the store is nil so disabled-feature
	// probes get a 503 Problem Details instead of a silent 404.
	pricingController := NewPricingController(s.model.Pricing)
	pricingController.RegisterRoutes(router)

	// Providers: Unified runtime status + lifecycle (install, upgrade, uninstall)
	{
		var provSvc *ProvidersService
		if s.providers.appMgr != nil {
			provSvc = NewProvidersService(s.cluster.router)
		}
		appsConfigFn := func() *pkgConfig.AppsConfig { return s.appsConfig }

		// Upstream release checks are coordinator-side: one outbound call per
		// provider for the whole cluster rather than one per worker. Gate on
		// the node's role rather than on configStore, which is also nil in
		// harnesses that are nonetheless coordinators.
		var versionsSvc *ProviderVersionsService
		if s.IsCoordinator() {
			ttl := upstreamCheckInterval(s.appsConfig)
			versionsSvc = NewProviderVersionsService(appsConfigFn, upstream.NewCache(ttl, 0))
		}

		providersController := NewProvidersController(s.services.Apps, provSvc, appsConfigFn, s.configStore, s, s.providers.appMgr, versionsSvc)
		providersController.RegisterPublicRoutes(router)
	}

	// Inference Logs: Query and stream inference request logs
	if s.inference.logStore != nil {
		s.services.InferenceLog = NewInferenceLogService(s.inference.logStore)
		s.controllers.InferenceLog = NewInferenceLogController(
			s.services.InferenceLog,
			func() string { return s.inference.logJobID },
		)
		s.controllers.InferenceLog.RegisterPublicRoutes(router)
	}

	// Jobs: unified progress + streaming for downloads, installs,
	// updates, sync, inference-log. Public routes (coord-only; this
	// group is coord-gated at mount time in routes.go). Cross-node
	// ?node=<remote> queries proxy onto the owning worker's internal
	// endpoint over mTLS; the proxy is wired only when cluster deps
	// (listener + coordinator) are live.
	if s.jobs != nil {
		var proxy *JobsProxy
		if s.cluster.listener != nil && s.cluster.coordinator != nil {
			// Cache the streaming mTLS client for the lifetime of this
			// route mount. Routes rebuild on role transition, so the
			// cache implicitly invalidates when the listener identity
			// changes. Without the cache each proxy request built a
			// fresh *http.Transport, leaking idle pools + TLS sessions.
			proxy = &JobsProxy{
				StreamClient:    s.cachedJobsStreamClient(),
				ResolveEndpoint: s.resolveJobsEndpoint,
				ListPeers:       s.listJobsPeers,
				ClusterPort:     s.config.Cluster.BindPort,
			}
		}
		jobsController := NewJobsControllerWithProxy(s.jobs, s.node.Name(), s.node.IsLocalNode, proxy)
		jobsController.cancelOwner = s.cancelUpdateRollout
		jobsController.RegisterPublicRoutes(router)
	}

	// ========================================================================
	// MISC ROUTES (Pending further decomposition)
	// ========================================================================

	// ========================================================================
	// CLUSTER ROUTES - Cluster topology management
	// ========================================================================
	// These routes manage the cluster itself (adding/removing nodes).
	// They use direct cluster client calls, not the routing layer.

	// Cluster membership is driven by the pairing flow (worker pair →
	// coord accept). The old admin-POST add path was
	// retired — mesh admission now happens inside the pair-accept
	// handler via admitClusterMember, which is the one writer. List
	// lives on /zzrouter/v1/nodes (NodesService) with richer probe
	// data. DELETE is kept for removing stale/retired peers.
	ch := s.clusterHandlers
	router.DELETE("/cluster/endpoints/:address", ch.handleRemoveClusterEndpoint) // CLUSTER: remove endpoint row
	router.POST("/cluster/connect", ch.handleConnectToClusterNode)               // CLUSTER: test connection
	router.POST("/cluster/validate", ch.handleValidateClusterConnections)        // CLUSTER: validate all
	// NOTE: /cluster/leave is served by the worker-side mTLS cluster
	// listener in pkg/cluster/node (registerWorkerRoutes), not here —
	// it is only reachable from a coordinator client cert.

	// E2E harness gate endpoints. Public-admin surface (the internal-API
	// mount on the public engine is gated by internalRequestOnlyMiddleware
	// and reserved for in-process dispatch). Coordinator-only — workers
	// have no keys/teams/groups state, and the route catalog on a worker
	// would misrepresent compat-gate auth tags.
	if !s.node.IsWorker() {
		router.GET("/server/routes", s.handleListRoutes)
		router.POST("/state/snapshot", s.handleStateSnapshot)
		router.POST("/state/restore", s.handleStateRestore)
	}
}

// RegisterAccessControlAPIRoutes mounts the teams + keys + spend surface on
// a caller-supplied router group. The group is expected to sit under
// accessControlAuthMiddleware; this function does not apply any auth of its
// own.
//
// Ordering contract: RegisterPublicAPIRoutes MUST run first so services.Teams
// and services.Keys are wired before this function tries to register their
// routes. If that ordering is violated, nil controllers here mean the routes
// silently disappear from the API surface — instead of failing quietly we
// panic so the mistake surfaces at startup rather than as mysterious 404s
// much later.
func RegisterAccessControlAPIRoutes(router *gin.RouterGroup, s *Server) {
	if s.controllers.Teams == nil {
		panic("RegisterAccessControlAPIRoutes: TeamsController not constructed: " +
			"RegisterPublicAPIRoutes must run before this function")
	}
	if s.controllers.Keys == nil {
		panic("RegisterAccessControlAPIRoutes: KeysController not constructed: " +
			"RegisterPublicAPIRoutes must run before this function")
	}
	s.controllers.Teams.RegisterPublicRoutes(router)
	s.controllers.Keys.RegisterPublicRoutes(router)
	router.GET("/spend/events", s.handleSpendEventsStream)
}
