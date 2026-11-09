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
