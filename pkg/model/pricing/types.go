// Package pricing provides model pricing data from external sources (LiteLLM).
// It fetches, caches, and serves per-token pricing for LLM models.
package pricing

import "time"

// ModelPricing holds pricing information for a single model.
type ModelPricing struct {
	InputCostPerToken  float64 `json:"input_cost_per_token"`
	OutputCostPerToken float64 `json:"output_cost_per_token"`

	MaxInputTokens  int `json:"max_input_tokens,omitempty"`
	MaxOutputTokens int `json:"max_output_tokens,omitempty"`
	MaxTokens       int `json:"max_tokens,omitempty"`

	Provider string `json:"litellm_provider"`
	Mode     string `json:"mode"`

	SupportsVision            bool `json:"supports_vision,omitempty"`
	SupportsFunctionCalling   bool `json:"supports_function_calling,omitempty"`
	SupportsParallelToolCalls bool `json:"supports_parallel_tool_calls,omitempty"`

	CacheCreationCostPerToken float64 `json:"cache_creation_input_token_cost,omitempty"`
	CacheReadCostPerToken     float64 `json:"cache_read_input_token_cost,omitempty"`

	OutputCostPerReasoningToken float64 `json:"output_cost_per_reasoning_token,omitempty"`

	InputCostPerAudioToken  float64 `json:"input_cost_per_audio_token,omitempty"`
	OutputCostPerAudioToken float64 `json:"output_cost_per_audio_token,omitempty"`

	OutputCostPerImage float64 `json:"output_cost_per_image,omitempty"`

	DeprecationDate string `json:"deprecation_date,omitempty"`
}

func (p *ModelPricing) InputCostPer1M() float64 {
	return p.InputCostPerToken * 1_000_000
}

func (p *ModelPricing) OutputCostPer1M() float64 {
	return p.OutputCostPerToken * 1_000_000
}

// ContextWindow returns MaxInputTokens if set, otherwise MaxTokens.
func (p *ModelPricing) ContextWindow() int {
	if p.MaxInputTokens > 0 {
		return p.MaxInputTokens
	}
	return p.MaxTokens
}

func (p *ModelPricing) HasPricing() bool {
	return p.InputCostPerToken > 0 || p.OutputCostPerToken > 0
}

// CacheMetadata holds metadata about the on-disk pricing cache.
type CacheMetadata struct {
	ETag         string    `json:"etag,omitempty"`
	LastModified string    `json:"last_modified,omitempty"`
	ContentHash  string    `json:"content_hash"`
	FetchedAt    time.Time `json:"fetched_at"`
	ModelCount   int       `json:"model_count"`
}

// DefaultSource is the default URL for LiteLLM pricing data.
const DefaultSource = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"

// DefaultRefreshInterval is the default refresh interval.
const DefaultRefreshInterval = 24 * time.Hour

// Metadata keys used when enriching cloud /v1/models responses with the
// derived capability/input-type fields. Values are kept as the upstream
// `litellm_*` strings so on-the-wire JSON keys remain stable for any
// downstream consumer that already reads them.
const (
	MetadataKeyCapabilities = "litellm_capabilities"
	MetadataKeyInputTypes   = "litellm_input_types"
)
