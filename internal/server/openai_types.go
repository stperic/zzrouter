// Request/response types for the OpenAI compatibility surface (/v1/*).
// JSON tags MUST marshal to bytes matching the OpenAI API spec — clients
// rely on exact field names, field order is irrelevant, omitempty is used
// where the spec allows fields to be absent.
package server

import "encoding/json"

// ---------- Request types ----------

// OpenAIChatMessage is one entry in a chat request's messages array. Content
// is json.RawMessage so both plain strings and vision-model content arrays
// pass through to the upstream provider without re-marshaling.
//
// Required-field validation is enforced manually in the handlers — we use
// json.Unmarshal (not gin's ShouldBindJSON) so the binding: struct tags that
// go-playground/validator reads would be dead code.
type OpenAIChatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// OpenAIChatCompletionRequest is the subset of the chat completions body that
// zzRouter needs for routing and validation. The raw body is forwarded to the
// upstream provider verbatim, so new/unknown fields are preserved.
type OpenAIChatCompletionRequest struct {
	Model       string              `json:"model"`
	Messages    []OpenAIChatMessage `json:"messages"`
	MaxTokens   *int                `json:"max_tokens,omitempty"`
	Temperature *float64            `json:"temperature,omitempty"`
	Stream      bool                `json:"stream,omitempty"`
}

// OpenAICompletionRequest is the legacy /v1/completions request.
type OpenAICompletionRequest struct {
	Model     string `json:"model"`
	Prompt    string `json:"prompt"`
	MaxTokens *int   `json:"max_tokens,omitempty"`
}

// OpenAIEmbeddingsRequest is the /v1/embeddings request.
type OpenAIEmbeddingsRequest struct {
	Model string `json:"model"`
	Input any    `json:"input"`
}

// OpenAIModelRoutingRequest is the minimal subset of any /v1/* body zzRouter
// needs to route a request to the backend holding the named model. Used for
// endpoints we don't parse fully (/v1/images/*, /v1/moderations).
type OpenAIModelRoutingRequest struct {
	Model string `json:"model"`
}

// ---------- Response types ----------

// OpenAIModelObject is a single entry in /v1/models (and the body of
// /v1/models/:model). In addition to the OpenAI base shape it carries
// two zzRouter extensions:
//
//   - Capabilities — a closed dict of per-model feature flags
//     (chat/completions/embeddings/tools/vision/stream/json_mode +
//     max_context_tokens). Derived at list time from the provider
//     registration and model-name heuristics; see
//     openai_capabilities.go.
//
//   - ZZRouter — a vendor extension object identifying the serving
//     node, provider, and protocol. Top-level vendor keys are
//     ignored by OpenAI SDKs but understood by zzRouter-aware
//     agent harnesses.
//
// Both fields use `omitempty` so handlers that do not populate them
// (e.g. the handshake model) produce a response byte-identical to
// OpenAI's sparse payload.
type OpenAIModelObject struct {
	ID           string                   `json:"id"`
	Object       string                   `json:"object"`
	Created      int64                    `json:"created"`
	OwnedBy      string                   `json:"owned_by"`
	Capabilities *OpenAIModelCapabilities `json:"capabilities,omitempty"`
	ZZRouter     *OpenAIZZRouterVendor    `json:"zzrouter,omitempty"`
	// Endpoints lists the /v1/* endpoints this model serves
	// ("chat", "completions", "embeddings", "rerank"). Derived from
	// Capabilities so agents can branch without reading capability bools.
	Endpoints []string `json:"endpoints,omitempty"`
	// ServedBy declares whether the request will be dispatched to a cloud
	// provider ("cloud") or to a local instance ("local"). Always present
	// on entries with at least one inference endpoint, so agents can
	// branch on routing without inspecting Runtime semantics.
	ServedBy string `json:"served_by,omitempty"`
	// Runtime carries per-node hot-state (status/node/port). Emitted only
	// for local-served models — a cloud-routed model has no local
	// instance lifecycle, and emitting Runtime would invite agents to
	// assume hot/cold matters when it doesn't.
	Runtime *OpenAIModelRuntime `json:"runtime,omitempty"`
	// AliasOf is set on alias entries pointing back to the canonical
	// model id. Lets clients dedupe when SourceID-style aliases are
	// emitted as separate /v1/models entries.
	AliasOf string `json:"alias_of,omitempty"`
	// VariantOf is set on a variant: its own name over the weights of the
	// model this names, with its own launch settings or request defaults.
	// GET /zzrouter/v1/providers/{provider}/resolved?model={id} says what
	// those are.
	VariantOf string `json:"variant_of,omitempty"`
}

// OpenAIModelRuntime carries the hot-state view of a model. Status is
// one of "cold" (deployed, no instance), "loading" (instance starting),
// "running" (serving requests). Cloud-backed models always report
// "running" since the upstream is the runtime.
type OpenAIModelRuntime struct {
	Status string `json:"status"`
	Node   string `json:"node,omitempty"`
	Port   int    `json:"port,omitempty"`
}

// OpenAIModelListResponse is the /v1/models envelope.
type OpenAIModelListResponse struct {
	Object string              `json:"object"`
	Data   []OpenAIModelObject `json:"data"`
}

// OpenAIModelDeleteResponse is the DELETE /v1/models/:id envelope.
type OpenAIModelDeleteResponse struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Deleted bool   `json:"deleted"`
}

// OpenAIChatCompletionMessage is the assistant message inside a non-streaming
// chat completion response.
type OpenAIChatCompletionMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// OpenAIChatCompletionChoice is one entry in the choices array of a
// non-streaming chat completion response.
type OpenAIChatCompletionChoice struct {
	Index        int                         `json:"index"`
	Message      OpenAIChatCompletionMessage `json:"message"`
	FinishReason string                      `json:"finish_reason"`
}

// OpenAIUsage mirrors the usage block the spec attaches to completions and
// embeddings responses.
type OpenAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// OpenAIChatCompletionResponse is a full non-streaming chat completion body.
type OpenAIChatCompletionResponse struct {
	ID      string                       `json:"id"`
	Object  string                       `json:"object"`
	Created int64                        `json:"created"`
	Model   string                       `json:"model"`
	Choices []OpenAIChatCompletionChoice `json:"choices"`
	Usage   OpenAIUsage                  `json:"usage"`
}

// OpenAIChatCompletionDelta is the delta block inside a streaming chunk.
// Fields are all optional — a chunk may carry just the role, just content,
// or an empty delta on the final finish chunk.
type OpenAIChatCompletionDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

// OpenAIChatCompletionChunkChoice is one choice in a streaming chunk. The
// finish_reason field is nullable in the spec, so the pointer is required
// to distinguish "still streaming" (null) from "stopped" ("stop").
type OpenAIChatCompletionChunkChoice struct {
	Index        int                       `json:"index"`
	Delta        OpenAIChatCompletionDelta `json:"delta"`
	FinishReason *string                   `json:"finish_reason"`
}

// OpenAIChatCompletionChunk is one SSE data chunk in a streaming chat
// completion response.
type OpenAIChatCompletionChunk struct {
	ID      string                            `json:"id"`
	Object  string                            `json:"object"`
	Created int64                             `json:"created"`
	Model   string                            `json:"model"`
	Choices []OpenAIChatCompletionChunkChoice `json:"choices"`
}
