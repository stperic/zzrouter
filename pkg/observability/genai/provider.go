// Package genai provides helpers that map zzrouter's internal vocabulary onto
// the OpenTelemetry GenAI semantic conventions
// (https://opentelemetry.io/docs/specs/semconv/gen-ai/).
//
// The helpers exist so the rest of the codebase keeps using zzrouter's
// internal provider keys and route paths; only this package knows about the
// OTel canonical names.
package genai

import (
	"strings"

	"go.opentelemetry.io/otel/semconv/v1.40.0/genaiconv"
)

// ProviderName maps a zzrouter internal provider key to its
// `gen_ai.provider.name` value as a typed OTel semconv attribute.
//
// For OTel-defined providers (cloud routes) this returns the canonical OTel
// value. For local backends OTel does not yet enumerate, it returns a stable
// lowercase custom value. Unknown keys are passed through after lowercasing
// and trimming so that the attribute is always populated and stable.
func ProviderName(internal string) genaiconv.ProviderNameAttr {
	key := strings.ToLower(strings.TrimSpace(internal))
	if v, ok := providerNames[key]; ok {
		return v
	}
	return genaiconv.ProviderNameAttr(key)
}

// providerNames is the single source of truth for provider mapping. Keep in
// sync with docs/plan_observability_dashboard_parity.md "Provider-name
// mapping for `gen_ai.provider.name`".
var providerNames = map[string]genaiconv.ProviderNameAttr{
	// Cloud routes — OTel canonical values.
	"openai":          genaiconv.ProviderNameOpenAI,
	"azure-openai":    genaiconv.ProviderNameAzureAIOpenAI,
	"azure_openai":    genaiconv.ProviderNameAzureAIOpenAI,
	"azure-inference": genaiconv.ProviderNameAzureAIInference,
	"azure_inference": genaiconv.ProviderNameAzureAIInference,
	"anthropic":       genaiconv.ProviderNameAnthropic,
	"bedrock":         genaiconv.ProviderNameAWSBedrock,
	"aws-bedrock":     genaiconv.ProviderNameAWSBedrock,
	"cohere":          genaiconv.ProviderNameCohere,
	"deepseek":        genaiconv.ProviderNameDeepseek,
	"gemini":          genaiconv.ProviderNameGCPGemini,
	"vertex":          genaiconv.ProviderNameGCPVertexAI,
	"vertex-ai":       genaiconv.ProviderNameGCPVertexAI,
	"vertex_ai":       genaiconv.ProviderNameGCPVertexAI,
	"groq":            genaiconv.ProviderNameGroq,
	"mistral":         genaiconv.ProviderNameMistralAI,
	"mistral-ai":      genaiconv.ProviderNameMistralAI,
	"mistral_ai":      genaiconv.ProviderNameMistralAI,
	"perplexity":      genaiconv.ProviderNamePerplexity,
	"xai":             genaiconv.ProviderNameXAI,
	"x-ai":            genaiconv.ProviderNameXAI,
	"x_ai":            genaiconv.ProviderNameXAI,
	"watsonx":         genaiconv.ProviderNameIBMWatsonxAI,
	"ibm-watsonx":     genaiconv.ProviderNameIBMWatsonxAI,

	// Local routes — custom (no OTel canonical). Stable lowercase keys.
	"vllm":      "vllm",
	"llama.cpp": "llama_cpp",
	"llama_cpp": "llama_cpp",
	"llamacpp":  "llama_cpp",
	"mlx":       "mlx",
	"ollama":    "ollama",
}
