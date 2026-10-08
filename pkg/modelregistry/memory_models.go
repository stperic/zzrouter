// Package modelregistry provides memory estimation for LLM models.
// This file contains the KnownModels database - a catalog of well-known model
// specifications used for architecture-aware memory estimation.
package modelregistry

// ============================================================================
// Known Models Database
// ============================================================================

// KnownModels contains specifications for well-known models
var KnownModels = map[string]*ModelSpec{
	// ========================================================================
	// Llama Family (Dense Transformer, GQA)
	// ========================================================================
	"llama-2-7b": {
		Architecture: ArchDenseTransformer, Attention: AttentionGQA,
		TotalParams: 7.0, ActiveParams: 7.0,
		NumLayers: 32, HiddenSize: 4096, NumHeads: 32, NumKVHeads: 32,
		DefaultContextLength: 4096, MaxContextLength: 4096,
		KVCacheMultiplier: 1.0,
	},
	"llama-2-13b": {
		Architecture: ArchDenseTransformer, Attention: AttentionGQA,
		TotalParams: 13.0, ActiveParams: 13.0,
		NumLayers: 40, HiddenSize: 5120, NumHeads: 40, NumKVHeads: 40,
		DefaultContextLength: 4096, MaxContextLength: 4096,
		KVCacheMultiplier: 1.0,
	},
	"llama-2-70b": {
		Architecture: ArchDenseTransformer, Attention: AttentionGQA,
		TotalParams: 70.0, ActiveParams: 70.0,
		NumLayers: 80, HiddenSize: 8192, NumHeads: 64, NumKVHeads: 8,
		DefaultContextLength: 4096, MaxContextLength: 4096,
		KVCacheMultiplier: 0.125, // 8 KV heads vs 64 attention heads
	},
	"llama-3-8b": {
		Architecture: ArchDenseTransformer, Attention: AttentionGQA,
		TotalParams: 8.0, ActiveParams: 8.0,
		NumLayers: 32, HiddenSize: 4096, NumHeads: 32, NumKVHeads: 8,
		DefaultContextLength: 8192, MaxContextLength: 8192,
		KVCacheMultiplier: 0.25, // 8 KV heads vs 32 attention heads
	},
	"llama-3-70b": {
		Architecture: ArchDenseTransformer, Attention: AttentionGQA,
		TotalParams: 70.0, ActiveParams: 70.0,
		NumLayers: 80, HiddenSize: 8192, NumHeads: 64, NumKVHeads: 8,
		DefaultContextLength: 8192, MaxContextLength: 8192,
		KVCacheMultiplier: 0.125,
	},
	"llama-3.1-8b": {
		Architecture: ArchDenseTransformer, Attention: AttentionGQA,
		TotalParams: 8.0, ActiveParams: 8.0,
		NumLayers: 32, HiddenSize: 4096, NumHeads: 32, NumKVHeads: 8,
		DefaultContextLength: 131072, MaxContextLength: 131072, // 128K context
		KVCacheMultiplier: 0.25,
	},
	"llama-3.1-70b": {
		Architecture: ArchDenseTransformer, Attention: AttentionGQA,
		TotalParams: 70.0, ActiveParams: 70.0,
		NumLayers: 80, HiddenSize: 8192, NumHeads: 64, NumKVHeads: 8,
		DefaultContextLength: 131072, MaxContextLength: 131072,
		KVCacheMultiplier: 0.125,
	},
	"llama-3.1-405b": {
		Architecture: ArchDenseTransformer, Attention: AttentionGQA,
		TotalParams: 405.0, ActiveParams: 405.0,
		NumLayers: 126, HiddenSize: 16384, NumHeads: 128, NumKVHeads: 8,
		DefaultContextLength: 131072, MaxContextLength: 131072,
		KVCacheMultiplier: 0.0625, // 8 KV heads vs 128 attention heads
	},
	"llama-3.2-1b": {
		Architecture: ArchDenseTransformer, Attention: AttentionGQA,
		TotalParams: 1.0, ActiveParams: 1.0,
		NumLayers: 16, HiddenSize: 2048, NumHeads: 32, NumKVHeads: 8,
		DefaultContextLength: 131072, MaxContextLength: 131072,
		KVCacheMultiplier: 0.25,
	},
	"llama-3.2-3b": {
		Architecture: ArchDenseTransformer, Attention: AttentionGQA,
		TotalParams: 3.0, ActiveParams: 3.0,
		NumLayers: 28, HiddenSize: 3072, NumHeads: 24, NumKVHeads: 8,
		DefaultContextLength: 131072, MaxContextLength: 131072,
		KVCacheMultiplier: 0.33,
	},

	// ========================================================================
	// Llama Vision (Vision-Language Models)
	// ========================================================================
	"llama-3.2-11b-vision": {
		Architecture: ArchVisionLanguage, Attention: AttentionGQA,
		TotalParams: 11.0, ActiveParams: 11.0,
		VisionEncoderParams: 0.6, VisionEncoderType: "vit",
		ImageResolution: 560,
		NumLayers:       32, HiddenSize: 4096, NumHeads: 32, NumKVHeads: 8,
		DefaultContextLength: 131072, MaxContextLength: 131072,
		KVCacheMultiplier:    0.25,
		VisionOverheadMB:     2048, // Vision encoder + image processing
		ActivationOverheadMB: 1024,
	},
	"llama-3.2-90b-vision": {
		Architecture: ArchVisionLanguage, Attention: AttentionGQA,
		TotalParams: 90.0, ActiveParams: 90.0,
		VisionEncoderParams: 0.6, VisionEncoderType: "vit",
		ImageResolution: 560,
		NumLayers:       80, HiddenSize: 8192, NumHeads: 64, NumKVHeads: 8,
		DefaultContextLength: 131072, MaxContextLength: 131072,
		KVCacheMultiplier:    0.125,
		VisionOverheadMB:     4096,
		ActivationOverheadMB: 2048,
	},

	// ========================================================================
	// Qwen Family (Dense, various sizes)
	// ========================================================================
	"qwen2-0.5b": {
		Architecture: ArchDenseTransformer, Attention: AttentionGQA,
		TotalParams: 0.5, ActiveParams: 0.5,
		NumLayers: 24, HiddenSize: 896, NumHeads: 14, NumKVHeads: 2,
		DefaultContextLength: 32768, MaxContextLength: 32768,
		KVCacheMultiplier: 0.14,
	},
	"qwen2-1.5b": {
		Architecture: ArchDenseTransformer, Attention: AttentionGQA,
		TotalParams: 1.5, ActiveParams: 1.5,
		NumLayers: 28, HiddenSize: 1536, NumHeads: 12, NumKVHeads: 2,
		DefaultContextLength: 32768, MaxContextLength: 32768,
		KVCacheMultiplier: 0.17,
	},
	"qwen2-7b": {
		Architecture: ArchDenseTransformer, Attention: AttentionGQA,
		TotalParams: 7.0, ActiveParams: 7.0,
		NumLayers: 28, HiddenSize: 3584, NumHeads: 28, NumKVHeads: 4,
		DefaultContextLength: 131072, MaxContextLength: 131072,
		KVCacheMultiplier: 0.14,
	},
	"qwen2-72b": {
		Architecture: ArchDenseTransformer, Attention: AttentionGQA,
		TotalParams: 72.0, ActiveParams: 72.0,
		NumLayers: 80, HiddenSize: 8192, NumHeads: 64, NumKVHeads: 8,
		DefaultContextLength: 131072, MaxContextLength: 131072,
		KVCacheMultiplier: 0.125,
	},
	"qwen2.5-0.5b": {
		Architecture: ArchDenseTransformer, Attention: AttentionGQA,
		TotalParams: 0.5, ActiveParams: 0.5,
		DefaultContextLength: 32768, MaxContextLength: 131072,
		KVCacheMultiplier: 0.14,
	},
	"qwen2.5-1.5b": {
		Architecture: ArchDenseTransformer, Attention: AttentionGQA,
		TotalParams: 1.5, ActiveParams: 1.5,
		DefaultContextLength: 32768, MaxContextLength: 131072,
		KVCacheMultiplier: 0.17,
	},
	"qwen2.5-3b": {
		Architecture: ArchDenseTransformer, Attention: AttentionGQA,
		TotalParams: 3.0, ActiveParams: 3.0,
		DefaultContextLength: 32768, MaxContextLength: 131072,
		KVCacheMultiplier: 0.2,
	},
	"qwen2.5-7b": {
		Architecture: ArchDenseTransformer, Attention: AttentionGQA,
		TotalParams: 7.0, ActiveParams: 7.0,
		DefaultContextLength: 131072, MaxContextLength: 131072,
		KVCacheMultiplier: 0.14,
	},
	"qwen2.5-14b": {
		Architecture: ArchDenseTransformer, Attention: AttentionGQA,
		TotalParams: 14.0, ActiveParams: 14.0,
		DefaultContextLength: 131072, MaxContextLength: 131072,
		KVCacheMultiplier: 0.14,
	},
	"qwen2.5-32b": {
		Architecture: ArchDenseTransformer, Attention: AttentionGQA,
		TotalParams: 32.0, ActiveParams: 32.0,
		DefaultContextLength: 131072, MaxContextLength: 131072,
		KVCacheMultiplier: 0.125,
	},
	"qwen2.5-72b": {
		Architecture: ArchDenseTransformer, Attention: AttentionGQA,
		TotalParams: 72.0, ActiveParams: 72.0,
		DefaultContextLength: 131072, MaxContextLength: 131072,
		KVCacheMultiplier: 0.125,
	},
	"qwen3-0.6b": {
		Architecture: ArchDenseTransformer, Attention: AttentionGQA,
		TotalParams: 0.6, ActiveParams: 0.6,
		DefaultContextLength: 32768, MaxContextLength: 131072,
		KVCacheMultiplier: 0.14,
	},
	"qwen3-1.7b": {
		Architecture: ArchDenseTransformer, Attention: AttentionGQA,
		TotalParams: 1.7, ActiveParams: 1.7,
		DefaultContextLength: 32768, MaxContextLength: 131072,
		KVCacheMultiplier: 0.17,
	},
	"qwen3-4b": {
		Architecture: ArchDenseTransformer, Attention: AttentionGQA,
		TotalParams: 4.0, ActiveParams: 4.0,
		DefaultContextLength: 32768, MaxContextLength: 131072,
		KVCacheMultiplier: 0.2,
	},
	"qwen3-8b": {
		Architecture: ArchDenseTransformer, Attention: AttentionGQA,
		TotalParams: 8.0, ActiveParams: 8.0,
		DefaultContextLength: 131072, MaxContextLength: 131072,
		KVCacheMultiplier: 0.14,
	},
	"qwen3-14b": {
		Architecture: ArchDenseTransformer, Attention: AttentionGQA,
		TotalParams: 14.0, ActiveParams: 14.0,
		DefaultContextLength: 131072, MaxContextLength: 131072,
		KVCacheMultiplier: 0.14,
	},
	"qwen3-32b": {
		Architecture: ArchDenseTransformer, Attention: AttentionGQA,
		TotalParams: 32.0, ActiveParams: 32.0,
		DefaultContextLength: 131072, MaxContextLength: 131072,
		KVCacheMultiplier: 0.125,
	},
	"qwen3-72b": {
		Architecture: ArchDenseTransformer, Attention: AttentionGQA,
		TotalParams: 72.0, ActiveParams: 72.0,
		DefaultContextLength: 131072, MaxContextLength: 131072,
		KVCacheMultiplier: 0.125,
	},

	// ========================================================================
	// Qwen Vision-Language Models
	// ========================================================================
	"qwen-vl": {
		Architecture: ArchVisionLanguage, Attention: AttentionMHA,
		TotalParams: 9.6, ActiveParams: 9.6,
		VisionEncoderParams: 1.9, VisionEncoderType: "vit",
		ImageResolution:      448,
		DefaultContextLength: 8192, MaxContextLength: 32768,
		KVCacheMultiplier: 1.0,
		VisionOverheadMB:  3072,
	},
	"qwen2-vl-2b": {
		Architecture: ArchVisionLanguage, Attention: AttentionGQA,
		TotalParams: 2.2, ActiveParams: 2.2,
		VisionEncoderParams: 0.68, VisionEncoderType: "vit",
		ImageResolution:      672,
		DefaultContextLength: 32768, MaxContextLength: 32768,
		KVCacheMultiplier:    0.17,
		VisionOverheadMB:     2048,
		ActivationOverheadMB: 512,
	},
	"qwen2-vl-7b": {
		Architecture: ArchVisionLanguage, Attention: AttentionGQA,
		TotalParams: 8.3, ActiveParams: 8.3,
		VisionEncoderParams: 0.68, VisionEncoderType: "vit",
		ImageResolution:      672,
		DefaultContextLength: 32768, MaxContextLength: 131072,
		KVCacheMultiplier:    0.14,
		VisionOverheadMB:     3072,
		ActivationOverheadMB: 1024,
	},
	"qwen2-vl-72b": {
		Architecture: ArchVisionLanguage, Attention: AttentionGQA,
		TotalParams: 73.0, ActiveParams: 73.0,
		VisionEncoderParams: 0.68, VisionEncoderType: "vit",
		ImageResolution:      672,
		DefaultContextLength: 32768, MaxContextLength: 131072,
		KVCacheMultiplier:    0.125,
		VisionOverheadMB:     6144,
		ActivationOverheadMB: 2048,
	},
	"qwen2.5-vl-3b": {
		Architecture: ArchVisionLanguage, Attention: AttentionGQA,
		TotalParams: 3.8, ActiveParams: 3.8,
		VisionEncoderParams: 0.68, VisionEncoderType: "vit",
		ImageResolution:      672,
		DefaultContextLength: 32768, MaxContextLength: 131072,
		KVCacheMultiplier: 0.2,
		VisionOverheadMB:  2048,
	},
	"qwen2.5-vl-7b": {
		Architecture: ArchVisionLanguage, Attention: AttentionGQA,
		TotalParams: 8.3, ActiveParams: 8.3,
		VisionEncoderParams: 0.68, VisionEncoderType: "vit",
		ImageResolution:      672,
		DefaultContextLength: 32768, MaxContextLength: 131072,
		KVCacheMultiplier: 0.14,
		VisionOverheadMB:  3072,
	},
	"qwen2.5-vl-72b": {
		Architecture: ArchVisionLanguage, Attention: AttentionGQA,
		TotalParams: 73.0, ActiveParams: 73.0,
		VisionEncoderParams: 0.68, VisionEncoderType: "vit",
		ImageResolution:      672,
		DefaultContextLength: 32768, MaxContextLength: 131072,
		KVCacheMultiplier: 0.125,
		VisionOverheadMB:  6144,
	},

	// ========================================================================
	// Mistral Family
	// ========================================================================
	"mistral-7b": {
		Architecture: ArchDenseTransformer, Attention: AttentionGQA,
		TotalParams: 7.0, ActiveParams: 7.0,
		NumLayers: 32, HiddenSize: 4096, NumHeads: 32, NumKVHeads: 8,
		DefaultContextLength: 32768, MaxContextLength: 32768,
		KVCacheMultiplier: 0.25,
	},
	"mistral-nemo-12b": {
		Architecture: ArchDenseTransformer, Attention: AttentionGQA,
		TotalParams: 12.0, ActiveParams: 12.0,
		NumLayers: 40, HiddenSize: 5120, NumHeads: 32, NumKVHeads: 8,
		DefaultContextLength: 131072, MaxContextLength: 131072,
		KVCacheMultiplier: 0.25,
	},
	"mistral-small-22b": {
		Architecture: ArchDenseTransformer, Attention: AttentionGQA,
		TotalParams: 22.0, ActiveParams: 22.0,
		DefaultContextLength: 32768, MaxContextLength: 32768,
		KVCacheMultiplier: 0.25,
	},
	"mistral-large-123b": {
		Architecture: ArchDenseTransformer, Attention: AttentionGQA,
		TotalParams: 123.0, ActiveParams: 123.0,
		DefaultContextLength: 131072, MaxContextLength: 131072,
		KVCacheMultiplier: 0.125,
	},

	// ========================================================================
	// Mixtral (Mixture of Experts)
	// ========================================================================
	"mixtral-8x7b": {
		Architecture: ArchMoE, Attention: AttentionGQA,
		TotalParams: 46.7, ActiveParams: 12.9, // Only 2 experts active
		NumExperts: 8, ActiveExperts: 2,
		NumLayers: 32, HiddenSize: 4096, NumHeads: 32, NumKVHeads: 8,
		DefaultContextLength: 32768, MaxContextLength: 32768,
		KVCacheMultiplier: 0.25,
	},
	"mixtral-8x22b": {
		Architecture: ArchMoE, Attention: AttentionGQA,
		TotalParams: 141.0, ActiveParams: 39.0, // Only 2 experts active
		NumExperts: 8, ActiveExperts: 2,
		NumLayers: 56, HiddenSize: 6144, NumHeads: 48, NumKVHeads: 8,
		DefaultContextLength: 65536, MaxContextLength: 65536,
		KVCacheMultiplier: 0.17,
	},

	// ========================================================================
	// DeepSeek Family
	// ========================================================================
	"deepseek-7b": {
		Architecture: ArchDenseTransformer, Attention: AttentionMHA,
		TotalParams: 7.0, ActiveParams: 7.0,
		DefaultContextLength: 4096, MaxContextLength: 4096,
		KVCacheMultiplier: 1.0,
	},
	"deepseek-67b": {
		Architecture: ArchDenseTransformer, Attention: AttentionGQA,
		TotalParams: 67.0, ActiveParams: 67.0,
		DefaultContextLength: 4096, MaxContextLength: 4096,
		KVCacheMultiplier: 0.25,
	},
	"deepseek-coder-1.3b": {
		Architecture: ArchCodeModel, Attention: AttentionGQA,
		TotalParams: 1.3, ActiveParams: 1.3,
		DefaultContextLength: 16384, MaxContextLength: 16384,
		KVCacheMultiplier: 0.5,
	},
	"deepseek-coder-6.7b": {
		Architecture: ArchCodeModel, Attention: AttentionGQA,
		TotalParams: 6.7, ActiveParams: 6.7,
		DefaultContextLength: 16384, MaxContextLength: 16384,
		KVCacheMultiplier: 0.25,
	},
	"deepseek-coder-33b": {
		Architecture: ArchCodeModel, Attention: AttentionGQA,
		TotalParams: 33.0, ActiveParams: 33.0,
		DefaultContextLength: 16384, MaxContextLength: 16384,
		KVCacheMultiplier: 0.125,
	},
	"deepseek-v2": {
		Architecture: ArchMoE, Attention: AttentionMQA, // Uses MLA (Multi-head Latent Attention)
		TotalParams: 236.0, ActiveParams: 21.0,
		NumExperts: 160, ActiveExperts: 6,
		DefaultContextLength: 131072, MaxContextLength: 131072,
		KVCacheMultiplier: 0.05, // MLA significantly reduces KV cache
	},
	"deepseek-v2.5": {
		Architecture: ArchMoE, Attention: AttentionMQA,
		TotalParams: 236.0, ActiveParams: 21.0,
		NumExperts: 160, ActiveExperts: 6,
		DefaultContextLength: 131072, MaxContextLength: 131072,
		KVCacheMultiplier: 0.05,
	},
	"deepseek-v3": {
		Architecture: ArchMoE, Attention: AttentionMQA,
		TotalParams: 671.0, ActiveParams: 37.0,
		NumExperts: 256, ActiveExperts: 8,
		DefaultContextLength: 131072, MaxContextLength: 131072,
		KVCacheMultiplier: 0.05,
	},

	// ========================================================================
	// Phi Family (Microsoft)
	// ========================================================================
	"phi-2": {
		Architecture: ArchDenseTransformer, Attention: AttentionMHA,
		TotalParams: 2.7, ActiveParams: 2.7,
		NumLayers: 32, HiddenSize: 2560, NumHeads: 32,
		DefaultContextLength: 2048, MaxContextLength: 2048,
		KVCacheMultiplier: 1.0,
	},
	"phi-3-mini": {
		Architecture: ArchDenseTransformer, Attention: AttentionGQA,
		TotalParams: 3.8, ActiveParams: 3.8,
		NumLayers: 32, HiddenSize: 3072, NumHeads: 32, NumKVHeads: 8,
		DefaultContextLength: 4096, MaxContextLength: 131072,
		KVCacheMultiplier: 0.25,
	},
	"phi-3-small": {
		Architecture: ArchDenseTransformer, Attention: AttentionGQA,
		TotalParams: 7.0, ActiveParams: 7.0,
		NumLayers: 32, HiddenSize: 4096, NumHeads: 32, NumKVHeads: 8,
		DefaultContextLength: 8192, MaxContextLength: 131072,
		KVCacheMultiplier: 0.25,
	},
	"phi-3-medium": {
		Architecture: ArchDenseTransformer, Attention: AttentionGQA,
		TotalParams: 14.0, ActiveParams: 14.0,
		NumLayers: 40, HiddenSize: 5120, NumHeads: 40, NumKVHeads: 10,
		DefaultContextLength: 4096, MaxContextLength: 131072,
		KVCacheMultiplier: 0.25,
	},
	"phi-3-vision": {
		Architecture: ArchVisionLanguage, Attention: AttentionGQA,
		TotalParams: 4.2, ActiveParams: 4.2,
		VisionEncoderParams: 0.4, VisionEncoderType: "clip",
		ImageResolution:      1344,
		DefaultContextLength: 131072, MaxContextLength: 131072,
		KVCacheMultiplier: 0.25,
		VisionOverheadMB:  1536,
	},
	"phi-3.5-mini": {
		Architecture: ArchDenseTransformer, Attention: AttentionGQA,
		TotalParams: 3.8, ActiveParams: 3.8,
		DefaultContextLength: 131072, MaxContextLength: 131072,
		KVCacheMultiplier: 0.25,
	},
	"phi-3.5-moe": {
		Architecture: ArchMoE, Attention: AttentionGQA,
		TotalParams: 41.9, ActiveParams: 6.6,
		NumExperts: 16, ActiveExperts: 2,
		DefaultContextLength: 131072, MaxContextLength: 131072,
		KVCacheMultiplier: 0.25,
	},
	"phi-3.5-vision": {
		Architecture: ArchVisionLanguage, Attention: AttentionGQA,
		TotalParams: 4.2, ActiveParams: 4.2,
		VisionEncoderParams: 0.4, VisionEncoderType: "clip",
		ImageResolution:      1344,
		DefaultContextLength: 131072, MaxContextLength: 131072,
		KVCacheMultiplier: 0.25,
		VisionOverheadMB:  2048,
	},
	"phi-4": {
		Architecture: ArchDenseTransformer, Attention: AttentionGQA,
		TotalParams: 14.0, ActiveParams: 14.0,
		NumLayers: 40, HiddenSize: 5120, NumHeads: 40, NumKVHeads: 10,
		DefaultContextLength: 16384, MaxContextLength: 16384,
		KVCacheMultiplier: 0.25,
	},

	// ========================================================================
	// Gemma Family (Google)
	// ========================================================================
	"gemma-2b": {
		Architecture: ArchDenseTransformer, Attention: AttentionMQA,
		TotalParams: 2.5, ActiveParams: 2.5,
		NumLayers: 18, HiddenSize: 2048, NumHeads: 8, NumKVHeads: 1,
		DefaultContextLength: 8192, MaxContextLength: 8192,
		KVCacheMultiplier: 0.125, // MQA
	},
	"gemma-7b": {
		Architecture: ArchDenseTransformer, Attention: AttentionMHA,
		TotalParams: 8.5, ActiveParams: 8.5,
		NumLayers: 28, HiddenSize: 3072, NumHeads: 16, NumKVHeads: 16,
		DefaultContextLength: 8192, MaxContextLength: 8192,
		KVCacheMultiplier: 1.0,
	},
	"gemma-2-2b": {
		Architecture: ArchDenseTransformer, Attention: AttentionGQA,
		TotalParams: 2.6, ActiveParams: 2.6,
		NumLayers: 26, HiddenSize: 2304, NumHeads: 8, NumKVHeads: 4,
		DefaultContextLength: 8192, MaxContextLength: 8192,
		KVCacheMultiplier: 0.5,
	},
	"gemma-2-9b": {
		Architecture: ArchDenseTransformer, Attention: AttentionGQA,
		TotalParams: 9.2, ActiveParams: 9.2,
		NumLayers: 42, HiddenSize: 3584, NumHeads: 16, NumKVHeads: 8,
		DefaultContextLength: 8192, MaxContextLength: 8192,
		KVCacheMultiplier: 0.5,
	},
	"gemma-2-27b": {
		Architecture: ArchDenseTransformer, Attention: AttentionGQA,
		TotalParams: 27.2, ActiveParams: 27.2,
		NumLayers: 46, HiddenSize: 4608, NumHeads: 32, NumKVHeads: 16,
		DefaultContextLength: 8192, MaxContextLength: 8192,
		KVCacheMultiplier: 0.5,
	},

	// ========================================================================
	// LLaVA (Vision-Language)
	// ========================================================================
	"llava-1.5-7b": {
		Architecture: ArchVisionLanguage, Attention: AttentionMHA,
		TotalParams: 7.0, ActiveParams: 7.0,
		VisionEncoderParams: 0.3, VisionEncoderType: "clip",
		ImageResolution:      336,
		DefaultContextLength: 4096, MaxContextLength: 4096,
		KVCacheMultiplier: 1.0,
		VisionOverheadMB:  1024,
	},
	"llava-1.5-13b": {
		Architecture: ArchVisionLanguage, Attention: AttentionMHA,
		TotalParams: 13.0, ActiveParams: 13.0,
		VisionEncoderParams: 0.3, VisionEncoderType: "clip",
		ImageResolution:      336,
		DefaultContextLength: 4096, MaxContextLength: 4096,
		KVCacheMultiplier: 1.0,
		VisionOverheadMB:  1536,
	},
	"llava-1.6-mistral-7b": {
		Architecture: ArchVisionLanguage, Attention: AttentionGQA,
		TotalParams: 7.0, ActiveParams: 7.0,
		VisionEncoderParams: 0.3, VisionEncoderType: "clip",
		ImageResolution:      672,
		DefaultContextLength: 32768, MaxContextLength: 32768,
		KVCacheMultiplier: 0.25,
		VisionOverheadMB:  1536,
	},
	"llava-1.6-34b": {
		Architecture: ArchVisionLanguage, Attention: AttentionGQA,
		TotalParams: 34.0, ActiveParams: 34.0,
		VisionEncoderParams: 0.3, VisionEncoderType: "clip",
		ImageResolution:      672,
		DefaultContextLength: 4096, MaxContextLength: 4096,
		KVCacheMultiplier: 0.25,
		VisionOverheadMB:  2048,
	},
	"llava-onevision-7b": {
		Architecture: ArchVisionLanguage, Attention: AttentionGQA,
		TotalParams: 8.0, ActiveParams: 8.0,
		VisionEncoderParams: 0.4, VisionEncoderType: "siglip",
		ImageResolution:      384,
		DefaultContextLength: 32768, MaxContextLength: 32768,
		KVCacheMultiplier: 0.25,
		VisionOverheadMB:  2048,
	},
	"llava-onevision-72b": {
		Architecture: ArchVisionLanguage, Attention: AttentionGQA,
		TotalParams: 73.0, ActiveParams: 73.0,
		VisionEncoderParams: 0.4, VisionEncoderType: "siglip",
		ImageResolution:      384,
		DefaultContextLength: 32768, MaxContextLength: 32768,
		KVCacheMultiplier: 0.125,
		VisionOverheadMB:  4096,
	},

	// ========================================================================
	// InternLM / InternVL (Vision-Language)
	// ========================================================================
	"internlm2-7b": {
		Architecture: ArchDenseTransformer, Attention: AttentionGQA,
		TotalParams: 7.0, ActiveParams: 7.0,
		DefaultContextLength: 200000, MaxContextLength: 200000,
		KVCacheMultiplier: 0.25,
	},
	"internlm2-20b": {
		Architecture: ArchDenseTransformer, Attention: AttentionGQA,
		TotalParams: 20.0, ActiveParams: 20.0,
		DefaultContextLength: 200000, MaxContextLength: 200000,
		KVCacheMultiplier: 0.25,
	},
	"internvl2-2b": {
		Architecture: ArchVisionLanguage, Attention: AttentionGQA,
		TotalParams: 2.2, ActiveParams: 2.2,
		VisionEncoderParams: 0.3, VisionEncoderType: "internvit",
		ImageResolution:      448,
		DefaultContextLength: 8192, MaxContextLength: 8192,
		KVCacheMultiplier: 0.25,
		VisionOverheadMB:  1024,
	},
	"internvl2-8b": {
		Architecture: ArchVisionLanguage, Attention: AttentionGQA,
		TotalParams: 8.0, ActiveParams: 8.0,
		VisionEncoderParams: 0.3, VisionEncoderType: "internvit",
		ImageResolution:      448,
		DefaultContextLength: 8192, MaxContextLength: 8192,
		KVCacheMultiplier: 0.25,
		VisionOverheadMB:  2048,
	},
	"internvl2-26b": {
		Architecture: ArchVisionLanguage, Attention: AttentionGQA,
		TotalParams: 26.0, ActiveParams: 26.0,
		VisionEncoderParams: 6.0, VisionEncoderType: "internvit",
		ImageResolution:      448,
		DefaultContextLength: 8192, MaxContextLength: 8192,
		KVCacheMultiplier: 0.25,
		VisionOverheadMB:  8192,
	},
	"internvl2-40b": {
		Architecture: ArchVisionLanguage, Attention: AttentionGQA,
		TotalParams: 40.0, ActiveParams: 40.0,
		VisionEncoderParams: 6.0, VisionEncoderType: "internvit",
		ImageResolution:      448,
		DefaultContextLength: 8192, MaxContextLength: 8192,
		KVCacheMultiplier: 0.25,
		VisionOverheadMB:  10240,
	},
	"internvl2.5-8b": {
		Architecture: ArchVisionLanguage, Attention: AttentionGQA,
		TotalParams: 8.0, ActiveParams: 8.0,
		VisionEncoderParams: 0.3, VisionEncoderType: "internvit",
		ImageResolution:      448,
		DefaultContextLength: 8192, MaxContextLength: 8192,
		KVCacheMultiplier: 0.25,
		VisionOverheadMB:  2048,
	},

	// ========================================================================
	// CogVLM (Vision-Language)
	// ========================================================================
	"cogvlm-17b": {
		Architecture: ArchVisionLanguage, Attention: AttentionMHA,
		TotalParams: 17.0, ActiveParams: 17.0,
		VisionEncoderParams: 4.4, VisionEncoderType: "eva",
		ImageResolution:      490,
		DefaultContextLength: 2048, MaxContextLength: 2048,
		KVCacheMultiplier: 1.0,
		VisionOverheadMB:  6144,
	},
	"cogvlm2-19b": {
		Architecture: ArchVisionLanguage, Attention: AttentionGQA,
		TotalParams: 19.0, ActiveParams: 19.0,
		VisionEncoderParams: 4.4, VisionEncoderType: "eva",
		ImageResolution:      1344,
		DefaultContextLength: 8192, MaxContextLength: 8192,
		KVCacheMultiplier: 0.25,
		VisionOverheadMB:  8192,
	},

	// ========================================================================
	// Yi (01.AI)
	// ========================================================================
	"yi-6b": {
		Architecture: ArchDenseTransformer, Attention: AttentionGQA,
		TotalParams: 6.0, ActiveParams: 6.0,
		DefaultContextLength: 4096, MaxContextLength: 200000,
		KVCacheMultiplier: 0.25,
	},
	"yi-9b": {
		Architecture: ArchDenseTransformer, Attention: AttentionGQA,
		TotalParams: 9.0, ActiveParams: 9.0,
		DefaultContextLength: 4096, MaxContextLength: 200000,
		KVCacheMultiplier: 0.25,
	},
	"yi-34b": {
		Architecture: ArchDenseTransformer, Attention: AttentionGQA,
		TotalParams: 34.0, ActiveParams: 34.0,
		DefaultContextLength: 4096, MaxContextLength: 200000,
		KVCacheMultiplier: 0.125,
	},
	"yi-vl-6b": {
		Architecture: ArchVisionLanguage, Attention: AttentionGQA,
		TotalParams: 6.7, ActiveParams: 6.7,
		VisionEncoderParams: 0.3, VisionEncoderType: "clip",
		ImageResolution:      448,
		DefaultContextLength: 4096, MaxContextLength: 4096,
		KVCacheMultiplier: 0.25,
		VisionOverheadMB:  1536,
	},
	"yi-vl-34b": {
		Architecture: ArchVisionLanguage, Attention: AttentionGQA,
		TotalParams: 34.0, ActiveParams: 34.0,
		VisionEncoderParams: 0.3, VisionEncoderType: "clip",
		ImageResolution:      448,
		DefaultContextLength: 4096, MaxContextLength: 4096,
		KVCacheMultiplier: 0.125,
		VisionOverheadMB:  3072,
	},

	// ========================================================================
	// Command (Cohere)
	// ========================================================================
	"command-r": {
		Architecture: ArchDenseTransformer, Attention: AttentionGQA,
		TotalParams: 35.0, ActiveParams: 35.0,
		DefaultContextLength: 131072, MaxContextLength: 131072,
		KVCacheMultiplier: 0.25,
	},
	"command-r-plus": {
		Architecture: ArchDenseTransformer, Attention: AttentionGQA,
		TotalParams: 104.0, ActiveParams: 104.0,
		DefaultContextLength: 131072, MaxContextLength: 131072,
		KVCacheMultiplier: 0.125,
	},

	// ========================================================================
	// Embedding Models
	// ========================================================================
	"nomic-embed-text": {
		Architecture: ArchEmbedding, Attention: AttentionMHA,
		TotalParams: 0.137, ActiveParams: 0.137,
		DefaultContextLength: 8192, MaxContextLength: 8192,
		KVCacheMultiplier: 0.0, // No KV cache needed for embeddings
	},
	"bge-large": {
		Architecture: ArchEmbedding, Attention: AttentionMHA,
		TotalParams: 0.335, ActiveParams: 0.335,
		DefaultContextLength: 512, MaxContextLength: 512,
		KVCacheMultiplier: 0.0,
	},
	"bge-m3": {
		Architecture: ArchEmbedding, Attention: AttentionMHA,
		TotalParams: 0.568, ActiveParams: 0.568,
		DefaultContextLength: 8192, MaxContextLength: 8192,
		KVCacheMultiplier: 0.0,
	},
	"e5-mistral-7b": {
		Architecture: ArchEmbedding, Attention: AttentionGQA,
		TotalParams: 7.0, ActiveParams: 7.0,
		DefaultContextLength: 32768, MaxContextLength: 32768,
		KVCacheMultiplier: 0.0,
	},
	"gte-qwen2-7b": {
		Architecture: ArchEmbedding, Attention: AttentionGQA,
		TotalParams: 7.0, ActiveParams: 7.0,
		DefaultContextLength: 131072, MaxContextLength: 131072,
		KVCacheMultiplier: 0.0,
	},

	// ========================================================================
	// Code Models
	// ========================================================================
	"codellama-7b": {
		Architecture: ArchCodeModel, Attention: AttentionGQA,
		TotalParams: 7.0, ActiveParams: 7.0,
		DefaultContextLength: 16384, MaxContextLength: 100000,
		KVCacheMultiplier: 0.25,
	},
	"codellama-13b": {
		Architecture: ArchCodeModel, Attention: AttentionGQA,
		TotalParams: 13.0, ActiveParams: 13.0,
		DefaultContextLength: 16384, MaxContextLength: 100000,
		KVCacheMultiplier: 0.25,
	},
	"codellama-34b": {
		Architecture: ArchCodeModel, Attention: AttentionGQA,
		TotalParams: 34.0, ActiveParams: 34.0,
		DefaultContextLength: 16384, MaxContextLength: 100000,
		KVCacheMultiplier: 0.125,
	},
	"codellama-70b": {
		Architecture: ArchCodeModel, Attention: AttentionGQA,
		TotalParams: 70.0, ActiveParams: 70.0,
		DefaultContextLength: 16384, MaxContextLength: 100000,
		KVCacheMultiplier: 0.125,
	},
	"starcoder2-3b": {
		Architecture: ArchCodeModel, Attention: AttentionGQA,
		TotalParams: 3.0, ActiveParams: 3.0,
		DefaultContextLength: 16384, MaxContextLength: 16384,
		KVCacheMultiplier: 0.25,
	},
	"starcoder2-7b": {
		Architecture: ArchCodeModel, Attention: AttentionGQA,
		TotalParams: 7.0, ActiveParams: 7.0,
		DefaultContextLength: 16384, MaxContextLength: 16384,
		KVCacheMultiplier: 0.25,
	},
	"starcoder2-15b": {
		Architecture: ArchCodeModel, Attention: AttentionGQA,
		TotalParams: 15.0, ActiveParams: 15.0,
		DefaultContextLength: 16384, MaxContextLength: 16384,
		KVCacheMultiplier: 0.17,
	},
	"qwen2.5-coder-0.5b": {
		Architecture: ArchCodeModel, Attention: AttentionGQA,
		TotalParams: 0.5, ActiveParams: 0.5,
		DefaultContextLength: 32768, MaxContextLength: 131072,
		KVCacheMultiplier: 0.14,
	},
	"qwen2.5-coder-1.5b": {
		Architecture: ArchCodeModel, Attention: AttentionGQA,
		TotalParams: 1.5, ActiveParams: 1.5,
		DefaultContextLength: 32768, MaxContextLength: 131072,
		KVCacheMultiplier: 0.17,
	},
	"qwen2.5-coder-3b": {
		Architecture: ArchCodeModel, Attention: AttentionGQA,
		TotalParams: 3.0, ActiveParams: 3.0,
		DefaultContextLength: 32768, MaxContextLength: 131072,
		KVCacheMultiplier: 0.2,
	},
	"qwen2.5-coder-7b": {
		Architecture: ArchCodeModel, Attention: AttentionGQA,
		TotalParams: 7.0, ActiveParams: 7.0,
		DefaultContextLength: 131072, MaxContextLength: 131072,
		KVCacheMultiplier: 0.14,
	},
	"qwen2.5-coder-14b": {
		Architecture: ArchCodeModel, Attention: AttentionGQA,
		TotalParams: 14.0, ActiveParams: 14.0,
		DefaultContextLength: 131072, MaxContextLength: 131072,
		KVCacheMultiplier: 0.14,
	},
	"qwen2.5-coder-32b": {
		Architecture: ArchCodeModel, Attention: AttentionGQA,
		TotalParams: 32.0, ActiveParams: 32.0,
		DefaultContextLength: 131072, MaxContextLength: 131072,
		KVCacheMultiplier: 0.125,
	},

	// ========================================================================
	// Other Notable Models
	// ========================================================================
	"tinyllama-1.1b": {
		Architecture: ArchDenseTransformer, Attention: AttentionGQA,
		TotalParams: 1.1, ActiveParams: 1.1,
		NumLayers: 22, HiddenSize: 2048, NumHeads: 32, NumKVHeads: 4,
		DefaultContextLength: 2048, MaxContextLength: 2048,
		KVCacheMultiplier: 0.125,
	},
	"stablelm-2-1.6b": {
		Architecture: ArchDenseTransformer, Attention: AttentionGQA,
		TotalParams: 1.6, ActiveParams: 1.6,
		DefaultContextLength: 4096, MaxContextLength: 4096,
		KVCacheMultiplier: 0.25,
	},
	"stablelm-2-12b": {
		Architecture: ArchDenseTransformer, Attention: AttentionGQA,
		TotalParams: 12.0, ActiveParams: 12.0,
		DefaultContextLength: 4096, MaxContextLength: 4096,
		KVCacheMultiplier: 0.25,
	},
	"falcon-7b": {
		Architecture: ArchDenseTransformer, Attention: AttentionMQA,
		TotalParams: 7.0, ActiveParams: 7.0,
		DefaultContextLength: 2048, MaxContextLength: 2048,
		KVCacheMultiplier: 0.125, // MQA
	},
	"falcon-40b": {
		Architecture: ArchDenseTransformer, Attention: AttentionGQA,
		TotalParams: 40.0, ActiveParams: 40.0,
		DefaultContextLength: 2048, MaxContextLength: 2048,
		KVCacheMultiplier: 0.125,
	},
	"falcon-180b": {
		Architecture: ArchDenseTransformer, Attention: AttentionGQA,
		TotalParams: 180.0, ActiveParams: 180.0,
		DefaultContextLength: 2048, MaxContextLength: 2048,
		KVCacheMultiplier: 0.125,
	},
	"mpt-7b": {
		Architecture: ArchDenseTransformer, Attention: AttentionMHA,
		TotalParams: 7.0, ActiveParams: 7.0,
		DefaultContextLength: 2048, MaxContextLength: 65536,
		KVCacheMultiplier: 1.0,
	},
	"mpt-30b": {
		Architecture: ArchDenseTransformer, Attention: AttentionMHA,
		TotalParams: 30.0, ActiveParams: 30.0,
		DefaultContextLength: 8192, MaxContextLength: 8192,
		KVCacheMultiplier: 1.0,
	},
	"olmo-7b": {
		Architecture: ArchDenseTransformer, Attention: AttentionMHA,
		TotalParams: 7.0, ActiveParams: 7.0,
		DefaultContextLength: 2048, MaxContextLength: 2048,
		KVCacheMultiplier: 1.0,
	},
	"olmo-2-7b": {
		Architecture: ArchDenseTransformer, Attention: AttentionGQA,
		TotalParams: 7.0, ActiveParams: 7.0,
		DefaultContextLength: 4096, MaxContextLength: 4096,
		KVCacheMultiplier: 0.25,
	},
	"olmo-2-13b": {
		Architecture: ArchDenseTransformer, Attention: AttentionGQA,
		TotalParams: 13.0, ActiveParams: 13.0,
		DefaultContextLength: 4096, MaxContextLength: 4096,
		KVCacheMultiplier: 0.25,
	},
}
