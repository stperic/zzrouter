package filesystem

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stperic/zzrouter/pkg/model/integrity"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// A GGUF-only repository (no config.json) groups like every other scan:
// a split set is one model with the summed size, and an incomplete set
// cannot load, so it is not listed.
func TestScanModelsGroupsGGUFShards(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	repo := filepath.Join(root, "Qwen", "Qwen2.5-7B-Instruct-GGUF")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	for name, size := range map[string]int{
		"qwen2.5-7b-instruct-q5_k_m-00001-of-00002.gguf": 3,
		"qwen2.5-7b-instruct-q5_k_m-00002-of-00002.gguf": 2,
		"qwen2.5-7b-instruct-q3_k_m.gguf":                4,
		"qwen2.5-7b-instruct-q8_0-00001-of-00003.gguf":   1,
	} {
		require.NoError(t, os.WriteFile(filepath.Join(repo, name), make([]byte, size), 0o644))
	}

	models, err := ScanModels(root)
	require.NoError(t, err)
	got := map[string]int64{}
	for _, m := range models {
		got[m.Name] = m.Size
		assert.Equal(t, "Qwen/Qwen2.5-7B-Instruct-GGUF", m.SourceID)
	}
	assert.Equal(t, map[string]int64{"qwen2.5-7b-instruct-q5_k_m": 5, "qwen2.5-7b-instruct-q3_k_m": 4}, got)
}

// A file the manifest assigns to a feature (a vision projector) is not a
// model, and a manifest that cannot be read hides nothing: the weights
// still list, grouped.
func TestScanModelsGGUFFeatureFilesAndBadManifest(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	repo := filepath.Join(root, "org", "vision-GGUF")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	for _, name := range []string{"m-q4_k_m.gguf", "mmproj-F16.gguf"} {
		require.NoError(t, os.WriteFile(filepath.Join(repo, name), []byte("x"), 0o644))
	}
	require.NoError(t, integrity.WriteManifest(repo, &integrity.ModelManifest{Version: 1, Files: []integrity.FileChecksum{
		{RelativePath: "m-q4_k_m.gguf", Size: 1}, {RelativePath: "mmproj-F16.gguf", Size: 1, Feature: "vision"},
	}}))

	names := func() []string {
		models, err := ScanModels(root)
		require.NoError(t, err)
		var out []string
		for _, m := range models {
			out = append(out, m.Name)
		}
		return out
	}
	assert.Equal(t, []string{"m-q4_k_m"}, names())

	require.NoError(t, os.WriteFile(filepath.Join(repo, integrity.ManifestFileName), []byte("{not json"), 0o644))
	assert.ElementsMatch(t, []string{"m-q4_k_m", "mmproj-F16"}, names(), "an unreadable manifest tags nothing")
}
