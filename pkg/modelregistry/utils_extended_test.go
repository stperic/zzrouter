package modelregistry

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGetModelsRootDir_EnvVar tests that ZZROUTER_MODEL_DIR env var takes priority.
func TestGetModelsRootDir_EnvVar(t *testing.T) {
	// Save original and restore after test
	original := os.Getenv("ZZROUTER_MODEL_DIR")
	defer func() { _ = os.Setenv("ZZROUTER_MODEL_DIR", original) }()

	testDir := t.TempDir()
	_ = os.Setenv("ZZROUTER_MODEL_DIR", testDir)

	dir, err := GetModelsRootDir()
	require.NoError(t, err)
	assert.Equal(t, testDir, dir)
}

// TestGetModelsRootDir_XDGDataHome tests XDG_DATA_HOME fallback.
func TestGetModelsRootDir_XDGDataHome(t *testing.T) {
	// Save originals and restore after test
	originalModelDir := os.Getenv("ZZROUTER_MODEL_DIR")
	originalXDG := os.Getenv("XDG_DATA_HOME")
	defer func() {
		_ = os.Setenv("ZZROUTER_MODEL_DIR", originalModelDir)
		_ = os.Setenv("XDG_DATA_HOME", originalXDG)
	}()

	// Clear ZZROUTER_MODEL_DIR to test XDG fallback
	_ = os.Unsetenv("ZZROUTER_MODEL_DIR")
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	if runtime.GOOS == "windows" {
		// Windows uses LOCALAPPDATA rather than the XDG convention.
		t.Setenv("APPDATA", t.TempDir())
		t.Setenv("LOCALAPPDATA", dataHome)
	}

	dir, err := GetModelsRootDir()
	require.NoError(t, err)
	if runtime.GOOS == "linux" && config.Paths().IsRoot() {
		// Root uses the machine layout, regardless of XDG_DATA_HOME.
		assert.Equal(t, filepath.Join(config.LinuxRootDir, "models"), dir)
		return
	}
	assert.Equal(t, filepath.Join(dataHome, "zzrouter", "models"), dir)
}

// TestGetModelsRootDir_DefaultPaths tests platform-specific default paths.
func TestGetModelsRootDir_DefaultPaths(t *testing.T) {
	// Save originals and restore after test
	originalModelDir := os.Getenv("ZZROUTER_MODEL_DIR")
	originalXDG := os.Getenv("XDG_DATA_HOME")
	defer func() {
		_ = os.Setenv("ZZROUTER_MODEL_DIR", originalModelDir)
		_ = os.Setenv("XDG_DATA_HOME", originalXDG)
	}()

	// Clear env vars to test defaults
	_ = os.Unsetenv("ZZROUTER_MODEL_DIR")
	_ = os.Unsetenv("XDG_DATA_HOME")

	dir, err := GetModelsRootDir()
	require.NoError(t, err)

	homeDir, _ := os.UserHomeDir()

	// Check platform-specific paths
	switch runtime.GOOS {
	case "darwin":
		assert.Contains(t, dir, "Library/Application Support/zzrouter/models")
	case "windows":
		// Windows uses config dir
		assert.Contains(t, dir, "zzrouter")
		assert.Contains(t, dir, "models")
	default:
		// Linux uses XDG default or legacy path
		expectedDefault := filepath.Join(homeDir, ".local", "share", "zzrouter", "models")
		// May also use legacy path if it exists
		if dir != expectedDefault {
			assert.Contains(t, dir, "zzrouter")
			assert.Contains(t, dir, "models")
		}
	}
}

// TestModelPath_Fields tests ModelPath helper methods.
func TestModelPath_Fields(t *testing.T) {
	path := &ModelPath{
		Organization: "meta-llama",
		ModelName:    "Llama-3-8B",
		Format:       metadata.FormatGGUF,
		FullPath:     "/test/models/meta-llama/Llama-3-8B",
		FileName:     "model.gguf",
	}

	// Test the path components are accessible
	assert.Equal(t, "meta-llama", path.Organization)
	assert.Equal(t, "Llama-3-8B", path.ModelName)
	assert.Equal(t, metadata.FormatGGUF, path.Format)
	assert.Contains(t, path.FullPath, "meta-llama")
	assert.Equal(t, "model.gguf", path.FileName)
}

// TestMatchesModelPattern_Wildcard_Extended tests asterisk patterns.
// Note: MatchesModelPattern only supports a single wildcard (*).
// Patterns with multiple wildcards (like *ama*) are not supported.
func TestMatchesModelPattern_Wildcard_Extended(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		match   bool
	}{
		{"match all", "*", true},
		{"prefix match", "llama*", true},
		{"suffix match", "*3.2", true},
		{"prefix and suffix", "lla*3.2", true}, // Single wildcard with prefix and suffix
		{"no prefix match", "mistral*", false},
		{"no suffix match", "*3.1", false},
		{"double wildcard not supported", "*ama*", false}, // Multiple wildcards not supported
	}

	modelName := "llama3.2"
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := MatchesModelPattern(modelName, tt.pattern)
			assert.Equal(t, tt.match, result)
		})
	}
}

// TestMatchesModelPattern_CaseInsensitive tests case-insensitive matching.
func TestMatchesModelPattern_CaseInsensitive(t *testing.T) {
	assert.True(t, MatchesModelPattern("Llama3.2", "llama3.2"))
	assert.True(t, MatchesModelPattern("llama3.2", "LLAMA3.2"))
	assert.True(t, MatchesModelPattern("LLaMA3.2", "llama*"))
}

// TestFormatConstants_Values tests that format constants are defined correctly.
func TestFormatConstants_Values(t *testing.T) {
	// Verify format constants are non-empty
	assert.NotEmpty(t, metadata.FormatGGUF)
	assert.NotEmpty(t, metadata.FormatHuggingFace)
	assert.NotEmpty(t, metadata.FormatTensorRTLLM)
	assert.NotEmpty(t, metadata.FormatONNX)

	// Verify they're unique
	formats := []string{metadata.FormatGGUF, metadata.FormatHuggingFace, metadata.FormatTensorRTLLM, metadata.FormatONNX}
	seen := make(map[string]bool)
	for _, f := range formats {
		assert.False(t, seen[f], "duplicate format constant: %s", f)
		seen[f] = true
	}
}
