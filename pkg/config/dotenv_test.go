package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// LoadDotEnvFrom Tests
// =============================================================================

func TestLoadDotEnvFrom_EmptyFile(t *testing.T) {
	// Create temp directory with empty .env
	tempDir, err := os.MkdirTemp("", "dotenv-test")
	require.NoError(t, err)
	defer func() { _ = os.RemoveAll(tempDir) }()

	envPath := filepath.Join(tempDir, ".env")
	require.NoError(t, os.WriteFile(envPath, []byte(""), 0600))

	count, err := LoadDotEnvFrom(envPath)
	require.NoError(t, err)
	assert.Equal(t, 0, count)
}

func TestLoadDotEnvFrom_ValidFile(t *testing.T) {
	// Create temp directory with valid .env
	tempDir, err := os.MkdirTemp("", "dotenv-test")
	require.NoError(t, err)
	defer func() { _ = os.RemoveAll(tempDir) }()

	// Save original env values
	origVal1 := os.Getenv("DOTENV_TEST_VAR1")
	origVal2 := os.Getenv("DOTENV_TEST_VAR2")
	defer func() {
		_ = os.Setenv("DOTENV_TEST_VAR1", origVal1)
		_ = os.Setenv("DOTENV_TEST_VAR2", origVal2)
	}()

	envContent := `
DOTENV_TEST_VAR1=value1
DOTENV_TEST_VAR2=value2
`
	envPath := filepath.Join(tempDir, ".env")
	require.NoError(t, os.WriteFile(envPath, []byte(envContent), 0600))

	count, err := LoadDotEnvFrom(envPath)
	require.NoError(t, err)
	assert.Equal(t, 2, count)

	assert.Equal(t, "value1", os.Getenv("DOTENV_TEST_VAR1"))
	assert.Equal(t, "value2", os.Getenv("DOTENV_TEST_VAR2"))
}

func TestLoadDotEnvFrom_QuotedValues(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "dotenv-test")
	require.NoError(t, err)
	defer func() { _ = os.RemoveAll(tempDir) }()

	// Save original env values
	origVal := os.Getenv("DOTENV_TEST_QUOTED")
	defer func() { _ = os.Setenv("DOTENV_TEST_QUOTED", origVal) }()

	envContent := `
DOTENV_TEST_QUOTED="quoted value with spaces"
`
	envPath := filepath.Join(tempDir, ".env")
	require.NoError(t, os.WriteFile(envPath, []byte(envContent), 0600))

	count, err := LoadDotEnvFrom(envPath)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, count, 1)

	assert.Equal(t, "quoted value with spaces", os.Getenv("DOTENV_TEST_QUOTED"))
}

func TestLoadDotEnvFrom_Comments(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "dotenv-test")
	require.NoError(t, err)
	defer func() { _ = os.RemoveAll(tempDir) }()

	// Save original env values
	origVal := os.Getenv("DOTENV_TEST_ACTUAL")
	defer func() { _ = os.Setenv("DOTENV_TEST_ACTUAL", origVal) }()

	envContent := `
# This is a comment
DOTENV_TEST_ACTUAL=real_value
# Another comment
`
	envPath := filepath.Join(tempDir, ".env")
	require.NoError(t, os.WriteFile(envPath, []byte(envContent), 0600))

	count, err := LoadDotEnvFrom(envPath)
	require.NoError(t, err)
	assert.Equal(t, 1, count) // Only 1 actual variable

	assert.Equal(t, "real_value", os.Getenv("DOTENV_TEST_ACTUAL"))
}

func TestLoadDotEnvFrom_SingleQuotes(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "dotenv-test")
	require.NoError(t, err)
	defer func() { _ = os.RemoveAll(tempDir) }()

	// Save original env values
	origVal := os.Getenv("DOTENV_TEST_SINGLE")
	defer func() { _ = os.Setenv("DOTENV_TEST_SINGLE", origVal) }()

	envContent := `
DOTENV_TEST_SINGLE='single quoted value'
`
	envPath := filepath.Join(tempDir, ".env")
	require.NoError(t, os.WriteFile(envPath, []byte(envContent), 0600))

	count, err := LoadDotEnvFrom(envPath)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, count, 1)

	assert.Equal(t, "single quoted value", os.Getenv("DOTENV_TEST_SINGLE"))
}

func TestLoadDotEnvFrom_ExistingEnvNotOverwritten(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "dotenv-test")
	require.NoError(t, err)
	defer func() { _ = os.RemoveAll(tempDir) }()

	// Set an env var that already exists
	origVal := os.Getenv("DOTENV_TEST_EXISTING")
	_ = os.Setenv("DOTENV_TEST_EXISTING", "original_value")
	defer func() { _ = os.Setenv("DOTENV_TEST_EXISTING", origVal) }()

	envContent := `
DOTENV_TEST_EXISTING=new_value
`
	envPath := filepath.Join(tempDir, ".env")
	require.NoError(t, os.WriteFile(envPath, []byte(envContent), 0600))

	_, err = LoadDotEnvFrom(envPath)
	require.NoError(t, err)

	// Original value should be preserved (not overwritten)
	assert.Equal(t, "original_value", os.Getenv("DOTENV_TEST_EXISTING"))
}

// =============================================================================
// GetDotEnvLocations Tests
// =============================================================================

func TestGetDotEnvLocations(t *testing.T) {
	locations := GetDotEnvLocations()

	// Should return at least one location
	assert.NotEmpty(t, locations)

	// Should include current directory .env
	hasCurrentDir := false
	for _, loc := range locations {
		if filepath.Base(loc) == ".env" {
			hasCurrentDir = true
			break
		}
	}
	assert.True(t, hasCurrentDir, "should include .env in locations")
}

// =============================================================================
// unquoteValue Tests
// =============================================================================

func TestUnquoteValue(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"double quoted", `"hello world"`, "hello world"},
		{"single quoted", `'hello world'`, "hello world"},
		{"no quotes", "hello", "hello"},
		{"empty", "", ""},
		{"only opening double quote", `"hello`, `"hello`},
		{"only opening single quote", `'hello`, `'hello`},
		{"mismatched quotes", `"hello'`, `"hello'`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := unquoteValue(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}
