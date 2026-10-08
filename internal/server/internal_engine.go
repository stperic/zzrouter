package server

import (
	"github.com/gin-gonic/gin"
)

// buildInternalEngine returns a *gin.Engine that serves ONLY the
// /zzrouter/v1/internal/* routes on the cluster-port listener. It is
// installed on the clusternode.Node via SetAdminAPIHandler and the
// clusternode layer enforces mTLS + OU=coordinator before requests
// ever reach this engine.
//
// No application-layer auth middleware runs here: the transport
// boundary is the auth boundary. RequestID and APIVersion are kept
// for operator traceability across coordinator→worker hops.
//
// Note: the public engine (s.engine) also mounts /internal/* under
// internalRequestOnlyMiddleware so Server.ServeClusterRequest can
// route in-process dispatch through s.engine.ServeHTTP. That mount
// is gated to local trusted callers only; cross-host /internal/*
// arrives here via the cluster mTLS listener.
func buildInternalEngine(s *Server) *gin.Engine {
	engine := gin.New()
	engine.Use(gin.Recovery(), modelAdmissionMiddleware())

	group := engine.Group("/zzrouter/v1/internal",
		RequestIDMiddleware(),
		APIVersionMiddleware(),
	)
	RegisterInternalAPIRoutes(group, s)
	if s.node.IsWorker() {
		registerClusterUpdateRoutes(group, s)
	}
	return engine
}

// buildCoordInternalEngine returns the narrow /zzrouter/v1/internal/*
// surface served on the coordinator's cluster listener. Only routes a
// worker legitimately calls on the coord land here — today that is
// POST /models/refresh (cache-coherency notify). The full inference
// engine is not reused on coord because workers must not reach
// coord-side lifecycle handlers (install, PATCH, runs) that were only
// intended for the coord→worker direction.
func buildCoordInternalEngine(s *Server) *gin.Engine {
	engine := gin.New()
	engine.Use(gin.Recovery(), modelAdmissionMiddleware())

	group := engine.Group("/zzrouter/v1/internal",
		RequestIDMiddleware(),
		APIVersionMiddleware(),
	)
	systemExecutor := NewSystemExecutor(
		s.node, s.role, s.providers.appMgr, s.model.Registry,
		s.GetSystemInfo, s.GetConnectionPoolStats,
		s.model.Cache.Invalidate, s.RefreshClusterEndpointsAsync,
		s.RefreshClusterEndpoint,
		s.MarkClusterEndpointDown,
	)
	group.POST("/models/refresh", systemExecutor.HandleInternalRefreshCache)
	group.POST("/goodbye", systemExecutor.HandleInternalGoodbye)
	return engine
}
