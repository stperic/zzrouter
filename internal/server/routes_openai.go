// OpenAI-compatible API routes (/v1/*).
//
// This file owns the OpenAI /v1/* surface. It is called from
// registerCompatibilityRoutes in routes_compat.go, which is the single
// dispatcher for all third-party compatibility surfaces (OpenAI, Ollama,
// native-wire passthrough, MCP transport).
//
// Core inference + extended endpoints live here. Stateful pass-through routes
// (files, batches, fine-tuning, assistants, threads, vector stores, uploads,
// responses retrieval) live in routes_compat_passthrough.go and are registered
// via registerOpenAIStatefulRoutes.
//
// Authentication: OptionalAuthMiddleware — accepts Authorization: Bearer or
// X-API-Key, allows anonymous access unless auth.require_compat_auth is set.
// The OpenAI responder is attached so errors emit the OpenAI envelope instead
// of RFC 7807.
package server

import (
	"encoding/json"

	"github.com/gin-gonic/gin"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/utils"
)

// skipIdempotencyOnStreamingBody short-circuits the idempotency
// middleware when the inference body has `"stream": true`. The cache
// would otherwise buffer the entire SSE chunk sequence into memory
// and replay it instantly on a duplicate, breaking live-stream timing.
// Streaming idempotency is a separate, harder problem (partial-replay,
// chunk-pacing) that we defer. Non-streaming requests — where the
// double-billing risk is concrete — remain protected.
func skipIdempotencyOnStreamingBody(body []byte) bool {
	var probe struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &probe)
	return probe.Stream
}

// registerOpenAIRoutes wires the /v1/* surface onto the engine. Returns the
// number of stateful pass-through routes registered (for the startup log).
func (s *Server) registerOpenAIRoutes() int {
	openaiResponder := s.responders.openai

	// Model catalog handlers — late-bound: modelService is set by bindOpenAIModelService
	// after RegisterPublicAPIRoutes populates s.services.Model.
	appsConfigFn := func() *pkgConfig.AppsConfig { return s.appsConfig }
	mh := &OpenAIModelHandlers{
		appsConfig:      appsConfigFn,
		groups:          s.model.Groups,
		adapter:         NewOpenAIAdapter(appsConfigFn),
		respondInternal: openaiResponder.Internal,
		runtimeLookup:   s.openAIModelRuntimeLookup,
		localNode:       s.GetNodename,
	}
	s.openaiModelHandlers = mh

	// Core inference group.
	// Inference handlers — dispatch methods are on *Server, injected as callbacks
	ih := &OpenAIInferenceHandlers{
		dispatchWithRecorder: s.resolveAndDispatchWithRecorder,
		dispatch:             s.resolveAndDispatchSimple,
		maxUploadBytes:       s.openAIMaxUploadBytes,
		forwardToDefault:     s.forwardBufferedToDefaultBackend,
	}

	openai := s.engine.Group("/v1",
		httperr.AttachResponder(openaiResponder),
		RequestIDMiddleware(),
		s.auth.OptionalAuthMiddleware(),
	)
	// Per-route idempotency middleware for the inference paths.
	// Each route gets its own store so a client reusing the same
	// Idempotency-Key across different routes (e.g. /chat/completions
	// then /completions) doesn't replay the wrong route's response —
	// same key + different route = different operation.
	// Streaming bodies bypass via skipIdempotencyOnStreamingBody (the
	// cache would otherwise buffer the entire SSE chunk sequence and
	// replay it instantly, breaking live-stream timing).
	newInferenceIdemMiddleware := func() gin.HandlerFunc {
		return newIdempotencyStore().middlewareWithBodySkip(skipIdempotencyOnStreamingBody)
	}
	{
		// Core inference endpoints. Idempotency-Key dedup closes the
		// agent-retry double-billing risk on non-streaming requests.
		openai.POST("/chat/completions",
			newInferenceIdemMiddleware(),
			ih.handleChatCompletions)
		openai.POST("/completions",
			newInferenceIdemMiddleware(),
			ih.handleCompletions)
		openai.GET("/models", mh.handleModels)
		openai.GET("/models/*model", mh.handleModelByID)
		openai.DELETE("/models/*model", mh.handleDeleteModelByID)

		openai.GET("/model/info", mh.handleModelInfo)

		// Extended endpoints.
		openai.POST("/embeddings", ih.handleEmbeddings)
		openai.POST("/rerank", ih.handleRerank)
		openai.POST("/images/generations", ih.handleImagesGenerations)
		openai.POST("/moderations", ih.handleModerations)

		// JSON-body pass-through (model field routing). /responses
		// also accepts stream:true bodies; same predicate applies.
		openai.POST("/responses",
			newInferenceIdemMiddleware(),
			s.handleResponses)
		openai.POST("/audio/speech", ih.handleAudioSpeech)

		// Multipart pass-through. All chat-default — these endpoints route
		// to cloud providers that handle every endpoint in one server.
		openai.POST("/audio/transcriptions", func(c *gin.Context) { ih.multipartRouteByModel(c) })
		openai.POST("/audio/translations", func(c *gin.Context) { ih.multipartRouteByModel(c) })
		openai.POST("/images/edits", func(c *gin.Context) { ih.multipartRouteByModel(c) })
		openai.POST("/images/variations", func(c *gin.Context) { ih.multipartRouteByModel(c) })

		// /v1/realtime is a bidirectional WebSocket upgrade plus three
		// HTTP helper endpoints for session/token minting. The upgrader
		// requires openai_compat.realtime_backend to be configured; when
		// it is not, the handler returns a structured 503 with guidance
		// instead of a generic NoRoute 404.
		s.registerRealtimeRoutes(openai)

		// Endpoint validation — many client apps send GET/POST to test
		// connectivity before making real requests.
		openai.GET("/chat/completions", handleEndpointProbe)
		openai.GET("/completions", handleEndpointProbe)
		openai.GET("/embeddings", handleEndpointProbe)
	}

	// Stateful /v1/* endpoints routed to openai_compat.default_backend
	// (Files, Batches, Fine-tuning, Assistants, Threads, Vector Stores,
	// Uploads, Responses retrieval). A fresh group is used so the stateful
	// routes do not share closure state with the core inference block above.
	openaiStateful := s.engine.Group("/v1",
		httperr.AttachResponder(openaiResponder),
		RequestIDMiddleware(),
		s.auth.OptionalAuthMiddleware(),
	)
	statefulCount := s.registerOpenAIStatefulRoutes(openaiStateful)

	// Root /v1 probe — some clients POST or GET /v1 directly to validate the
	// endpoint.
	v1root := s.engine.Group("/v1",
		httperr.AttachResponder(openaiResponder),
		RequestIDMiddleware(),
		s.auth.OptionalAuthMiddleware(),
	)
	{
		v1root.GET("", handleEndpointProbe)
		v1root.POST("", handleEndpointProbe)
	}

	utils.LogDebugf("[Routes] Registered OpenAI endpoints: 18 core + %d stateful pass-through", statefulCount)
	return statefulCount
}
