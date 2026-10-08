// Package modelregistry provides memory estimation for LLM models.
// This file contains architecture-aware functions to estimate model memory requirements
// based on model type, architecture, parameters, quantization, and format.
package modelregistry

import (
	"regexp"
	"strconv"
	"strings"
)

// ============================================================================
// Model Architecture Types
// ============================================================================

// ModelArchitecture represents the type of model architecture
type ModelArchitecture string

const (
	// ArchDenseTransformer is a standard dense transformer (Llama, Mistral, etc.)
	ArchDenseTransformer ModelArchitecture = "dense_transformer"

	// ArchMoE is a Mixture of Experts model (Mixtral, DeepSeek V2/V3, etc.)
	ArchMoE ModelArchitecture = "moe"

	// ArchVisionLanguage is a Vision-Language model (LLaVA, Qwen-VL, etc.)
	ArchVisionLanguage ModelArchitecture = "vision_language"

	// ArchMultiModal is a multi-modal model with audio/video (Qwen2-Audio, etc.)
	ArchMultiModal ModelArchitecture = "multi_modal"

	// ArchEmbedding is an embedding model (nomic-embed, bge, etc.)
	ArchEmbedding ModelArchitecture = "embedding"

	// ArchCodeModel is a code-specialized model (often has larger context)
	ArchCodeModel ModelArchitecture = "code"

	// ArchUnknown is for models we can't classify
	ArchUnknown ModelArchitecture = "unknown"
)

// AttentionType represents the attention mechanism
type AttentionType string

const (
	// AttentionMHA is Multi-Head Attention (standard, full KV cache)
	AttentionMHA AttentionType = "mha"

	// AttentionGQA is Grouped-Query Attention (reduced KV cache)
	AttentionGQA AttentionType = "gqa"

	// AttentionMQA is Multi-Query Attention (minimal KV cache)
	AttentionMQA AttentionType = "mqa"
)

// ============================================================================
// Model Specification
// ============================================================================

// ModelSpec contains detailed specifications for a known model
type ModelSpec struct {
	// Basic info
	Name         string            // Model identifier pattern
	Architecture ModelArchitecture // Architecture type
	Attention    AttentionType     // Attention mechanism

	// Parameter counts (in billions)
	TotalParams  float64 // Total parameters
	ActiveParams float64 // Active parameters (for MoE, same as Total for dense)

	// For MoE models
	NumExperts    int // Total number of experts
	ActiveExperts int // Experts active per forward pass

	// For Vision-Language models
	VisionEncoderParams float64 // Vision encoder parameters (in billions)
	VisionEncoderType   string  // "vit", "siglip", "clip", "eva", etc.
	ImageResolution     int     // Max image resolution (e.g., 384, 448, 672)

	// Architecture details
	NumLayers        int // Number of transformer layers
	HiddenSize       int // Hidden dimension
	NumHeads         int // Number of attention heads
	NumKVHeads       int // Number of KV heads (for GQA)
	IntermediateSize int // FFN intermediate size
	VocabSize        int // Vocabulary size

	// Context and memory
	DefaultContextLength int     // Default context window
	MaxContextLength     int     // Maximum supported context
	KVCacheMultiplier    float64 // Multiplier for KV cache estimation (1.0 = standard)

	// Additional memory overhead (in MB)
	ActivationOverheadMB int64 // Activation memory overhead
	VisionOverheadMB     int64 // Vision processing overhead
}

// ============================================================================
// Memory Estimation Types
// ============================================================================

// ModelMemoryEstimate represents estimated memory requirements for a model
type ModelMemoryEstimate struct {
	// Model identification
	ModelName    string            `json:"model_name"`
	Format       string            `json:"format"`       // gguf, hf_transformers, mlx, etc.
	Quantization string            `json:"quantization"` // q4_k_m, q8_0, fp16, bf16, etc.
	Architecture ModelArchitecture `json:"architecture"` // dense_transformer, moe, vision_language, etc.

	// Model size info
	ParameterCount int64 `json:"parameter_count"` // Total parameters
	ActiveParams   int64 `json:"active_params"`   // Active parameters (for MoE)
	VisionParams   int64 `json:"vision_params"`   // Vision encoder parameters (for VLM)

	// Memory estimates (in MB)
	BaseMemoryMB        int64   `json:"base_memory_mb"`        // Model weights only
	VisionMemoryMB      int64   `json:"vision_memory_mb"`      // Vision encoder memory (for VLM)
	KVCachePerTokenMB   float64 `json:"kv_cache_per_token_mb"` // KV cache per token
	ActivationMemoryMB  int64   `json:"activation_memory_mb"`  // Activation/intermediate memory
	MinMemoryMB         int64   `json:"min_memory_mb"`         // Minimum to load model
	RecommendedMemoryMB int64   `json:"recommended_memory_mb"` // For typical context window
	MaxMemoryMB         int64   `json:"max_memory_mb"`         // For maximum context window

	// Context information
	DefaultContextLength int `json:"default_context_length"`
	MaxContextLength     int `json:"max_context_length"`

	// Estimation confidence (0.0 - 1.0)
	Confidence float64 `json:"confidence"`

	// Source of estimation
	EstimationSource string `json:"estimation_source"` // "known_model", "parsed", "heuristic"
}

// ============================================================================
// Quantization Multipliers
// ============================================================================

// QuantizationMultiplier maps quantization formats to memory multipliers
// Relative to FP16 (which is the baseline = 1.0)
var QuantizationMultiplier = map[string]float64{
	// Full precision
	"fp32": 2.0,
	"f32":  2.0,

	// Half precision (baseline)
	"fp16":  1.0,
	"f16":   1.0,
	"bf16":  1.0,
	"bfp16": 1.0,

	// 8-bit quantization
	"q8_0":  0.50,
	"q8_1":  0.50,
	"int8":  0.50,
	"i8":    0.50,
	"8bit":  0.50,
	"q8":    0.50,
	"iq8":   0.50,
	"iq8_s": 0.50,

	// 6-bit quantization
	"q6_k":  0.375,
	"iq6_k": 0.375,

	// 5-bit quantization
	"q5_0":   0.31,
	"q5_1":   0.31,
	"q5_k":   0.31,
	"q5_k_s": 0.31,
	"q5_k_m": 0.31,
	"iq5_k":  0.31,
	"iq5_s":  0.31,
	"iq5_m":  0.31,
	"iq5_xs": 0.31,

	// 4-bit quantization (most common for GGUF)
	"q4_0":   0.25,
	"q4_1":   0.25,
	"q4_k":   0.25,
	"q4_k_s": 0.25,
	"q4_k_m": 0.25,
	"int4":   0.25,
	"i4":     0.25,
	"4bit":   0.25,
	"q4":     0.25,
	"iq4_k":  0.25,
	"iq4_nl": 0.25,
	"iq4_xs": 0.25,

	// 3-bit quantization
	"q3_k":    0.19,
	"q3_k_s":  0.19,
	"q3_k_m":  0.19,
	"q3_k_l":  0.19,
	"iq3_k":   0.19,
	"iq3_s":   0.19,
	"iq3_m":   0.19,
	"iq3_xs":  0.19,
	"iq3_xxs": 0.19,

	// 2-bit quantization
	"q2_k":    0.125,
	"iq2_k":   0.125,
	"iq2_s":   0.125,
	"iq2_m":   0.125,
	"iq2_xs":  0.125,
	"iq2_xxs": 0.125,

	// 1-bit quantization
	"iq1_s": 0.0625,
	"iq1_m": 0.0625,
}

// ============================================================================
// Main Estimation Functions
// ============================================================================

// EstimateModelMemory estimates memory requirements for a model
// This is the primary function for architecture-aware memory estimation
func EstimateModelMemory(modelName, format, quantization string) *ModelMemoryEstimate {
	estimate := &ModelMemoryEstimate{
		ModelName:        modelName,
		Format:           format,
		Quantization:     quantization,
		EstimationSource: "heuristic",
		Confidence:       0.5,
		Architecture:     ArchUnknown,
	}

	// Try to find a known model specification
	spec := findModelSpec(modelName)

	if spec != nil {
		// Use known model specification
		estimate.Architecture = spec.Architecture
		estimate.EstimationSource = "known_model"
		estimate.Confidence = 0.95

		estimate.ParameterCount = int64(spec.TotalParams * 1e9)
		estimate.ActiveParams = int64(spec.ActiveParams * 1e9)
		estimate.VisionParams = int64(spec.VisionEncoderParams * 1e9)
		estimate.DefaultContextLength = spec.DefaultContextLength
		estimate.MaxContextLength = spec.MaxContextLength

		// Calculate memory based on architecture
		estimate.calculateMemoryFromSpec(spec, quantization)
	} else {
		// Fallback to heuristic estimation
		estimate.estimateFromName(modelName, quantization)
	}

	return estimate
}

// ============================================================================
// Memory Calculation Methods
// ============================================================================

// calculateMemoryFromSpec calculates memory using known model specification
func (e *ModelMemoryEstimate) calculateMemoryFromSpec(spec *ModelSpec, quantization string) {
	quant := normalizeQuantization(quantization)
	multiplier := getQuantizationMultiplier(quant)

	// Base memory calculation depends on architecture
	switch spec.Architecture {
	case ArchMoE:
		// MoE: All experts must be loaded, but activation memory is lower
		totalParamsBytes := spec.TotalParams * 1e9 * 2.0 * multiplier
		e.BaseMemoryMB = int64(totalParamsBytes / (1024 * 1024))
		// Activation memory based on active params
		e.ActivationMemoryMB = int64(spec.ActiveParams * 1e9 * 0.1 / (1024 * 1024)) // ~10% of active params

	case ArchVisionLanguage:
		// VLM: Language model + Vision encoder
		langParamsBytes := (spec.TotalParams - spec.VisionEncoderParams) * 1e9 * 2.0 * multiplier
		// Vision encoder typically runs in FP16 even when LLM is quantized
		visionParamsBytes := spec.VisionEncoderParams * 1e9 * 2.0
		e.BaseMemoryMB = int64(langParamsBytes / (1024 * 1024))
		e.VisionMemoryMB = int64(visionParamsBytes/(1024*1024)) + spec.VisionOverheadMB
		e.ActivationMemoryMB = spec.ActivationOverheadMB

	case ArchEmbedding:
		// Embedding models don't need KV cache
		totalParamsBytes := spec.TotalParams * 1e9 * 2.0 * multiplier
		e.BaseMemoryMB = int64(totalParamsBytes / (1024 * 1024))
		e.ActivationMemoryMB = 256 // Minimal activation memory

	default:
		// Dense transformer (standard)
		totalParamsBytes := spec.TotalParams * 1e9 * 2.0 * multiplier
		e.BaseMemoryMB = int64(totalParamsBytes / (1024 * 1024))
		e.ActivationMemoryMB = int64(spec.TotalParams * 0.05 * 1024) // ~5% of params in MB
	}

	// KV cache calculation
	if spec.Architecture != ArchEmbedding {
		// KV cache = 2 * num_layers * hidden_size * 2 bytes * kv_multiplier
		// Simplified: params_B * 0.0001 MB per token * kv_multiplier
		kvMultiplier := spec.KVCacheMultiplier
		if kvMultiplier == 0 {
			kvMultiplier = 1.0
		}
		e.KVCachePerTokenMB = spec.TotalParams * 0.0001 * kvMultiplier
	}

	// Calculate final memory estimates
	e.calculateFinalMemory(spec.DefaultContextLength, spec.MaxContextLength)
}

// estimateFromName estimates memory from model name (fallback)
func (e *ModelMemoryEstimate) estimateFromName(modelName string, quantization string) {
	lower := strings.ToLower(modelName)

	// Detect architecture from name patterns
	e.Architecture = detectArchitectureFromName(lower)

	// Parse parameter count
	params := parseParameterCount(modelName)
	if params == 0 {
		// Default to 7B if unknown
		params = 7_000_000_000
		e.Confidence = 0.3
	}

	e.ParameterCount = params
	e.ActiveParams = params

	// Adjust for detected architecture
	switch e.Architecture {
	case ArchVisionLanguage:
		// Assume ~10% vision encoder overhead
		e.VisionParams = int64(float64(params) * 0.1)
		e.VisionMemoryMB = int64(float64(e.VisionParams) * 2.0 / (1024 * 1024))
		e.VisionMemoryMB += 2048 // Additional vision processing overhead
		e.Confidence *= 0.8

	case ArchMoE:
		// MoE models typically have 8x params but only activate 2
		e.ActiveParams = params / 4
		e.Confidence *= 0.7
	}

	// Calculate base memory
	quant := normalizeQuantization(quantization)
	multiplier := getQuantizationMultiplier(quant)
	bytesPerParam := 2.0 * multiplier
	e.BaseMemoryMB = int64(float64(params) * bytesPerParam / (1024 * 1024))

	// KV cache estimation
	kvMultiplier := 0.25 // Assume GQA as default
	e.KVCachePerTokenMB = float64(params) / 1e9 * 0.0001 * kvMultiplier

	// Context defaults based on architecture
	defaultCtx := 4096
	maxCtx := 8192
	if e.Architecture == ArchCodeModel {
		defaultCtx = 16384
		maxCtx = 32768
	}
	e.DefaultContextLength = defaultCtx
	e.MaxContextLength = maxCtx

	e.calculateFinalMemory(defaultCtx, maxCtx)
}

// calculateFinalMemory calculates min/recommended/max memory
func (e *ModelMemoryEstimate) calculateFinalMemory(defaultCtx, maxCtx int) {
	// Total base = model weights + vision (if VLM) + activation overhead
	totalBase := e.BaseMemoryMB + e.VisionMemoryMB + e.ActivationMemoryMB

	// Minimum memory (just to load)
	e.MinMemoryMB = int64(float64(totalBase) * 1.05)

	// Recommended memory (default context)
	kvCacheDefault := int64(e.KVCachePerTokenMB * float64(defaultCtx))
	e.RecommendedMemoryMB = totalBase + kvCacheDefault + 512 // 512MB overhead

	// Maximum memory (max context)
	kvCacheMax := int64(e.KVCachePerTokenMB * float64(maxCtx))
	e.MaxMemoryMB = totalBase + kvCacheMax + 1024 // 1GB overhead

	// Ensure ordering
	if e.MinMemoryMB >= e.RecommendedMemoryMB {
		e.MinMemoryMB = int64(float64(e.RecommendedMemoryMB) * 0.9)
	}
	if e.RecommendedMemoryMB >= e.MaxMemoryMB {
		e.MaxMemoryMB = e.RecommendedMemoryMB + 2048
	}
}

// ============================================================================
// Helper Functions
// ============================================================================

// findModelSpec finds a known model specification by name
func findModelSpec(modelName string) *ModelSpec {
	lower := strings.ToLower(modelName)

	// Find the longest matching key (most specific match wins)
	var bestMatch *ModelSpec
	bestMatchLen := 0

	for key, spec := range KnownModels {
		if strings.Contains(lower, key) && len(key) > bestMatchLen {
			bestMatch = spec
			bestMatchLen = len(key)
		}
	}

	return bestMatch
}

// detectArchitectureFromName detects model architecture from name patterns
func detectArchitectureFromName(name string) ModelArchitecture {
	// Vision-Language patterns
	vlPatterns := []string{
		"vl", "vision", "llava", "cogvlm", "internvl",
		"qwen-vl", "qwen2-vl", "qwen2.5-vl",
		"yi-vl", "phi-3-vision", "phi-3.5-vision",
		"minicpm-v", "deepseek-vl",
	}
	for _, p := range vlPatterns {
		if strings.Contains(name, p) {
			return ArchVisionLanguage
		}
	}

	// MoE patterns
	moePatterns := []string{
		"moe", "mixtral", "8x7b", "8x22b",
		"deepseek-v2", "deepseek-v3",
		"phi-3.5-moe", "qwen-moe",
	}
	for _, p := range moePatterns {
		if strings.Contains(name, p) {
			return ArchMoE
		}
	}

	// Code model patterns
	codePatterns := []string{
		"code", "coder", "codellama", "starcoder",
		"deepseek-coder", "qwen2.5-coder",
	}
	for _, p := range codePatterns {
		if strings.Contains(name, p) {
			return ArchCodeModel
		}
	}

	// Embedding patterns
	embedPatterns := []string{
		"embed", "bge", "gte", "e5-", "nomic",
	}
	for _, p := range embedPatterns {
		if strings.Contains(name, p) {
			return ArchEmbedding
		}
	}

	return ArchDenseTransformer
}

// parseParameterCount extracts parameter count from model name
func parseParameterCount(modelName string) int64 {
	lower := strings.ToLower(modelName)

	// Check known models first
	for key, spec := range KnownModels {
		if strings.Contains(lower, key) {
			return int64(spec.TotalParams * 1e9)
		}
	}

	// Parse from name patterns
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`(?i)[\-_]?(\d+(?:\.\d+)?)[bB][\-_\.]`),
		regexp.MustCompile(`(?i)[\-_]?(\d+(?:\.\d+)?)[bB]$`),
		regexp.MustCompile(`(?i)^(\d+(?:\.\d+)?)[bB][\-_]`),
		regexp.MustCompile(`(?i)[\-_]?(\d+(?:\.\d+)?)[\-_][bB]`),
		regexp.MustCompile(`(?i)[\-_x](\d+(?:\.\d+)?)[bB]`),
	}

	for _, pattern := range patterns {
		matches := pattern.FindStringSubmatch(lower)
		if len(matches) >= 2 {
			if params, err := strconv.ParseFloat(matches[1], 64); err == nil && params > 0 {
				return int64(params * 1e9)
			}
		}
	}

	// Million params
	millionPattern := regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)[mM][\-_\.]`)
	if matches := millionPattern.FindStringSubmatch(lower); len(matches) >= 2 {
		if params, err := strconv.ParseFloat(matches[1], 64); err == nil {
			return int64(params * 1e6)
		}
	}

	return 0
}

// normalizeQuantization normalizes quantization string for lookup
func normalizeQuantization(quant string) string {
	if quant == "" {
		return "fp16"
	}
	lower := strings.ToLower(quant)
	lower = strings.ReplaceAll(lower, "-", "_")
	lower = strings.ReplaceAll(lower, " ", "_")
	return lower
}

// getQuantizationMultiplier returns the memory multiplier for a quantization format
func getQuantizationMultiplier(quant string) float64 {
	quant = normalizeQuantization(quant)
	if mult, ok := QuantizationMultiplier[quant]; ok {
		return mult
	}
	for key, mult := range QuantizationMultiplier {
		if strings.HasPrefix(quant, key) || strings.HasPrefix(key, quant) {
			return mult
		}
	}
	return 1.0
}
