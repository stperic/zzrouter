package ollama

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/config/backend"
)

// TestListTags_ReturnsAllModels pins a regression spotted
// during cluster live-verify: a worker daemon with 2 Ollama models
// surfaced only 1 of them in /v1/models. If the connector itself drops
// entries here, this test will catch it. If the connector preserves
// both, the bug is downstream (registry buildModelList /
// deduplicateByFullPath, or the cluster-broadcast aggregation).
func TestListTags_ReturnsAllModels(t *testing.T) {
	body := `{"models":[
		{"name":"qwen2.5:0.5b","model":"qwen2.5:0.5b","size":397821319,
		 "modified_at":"2026-04-26T20:14:34-04:00",
		 "digest":"a8b0c5","details":{"format":"gguf","family":"qwen2",
		 "parameter_size":"494.03M","quantization_level":"Q4_K_M"}},
		{"name":"smollm:135m","model":"smollm:135m","size":91739413,
		 "modified_at":"2026-04-27T11:42:09-04:00",
		 "digest":"b0b2a4","details":{"format":"gguf","family":"llama",
		 "parameter_size":"134.52M","quantization_level":"Q4_0"}}
	]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/show" {
			require.Equal(t, "POST", r.Method)
			_, _ = w.Write([]byte(`{"capabilities":[]}`))
			return
		}
		require.Equal(t, "/api/tags", r.URL.Path)
		require.Equal(t, "GET", r.Method)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c := NewConnector()
	got, err := c.ListTags(context.Background(), &backend.Resolved{Endpoint: srv.URL}, "ollama")
	require.NoError(t, err)
	require.Len(t, got, 2, "connector must return both daemon models")
	require.Equal(t, "qwen2.5:0.5b", got[0].Name)
	require.Equal(t, "smollm:135m", got[1].Name)
	for _, m := range got {
		require.Equal(t, "ollama", m.AssignedApp)
		require.Equal(t, "", m.FullPath, "Ollama daemon owns the files; FullPath stays empty")
	}
}

// TestListTags_EmptyDaemon pins the empty-list case so
// callers don't get a nil-vs-empty footgun.
func TestListTags_EmptyDaemon(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer srv.Close()

	c := NewConnector()
	got, err := c.ListTags(context.Background(), &backend.Resolved{Endpoint: srv.URL}, "ollama")
	require.NoError(t, err)
	require.Empty(t, got)
}
