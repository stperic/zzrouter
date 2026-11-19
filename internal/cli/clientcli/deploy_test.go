package clientcli

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/apipath"
	"github.com/stperic/zzrouter/pkg/config"
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
	require.NotNil(t, flags.Lookup("registry"))

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

func TestDeployProviderAndRegistryRemainIndependent(t *testing.T) {
	for _, tc := range []struct {
		name, model, provider, registry string
		all                             bool
	}{
		{"explicit-both", "Qwen/Qwen2.5-0.5B-Instruct-GGUF", "llamacpp", "hf", false},
		{"automatic-source", "Qwen/Qwen2.5-0.5B-Instruct-GGUF", "llamacpp", "", false},
		{"registry-only", "openai/gpt-oss:20b", "", "ollama", false},
		{"notation-preserved", "hf/org/model#Q4_K_M@worker", "llamacpp", "", false},
		{"all-nodes-assessed", "org/model", "llamacpp", "huggingface", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
			requests := make(chan pkgClient.DeployRequest, 1)
			var assessments atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case apipath.Health:
				case apipath.NodesCompatible:
					assessments.Add(1)
					require.Equal(t, tc.registry, r.URL.Query().Get("registry"))
					require.Equal(t, tc.provider, r.URL.Query().Get("provider"))
					_, _ = w.Write([]byte(`{"data":[{"name":"worker"}]}`))
				case apipath.Deployments:
					require.Equal(t, http.MethodPost, r.Method)
					var sent pkgClient.DeployRequest
					require.NoError(t, json.NewDecoder(r.Body).Decode(&sent))
					requests <- sent
					w.WriteHeader(http.StatusAccepted)
					_, _ = w.Write([]byte(`{"data":{"id":"deploy-test","model":"selected-model","nodes":[{"node":"worker","status":"completed"}]}}`))
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			t.Cleanup(upstream.Close)
			host, portText, err := net.SplitHostPort(upstream.Listener.Addr().String())
			require.NoError(t, err)
			port, err := strconv.Atoi(portText)
			require.NoError(t, err)
			require.NoError(t, config.NewConfigManager("zzrouter").SaveClientConfig(&config.ClientConfig{Node: config.NodeConnection{Address: host, Port: port}}))
			args := []string{tc.model, "--file", "weights.gguf"}
			if tc.provider != "" {
				args = append(args, "--provider", tc.provider)
			}
			if tc.registry != "" {
				args = append(args, "--registry", tc.registry)
			}
			if tc.all {
				args = append(args, "--to-all", "--force")
			} else {
				args = append(args, "--node", "worker")
			}
			command := NewDeployCmd()
			command.SetArgs(args)
			require.NoError(t, command.ExecuteContext(t.Context()))
			require.Len(t, requests, 1)
			sent := <-requests
			require.Equal(t, tc.all, assessments.Load() == 1)
			require.Equal(t, tc.model, sent.Model)
			require.Equal(t, tc.provider, sent.Provider)
			require.Equal(t, tc.registry, sent.Registry)
			require.Equal(t, "weights.gguf", sent.File)
			require.Equal(t, []string{"worker"}, sent.Nodes)
		})
	}
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
