// Package cli provides extended tests for cluster commands.
// Basic command tests are in signin_test.go; this file contains extended tests
// for helper functions and detailed flag verification.
package clientcli

import (
	"testing"

	shared "github.com/stperic/zzrouter/internal/cli/clientcli/shared"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStatusCmd_Flags verifies the flag configuration for status command
func TestStatusCmd_Flags(t *testing.T) {
	cmd := NewStatusCmd()
	require.NotNil(t, cmd)

	flags := cmd.Flags()

	verboseFlag := flags.Lookup("verbose")
	require.NotNil(t, verboseFlag, "expected 'verbose' flag to exist")
	assert.Equal(t, "v", verboseFlag.Shorthand)

	jsonFlag := flags.Lookup("json")
	require.NotNil(t, jsonFlag, "expected 'json' flag to exist")
	assert.Equal(t, "j", jsonFlag.Shorthand)
}

// TestNodesCmd_Flags verifies the flag configuration for nodes command
func TestNodesCmd_Flags(t *testing.T) {
	cmd := NewNodesCmd()
	require.NotNil(t, cmd)

	flags := cmd.Flags()

	detailedFlag := flags.Lookup("detailed")
	require.NotNil(t, detailedFlag, "expected 'detailed' flag to exist")
	assert.Equal(t, "d", detailedFlag.Shorthand)

	fieldsFlag := flags.Lookup("fields")
	require.NotNil(t, fieldsFlag, "expected 'fields' flag to exist")
	assert.Equal(t, "f", fieldsFlag.Shorthand)

	resourcesFlag := flags.Lookup("resources")
	require.NotNil(t, resourcesFlag, "expected 'resources' flag to exist")
	assert.Equal(t, "r", resourcesFlag.Shorthand)

	refreshFlag := flags.Lookup("refresh")
	require.NotNil(t, refreshFlag, "expected 'refresh' flag to exist")
	assert.Equal(t, "R", refreshFlag.Shorthand)

	jsonFlag := flags.Lookup("json")
	require.NotNil(t, jsonFlag, "expected 'json' flag to exist")
	assert.Equal(t, "j", jsonFlag.Shorthand)
}

// TestAppsCmd_Flags verifies the flag configuration for apps command
func TestAppsCmd_Flags(t *testing.T) {
	cmd := NewProvidersCmd()
	require.NotNil(t, cmd)

	flags := cmd.Flags()

	nodeFlag := flags.Lookup("node")
	require.NotNil(t, nodeFlag, "expected 'node' flag to exist")
	assert.Equal(t, "n", nodeFlag.Shorthand)

	providerFlag := flags.Lookup("provider")
	require.NotNil(t, providerFlag, "expected 'provider' flag to exist")
	assert.Equal(t, "a", providerFlag.Shorthand)

	jsonFlag := flags.Lookup("json")
	require.NotNil(t, jsonFlag, "expected 'json' flag to exist")
	assert.Equal(t, "j", jsonFlag.Shorthand)
}

func TestFormatUptimeDuration(t *testing.T) {
	tests := []struct {
		name     string
		seconds  int
		expected string
	}{
		{
			name:     "zero seconds",
			seconds:  0,
			expected: "0s",
		},
		{
			name:     "less than a minute",
			seconds:  45,
			expected: "45s",
		},
		{
			name:     "exactly one minute",
			seconds:  60,
			expected: "1m",
		},
		{
			name:     "multiple minutes",
			seconds:  300, // 5 minutes
			expected: "5m",
		},
		{
			name:     "exactly one hour",
			seconds:  3600,
			expected: "1h 0m",
		},
		{
			name:     "hours and minutes",
			seconds:  3660, // 1 hour 1 minute
			expected: "1h 1m",
		},
		{
			name:     "multiple hours",
			seconds:  7320, // 2 hours 2 minutes
			expected: "2h 2m",
		},
		{
			name:     "exactly one day",
			seconds:  86400, // 24 hours
			expected: "1d 0h",
		},
		{
			name:     "days and hours",
			seconds:  90000, // 25 hours
			expected: "1d 1h",
		},
		{
			name:     "multiple days",
			seconds:  259200, // 3 days
			expected: "3d 0h",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := shared.FormatUptimeDuration(tt.seconds)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestGetStatusIcon(t *testing.T) {
	tests := []struct {
		name     string
		status   string
		expected string
	}{
		{
			name:     "healthy status",
			status:   "healthy",
			expected: "✓",
		},
		{
			name:     "empty status (default healthy)",
			status:   "",
			expected: "✓",
		},
		{
			name:     "unhealthy status",
			status:   "unhealthy",
			expected: "✗",
		},
		{
			name:     "any other status",
			status:   "degraded",
			expected: "✗",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := getStatusIcon(tt.status)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestGetBoolFieldValue(t *testing.T) {
	tests := []struct {
		name     string
		m        map[string]any
		key      string
		expected bool
	}{
		{
			name:     "key exists with true",
			m:        map[string]any{"enabled": true},
			key:      "enabled",
			expected: true,
		},
		{
			name:     "key exists with false",
			m:        map[string]any{"enabled": false},
			key:      "enabled",
			expected: false,
		},
		{
			name:     "key does not exist",
			m:        map[string]any{"other": "value"},
			key:      "enabled",
			expected: false,
		},
		{
			name:     "key exists but not bool type",
			m:        map[string]any{"enabled": "true"},
			key:      "enabled",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := shared.GetBoolFieldValue(tt.m, tt.key)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestGetStrArrayField(t *testing.T) {
	tests := []struct {
		name     string
		m        map[string]any
		key      string
		expected []string
	}{
		{
			name:     "key exists with string array",
			m:        map[string]any{"formats": []any{"gguf", "mlx", "safetensors"}},
			key:      "formats",
			expected: []string{"gguf", "mlx", "safetensors"},
		},
		{
			name:     "key does not exist",
			m:        map[string]any{"other": "value"},
			key:      "formats",
			expected: nil,
		},
		{
			name:     "key exists but empty array",
			m:        map[string]any{"formats": []any{}},
			key:      "formats",
			expected: []string{},
		},
		{
			name:     "key exists but not array type",
			m:        map[string]any{"formats": "gguf"},
			key:      "formats",
			expected: nil,
		},
		{
			name:     "array with mixed types (only strings extracted)",
			m:        map[string]any{"formats": []any{"gguf", 123, "mlx"}},
			key:      "formats",
			expected: []string{"gguf", "mlx"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := shared.GetStrArrayField(tt.m, tt.key)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// NOTE: TestParseURL and TestMaskKey are defined in signin_test.go

func TestFilterProviders(t *testing.T) {
	providers := []shared.ProviderInfo{
		{Name: "ollama", Node: "localhost"},
		{Name: "vllm", Node: "localhost"},
		{Name: "llama-cpp", Node: "gpu-server"},
		{Name: "mlx", Node: "macbook"},
	}

	tests := []struct {
		name     string
		pattern  string
		expected []string // Expected provider names
	}{
		{
			name:     "empty pattern returns all",
			pattern:  "",
			expected: []string{"ollama", "vllm", "llama-cpp", "mlx"},
		},
		{
			name:     "exact match",
			pattern:  "ollama",
			expected: []string{"ollama"},
		},
		{
			name:     "partial match (contains)",
			pattern:  "llama",
			expected: []string{"ollama", "llama-cpp"}, // both contain "llama"
		},
		{
			name:     "partial match specific",
			pattern:  "llama-",
			expected: []string{"llama-cpp"},
		},
		{
			name:     "case insensitive",
			pattern:  "OLLAMA",
			expected: []string{"ollama"},
		},
		{
			name:     "no match",
			pattern:  "nonexistent",
			expected: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := shared.FilterProviders(providers, tt.pattern)

			actualNames := make([]string, len(result))
			for i, prov := range result {
				actualNames[i] = prov.Name
			}

			assert.Equal(t, tt.expected, actualNames)
		})
	}
}
