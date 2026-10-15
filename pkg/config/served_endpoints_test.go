package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stretchr/testify/require"
)

func TestServesEndpoint(t *testing.T) {
	for _, caps := range []*AppCapabilities{nil, {}, {WireEndpoints: []string{"chat_completions", "embeddings", "rerank"}}} {
		svc := ServiceConfig{Name: "unknown", Capabilities: caps}
		for _, endpoint := range []string{"", "chat", "embeddings", "reranking"} {
			require.NoError(t, svc.ValidateRunTarget("model", endpoint))
		}
		require.ErrorContains(t, svc.ValidateRunTarget("model", "typo"), "unknown run endpoint")
	}
	svc := ServiceConfig{Name: "chat-only", Capabilities: &AppCapabilities{WireEndpoints: []string{"chat_completions"}}}
	require.NoError(t, svc.ValidateRunTarget("model", ""))
	for _, endpoint := range []string{"embeddings", "reranking"} {
		err := svc.ValidateRunTarget("model", endpoint)
		require.ErrorContains(t, err, "chat-only")
		require.ErrorContains(t, err, endpoint)
		require.ErrorContains(t, err, "chat_completions")
	}
	svc.Features = map[string]Feature{"embeddings": {WireEndpoints: []string{"embeddings"}}}
	require.NoError(t, svc.ServesEndpoint("embeddings"))
	require.Error(t, svc.ServesEndpoint("reranking"))
	require.True(t, svc.IsEndpointAware())
	svc.Mode = "cloud"
	require.NoError(t, svc.ValidateRunTarget("model", "reranking"))
	svc.Mode = "on-demand"
	svc.RunScope = RunScopeNode
	require.NoError(t, svc.ValidateRunTarget("", ""))
	require.Error(t, svc.ValidateRunTarget("", "chat"))
}

// The chat run carries every text API, so a provider declaring any one of them serves it.
func TestChatEndpointServedByAnyTextAPI(t *testing.T) {
	for _, wire := range []string{"chat_completions", "completions", "messages", "responses"} {
		svc := ServiceConfig{Name: wire + "-only", Capabilities: &AppCapabilities{WireEndpoints: []string{wire}}}
		require.NoError(t, svc.ValidateRunTarget("model", "chat"), wire)
		require.Error(t, svc.ValidateRunTarget("model", "embeddings"), wire)
	}
	svc := ServiceConfig{Name: "embeddings-only", Capabilities: &AppCapabilities{WireEndpoints: []string{"embeddings"}}}
	require.NoError(t, svc.ValidateRunTarget("model", "embeddings"))
	require.ErrorContains(t, svc.ValidateRunTarget("model", "chat"), "embeddings-only")
}

func TestEndpointFeatureLoadValidation(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"unknown", "typo: {}", "typo"},
		{"builtin", "embeddings: {}", "requires a runtime"},
		{"file", "embeddings: {files: [x], flag: x}", "requires a runtime"},
		{"wrong wire", "reranking: {runtime: vlm, execution: {type: python, command: python3}, wire_endpoints: [embeddings]}", "rerank"},
		{"invalid overlay stall", "vision: {runtime: vlm, execution: {type: python, command: python3}, wire_endpoints: [chat_completions], health_check: {readiness_probe: {stall_timeout: invalid}}}", "stall_timeout"},
		{"health without runtime", "vision: {health_check: {path: /health}}", "requires a runtime"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, templates.InstallDefaults(dir))
			body := "protocol: openai\nfeatures:\n  " + tc.body + "\nruntime:\n  port_range: [8000, 8009]\n  base_port: 8000\n  execution: {type: cli, command: engine}\ncapabilities:\n  wire_endpoints: [chat_completions]\n"
			require.NoError(t, os.WriteFile(filepath.Join(dir, "on-demand", "mlx", "config.yaml"), []byte(body), 0600))
			_, err := LoadAppsConfig(dir)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestRuntimeExecutionIsDeterministic(t *testing.T) {
	first, last := &ExecutionConfig{Command: "a"}, &ExecutionConfig{Command: "z"}
	svc := ServiceConfig{Features: map[string]Feature{"vision": {Runtime: "shared", Execution: last}, "embeddings": {Runtime: "shared", Execution: first}}}
	for range 100 {
		require.Same(t, last, svc.RuntimeExecution("shared"))
	}
}
