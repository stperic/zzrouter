// package server provides HTTP handlers for the zzrouter host server.
// Internal Routes - Cluster-internal API endpoints
//
// ============================================================================
// PURPOSE
// ============================================================================
// This file registers all internal API endpoints under /zzrouter/internal/*
// These endpoints are cluster-facing and handle:
//   - Direct provider execution (no routing logic)
//   - Local-only operations
//   - Node-to-node communication
//
// Authentication: CLUSTER KEY required
// Called by: Routing layer (LocalOnlyRouter, ClusterAwareRouter)
//
// ============================================================================
// IMPORTANT: ALL ROUTES ARE LOCAL-ONLY
// ============================================================================
// Internal routes NEVER do cluster routing. They execute directly on this node.
// The public API routes handle routing decisions and call these internal routes.
//
// Flow: Client → Public API → Routing Layer → Internal API (this file)
//
// ============================================================================

package server

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	clusterid "github.com/stperic/zzrouter/pkg/cluster/id"
	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	clusternode "github.com/stperic/zzrouter/pkg/cluster/node"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/utils"
)

// RegisterInternalAPIRoutes registers all internal API endpoints
// These endpoints do the ACTUAL WORK (call providers directly)
// They should ONLY be called by the routing layer, never by clients directly
func RegisterInternalAPIRoutes(router *gin.RouterGroup, s *Server) {
	utils.LogDebugf("[InternalAPI] Registering /zzrouter/internal/* routes")

	// Initialize executors (direct provider access, no routing)
	modelExecutor := s.newInternalExecutor()
	runsExecutor := s.newRunsExecutor()
	loadExecutor := s.newLoadExecutor()
	deploymentsExecutor := s.newDeploymentsExecutor()

	// Wire auto_deploy on /runs when both DeploymentsService and the
	// jobs registry are available. On a worker without coord-side
	// services, auto_deploy=true requests refuse with 503 instead of
	// silently 404'ing — runsExecutor.handleAutoDeployLaunch checks
	// nullity at the call site.
	if s.services.Deployments != nil && s.jobs != nil {
		runsExecutor.WithAutoDeploy(
			// Exact-only: suffix-index fallback would skip deploys when a
			// similarly-named (but different-variant) model is already in
			// the pool, leaving the launcher to fail against a missing file.
			func(name string) bool {
				return s.model.Cache.HasModelExact(context.Background(), name)
			},
			s.services.Deployments.Deploy,
			s.jobs,
		)
	}

	hostsExecutor := NewNodesExecutor(s.GetLocalNodeInfo, s.isLocalNodeCompatible)
	hostsExecutor.eligibleProviders = s.eligibleDeployProviders
	systemExecutor := NewSystemExecutor(s.node, s.role, s.providers.appMgr, s.model.Registry, s.GetSystemInfo, s.GetConnectionPoolStats, s.model.Cache.Invalidate, s.RefreshClusterEndpointsAsync, s.RefreshClusterEndpoint, s.MarkClusterEndpointDown)

	// ========================================================================
	// SYSTEM - Health and version for cluster monitoring
	// ========================================================================
	router.GET("/health", s.handleHealth)
	router.GET("/version", handleGetVersion)

	// E2E harness gate endpoints (route catalog + state snapshot/restore)
	// live on the public admin surface — see routes_public.go. Mounting
	// them here would dead-end at internalRequestOnlyMiddleware().

	// ========================================================================
	// MODELS - Direct model registry operations
	// ========================================================================
	router.GET("/models", modelExecutor.HandleInternalListModels)
	router.GET("/models/show", modelExecutor.HandleInternalShowModel)
	router.DELETE("/models", modelExecutor.HandleInternalDeleteModels)
	router.POST("/models/rescan", modelExecutor.HandleInternalRescanModels)   // Drop local registry scan cache (worker-side)
	router.POST("/models/refresh", systemExecutor.HandleInternalRefreshCache) // Worker-→-coord notification (coord re-probes peers)

	// ========================================================================
	// RUNS - Direct runs manager operations
	// ========================================================================
	// IMPORTANT: Register specific routes BEFORE parameterized routes
	router.GET("/runs", runsExecutor.HandleInternalListRuns)
	router.POST("/runs", runsExecutor.HandleInternalLaunchRun)
	router.POST("/runs/load", loadExecutor.HandleInternalLoadModel)     // Specific route before /runs/:id
	router.POST("/runs/preview", loadExecutor.HandleInternalPreviewRun) // Specific route before /runs/:id

	// Parameterized routes come after specific routes
	router.GET("/runs/:id", runsExecutor.HandleInternalGetRun)
	router.DELETE("/runs/:id", runsExecutor.HandleInternalStopRun)
	router.POST("/runs/:id/restart", runsExecutor.HandleInternalRestartRun)
	router.GET("/runs/:id/health", runsExecutor.HandleInternalGetRunHealth)
	router.GET("/runs/:id/logs", runsExecutor.HandleInternalGetRunLogs)
	// Coord's public /runs/:id/probes?node=W dispatches here via
	// s.cluster.router.Unicast. Local-only handler; never calls
	// HandleClusterAction (which is the public-side dispatcher).
	router.GET("/runs/:id/probes", s.logsHandlers.handleInternalGetRunProbes)

	// ========================================================================
	// DEPLOYMENTS - Direct download/registration operations
	// ========================================================================
	router.POST("/deployments", deploymentsExecutor.HandleInternalDeploy)
	router.GET("/deployments", deploymentsExecutor.HandleInternalListDeployments)
	router.DELETE("/deployments/all", deploymentsExecutor.HandleInternalStopAllDownloads) // Stop all downloads
	router.DELETE("/deployments/stop", deploymentsExecutor.HandleInternalStopDownload)    // Stop single download (key via query param)

	// ========================================================================
	// PROVIDERS - Unified runtime status + lifecycle operations
	// ========================================================================
	providersExec := NewProvidersExecutor(
		s.providers.appMgr, s.node.Name(), s.GetLocalAppsInfo,
		func() *pkgConfig.AppsConfig { return s.appsConfig }, s.configStore,
		s.FinalizeOnboarding, s.FinalizeOffboarding, s.publishSelfSnapshot,
	)
	router.POST("/providers/:name/install/authority", providersExec.HandleInternalInstallAuthority)
	router.GET("/providers", providersExec.HandleInternalListProviders)
	router.GET("/providers/:name", providersExec.HandleInternalGetProvider)
	router.PATCH("/providers/:name", providersExec.HandleInternalUpdateProvider)

	// ========================================================================
	// PARAMS - Direct parameter management (coordinator-only)
	// ========================================================================
	// Parameter CRUD is coordinator-only. Workers receive pre-merged parameters
	// from the coordinator in the launch request and only resolve "auto" values.
	paramsExecutor := s.newParamsExecutor()
	router.GET("/providers/:name/resolved", paramsExecutor.HandleResolved)
	if !s.node.IsWorker() {
		// Read shims — delegate to Resolve(); retired mutators 410.
		router.GET("/providers/:name/parameters", paramsExecutor.HandleInternalGetAppParameters)
		router.GET("/providers/:name/nodes/parameters", paramsExecutor.HandleInternalGetNodeParameters)
		router.GET("/providers/:name/models/parameters", paramsExecutor.HandleInternalGetModelParameters)
		router.PUT("/providers/:name/parameters", paramsExecutor.RetiredHandler)
		router.DELETE("/providers/:name/parameters/:key", paramsExecutor.RetiredHandler)
		router.PATCH("/providers/:name/parameters/:key/ignore", paramsExecutor.RetiredHandler)
		router.PUT("/providers/:name/nodes/parameters", paramsExecutor.RetiredHandler)
		router.DELETE("/providers/:name/nodes/parameters/:key", paramsExecutor.RetiredHandler)
		router.PATCH("/providers/:name/nodes/parameters/:key/ignore", paramsExecutor.RetiredHandler)
		router.PUT("/providers/:name/models/parameters", paramsExecutor.RetiredHandler)
		router.DELETE("/providers/:name/models/parameters/:key", paramsExecutor.RetiredHandler)
		router.PATCH("/providers/:name/models/parameters/:key/ignore", paramsExecutor.RetiredHandler)
	}

	// Service management — registered on ALL nodes so the coord's public
	// /providers/:name/service/* handlers can reach the node the caller
	// named. Local-only, like every internal handler: it reports and acts
	// on the process running here and never re-dispatches.
	{
		serviceExecutor := s.newParamsExecutor()
		router.GET("/providers/:name/service/status", serviceExecutor.HandleInternalGetServiceStatus)
		router.POST("/providers/:name/service/apply", serviceExecutor.HandleInternalApplyService)
		for _, action := range []string{"start", "stop", "restart"} {
			router.POST("/providers/:name/service/"+action, serviceExecutor.HandleInternalServiceControl(action))
		}
	}

	// Parameter auto-resolution — registered on ALL nodes (workers + coordinator).
	// Coordinators route resolve requests here via unicast so each node resolves
	// against its own hardware (GPU, memory, etc.).
	{
		resolveExecutor := s.newParamsExecutor()
		router.POST("/providers/:name/resolve", resolveExecutor.HandleInternalResolve)
	}

	// ========================================================================
	// DISCOVER - Read-only hardware/network inventory (per-node local view)
	// ========================================================================
	// Mounted on every node so the coord's public /discover/* handlers can
	// fan out via s.cluster.router.Unicast and reach each peer's local
	// inventory over mTLS. The internal handlers ALWAYS run locally —
	// no Route() wrapper, no ?node= dispatch, no cross-host loops. The
	// public surface (registered only on coord under registerPublicRoutes)
	// is the layer that decides which peer to ask. See discovery_controller.go.
	s.ensureDiscoveryController().RegisterInternalRoutes(router)

	// ========================================================================
	// NODES - Direct node information
	// ========================================================================
	router.GET("/nodes", hostsExecutor.HandleInternalListNodes)
	router.GET("/nodes/compatible", hostsExecutor.HandleInternalListCompatibleNodes)

	// ========================================================================
	// SYSTEM - Direct system information
	// ========================================================================
	router.GET("/system", systemExecutor.HandleInternalGetSystemInfo)

	// ========================================================================
	// RESOURCES - Resource metrics for cluster routing
	// ========================================================================
	resourcesExecutor := NewResourcesExecutor(s.node.Name(), s.cluster.resources, s.providers.appMgr)
	router.GET("/resources", resourcesExecutor.HandleInternalGetResources)

	// ========================================================================
	// JOBS - Per-node streaming registry for long-running operations
	// ========================================================================
	// Mounted on every cluster-mode node (coord + worker) so the future
	// public-surface proxy can forward to any node's /internal/jobs/*.
	if s.jobs != nil {
		NewJobsController(s.jobs, s.node.Name(), s.node.IsLocalNode).RegisterInternalRoutes(router)
	}

	// ========================================================================
	// SYNC - Internal model sync for multi-node pulls
	// ========================================================================
	// These endpoints allow worker nodes to sync models from master node
	// over the internal LAN instead of downloading from the internet.
	// Security: Cluster key required, path validation, SHA256 verification
	// refreshIndex fires when a cross-node sync completes on this node.
	// Invalidates the cluster-join ModelCache, which cascades to the
	// Registry scan cache, so the new model propagates to readers on
	// the next query.
	var refreshIndex func() error
	if s.model.Cache != nil {
		refreshIndex = func() error {
			s.model.Cache.Invalidate()
			return nil
		}
	}
	getEndpoints := func() []*mesh.Endpoint {
		if s.cluster.coordinator == nil {
			return nil
		}
		return s.cluster.coordinator.GetAllEndpoints()
	}
	// Workers dial the coordinator over mTLS for sync. Mode flips at
	// runtime (Unclaimed→Worker on claim), so resolve on each call.
	// Returns nil on non-worker modes; SyncExecutor refuses the
	// request in that case.
	mtlsClientFn := func() *http.Client {
		if s.cluster.listener == nil || s.cluster.listener.Mode() != clusternode.Worker {
			return nil
		}
		client, err := s.cluster.listener.DialClient(clusterid.RoleCoordinator.OU())
		if err != nil {
			return nil
		}
		return client
	}
	syncExecutor := NewSyncExecutor(
		mtlsClientFn,
		s.config.Cluster.BindPort,
		func() context.Context { return s.shutdownCtx },
		refreshIndex,
		getEndpoints,
	).WithJobs(s.jobs)
	syncExecutor.ensureFeatures = s.providers.appMgr.EnsureModelFeatures
	router.GET("/sync/manifest", syncExecutor.HandleGetManifest) // Get file manifest with checksums
	router.POST("/sync/manifest", syncExecutor.HandleGetManifest)
	router.GET("/sync/file", syncExecutor.HandleDownloadFile)       // Download model file (supports Range)
	router.GET("/sync/exists", syncExecutor.HandleCheckModelExists) // Check if model exists on node
	router.POST("/sync/exists", syncExecutor.HandleCheckModelExists)
	router.POST("/sync/deploy", syncExecutor.HandleSyncPull) // Instruct node to sync from source

	// Provider-tree fan-out (plan §5.2): coordinator pushes updated
	// config.yaml / schema.yaml bytes here whenever AppsConfigStore
	// mutates. Worker-only; standalone + coord refuse via
	// HandleProviderSync's own role check.
	router.POST("/sync/providers/:name", s.HandleProviderSync)

	// PROVIDERS - Lifecycle operations (only when providerAppMgr is available)
	if s.providers.appMgr != nil {
		router.GET("/providers/:name/status", providersExec.HandleInternalGetProviderStatus)
		router.GET("/providers/:name/install/status", providersExec.HandleInternalGetInstallStatus)
		router.POST("/providers/:name/install", providersExec.HandleInternalInstallProvider)
		router.POST("/providers/:name/install/preflight", providersExec.HandleInternalPreflightInstall)
		router.POST("/providers/:name/install/plan", providersExec.HandleInternalGetInstallPlan)
		router.GET("/providers/:name/environment", providersExec.HandleInternalProviderEnvironment)
		router.POST("/providers/:name/install/verify", providersExec.HandleInternalVerifyInstall)
		router.POST("/providers/:name/install/verify-step", providersExec.HandleInternalVerifyInstallStep)
		router.POST("/providers/:name/install/execute-step", providersExec.HandleInternalExecuteStep)
		router.DELETE("/providers/:name/install/disposable/:plan_id", providersExec.HandleInternalDeleteDisposable)
		router.POST("/providers/:name/upgrade", providersExec.HandleInternalUpgradeProvider)
		router.DELETE("/providers/:name", providersExec.HandleInternalUninstallProvider)
	}

	// ========================================================================
	// OLLAMA COMPAT - Internal Ollama compatibility endpoints
	// ========================================================================
	// /api/tags now flows through ModelService (the canonical cluster-aware
	// catalog), so the /internal/ollama/tags broadcast endpoint is no longer
	// needed. /api/ps remains a live-polling query (operational state, not
	// cached state) and keeps its internal endpoint.
	//
	// On workers, ollamaHandlers is nil (compat routes only register on
	// coordinators), so build a minimal instance here for /ollama/ps.
	oh := s.ollamaHandlers
	if oh == nil {
		oh = &OllamaHandlers{
			appMgr:     s.providers.appMgr,
			appsConfig: func() *pkgConfig.AppsConfig { return s.appsConfig },
			nodeName:   s.node.Nodename,
		}
	}
	router.GET("/ollama/ps", oh.HandleOllamaPsLocal)
}
