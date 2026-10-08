package modelregistry

import (
	"testing"

	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
)

// ============================================================================
// Utils Tests - ParseModelID, GetProviderFormat, MatchesModelPattern
// ============================================================================

func TestMatchesModelPattern(t *testing.T) {
	tests := []struct {
		name      string
		modelName string
		pattern   string
		expected  bool
	}{
		// Exact match
		{"exact match", "Qwen/Qwen3-0.6B", "Qwen/Qwen3-0.6B", true},
		{"exact match case insensitive", "qwen/qwen3-0.6b", "Qwen/Qwen3-0.6B", true},

		// Full wildcard
		{"full wildcard", "any/model", "*", true},

		// Prefix wildcard
		{"prefix wildcard", "lmstudio-community/Phi-4-GGUF", "lmstudio-community/*", true},
		{"prefix wildcard no match", "TheBloke/Phi-4-GGUF", "lmstudio-community/*", false},

		// Suffix wildcard
		{"suffix wildcard", "meta-llama/Llama-3", "*/Llama-3", true},
		{"suffix wildcard no match", "meta-llama/Phi-4", "*/Llama-3", false},

		// Middle wildcard
		{"middle wildcard", "org/model-suffix", "org/*suffix", true},
		{"middle wildcard no match", "org/model-other", "org/*suffix", false},

		// Edge cases
		{"empty model", "", "pattern", false},
		{"empty pattern", "model", "", false},
		{"both empty", "", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MatchesModelPattern(tt.modelName, tt.pattern)
			if got != tt.expected {
				t.Errorf("MatchesModelPattern(%q, %q) = %v, want %v", tt.modelName, tt.pattern, got, tt.expected)
			}
		})
	}
}

// ============================================================================
// Identifier Parser Tests
// ============================================================================

func TestParse_BasicFormats(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantNode  string
		wantRepo  string
		wantModel string
		wantTag   string
		wantErr   bool
	}{
		{
			name:      "simple model",
			input:     "llama2",
			wantModel: "llama2",
		},
		{
			name:      "model with tag",
			input:     "llama2:7b",
			wantModel: "llama2",
			wantTag:   "7b",
		},
		{
			name:      "repo/model",
			input:     "ollama/llama2",
			wantRepo:  "ollama",
			wantModel: "llama2",
		},
		{
			name:      "org/model format",
			input:     "meta-llama/Llama-3-8B",
			wantModel: "meta-llama/Llama-3-8B",
		},
		{
			name:      "hf repo with org/model",
			input:     "hf/meta-llama/Llama-3-8B",
			wantRepo:  "huggingface",
			wantModel: "meta-llama/Llama-3-8B",
		},
		{
			name:      "empty input",
			input:     "",
			wantModel: "",
		},
		{
			name:      "whitespace only",
			input:     "   ",
			wantModel: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, err := Parse(tt.input, ParseOpts{})
			if (err != nil) != tt.wantErr {
				t.Errorf("Parse() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr {
				return
			}
			if id.Node != tt.wantNode {
				t.Errorf("Node = %q, want %q", id.Node, tt.wantNode)
			}
			if id.Repository != tt.wantRepo {
				t.Errorf("Repository = %q, want %q", id.Repository, tt.wantRepo)
			}
			if id.ModelName != tt.wantModel {
				t.Errorf("ModelName = %q, want %q", id.ModelName, tt.wantModel)
			}
			if id.Tag != tt.wantTag {
				t.Errorf("Tag = %q, want %q", id.Tag, tt.wantTag)
			}
		})
	}
}

func TestParse_NodeFormats(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantNode  string
		wantModel string
		wantErr   bool
	}{
		{
			name:      "host-first format",
			input:     "localhost::llama2",
			wantNode:  "localhost",
			wantModel: "llama2",
		},
		{
			name:      "host-first with port",
			input:     "10.0.1.5:8080::model",
			wantNode:  "10.0.1.5:8080",
			wantModel: "model",
		},
		{
			name:      "model-first format",
			input:     "llama2:7b@my-server",
			wantNode:  "my-server",
			wantModel: "llama2",
		},
		{
			name:    "invalid - both separators",
			input:   "host::model@other",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, err := Parse(tt.input, ParseOpts{})
			if (err != nil) != tt.wantErr {
				t.Errorf("Parse() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr {
				return
			}
			if id.Node != tt.wantNode {
				t.Errorf("Node = %q, want %q", id.Node, tt.wantNode)
			}
			if id.ModelName != tt.wantModel {
				t.Errorf("ModelName = %q, want %q", id.ModelName, tt.wantModel)
			}
		})
	}
}

// ============================================================================
// Format Detection Tests
// ============================================================================
//
// TestIsModelFile has moved to pkg/modelregistry/source/filesystem
// alongside the code it exercises.

func TestExtractQuantization(t *testing.T) {
	tests := []struct {
		filename string
		expected string
	}{
		{"model-Q4_K_M.gguf", "Q4_K_M"},
		{"model-Q8_0.gguf", "Q8_0"},
		{"model-Q5_K_S.gguf", "Q5_K_S"},
		{"model-Q3_K_L.gguf", "Q3_K_L"},
		{"model-Q4_0.gguf", "Q4_0"},
		{"model-Q5_1.gguf", "Q5_1"},
		{"model-q4_k_m.gguf", "Q4_K_M"}, // Case insensitive
		{"model.gguf", ""},              // No quantization
		{"model-fp16.gguf", ""},         // Not GGUF quantization
	}

	for _, tt := range tests {
		t.Run(tt.filename, func(t *testing.T) {
			got := metadata.ExtractQuantization(tt.filename)
			if got != tt.expected {
				t.Errorf("metadata.ExtractQuantization(%q) = %q, want %q", tt.filename, got, tt.expected)
			}
		})
	}
}

// ============================================================================
// metadata.ModelMetadata Tests
// ============================================================================

func TestModelMetadata_ToOllamaFormat(t *testing.T) {
	md := &metadata.ModelMetadata{
		Name:         "llama2",
		Size:         1024 * 1024 * 1024,
		Format:       metadata.FormatGGUF,
		Quantization: "Q4_K_M",
		SourceRepo:   metadata.SourceHuggingFace,
		SourceID:     "TheBloke/Llama-2-7B-Chat-GGUF",
	}

	result := md.ToOllamaFormat("localhost")

	if result["name"] != "llama2" {
		t.Errorf("name = %v, want %v", result["name"], "llama2")
	}
	if result["model"] != "llama2" {
		t.Errorf("model = %v, want %v", result["model"], "llama2")
	}
	if result["node"] != "localhost" {
		t.Errorf("node = %v, want %v", result["node"], "localhost")
	}

	details, ok := result["details"].(map[string]any)
	if !ok {
		t.Fatal("details should be a map")
	}
	if details["format"] != metadata.FormatGGUF {
		t.Errorf("details.format = %v, want %v", details["format"], metadata.FormatGGUF)
	}
}

func TestModelMetadata_ToOllamaFormat_WithNode(t *testing.T) {
	// Test when metadata has its own node
	md := &metadata.ModelMetadata{
		Name: "model",
		Node: "remote-server",
	}

	result := md.ToOllamaFormat("localhost")

	// Should use metadata's node, not the provided default
	if result["node"] != "remote-server" {
		t.Errorf("node = %v, want %v", result["node"], "remote-server")
	}
}

func TestModelMetadata_ToOllamaFormat_WithExtra(t *testing.T) {
	md := &metadata.ModelMetadata{
		Name:   "model",
		Format: metadata.FormatGGUF,
		Extra: map[string]any{
			"details": map[string]any{
				"family":     "llama",
				"parameters": "7B",
			},
		},
	}

	result := md.ToOllamaFormat("")

	details, ok := result["details"].(map[string]any)
	if !ok {
		t.Fatal("details should be a map")
	}
	if details["format"] != metadata.FormatGGUF {
		t.Errorf("details.format = %v, want %v", details["format"], metadata.FormatGGUF)
	}
}

// ============================================================================
// Type Constants Tests
// ============================================================================

func TestFormatConstants(t *testing.T) {
	// Verify format constants are defined correctly
	tests := []struct {
		format   string
		expected string
	}{
		{metadata.FormatGGUF, "gguf"},
		{metadata.FormatSafetensors, "safetensors"},
		{metadata.FormatHuggingFace, "hf_transformers"},
		{metadata.FormatMLX, "mlx"},
		{metadata.FormatPyTorch, "pytorch"},
		{metadata.FormatONNX, "onnx"},
		{metadata.FormatTensorRTLLM, "tensorrt_llm"},
	}

	for _, tt := range tests {
		if tt.format != tt.expected {
			t.Errorf("Format constant %q = %q, want %q", tt.format, tt.format, tt.expected)
		}
	}
}

func TestSourceConstants(t *testing.T) {
	// Verify source constants are defined correctly
	tests := []struct {
		source   string
		expected string
	}{
		{metadata.SourceHuggingFace, "huggingface"},
		{metadata.SourceOllama, "ollama"},
		{metadata.SourceLocal, "local"},
		{metadata.SourceUnknown, "unknown"},
	}

	for _, tt := range tests {
		if tt.source != tt.expected {
			t.Errorf("Source constant %q = %q, want %q", tt.source, tt.source, tt.expected)
		}
	}
}
