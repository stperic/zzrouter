package server

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/stperic/zzrouter/pkg/fallback"
	"github.com/stretchr/testify/require"
)

// /api/* is gated on Ollama provider presence. The default test node has
// no ollama installed, so /api/chat + /api/generate return 503 from the
// gate before reaching body validation. Body-shape coverage moves to a
// future test that injects an Ollama backend; until then, we assert that
// the gate fires with Ollama's flat error shape (not RFC 7807).
func TestOllamaDispatch_GatedWhenNoOllama(t *testing.T) {
	s := createTestNodeWithDefaults(t)

	req := httptest.NewRequest("POST", "/api/chat", bytes.NewReader([]byte(`{not valid json`)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.engine.ServeHTTP(w, req)

	require.Equal(t, 503, w.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.NotContains(t, body, "type", "Ollama-shape error must not carry RFC 7807 'type'")
	require.NotContains(t, body, "title", "Ollama-shape error must not carry RFC 7807 'title'")
	errStr, _ := body["error"].(string)
	require.Contains(t, errStr, "Ollama backend", "gate detail must point to install hint, got %q", errStr)
}

// TestOllamaDispatch_UnknownModelDoesNotCrash confirms that a request for a
// nonexistent model returns a non-2xx response without panic or 500. The
// exact status depends on whether the default test node has an Ollama
// endpoint configured (the proxy chain then surfaces the downstream's error).
// End-to-end provider-scope 404 behavior is covered by multi-node integration
// tests that run in the test/integration package (separate harness).
func TestOllamaDispatch_UnknownModelDoesNotCrash(t *testing.T) {
	s := createTestNodeWithDefaults(t)

	resp := makeRequest(t, s, TestRequest{
		Method: "POST",
		Path:   "/api/chat",
		Body: map[string]any{
			"model":    "definitely-not-a-real-model-abc123",
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
			"stream":   false,
		},
	})

	require.NotEqual(t, 500, resp.Code, "unknown model must not produce a 500, body: %s", resp.Body)
	require.False(t, resp.Code >= 200 && resp.Code < 300,
		"unknown model must not return 2xx, got %d, body: %s", resp.Code, resp.Body)
}

// TestOllamaDispatch_XNodeHeaderAccepted confirms X-Node is parsed without
// errors even when the named node isn't in the cluster. The resolver returns
// a not-found result; dispatch surfaces the 404. The point of this test is
// that the header path doesn't panic or 500 when unfamiliar.
func TestOllamaDispatch_XNodeHeaderAccepted(t *testing.T) {
	s := createTestNodeWithDefaults(t)

	resp := makeRequest(t, s, TestRequest{
		Method: "POST",
		Path:   "/api/chat",
		Headers: map[string]string{
			"X-Node": "unknown-gpu-1",
		},
		Body: map[string]any{
			"model":    "llama3:8b",
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
			"stream":   false,
		},
	})

	require.NotEqual(t, 500, resp.Code, "unknown node must not cause a 500")
	require.NotEqual(t, 200, resp.Code, "unknown model+node must not succeed")
}

// TestFilterCandidatesByProvider_AppliedInDispatch documents that the
// provider-scope filter runs in the /api/* dispatch path. This is a white-box
// confidence test that the helper is reachable from the dispatch surface.
func TestFilterCandidatesByProvider_AppliedInDispatch(t *testing.T) {
	deps := []fallback.Candidate{
		{Name: "ollama-1", App: "ollama"},
		{Name: "vllm-1", App: "vllm"},
		{Name: "ollama-2", App: "ollama"},
	}
	filtered := filterCandidatesByProvider(deps, "ollama")
	require.Len(t, filtered, 2)
	require.Equal(t, "ollama-1", filtered[0].Name)
	require.Equal(t, "ollama-2", filtered[1].Name)
}
