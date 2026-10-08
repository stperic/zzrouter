package server

import (
	"encoding/json"
	"net/http"
	"testing"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProviderSchemaReportsReleaseDiagnostics(t *testing.T) {
	cfg := DefaultTestNodeConfig()
	cfg.SeedProvidersDir = true
	server := createTestNode(t, cfg)
	response := makeAuthRequest(t, server, http.MethodGet, "/zzrouter/v1/providers/mlx/schema", TestAdminKey, nil)
	require.Equal(t, http.StatusOK, response.Code, string(response.Body))
	var result schemaResponse
	require.NoError(t, json.Unmarshal(response.Body, &result))
	require.NotNil(t, result.Diagnostics)
	assert.Contains(t, result.Diagnostics.Runtimes, "mlx-vlm")
	assert.Equal(t, "unified", result.Diagnostics.Memory.Kind)
}

func TestProviderSyncRejectsUnsafeDiagnosticsBeforeWrite(t *testing.T) {
	cfg := DefaultTestNodeConfig()
	cfg.ClusterMode = pkgConfig.ClusterModeWorker
	cfg.SeedProvidersDir = true
	server := createTestNode(t, cfg)
	original, err := server.configStore.ReadProviderSchemaBytes("ollama")
	require.NoError(t, err)
	body := ProviderSyncBody{
		Kind:       pkgConfig.KindExternal,
		ConfigYAML: []byte("description: test\nenabled: true\nprotocol: ollama\nruntime:\n  endpoint: http://localhost:11434\ncapabilities:\n  wire_endpoints: [chat_completions]\n"),
		SchemaYAML: []byte("diagnostics:\n  runtime: test\n  runtimes:\n    test:\n      checks: [shell]\n      kernels: external\n"),
	}
	response := makeInternalRequest(t, server, TestRequest{Method: http.MethodPost, Path: "/zzrouter/v1/internal/sync/providers/ollama", Body: body})
	require.Equal(t, http.StatusBadRequest, response.Code, string(response.Body))
	assert.Contains(t, string(response.Body), "unknown diagnostic check")
	after, err := server.configStore.ReadProviderSchemaBytes("ollama")
	require.NoError(t, err)
	assert.Equal(t, original, after)
}
