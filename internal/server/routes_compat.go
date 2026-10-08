// Compatibility routes dispatcher.
//
// Third-party API compatibility is intentionally split across multiple
// surfaces so each one stays small and greppable:
//
//   - routes_openai.go             — OpenAI /v1/* core + extended endpoints
//   - routes_compat_passthrough.go — OpenAI /v1/* stateful pass-through
//   - routes_ollama.go             — Ollama /api/*
//   - routes_messages.go           — Anthropic /v1/messages
//
// Future surfaces (native-wire provider passthrough, MCP transport, rerank)
// live in their own files and are wired in from here so the dispatcher
// remains the single place to audit what's exposed.
//
// All compatibility surfaces use OptionalAuthMiddleware — anonymous access by
// default for drop-in compatibility with existing tools, unless
// auth.require_compat_auth is set in node.yaml.
package server

import (
	"net/http"

	"github.com/gin-gonic/gin"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/routing"
	"github.com/stperic/zzrouter/pkg/utils"
)

// registerCompatibilityRoutes wires every third-party compatibility surface.
// Called from the route setup in routes.go during server init.
//
// Registration precedes the services these handlers call: gin closures
// capture each handler struct by pointer and read its fields per request,
// so the services are bound afterwards by bindCompatServices. Call that
// too -- a surface registered without it panics on its first request.
func (s *Server) registerCompatibilityRoutes() {
	utils.LogDebugf("[Routes] Registering compatibility routes")
	s.registerOpenAIRoutes()
	s.registerOllamaRoutes()
	s.registerMessagesRoutes()
	s.registerNativeWireRoutes()
	s.registerMCPRoutes()
}

// bindCompatServices fills in the services the compat handlers resolve at
// request time.
//
// A coordinator used to get this for free: RegisterPublicAPIRoutes builds
// the same services while registering the management API, and on a
// coordinator both surfaces live on one engine. A worker registers no
// management API -- only the mTLS compat engine -- so nothing built them,
// and every coordinator-proxied /api/* inference request called a method on
// a nil OllamaService. The worker panicked, gin recovered, and the
// coordinator reported the 500 as "backend returned status 500", naming the
// backend for a fault that was never in one.
//
// Idempotent: the coordinator calls it before the management API attaches
// its own hooks to these same services.
func (s *Server) bindCompatServices(routingRouter routing.Router) {
	if s.services.Model == nil {
		s.services.Model = NewModelService(s, s, s, s, routingRouter,
			func() *pkgConfig.AppsConfig { return s.appsConfig })
	}
	if s.services.Ollama == nil {
		s.services.Ollama = s.newOllamaService()
	}
	s.bindOllamaService()
	if s.openaiModelHandlers != nil {
		s.openaiModelHandlers.modelService = s.services.Model
	}
}

// handleEndpointProbe responds to GET requests on POST-only endpoints. Many
// client apps (BoltAI, Cursor, etc.) send GET to validate connectivity before
// making POST requests. Returns an OpenAI-compatible 200 OK.
func handleEndpointProbe(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"object":  "endpoint",
		"message": "This endpoint accepts POST requests. See https://platform.openai.com/docs/api-reference for usage.",
	})
}
