package server

import (
	"github.com/gin-gonic/gin"

	"github.com/stperic/zzrouter/pkg/httperr"
)

// buildWorkerCompatEngine returns a *gin.Engine serving the
// OpenAI (/v1/*), Ollama (/api/*), and NativeWire (/mcp + configured
// mounts) surfaces — but ONLY when this node is in Worker mode and
// ONLY on the cluster mTLS listener (installed via
// clusternode.Node.SetWorkerCompatHandler).
//
// Coord-proxied inference traffic flows through here: the coord dials
// the worker's cluster port over mTLS, transport-level OU=coordinator
// gate verifies the client cert, then this engine handles the request.
// Workers never expose this surface on the admin port — external
// clients hit the coord, never the worker.
//
// Auth model:
//   - Transport boundary IS the auth boundary (mTLS + OU check happens
//     before any request reaches this engine).
//   - clusterTrustedMiddleware sets CtxKeyClusterTrusted=true so
//     downstream OptionalAuth/RequireCompatAuth code paths grant the
//     synthetic admin context (forwarded X-API-Key from the original
//     caller may not match this worker's keys; mTLS is the auth).
//
// Same handler functions as the coordinator's compat surface — the
// difference is wire transport (mTLS vs plain HTTP) + the trusted-
// cluster context, NOT the request handling logic.
func buildWorkerCompatEngine(s *Server) *gin.Engine {
	engine := gin.New()
	engine.Use(gin.Recovery(), modelAdmissionMiddleware())
	engine.Use(RequestIDMiddleware())
	engine.Use(httperr.AttachByPath(s.responders.dispatcher))
	engine.Use(APIVersionMiddleware())
	engine.Use(clusterTrustedMiddleware())
	engine.Use(clusterHeadersFromCoordinator())

	// Mount the same handler set the coord serves on its admin port.
	// Reuse registerCompatibilityRoutes by temporarily swapping s.engine
	// — the registration helpers attach to s.engine via s methods.
	prevEngine := s.engine
	s.engine = engine
	defer func() { s.engine = prevEngine }()
	s.registerCompatibilityRoutes()
	// Without this the handlers registered above hold nil services: the
	// worker never registers the management API that builds them.
	s.bindCompatServices(s.cluster.router)
	// /metrics on workers lives on the cluster mTLS port (admin port
	// is localhost-only). Coord scrapes via /metrics?node=<name> proxy.
	engine.GET("/metrics", func(c *gin.Context) {
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
	return engine
}

// clusterTrustedMiddleware sets the trusted-cluster context flag for
// any request that reaches it. Safe ONLY because this middleware is
// installed exclusively on engines mounted behind the mTLS+OU gate
// (worker compat engine on the cluster port). Mounting it on the
// admin port engine would let any external client claim cluster trust.
func clusterTrustedMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set(string(CtxKeyClusterTrusted), true)
		c.Next()
	}
}
