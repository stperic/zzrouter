package filesystem

import (
	"testing"
)

// TestIsModelFile pins the extension matrix and the variant-directory
// skip rules. Moved from pkg/modelregistry/modelregistry_test.go as
// part of the source-package split.
func TestIsModelFile(t *testing.T) {
	t.Parallel()

	tests := []struct {
		path     string
		expected bool
	}{
		{"model.gguf", true},
		{"model.safetensors", true},
		{"model.bin", true},
		{"model.onnx", true},
		{"model.engine", true},
		{"model.llm", true},
		{"model.txt", false},
		{"model.json", false},
		{"config.yaml", false},
		// Skip files in variant directories
		{"onnx/model.onnx", false},
		{"openvino/model.bin", false},
		{"coreml/model.bin", false},
		{"tflite/model.bin", false},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			t.Parallel()
			got := IsModelFile(tt.path)
			if got != tt.expected {
				t.Errorf("IsModelFile(%q) = %v, want %v", tt.path, got, tt.expected)
			}
		})
	}
}
