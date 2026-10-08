// OpenAI-compatible per-model capabilities derivation for /v1/models.
//
// # Problem
//
// Agent harnesses and routing layers that point at zzRouter need to know
// which feature a given model supports before they send it a request.
// OpenAI's own /v1/models payload is sparse — it only carries
// {id, object, created, owned_by} — so clients today hard-code
// per-model behavior based on the `id` prefix. That pattern does not
// survive well on a server like zzRouter where models come from many
// providers with different capability profiles.
//
// # Design
//
// The /v1/models response adds a `capabilities` object whose shape is
// pinned in pkg/protocol/openai/vocab.go (and published in openapi.yaml
// via the OpenAIModelCapabilities schema). The shape is a closed dict
// of boolean flags plus a single `max_context_tokens` integer:
//
//	{
//	  "chat":               bool,
//	  "completions":        bool,
//	  "embeddings":         bool,
//	  "tools":              bool,
//	  "vision":             bool,
//	  "stream":             bool,
//	  "json_mode":          bool,
//	  "max_context_tokens": int  // 0 = unknown
//	}
//
// The dict shape is deliberately exhaustive: every defined capability
// is always emitted with an explicit value, so clients can branch on
// `caps.tools` without null-checking. New capabilities are added by
// extending the struct and bumping pkg/protocol/openai/SpecVersion.
//
// # Derivation
//
// Capabilities are derived from the provider registration and the
// model name. The rules are deliberately conservative — a false
// negative (claiming a feature is absent when it works) is always
// preferable to a false positive (claiming a feature works when it
// crashes). Rules:
//
//   - Protocol "openai": chat, completions, stream, json_mode all true.
//     tools defaults to true; overridden off for providers that have
//     not been verified to handle tool calls end-to-end.
//   - Protocol "ollama": chat and stream true; completions and
//     tools true. json_mode is false (Ollama does not accept
//     response_format).
//   - Model reports an encoder-only architecture (family "bert",
//     "nomic-bert", ...) or its name contains an embedding marker
//     ("embed", "bge", "e5-", "gte-", "minilm") → embeddings=true,
//     chat/completions/tools=false. The architecture is the stronger
//     signal and is checked even when the name says nothing.
//   - Node-reported vision presence sets vision=true. Legacy providers
//     without a feature declaration retain the model-name fallback.
//
// max_context_tokens is 0 (unknown) in the first cut. A future
// revision will read it from a `context_tokens` field in apps.yaml.
//
// The derivation function is a pure function of (cache.CachedModel, AppProtocol).
// It has no side effects and is safe to call from the hot path of
// handleModels.

package server

import (
	"strings"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/model/cache"
)

// OpenAIModelCapabilities is the closed dict of per-model feature
// flags returned by /v1/models. Field names and JSON tags MUST stay
// in sync with the OpenAIModelCapabilities schema in openapi.yaml —
// the drift-check test enforces this.
type OpenAIModelCapabilities struct {
	Chat             bool `json:"chat"`
	Completions      bool `json:"completions"`
	Embeddings       bool `json:"embeddings"`
	Rerank           bool `json:"rerank"`
	Tools            bool `json:"tools"`
	Vision           bool `json:"vision"`
	Stream           bool `json:"stream"`
	JSONMode         bool `json:"json_mode"`
	MaxContextTokens int  `json:"max_context_tokens,omitempty"`
}

// OpenAIZZRouterVendor is the zzRouter-specific extension added to
// /v1/models entries. The top-level `zzrouter` key is documented in
// openapi.yaml as a vendor extension clients may safely ignore.
type OpenAIZZRouterVendor struct {
	Node     string `json:"node,omitempty"`
	Provider string `json:"provider,omitempty"`
	Protocol string `json:"protocol,omitempty"`
}

// deriveCapabilities computes the OpenAIModelCapabilities for one
// cache.CachedModel by looking up its provider in appsConfig and applying
// the protocol-specific rule set documented at the top of this file.
//
// A nil appsConfig, a missing provider entry, or an empty protocol
// all result in a minimal "chat-only" capability set — that is the
// safest default because /v1/chat/completions is the endpoint every
// OpenAI client hits first.
func deriveCapabilities(m *cache.CachedModel, appsConfig *pkgConfig.AppsConfig) OpenAIModelCapabilities {
	caps := OpenAIModelCapabilities{
		// Conservative baseline: chat works for every registered
		// model. Everything else is opt-in.
		Chat:   true,
		Stream: true,
	}
	if m == nil {
		return caps
	}

	protocol := lookupProtocol(m, appsConfig)
	nameLower := strings.ToLower(m.Name)

	// Rerank models are a separate universe from chat models: they
	// accept a query + candidate list and return relevance scores, not
	// chat turns. Checked before the embedding rules because rerank
	// models are usually built on an encoder-only architecture too, and
	// the name is the only thing that separates the two.
	if isRerankName(nameLower) {
		return OpenAIModelCapabilities{Rerank: true}
	}

	// Embedding models are likewise a separate universe — emitting
	// chat=true on a bge-small-en would be a lie that causes clients to
	// get an opaque 400 from the backend. The architecture is checked
	// alongside the name because it is the stronger signal: a model can
	// be named to no convention at all (all-minilm) and still report an
	// encoder-only family that cannot generate a chat turn.
	if isEmbeddingName(nameLower) || isEncoderOnlyArchitecture(m) {
		return OpenAIModelCapabilities{Embeddings: true}
	}

	switch protocol {
	case string(pkgConfig.ProtocolOpenAI):
		caps.Completions = true
		caps.Tools = true
		caps.JSONMode = true
	case string(pkgConfig.ProtocolOllama):
		caps.Completions = true
		// Tool support in Ollama is model-dependent; claim it by
		// default so tool-aware clients can try, and rely on
		// backend error propagation to flag failures.
		caps.Tools = true
		caps.JSONMode = false
	default:
		// Unknown protocol: fall back to chat-only.
	}

	caps.Vision = m.FeatureState("vision") == "present"
	if !caps.Vision && m.FeatureState("vision") == "unknown" && !declaresFeatures(m.Provider, appsConfig) {
		caps.Vision = isVisionName(nameLower)
	}

	return caps
}

// lookupProtocol finds the app protocol for a cache.CachedModel by reading
// the provider entry out of appsConfig. Returns an empty string when
// appsConfig is nil or the provider is not registered.
func lookupProtocol(m *cache.CachedModel, appsConfig *pkgConfig.AppsConfig) string {
	if m == nil || m.Provider == "" {
		return ""
	}
	svc, ok := appsConfig.LookupApp(m.Provider)
	if !ok {
		return ""
	}
	return string(svc.Protocol)
}

// isEmbeddingName reports whether a lowercased model name matches any
// of the common embedding-model conventions. Kept deliberately small
// — the risk of a false positive (incorrectly disabling chat on a
// model whose name happens to contain one of these substrings) is
// higher than the risk of a false negative (failing to expose
// embeddings=true on a less-common model).
func isEmbeddingName(lower string) bool {
	markers := []string{
		"embed",
		"bge-",
		"e5-",
		"gte-",
		"-embedding",
		"minilm",
		"mpnet",
		"paraphrase-",
	}
	for _, marker := range markers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// encoderOnlyArchitectures are model families with no decoder stack.
// They physically cannot generate a chat turn, so a model reporting one
// is an embedding or rerank model whatever it happens to be called.
//
// Kept as a closed set for the same reason the name markers are: a
// false positive here disables chat on a working model, which is worse
// than missing a less common family.
var encoderOnlyArchitectures = map[string]struct{}{
	"bert":        {},
	"nomic-bert":  {},
	"nomic_bert":  {},
	"distilbert":  {},
	"roberta":     {},
	"xlm-roberta": {},
	"mpnet":       {},
	"jina-bert":   {},
	"gte":         {},
}

// isEncoderOnlyArchitecture reports whether the provider described this
// model as an encoder-only family. Ollama populates Details from the
// model card, so this is the provider's own answer rather than an
// inference drawn from the name.
func isEncoderOnlyArchitecture(m *cache.CachedModel) bool {
	if m == nil || m.Details == nil {
		return false
	}
	if family, ok := m.Details["family"].(string); ok && isEncoderOnlyFamily(family) {
		return true
	}
	// Multi-architecture models list every family they embed; any
	// encoder-only entry is enough, since a decoder model never lists one.
	families, ok := m.Details["families"].([]any)
	if !ok {
		return false
	}
	for _, f := range families {
		if name, ok := f.(string); ok && isEncoderOnlyFamily(name) {
			return true
		}
	}
	return false
}

// isEncoderOnlyFamily normalizes an architecture name before lookup.
func isEncoderOnlyFamily(family string) bool {
	_, ok := encoderOnlyArchitectures[strings.ToLower(strings.TrimSpace(family))]
	return ok
}

// isRerankName reports whether a lowercased model name matches any of
// the common rerank-model conventions. The "rerank" substring is the
// dominant naming pattern across Cohere, Voyage, Jina, and open-source
// rerank model families; additional markers can be added here as new
// providers register rerank-capable models under different names.
func isRerankName(lower string) bool {
	markers := []string{
		"rerank",
		"reranker",
	}
	for _, marker := range markers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// isVisionName reports whether a lowercased model name matches any of
// the common multimodal/vision-model conventions.
func isVisionName(lower string) bool {
	markers := []string{
		"vision",
		"-vl-",
		"llava",
		"multimodal",
	}
	for _, marker := range markers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// declaresFeatures reports whether provider's config has a features block,
// which makes its feature evidence the only source for capabilities.
func declaresFeatures(provider string, appsConfig *pkgConfig.AppsConfig) bool {
	if appsConfig == nil {
		return false
	}
	svc, ok := appsConfig.LookupApp(provider)
	return ok && svc.Features != nil
}
