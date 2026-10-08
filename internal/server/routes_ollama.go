// Ollama-compatible API routes (/api/*).
//
// This file owns the Ollama /api/* surface. It is called from
// registerCompatibilityRoutes in routes_compat.go.
//
// Ollama uses a flat error shape ({"error":"..."}) distinct from both the
// OpenAI envelope and RFC 7807, so the group installs its own responder.
// Authentication uses the same OptionalAuthMiddleware as OpenAI — anonymous
// by default for drop-in compatibility with tools like Open WebUI.
package server

import (
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/utils"
)

// registerOllamaRoutes wires the /api/* surface onto the engine.
// OllamaHandlers is created lazily because s.services.Ollama is populated
// later by RegisterPublicAPIRoutes. Gin closures capture *OllamaHandlers
// by pointer, so fields are resolved at request time, not registration time.
func (s *Server) registerOllamaRoutes() {
	h := &OllamaHandlers{
		ollamaDaemon:          s.ollamaDaemon,
		proxyToOllamaProvider: s.proxyToOllamaProvider,
		handleRoute:           s.HandleRoute,
		appMgr:                s.providers.appMgr,
		appsConfig:            func() *pkgConfig.AppsConfig { return s.appsConfig },
		adapter:               NewOllamaAdapter(),
		groups:                s.model.Groups,
		nodeName:              s.node.Nodename,
		// modelService is late-bound in bindOllamaService once
		// s.services.Model is populated by RegisterPublicAPIRoutes.
	}
	// Stash for routes_internal.go and late-bind of OllamaService
	s.ollamaHandlers = h

	// Root probe — Ollama CLI on Windows v0.20+ sends `HEAD /` (and
	// some clients `GET /`) as a connectivity handshake BEFORE making
	// any /api/* call. Without a 200 here the CLI aborts with a generic
	// "Error: something went wrong" and never reaches the rest of the
	// surface. Match the real daemon's response shape exactly: 200 with
	// "Ollama is running" plain-text body. Registered on the engine
	// root, not under /api/*, because that's where the CLI sends it.
	s.engine.GET("/", handleOllamaRootProbe)
	s.engine.HEAD("/", handleOllamaRootProbe)

	ollama := s.engine.Group("/api",
		httperr.AttachResponder(s.responders.ollama),
		RequestIDMiddleware(),
		s.auth.OptionalAuthMiddleware(),
		h.requireOllamaPresence(),
	)
	{
		// Version endpoint (required for Ollama compatibility).
		ollama.GET("/version", h.handleOllamaVersion)

		// Account/auth handshake endpoint. Ollama CLI v0.20+ on Windows
		// calls POST /api/me on every command as an account handshake;
		// the daemon answers 401 with a signin_url body when no account
		// is linked. If this endpoint returns 404 the CLI fails with the
		// generic "Error: something went wrong" before honoring
		// OLLAMA_HOST for the actual operation. We proxy to the upstream
		// Ollama daemon so its auth state (including the per-host SSH
		// keypair in ~/.ollama/id_ed25519) flows through unchanged.
		ollama.POST("/me", h.handleOllamaMe)

		// Inference endpoints.
		ollama.POST("/generate", h.ollamaInferenceHandler("generate"))
		ollama.POST("/chat", h.ollamaInferenceHandler("chat"))
		ollama.POST("/embeddings", h.ollamaInferenceHandler("embeddings"))
		// /api/embed is the newer Ollama alias for /api/embeddings.
		ollama.POST("/embed", h.ollamaInferenceHandler("embed"))

		// Model management.
		ollama.GET("/tags", h.handleOllamaTags)
		ollama.GET("/ps", h.handleOllamaPs)
		ollama.POST("/pull", h.handleOllamaPull)
		ollama.POST("/create", h.ollamaManagementHandler("/api/create", false))
		ollama.DELETE("/delete", h.ollamaManagementHandler("/api/delete", true))
		ollama.POST("/copy", h.ollamaManagementHandler("/api/copy", true))
		// /api/show flows through ModelService (the canonical show path with
		// cluster routing) and enriches with auto-route metadata when the
		// model is in a group. Other management verbs stay on the direct-
		// proxy path until ModelService has equivalents for them.
		ollama.POST("/show", h.handleOllamaShow)
		ollama.POST("/push", h.ollamaManagementHandler("/api/push", true))

		// Blob operations (for model layer management).
		ollama.HEAD("/blobs/:digest", h.handleOllamaBlobsHead)
		ollama.POST("/blobs/:digest", h.handleOllamaBlobsPost)
	}

	utils.LogDebugf("[Routes] Registered Ollama endpoints: 16")
}

// bindOllamaService late-binds the OllamaService and ModelService to the
// handlers. Called from RegisterPublicAPIRoutes after services are populated.
func (s *Server) bindOllamaService() {
	if s.ollamaHandlers != nil {
		s.ollamaHandlers.ollama = s.services.Ollama
		s.ollamaHandlers.modelService = s.services.Model
	}
}
