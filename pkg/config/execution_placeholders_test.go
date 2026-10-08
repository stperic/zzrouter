package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateExecutionCommand_PlaceholderContract(t *testing.T) {
	tests := []struct {
		name    string
		exec    ExecutionConfig
		wantErr string
	}{
		{
			name: "weights by path with a naming flag",
			exec: ExecutionConfig{
				Type:      "cli",
				Command:   "llama-server",
				Args:      []string{"--model", "${MODEL_PATH}", "--alias", "${MODEL}", "--port", "${PORT}"},
				WireModel: WireModelName,
			},
		},
		{
			name: "no model at all is fine",
			exec: ExecutionConfig{Type: "cli", Command: "serve", Args: []string{"--port", "${PORT}"}},
		},
		{
			name: "allow-listed env var",
			exec: ExecutionConfig{Type: "cli", Command: "serve", Args: []string{"--home", "${HF_HOME}"}},
		},
		{
			name: "name without a path lets the engine fetch its own copy",
			exec: ExecutionConfig{
				Type:    "python",
				Command: "python3",
				Args:    []string{"-m", "mlx_lm", "server", "--model", "${MODEL}"},
			},
			wantErr: "never points the engine at local weights",
		},
		{
			name: "typo'd placeholder would reach argv verbatim",
			exec: ExecutionConfig{
				Type:    "cli",
				Command: "serve",
				Args:    []string{"--model", "${MODEL_PAHT}"},
			},
			wantErr: "unknown placeholder ${MODEL_PAHT}",
		},
		{
			name: "retired placeholder is no longer expanded",
			exec: ExecutionConfig{
				Type:    "cli",
				Command: "serve",
				Args:    []string{"--model", "${MODEL_ABSOLUTE_PATH}"},
			},
			wantErr: "unknown placeholder ${MODEL_ABSOLUTE_PATH}",
		},
		{
			name: "unknown wire model",
			exec: ExecutionConfig{
				Type:      "cli",
				Command:   "serve",
				Args:      []string{"--model", "${MODEL_PATH}"},
				WireModel: "repo-id",
			},
			wantErr: `wire_model "repo-id"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateExecutionCommand("test-provider", tc.exec)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestWireModel_Resolved(t *testing.T) {
	assert.Equal(t, WireModelName, WireModel("").Resolved(), "engines honor the client's name unless they say otherwise")
	assert.Equal(t, WireModelPath, WireModelPath.Resolved())
}
