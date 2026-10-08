package modelregistry

import (
	"testing"
)

func TestEstimateModelMemory_DenseTransformer(t *testing.T) {
	tests := []struct {
		name         string
		modelName    string
		format       string
		quantization string
		wantArch     ModelArchitecture
		wantMinMemMB int64
		wantMaxMemMB int64
	}{
		{
			name:         "Llama 2 7B Q4",
			modelName:    "meta-llama/Llama-2-7B-Chat-GGUF",
			format:       "gguf",
			quantization: "q4_k_m",
			wantArch:     ArchDenseTransformer,
			wantMinMemMB: 3000,
			wantMaxMemMB: 6000,
		},
		{
			name:         "Llama 3 8B FP16",
			modelName:    "meta-llama/Llama-3-8B-Instruct",
			format:       "hf_transformers",
			quantization: "fp16",
			wantArch:     ArchDenseTransformer,
			wantMinMemMB: 14000,
			wantMaxMemMB: 25000,
		},
		{
			name:         "Qwen 72B Q4",
			modelName:    "Qwen/Qwen2.5-72B-Instruct-GGUF",
			format:       "gguf",
			quantization: "q4_k_m",
			wantArch:     ArchDenseTransformer,
			wantMinMemMB: 30000,
			wantMaxMemMB: 60000,
		},
		{
			name:         "TinyLlama 1.1B Q8",
			modelName:    "TinyLlama/TinyLlama-1.1B-Chat-v1.0-GGUF",
			format:       "gguf",
			quantization: "q8_0",
			wantArch:     ArchDenseTransformer,
			wantMinMemMB: 500,
			wantMaxMemMB: 2500,
		},
		{
			name:         "Gemma 2 27B Q4",
			modelName:    "google/gemma-2-27b-it",
			format:       "gguf",
			quantization: "q4_k_m",
			wantArch:     ArchDenseTransformer,
			wantMinMemMB: 12000,
			wantMaxMemMB: 25000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			estimate := EstimateModelMemory(tt.modelName, tt.format, tt.quantization)

			if estimate == nil {
				t.Fatal("EstimateModelMemory returned nil")
			}

			if estimate.Architecture != tt.wantArch {
				t.Errorf("Architecture = %v, want %v", estimate.Architecture, tt.wantArch)
			}

			if estimate.RecommendedMemoryMB < tt.wantMinMemMB || estimate.RecommendedMemoryMB > tt.wantMaxMemMB {
				t.Errorf("RecommendedMemoryMB = %d, want between %d and %d",
					estimate.RecommendedMemoryMB, tt.wantMinMemMB, tt.wantMaxMemMB)
			}

			// Verify ordering
			if estimate.MinMemoryMB >= estimate.RecommendedMemoryMB {
				t.Errorf("MinMemoryMB (%d) should be < RecommendedMemoryMB (%d)",
					estimate.MinMemoryMB, estimate.RecommendedMemoryMB)
			}
			if estimate.RecommendedMemoryMB >= estimate.MaxMemoryMB {
				t.Errorf("RecommendedMemoryMB (%d) should be < MaxMemoryMB (%d)",
					estimate.RecommendedMemoryMB, estimate.MaxMemoryMB)
			}
		})
	}
}

func TestEstimateModelMemory_VisionLanguage(t *testing.T) {
	tests := []struct {
		name         string
		modelName    string
		format       string
		quantization string
		wantMinMemMB int64
		wantMaxMemMB int64
	}{
		{
			name:         "Llama 3.2 11B Vision Q4",
			modelName:    "meta-llama/Llama-3.2-11B-Vision-Instruct",
			format:       "gguf",
			quantization: "q4_k_m",
			wantMinMemMB: 5000,
			wantMaxMemMB: 15000,
		},
		{
			name:         "Qwen2-VL 7B Q4",
			modelName:    "Qwen/Qwen2-VL-7B-Instruct-GPTQ-Int4",
			format:       "gguf",
			quantization: "q4_k_m",
			wantMinMemMB: 4000,
			wantMaxMemMB: 12000,
		},
		{
			name:         "LLaVA 1.5 13B FP16",
			modelName:    "liuhaotian/llava-v1.5-13b",
			format:       "hf_transformers",
			quantization: "fp16",
			wantMinMemMB: 24000,
			wantMaxMemMB: 40000,
		},
		{
			name:         "InternVL2 8B Q4",
			modelName:    "OpenGVLab/InternVL2-8B",
			format:       "gguf",
			quantization: "q4_k_m",
			wantMinMemMB: 4000,
			wantMaxMemMB: 12000,
		},
		{
			name:         "Phi-3-Vision Q4",
			modelName:    "microsoft/Phi-3-vision-128k-instruct",
			format:       "gguf",
			quantization: "q4_k_m",
			wantMinMemMB: 2000,
			wantMaxMemMB: 8000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			estimate := EstimateModelMemory(tt.modelName, tt.format, tt.quantization)

			if estimate == nil {
				t.Fatal("EstimateModelMemory returned nil")
			}

			if estimate.Architecture != ArchVisionLanguage {
				t.Errorf("Architecture = %v, want %v", estimate.Architecture, ArchVisionLanguage)
			}

			// VLMs should have vision memory component
			if estimate.VisionMemoryMB <= 0 {
				t.Errorf("VisionMemoryMB = %d, want > 0 for VLM", estimate.VisionMemoryMB)
			}

			if estimate.RecommendedMemoryMB < tt.wantMinMemMB || estimate.RecommendedMemoryMB > tt.wantMaxMemMB {
				t.Errorf("RecommendedMemoryMB = %d, want between %d and %d",
					estimate.RecommendedMemoryMB, tt.wantMinMemMB, tt.wantMaxMemMB)
			}
		})
	}
}

func TestEstimateModelMemory_MoE(t *testing.T) {
	tests := []struct {
		name           string
		modelName      string
		format         string
		quantization   string
		wantMinMemMB   int64
		wantMaxMemMB   int64
		wantActiveLess bool // Active params should be less than total
	}{
		{
			name:           "Mixtral 8x7B Q4",
			modelName:      "mistralai/Mixtral-8x7B-Instruct-v0.1",
			format:         "gguf",
			quantization:   "q4_k_m",
			wantMinMemMB:   20000,
			wantMaxMemMB:   40000,
			wantActiveLess: true,
		},
		{
			name:           "Mixtral 8x22B Q4",
			modelName:      "mistralai/Mixtral-8x22B-Instruct-v0.1",
			format:         "gguf",
			quantization:   "q4_k_m",
			wantMinMemMB:   60000,
			wantMaxMemMB:   120000,
			wantActiveLess: true,
		},
		{
			name:           "DeepSeek V2 Q4",
			modelName:      "deepseek-ai/DeepSeek-V2-Chat",
			format:         "gguf",
			quantization:   "q4_k_m",
			wantMinMemMB:   100000,
			wantMaxMemMB:   200000,
			wantActiveLess: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			estimate := EstimateModelMemory(tt.modelName, tt.format, tt.quantization)

			if estimate == nil {
				t.Fatal("EstimateModelMemory returned nil")
			}

			if estimate.Architecture != ArchMoE {
				t.Errorf("Architecture = %v, want %v", estimate.Architecture, ArchMoE)
			}

			// MoE models should have active params less than total
			if tt.wantActiveLess && estimate.ActiveParams >= estimate.ParameterCount {
				t.Errorf("ActiveParams (%d) should be < ParameterCount (%d) for MoE",
					estimate.ActiveParams, estimate.ParameterCount)
			}

			if estimate.RecommendedMemoryMB < tt.wantMinMemMB || estimate.RecommendedMemoryMB > tt.wantMaxMemMB {
				t.Errorf("RecommendedMemoryMB = %d, want between %d and %d",
					estimate.RecommendedMemoryMB, tt.wantMinMemMB, tt.wantMaxMemMB)
			}
		})
	}
}

func TestEstimateModelMemory_CodeModels(t *testing.T) {
	tests := []struct {
		name         string
		modelName    string
		format       string
		quantization string
		wantMinMemMB int64
		wantMaxMemMB int64
	}{
		{
			name:         "CodeLlama 7B Q4",
			modelName:    "codellama/CodeLlama-7b-Instruct-hf",
			format:       "gguf",
			quantization: "q4_k_m",
			wantMinMemMB: 3000,
			wantMaxMemMB: 8000,
		},
		{
			name:         "Qwen2.5-Coder 7B Q4",
			modelName:    "Qwen/Qwen2.5-Coder-7B-Instruct",
			format:       "gguf",
			quantization: "q4_k_m",
			wantMinMemMB: 3000,
			wantMaxMemMB: 8000,
		},
		{
			name:         "StarCoder2 15B Q4",
			modelName:    "bigcode/starcoder2-15b",
			format:       "gguf",
			quantization: "q4_k_m",
			wantMinMemMB: 6000,
			wantMaxMemMB: 15000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			estimate := EstimateModelMemory(tt.modelName, tt.format, tt.quantization)

			if estimate == nil {
				t.Fatal("EstimateModelMemory returned nil")
			}

			if estimate.Architecture != ArchCodeModel {
				t.Errorf("Architecture = %v, want %v", estimate.Architecture, ArchCodeModel)
			}

			if estimate.RecommendedMemoryMB < tt.wantMinMemMB || estimate.RecommendedMemoryMB > tt.wantMaxMemMB {
				t.Errorf("RecommendedMemoryMB = %d, want between %d and %d",
					estimate.RecommendedMemoryMB, tt.wantMinMemMB, tt.wantMaxMemMB)
			}
		})
	}
}

func TestEstimateModelMemory_EmbeddingModels(t *testing.T) {
	tests := []struct {
		name         string
		modelName    string
		format       string
		quantization string
		wantMinMemMB int64
		wantMaxMemMB int64
	}{
		{
			name:         "Nomic Embed Text",
			modelName:    "nomic-ai/nomic-embed-text-v1.5",
			format:       "gguf",
			quantization: "fp16",
			wantMinMemMB: 200,
			wantMaxMemMB: 1200, // Includes base memory (~260MB) + activation overhead (256MB) + general overhead (512MB)
		},
		{
			name:         "BGE Large",
			modelName:    "BAAI/bge-large-en-v1.5",
			format:       "gguf",
			quantization: "fp16",
			wantMinMemMB: 500,
			wantMaxMemMB: 2000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			estimate := EstimateModelMemory(tt.modelName, tt.format, tt.quantization)

			if estimate == nil {
				t.Fatal("EstimateModelMemory returned nil")
			}

			if estimate.Architecture != ArchEmbedding {
				t.Errorf("Architecture = %v, want %v", estimate.Architecture, ArchEmbedding)
			}

			// Embedding models should have zero or minimal KV cache
			if estimate.KVCachePerTokenMB > 0.001 {
				t.Errorf("KVCachePerTokenMB = %f, want ~0 for embedding model", estimate.KVCachePerTokenMB)
			}

			if estimate.RecommendedMemoryMB < tt.wantMinMemMB || estimate.RecommendedMemoryMB > tt.wantMaxMemMB {
				t.Errorf("RecommendedMemoryMB = %d, want between %d and %d",
					estimate.RecommendedMemoryMB, tt.wantMinMemMB, tt.wantMaxMemMB)
			}
		})
	}
}

func TestGetQuantizationMultiplier(t *testing.T) {
	tests := []struct {
		quant    string
		wantMult float64
	}{
		{"fp16", 1.0},
		{"FP16", 1.0},
		{"bf16", 1.0},
		{"q8_0", 0.5},
		{"Q8_0", 0.5},
		{"q4_k_m", 0.25},
		{"Q4_K_M", 0.25},
		{"q3_k_m", 0.19},
		{"q2_k", 0.125},
		{"unknown", 1.0},
	}

	for _, tt := range tests {
		t.Run(tt.quant, func(t *testing.T) {
			mult := getQuantizationMultiplier(tt.quant)
			if mult != tt.wantMult {
				t.Errorf("getQuantizationMultiplier(%q) = %f, want %f", tt.quant, mult, tt.wantMult)
			}
		})
	}
}

func TestVLMMemoryVsDenseTransformer(t *testing.T) {
	// VLM should require more memory than equivalent dense transformer
	vlmEstimate := EstimateModelMemory("Qwen/Qwen2-VL-7B-Instruct", "gguf", "q4_k_m")
	denseEstimate := EstimateModelMemory("Qwen/Qwen2-7B-Instruct", "gguf", "q4_k_m")

	if vlmEstimate == nil || denseEstimate == nil {
		t.Fatal("EstimateModelMemory returned nil")
	}

	// VLM should have additional vision memory
	if vlmEstimate.VisionMemoryMB <= 0 {
		t.Error("VLM should have VisionMemoryMB > 0")
	}

	if denseEstimate.VisionMemoryMB != 0 {
		t.Error("Dense transformer should have VisionMemoryMB = 0")
	}

	// VLM recommended memory should be higher (due to vision overhead)
	if vlmEstimate.RecommendedMemoryMB <= denseEstimate.RecommendedMemoryMB {
		t.Errorf("VLM RecommendedMemoryMB (%d) should be > Dense (%d)",
			vlmEstimate.RecommendedMemoryMB, denseEstimate.RecommendedMemoryMB)
	}
}

func TestMoEMemoryCharacteristics(t *testing.T) {
	// MoE models should have total params > active params
	estimate := EstimateModelMemory("mistralai/Mixtral-8x7B-v0.1", "gguf", "q4_k_m")

	if estimate == nil {
		t.Fatal("EstimateModelMemory returned nil")
	}

	if estimate.Architecture != ArchMoE {
		t.Errorf("Architecture = %v, want %v", estimate.Architecture, ArchMoE)
	}

	// Total params should be ~46.7B for Mixtral 8x7B
	if estimate.ParameterCount < 40_000_000_000 || estimate.ParameterCount > 50_000_000_000 {
		t.Errorf("ParameterCount = %d, want ~46.7B", estimate.ParameterCount)
	}

	// Active params should be ~12.9B (2 experts of 8)
	if estimate.ActiveParams >= estimate.ParameterCount {
		t.Errorf("ActiveParams (%d) should be < ParameterCount (%d)",
			estimate.ActiveParams, estimate.ParameterCount)
	}

	expectedActiveB := 12.9 * 1e9
	tolerance := 2.0 * 1e9
	if float64(estimate.ActiveParams) < expectedActiveB-tolerance || float64(estimate.ActiveParams) > expectedActiveB+tolerance {
		t.Errorf("ActiveParams = %d, want ~12.9B", estimate.ActiveParams)
	}
}

func TestKnownModelConfidence(t *testing.T) {
	// Known models should have high confidence
	knownEstimate := EstimateModelMemory("meta-llama/Llama-3-8B-Instruct", "gguf", "q4_k_m")
	unknownEstimate := EstimateModelMemory("some-random/unknown-model-7b", "gguf", "q4_k_m")

	if knownEstimate.Confidence < 0.9 {
		t.Errorf("Known model confidence = %f, want >= 0.9", knownEstimate.Confidence)
	}

	if knownEstimate.EstimationSource != "known_model" {
		t.Errorf("EstimationSource = %s, want 'known_model'", knownEstimate.EstimationSource)
	}

	if unknownEstimate.Confidence > 0.7 {
		t.Errorf("Unknown model confidence = %f, want <= 0.7", unknownEstimate.Confidence)
	}
}
