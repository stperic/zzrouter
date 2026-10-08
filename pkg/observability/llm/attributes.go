// Package llm provides LLM-specific observability components.
package llm

import (
	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/semconv/v1.40.0/genaiconv"

	"github.com/stperic/zzrouter/pkg/observability/genai"
)

// Semantic attribute keys for LLM span attributes.
//
// The inference-span attributes are OpenTelemetry GenAI semantic-convention
// keys (gen_ai.* / server.* / error.*) so that any OTel-aware backend
// auto-renders zzrouter spans without per-attribute mapping. Concepts OTel
// does not define — model-load timing, instance metadata, internal routing
// decisions — keep their existing zzrouter-namespaced llm.* keys.
const (
	// Inference-span attributes (OTel GenAI + general semconv).
	ModelNameKey     = semconv.GenAIRequestModelKey
	ModelProviderKey = semconv.GenAIProviderNameKey
	RequestTypeKey   = semconv.GenAIOperationNameKey

	// Response and server attributes set after dispatch picks a backend.
	ResponseModelKey     = semconv.GenAIResponseModelKey
	ResponseIDKey        = semconv.GenAIResponseIDKey
	ServerAddressKey     = semconv.ServerAddressKey
	ServerPortKey        = semconv.ServerPortKey
	ErrorTypeKey         = semconv.ErrorTypeKey
	UsageInputTokensKey  = semconv.GenAIUsageInputTokensKey
	UsageOutputTokensKey = semconv.GenAIUsageOutputTokensKey

	// Streaming flag — OTel does not define a span attribute for streaming
	// state; kept under the zzrouter llm.* namespace.
	RequestStreamKey = attribute.Key("llm.request.stream")

	// Model-load timing — no OTel canonical exists; zzrouter-namespaced.
	ModelLoadTimeKey = attribute.Key("llm.model_load_time_ms")

	// Instance attributes — zzrouter-internal, no OTel canonical.
	InstanceIDKey   = attribute.Key("llm.instance.id")
	InstancePortKey = attribute.Key("llm.instance.port")

	// Routing attributes — zzrouter-internal, no OTel canonical.
	RoutingDecisionKey = attribute.Key("llm.routing.decision")
	RoutingNodeKey     = attribute.Key("llm.routing.host")
)

// Request types for the RequestTypeKey attribute.
const (
	RequestTypeChat       = "chat"
	RequestTypeCompletion = "completion"
	RequestTypeEmbedding  = "embedding"
	RequestTypeGenerate   = "generate"
)

// Routing decisions for the RoutingDecisionKey attribute.
const (
	RoutingDecisionLocal     = "local"
	RoutingDecisionRemote    = "remote"
	RoutingDecisionBroadcast = "broadcast"
	RoutingDecisionLoadFirst = "load_first"
	RoutingDecisionFallback  = "fallback"
)

// ModelName returns an attribute for the request model
// (`gen_ai.request.model`). Used for both inference and model-load spans —
// in the load case, "the model being prepared".
//
// Caller-controlled input passes through genai.GateModelLabel — when
// an allowlist is configured, names that fail the check collapse to
// UnknownModelLabel. The gate fires once at the helper rather than at
// every call site, so the closure of "everywhere a model name lands
// on a span/metric" is governed by one chokepoint.
func ModelName(name string) attribute.KeyValue {
	return ModelNameKey.String(genai.GateModelLabel(name))
}

// ModelProvider returns an attribute for the model provider
// (`gen_ai.provider.name`). The input is mapped through
// genai.ProviderName, so callers that pass zzrouter-internal provider keys
// (`vllm`, `azure-openai`, ...) produce the OTel canonical value
// (`vllm`, `azure.ai.openai`, ...) on the span.
func ModelProvider(provider string) attribute.KeyValue {
	return ModelProviderKey.String(string(genai.ProviderName(provider)))
}

// RequestType returns an attribute for the inference operation
// (`gen_ai.operation.name`). The string is translated to the OTel canonical
// closed-enum value via RequestTypeToOperation. Unknown inputs map to the
// empty string so the span and metric side agree — passing the raw input
// through would let user-controlled cardinality leak into a closed-enum
// label dimension.
func RequestType(reqType string) attribute.KeyValue {
	return RequestTypeKey.String(string(RequestTypeToOperation(reqType)))
}

// Operation returns the typed `gen_ai.operation.name` attribute. Prefer
// this over RequestType when the caller already holds a typed value.
func Operation(op genaiconv.OperationNameAttr) attribute.KeyValue {
	return RequestTypeKey.String(string(op))
}

// RequestStream returns the zzrouter-namespaced streaming flag attribute.
// No OTel GenAI canonical exists for this concept.
func RequestStream(streaming bool) attribute.KeyValue {
	return RequestStreamKey.Bool(streaming)
}

// ResponseModel returns the `gen_ai.response.model` attribute — the model
// the upstream backend reported actually using. Distinct from request.model
// when model-group routing resolves a group name to a concrete deployment.
//
// Gated through genai.GateModelLabel so an upstream that auto-versions
// to an unbounded set of model identifiers (or a misbehaving backend
// echoing arbitrary strings) can't explode the response.model
// dimension.
func ResponseModel(name string) attribute.KeyValue {
	return ResponseModelKey.String(genai.GateModelLabel(name))
}

// ResponseID returns the `gen_ai.response.id` attribute — the upstream's
// response identifier (e.g. OpenAI's `id` field).
func ResponseID(id string) attribute.KeyValue {
	return ResponseIDKey.String(id)
}

// ServerAddress returns the `server.address` attribute — the upstream
// backend host the request was dispatched to.
func ServerAddress(addr string) attribute.KeyValue {
	return ServerAddressKey.String(addr)
}

// ServerPort returns the `server.port` attribute — paired with
// ServerAddress when the backend uses a non-default port.
func ServerPort(port int) attribute.KeyValue {
	return ServerPortKey.Int(port)
}

// ErrorType returns the `error.type` attribute. Caller-side closed-enum
// values (e.g. upstream_rate_limited, model_not_found) match the metric-side
// taxonomy enforced via SetErrorFromStatus / pre-backend tagging.
func ErrorType(errType string) attribute.KeyValue {
	return ErrorTypeKey.String(errType)
}

// UsageInputTokens returns the `gen_ai.usage.input_tokens` attribute.
func UsageInputTokens(n int64) attribute.KeyValue {
	return UsageInputTokensKey.Int64(n)
}

// UsageOutputTokens returns the `gen_ai.usage.output_tokens` attribute.
func UsageOutputTokens(n int64) attribute.KeyValue {
	return UsageOutputTokensKey.Int64(n)
}

// ModelLoadTime returns an attribute for model load time in milliseconds.
func ModelLoadTime(ms float64) attribute.KeyValue {
	return ModelLoadTimeKey.Float64(ms)
}

// InstanceID returns an attribute for the instance ID.
func InstanceID(id string) attribute.KeyValue {
	return InstanceIDKey.String(id)
}

// InstancePort returns an attribute for the instance port.
func InstancePort(port int) attribute.KeyValue {
	return InstancePortKey.Int(port)
}

// RoutingDecision returns an attribute for the routing decision.
func RoutingDecision(decision string) attribute.KeyValue {
	return RoutingDecisionKey.String(decision)
}

// RoutingNode returns an attribute for the routing host.
func RoutingNode(host string) attribute.KeyValue {
	return RoutingNodeKey.String(host)
}
