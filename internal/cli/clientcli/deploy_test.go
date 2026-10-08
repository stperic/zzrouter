package clientcli

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewDeployCmd(t *testing.T) {
	cmd := NewDeployCmd()

	require.NotNil(t, cmd, "NewDeployCmd() should not return nil")
	assert.Equal(t, "deploy [model]", cmd.Use)
	assert.NotEmpty(t, cmd.Short, "cmd.Short should not be empty")
	assert.NotEmpty(t, cmd.Long, "cmd.Long should not be empty")
	require.NotNil(t, cmd.RunE, "cmd.RunE should not be nil")

	// Verify expected flags exist
	flags := cmd.Flags()

	appFlag := flags.Lookup("provider")
	require.NotNil(t, appFlag, "expected 'app' flag to exist")
	assert.Equal(t, "a", appFlag.Shorthand)

	modelFlag := flags.Lookup("model")
	require.NotNil(t, modelFlag, "expected 'model' flag to exist")

	nodeFlag := flags.Lookup("node")
	require.NotNil(t, nodeFlag, "expected 'node' flag to exist")
	assert.Equal(t, "n", nodeFlag.Shorthand)

	fileFlag := flags.Lookup("file")
	require.NotNil(t, fileFlag, "expected 'file' flag to exist")

	// Multi-node flags
	toFlag := flags.Lookup("to")
	require.NotNil(t, toFlag, "expected 'to' flag to exist")

	toAllFlag := flags.Lookup("to-all")
	require.NotNil(t, toAllFlag, "expected 'to-all' flag to exist")

	// Verify subcommands exist
	subcommands := cmd.Commands()
	subcommandNames := make([]string, len(subcommands))
	for i, sub := range subcommands {
		subcommandNames[i] = sub.Name()
	}

	assert.Contains(t, subcommandNames, "status", "should have 'status' subcommand")
	assert.Contains(t, subcommandNames, "stop", "should have 'stop' subcommand")
}

func TestValidateNotSubcommand(t *testing.T) {
	tests := []struct {
		name        string
		modelName   string
		expectError bool
	}{
		{
			name:        "valid model name",
			modelName:   "llama2:7b",
			expectError: false,
		},
		{
			name:        "valid HuggingFace model",
			modelName:   "meta-llama/Llama-3-8B",
			expectError: false,
		},
		{
			name:        "reserved word: status",
			modelName:   "status",
			expectError: true,
		},
		{
			name:        "reserved word: ps",
			modelName:   "ps",
			expectError: true,
		},
		{
			name:        "reserved word: list",
			modelName:   "list",
			expectError: true,
		},
		{
			name:        "reserved word: ls",
			modelName:   "ls",
			expectError: true,
		},
		{
			name:        "reserved word: clean",
			modelName:   "clean",
			expectError: true,
		},
		{
			name:        "reserved word: stop",
			modelName:   "stop",
			expectError: true,
		},
		{
			name:        "reserved word: help",
			modelName:   "help",
			expectError: true,
		},
		{
			name:        "reserved word: version",
			modelName:   "version",
			expectError: true,
		},
		{
			name:        "case insensitive: STATUS",
			modelName:   "STATUS",
			expectError: true,
		},
		{
			name:        "case insensitive: Status",
			modelName:   "Status",
			expectError: true,
		},
		{
			name:        "whitespace is trimmed",
			modelName:   "  status  ",
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateNotSubcommand(tt.modelName)

			if tt.expectError {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), "is not a valid model name")
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestDetectFormat(t *testing.T) {
	tests := []struct {
		name     string
		model    string
		expected string
	}{
		{
			name:     "GGUF format in name",
			model:    "TheBloke/Llama-2-7B-Chat-GGUF",
			expected: "gguf",
		},
		{
			name:     "gguf lowercase",
			model:    "model-gguf-v2",
			expected: "gguf",
		},
		{
			name:     "MLX format",
			model:    "mlx-community/Llama-3-8B-mlx",
			expected: "mlx",
		},
		{
			name:     "safetensors format",
			model:    "model-safetensors",
			expected: "safetensors",
		},
		{
			name:     "auto-detect for unknown",
			model:    "llama2:7b",
			expected: "auto-detect",
		},
		{
			name:     "auto-detect for simple name",
			model:    "mistral",
			expected: "auto-detect",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := detectFormat(tt.model)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestNewPullStatusCmd(t *testing.T) {
	cmd := newDeployStatusCmd()

	require.NotNil(t, cmd, "newDeployStatusCmd() should not return nil")
	assert.Equal(t, "status [job_id]", cmd.Use)
	assert.NotEmpty(t, cmd.Short)
	require.NotNil(t, cmd.RunE)
}

func TestNewPullStopCmd(t *testing.T) {
	cmd := newDeployStopCmd()

	require.NotNil(t, cmd, "newDeployStopCmd() should not return nil")
	assert.Equal(t, "stop", cmd.Use)
	assert.NotEmpty(t, cmd.Short)
	require.NotNil(t, cmd.RunE)
}
