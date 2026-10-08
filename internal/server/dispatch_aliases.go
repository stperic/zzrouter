// package server — type re-exports from pkg/dispatch/adapter.
//
// These are zero-cost Go type aliases. ShowModelResponse, ModelRouteInfo,
// the ProtocolAdapter interface + RunningInstance moved to
// pkg/dispatch/adapter when dispatch-pipeline extraction (Slice 6)
// began. The aliases keep server-local call-sites unchanged —
// model_service, openai_adapter, ollama_adapter, and their tests reference
// these names dozens of times.
//
// Concrete adapters (OpenAIAdapter, OllamaAdapter) continue to live in
// server; they satisfy adapter.ProtocolAdapter implicitly via Go's
// structural typing.
//
// Cleanup trigger: if callers outside the server package start importing
// pkg/dispatch/adapter directly (non-test), delete the aliases and inline
// the adapter.* names at the remaining server call-sites in one sweep.

package server

import (
	"github.com/stperic/zzrouter/pkg/dispatch/adapter"
	"github.com/stperic/zzrouter/pkg/dispatch/affinity"
	"github.com/stperic/zzrouter/pkg/dispatch/normalizer"
	"github.com/stperic/zzrouter/pkg/dispatch/wire"
)

// ===== pkg/dispatch/adapter re-exports =====

type (
	ShowModelResponse           = adapter.ShowModelResponse
	ModelRouteInfo              = adapter.ModelRouteInfo
	ProtocolAdapter             = adapter.ProtocolAdapter
	OptionalRunningModelAdapter = adapter.OptionalRunningModelAdapter
	RunningInstance             = adapter.RunningInstance
)

// ===== pkg/dispatch/normalizer re-exports =====

type (
	ResponseNormalizers = normalizer.Registry
	BodyNormalizer      = normalizer.BodyNormalizer
	StreamNormalizer    = normalizer.StreamNormalizer
)

var (
	NewResponseNormalizers = normalizer.NewRegistry

	// Function-value aliases preserve server-local call-site spelling.
	// Callers pre-extraction used `applyBodyNormalizer`, `newNormalizingReader`,
	// `composeStreamTransforms`; the generic helpers now live in
	// pkg/dispatch/normalizer with capitalized names.
	applyBodyNormalizer     = normalizer.ApplyBody
	newNormalizingReader    = normalizer.NewStreamReader
	composeStreamTransforms = normalizer.ComposeStream
)

// ===== pkg/dispatch/affinity re-exports =====

type responseAffinity = affinity.Affinity

var newResponseAffinity = affinity.New

// ===== pkg/dispatch/wire re-exports =====
//
// Only names consumed by 3+ server-side call-sites are re-exported; the
// rest were inlined when Slice 6 commit 4 ran — grep showed zero or
// one external caller and the import churn wasn't worth the alias. If a
// new caller arrives and the import noise grows, reinstate the alias
// here rather than peppering `wire.X` everywhere.

var (
	isStreamingResponse        = wire.IsStreaming
	mergeUpstreamHeaders       = wire.MergeUpstreamHeaders
	sendSSEComment             = wire.SendSSEComment
	rewriteModelInBody         = wire.RewriteModelInBody
	forceUsageReporting        = wire.ForceUsageReporting
	copyStreamWithMetrics      = wire.CopyWithMetrics
	captureNonStreamingMetrics = wire.CaptureNonStreamingMetrics
)
