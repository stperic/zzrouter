package server

import (
	"testing"

	"github.com/gin-gonic/gin"
)

// TestEnrichAndGroupAppsByNode tests provider grouping and filtering
func TestEnrichAndGroupAppsByNode(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name            string
		providers       []map[string]any
		requestedFormat string
		source          string
		expectedNodes   int
		expectedMsg     string
	}{
		{
			name: "Filter by GGUF format",
			providers: []map[string]any{
				{"key": "ollama-1", "name": "Ollama", "type": "ollama", "running": true, "node": "host1"},
				{"key": "vllm-1", "name": "vLLM", "type": "vllm", "running": true, "node": "host1"},
				{"key": "llama-cpp-1", "name": "Llama.cpp", "type": "llama.cpp", "running": true, "node": "host2"},
			},
			requestedFormat: "gguf",
			source:          "huggingface",
			expectedNodes:   1, // Only host2 (ollama filtered by source)
			expectedMsg:     "Should return only hosts with GGUF-compatible apps (excluding Ollama for non-Ollama source)",
		},
		{
			name: "Ollama source - only Ollama apps",
			providers: []map[string]any{
				{"key": "ollama-1", "name": "Ollama", "type": "ollama", "running": true, "node": "host1"},
				{"key": "vllm-1", "name": "vLLM", "type": "vllm", "running": true, "node": "host1"},
				{"key": "ollama-2", "name": "Ollama", "type": "ollama", "running": true, "node": "host2"},
			},
			requestedFormat: "gguf",
			source:          "ollama",
			expectedNodes:   2, // host1 and host2 with Ollama only
			expectedMsg:     "Should return only Ollama apps for Ollama source",
		},
		{
			name: "All formats (*)",
			providers: []map[string]any{
				{"key": "ollama-1", "name": "Ollama", "type": "ollama", "running": true, "node": "host1"},
				{"key": "vllm-1", "name": "vLLM", "type": "vllm", "running": true, "node": "host1"},
			},
			requestedFormat: "*",
			source:          "huggingface",
			expectedNodes:   1, // host1 with vllm only (ollama filtered by source)
			expectedMsg:     "Should return all compatible apps when format is *",
		},
		{
			name: "No compatible apps",
			providers: []map[string]any{
				{"key": "ollama-1", "name": "Ollama", "type": "ollama", "running": true, "node": "host1"},
			},
			requestedFormat: "mlx",
			source:          "huggingface",
			expectedNodes:   0, // No MLX apps, and Ollama filtered by source
			expectedMsg:     "Should return no hosts when no compatible apps exist",
		},
		{
			name: "Non-Ollama source excludes Ollama",
			providers: []map[string]any{
				{"key": "ollama-1", "name": "Ollama", "type": "ollama", "running": true, "node": "host1"},
				{"key": "vllm-1", "name": "vLLM", "type": "vllm", "running": true, "node": "host2"},
			},
			requestedFormat: "*",
			source:          "huggingface",
			expectedNodes:   1, // Only host2 (Ollama excluded)
			expectedMsg:     "Should exclude Ollama apps for non-Ollama sources",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// V1 function enrichAndGroupAppsByNodeWithDisk removed - test disabled
			// TODO: Rewrite test for V2 compatible hosts handler if needed
			t.Skip("V1 function removed - test needs to be rewritten for V2")
		})
	}
}

// TestHandleListNodesCompatible_Integration tests the full endpoint
// Skipped: Requires authentication setup - development mode bypass not working in test context
func TestHandleListNodesCompatible_Integration(t *testing.T) {
	t.Skip("Skipped: Requires authentication setup - development mode bypass not working in test context")
}

// TestOllamaOneToOneRule tests the Ollama bidirectional filtering
func TestOllamaOneToOneRule(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name             string
		source           string
		providerType     string
		shouldBeIncluded bool
		description      string
	}{
		{
			name:             "Ollama source with Ollama provider",
			source:           "ollama",
			providerType:     "ollama",
			shouldBeIncluded: true,
			description:      "Ollama models should use Ollama provider",
		},
		{
			name:             "Ollama source with vLLM provider",
			source:           "ollama",
			providerType:     "vllm",
			shouldBeIncluded: false,
			description:      "Ollama models should NOT use non-Ollama apps",
		},
		{
			name:             "HuggingFace source with Ollama provider",
			source:           "huggingface",
			providerType:     "ollama",
			shouldBeIncluded: false,
			description:      "Non-Ollama models should NOT use Ollama provider",
		},
		{
			name:             "HuggingFace source with vLLM provider",
			source:           "huggingface",
			providerType:     "vllm",
			shouldBeIncluded: true,
			description:      "Non-Ollama models can use other apps",
		},
		{
			name:             "Empty source with Ollama provider",
			source:           "",
			providerType:     "ollama",
			shouldBeIncluded: true,
			description:      "No source filter should include Ollama",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// V1 function enrichAndGroupAppsByNodeWithDisk removed - test disabled
			// TODO: Rewrite test for V2 compatible hosts handler if needed
			t.Skip("V1 function removed - test needs to be rewritten for V2")
		})
	}
}
