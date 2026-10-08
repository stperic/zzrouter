package clientcli

import (
	"testing"
	"time"

	shared "github.com/stperic/zzrouter/internal/cli/clientcli/shared"

	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewListCmd(t *testing.T) {
	cmd := NewListCmd()

	require.NotNil(t, cmd, "NewListCmd() should not return nil")
	assert.Equal(t, "list [model_name]", cmd.Use)
	assert.NotEmpty(t, cmd.Short, "cmd.Short should not be empty")
	assert.NotEmpty(t, cmd.Long, "cmd.Long should not be empty")
	require.NotNil(t, cmd.RunE, "cmd.RunE should not be nil")

	// Verify expected flags exist
	flags := cmd.Flags()

	nodeFlag := flags.Lookup("node")
	require.NotNil(t, nodeFlag, "expected 'node' flag to exist")
	assert.Equal(t, "n", nodeFlag.Shorthand)
	assert.Equal(t, "*", nodeFlag.DefValue)

	repoFlag := flags.Lookup("registry")
	require.NotNil(t, repoFlag, "expected 'repo' flag to exist")
	assert.Equal(t, "r", repoFlag.Shorthand)

	providerFlag := flags.Lookup("provider")
	require.NotNil(t, providerFlag, "expected 'provider' flag to exist")
	assert.Equal(t, "a", providerFlag.Shorthand)
	assert.Equal(t, "*", providerFlag.DefValue)

	modelFlag := flags.Lookup("model")
	require.NotNil(t, modelFlag, "expected 'model' flag to exist")
	assert.Equal(t, "m", modelFlag.Shorthand)

	sortFlag := flags.Lookup("sort")
	require.NotNil(t, sortFlag, "expected 'sort' flag to exist")
	assert.Equal(t, "s", sortFlag.Shorthand)
	assert.Equal(t, "date", sortFlag.DefValue)

	refreshFlag := flags.Lookup("refresh")
	require.NotNil(t, refreshFlag, "expected 'refresh' flag to exist")
	assert.Equal(t, "R", refreshFlag.Shorthand)
}

func TestSortModelRows(t *testing.T) {
	// Helper to create test time
	now := time.Now()
	earlier := now.Add(-1 * time.Hour)
	evenEarlier := now.Add(-2 * time.Hour)

	tests := []struct {
		name     string
		models   []pkgClient.ModelMetadata
		sortBy   string
		expected []string // Expected order of model names
	}{
		{
			name: "sort by node ascending",
			models: []pkgClient.ModelMetadata{
				{Name: "model-c", Node: "node-z"},
				{Name: "model-a", Node: "node-a"},
				{Name: "model-b", Node: "node-m"},
			},
			sortBy:   "node",
			expected: []string{"model-a", "model-b", "model-c"},
		},
		{
			name: "sort by name descending via default when sortBy is unknown",
			models: []pkgClient.ModelMetadata{
				{Name: "model-c", Node: "node-z"},
				{Name: "model-a", Node: "node-a"},
			},
			sortBy:   "unknown-field",
			expected: []string{"model-c", "model-a"},
		},
		{
			name: "sort by repo ascending",
			models: []pkgClient.ModelMetadata{
				{Name: "model-a", SourceRepo: "ollama"},
				{Name: "model-b", SourceRepo: "huggingface"},
				{Name: "model-c", SourceRepo: "gguf"},
			},
			sortBy:   "registry",
			expected: []string{"model-c", "model-b", "model-a"},
		},
		{
			name: "sort by model name ascending",
			models: []pkgClient.ModelMetadata{
				{Name: "zephyr"},
				{Name: "llama"},
				{Name: "mistral"},
			},
			sortBy:   "model",
			expected: []string{"llama", "mistral", "zephyr"},
		},
		{
			name: "sort by name alias",
			models: []pkgClient.ModelMetadata{
				{Name: "zephyr"},
				{Name: "llama"},
			},
			sortBy:   "name",
			expected: []string{"llama", "zephyr"},
		},
		{
			name: "sort by size descending (largest first)",
			models: []pkgClient.ModelMetadata{
				{Name: "small", Size: 100},
				{Name: "large", Size: 1000},
				{Name: "medium", Size: 500},
			},
			sortBy:   "size",
			expected: []string{"large", "medium", "small"},
		},
		{
			name: "sort by date descending (most recent first)",
			models: []pkgClient.ModelMetadata{
				{Name: "oldest", ModifiedAt: evenEarlier},
				{Name: "newest", ModifiedAt: now},
				{Name: "middle", ModifiedAt: earlier},
			},
			sortBy:   "date",
			expected: []string{"newest", "middle", "oldest"},
		},
		{
			name: "sort by modified alias",
			models: []pkgClient.ModelMetadata{
				{Name: "oldest", ModifiedAt: evenEarlier},
				{Name: "newest", ModifiedAt: now},
			},
			sortBy:   "modified",
			expected: []string{"newest", "oldest"},
		},
		{
			name: "default sort (date)",
			models: []pkgClient.ModelMetadata{
				{Name: "oldest", ModifiedAt: evenEarlier},
				{Name: "newest", ModifiedAt: now},
			},
			sortBy:   "unknown",
			expected: []string{"newest", "oldest"},
		},
		{
			name:     "empty models slice",
			models:   []pkgClient.ModelMetadata{},
			sortBy:   "date",
			expected: []string{},
		},
		{
			name: "case insensitive node sort",
			models: []pkgClient.ModelMetadata{
				{Name: "model-b", Node: "Znode"},
				{Name: "model-a", Node: "anode"},
			},
			sortBy:   "node",
			expected: []string{"model-a", "model-b"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Make a copy to avoid mutating test data
			models := make([]pkgClient.ModelMetadata, len(tt.models))
			copy(models, tt.models)

			shared.SortModelRows(models, tt.sortBy)

			// Extract names in order
			actual := make([]string, len(models))
			for i, m := range models {
				actual[i] = m.Name
			}

			assert.Equal(t, tt.expected, actual, "models should be sorted correctly")
		})
	}
}

func TestFormatModelWithRegistry(t *testing.T) {
	tests := []struct {
		registry string
		model    string
		expected string
	}{
		{"cloudflare", "gemma-4-26b", "cloudflare:gemma-4-26b"},
		{"groq", "qwen3-32b", "groq:qwen3-32b"},
		{"ollama", "llama3:8b", "ollama:llama3:8b"},
		{"", "model-name", "model-name"},
	}

	for _, tt := range tests {
		t.Run(tt.registry+"/"+tt.model, func(t *testing.T) {
			result := shared.FormatModelWithRegistry(tt.registry, tt.model)
			assert.Equal(t, tt.expected, result)
		})
	}
}
