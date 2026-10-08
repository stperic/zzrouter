package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestOllamaPs_GatedWhenNoOllama confirms /api/ps returns 503 with the
// install hint when no Ollama backend is registered in the cluster. The
// shape contract for non-empty responses lives in TestOllamaPs_NodeSuffixContract
// + TestOllamaPs_EntryFieldsPreserved (white-box marshaling tests).
func TestOllamaPs_GatedWhenNoOllama(t *testing.T) {
	s := createTestNodeWithDefaults(t)

	resp := makeRequest(t, s, TestRequest{
		Method: "GET",
		Path:   "/api/ps",
	})

	require.Equal(t, 503, resp.Code, "raw body: %s", resp.Body)
	var body map[string]any
	require.NoError(t, json.Unmarshal(resp.Body, &body))
	errStr, _ := body["error"].(string)
	require.Contains(t, errStr, "Ollama backend",
		"gate detail must point to install hint, got %q", errStr)
}

// TestOllamaPs_NodeSuffixContract locks in the per-instance attribution
// contract: when the local handler emits entries, Name is suffixed with
// `@<node>` to form the canonical addressable identifier `<model>@<node>`
// (artifact-on-the-left, qualifier-on-the-right — same grammar as email,
// HuggingFace revisions, Docker digests, Go module versions). The Model
// field stays bare so clients can group by logical model.
//
// This convention is provider-agnostic: any future canonical
// RunningInstance (vLLM, MLX, llama.cpp, cloud) must follow the same
// Name = "<Model>@<Node>" rule.
func TestOllamaPs_NodeSuffixContract(t *testing.T) {
	// Build a minimal entry directly and verify the wire-format expectations
	// clients depend on. Not an end-to-end test (that requires live Ollama);
	// it locks in what Name/Model SHOULD look like after the handler runs.
	entry := OllamaPsEntry{
		Name:  "llama3:8b@gpu-1",
		Model: "llama3:8b",
	}

	buf, err := json.Marshal(entry)
	require.NoError(t, err)
	s := string(buf)
	require.Contains(t, s, `"name":"llama3:8b@gpu-1"`,
		"Name must carry the <model>@<node> suffix for addressability")
	require.Contains(t, s, `"model":"llama3:8b"`,
		"Model field must stay bare so clients can group-by-logical-model")
	require.False(t, strings.Contains(s, `"name":"gpu-1@llama3:8b"`),
		"node-prefix (<node>@<model>) order violates canonical grammar")
}

// TestOllamaPs_EntryFieldsPreserved — OllamaPsEntry carries runtime fields
// (size_vram, expires_at, context_length) that Open WebUI's stop-model and
// TTL displays depend on. This test locks in the JSON wire-format for
// those fields so refactors don't silently drop them.
func TestOllamaPs_EntryFieldsPreserved(t *testing.T) {
	entry := OllamaPsEntry{
		Name:          "llama3:8b@node-a",
		Model:         "llama3:8b",
		Size:          5_400_000_000,
		Digest:        "sha256:abc",
		SizeVRAM:      5_400_000_000,
		ContextLength: 8192,
		// ExpiresAt is a time.Time; leaving zero is fine for the shape check.
		Details: OllamaModelDetails{
			Format:        "gguf",
			Family:        "llama",
			ParameterSize: "8B",
			QuantLevel:    "Q4_K_M",
		},
	}

	buf, err := json.Marshal(entry)
	require.NoError(t, err)
	s := string(buf)
	for _, want := range []string{
		`"size":5400000000`,
		`"size_vram":5400000000`,
		`"context_length":8192`,
		`"digest":"sha256:abc"`,
		`"format":"gguf"`,
		`"family":"llama"`,
		`"parameter_size":"8B"`,
		`"quantization_level":"Q4_K_M"`, // wire field; internal is quant_level
	} {
		require.Contains(t, s, want, "wire format missing %s in body: %s", want, s)
	}
}
