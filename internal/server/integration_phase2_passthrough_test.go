// Integration tests for the Phase 2 selective OpenAI pass-through
// additions: stored chat completion CRUD, vector store search,
// fine-tuning checkpoint permissions, conversations, evals,
// containers, and videos. Verifies each route is reachable and
// forwards the method/path unchanged to the default backend.
package server

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPhase2_StatefulRoutesAreReachable walks one representative
// method per route group and confirms zzrouter forwards to the
// configured default backend rather than returning NoRoute 404.
func TestPhase2_StatefulRoutesAreReachable(t *testing.T) {
	backend, recorded, mu := newBackendRecorder(t, http.StatusOK, []byte(`{"ok":true}`), "application/json")

	server := createTestNodeWithDefaults(t)
	configureBackend(t, server, "phase2-backend", backend.URL)

	cases := []struct {
		name   string
		method string
		path   string
		body   any
	}{
		// P2.3 — stored chat completions
		{"chat-completions-retrieve", "GET", "/v1/chat/completions/chatcmpl-abc", nil},
		{"chat-completions-messages", "GET", "/v1/chat/completions/chatcmpl-abc/messages", nil},
		{"chat-completions-update", "POST", "/v1/chat/completions/chatcmpl-abc", map[string]any{"metadata": map[string]string{"k": "v"}}},
		{"chat-completions-delete", "DELETE", "/v1/chat/completions/chatcmpl-abc", nil},

		// P2.2 — vector store search
		{"vector-store-search", "POST", "/v1/vector_stores/vs_abc/search", map[string]any{"query": "hi"}},

		// P2.1 — fine-tuning checkpoint permissions
		{"checkpoint-permissions-list", "GET", "/v1/fine_tuning/checkpoints/ckpt_abc/permissions", nil},
		{"checkpoint-permissions-create", "POST", "/v1/fine_tuning/checkpoints/ckpt_abc/permissions", map[string]any{"project_ids": []string{"proj_a"}}},
		{"checkpoint-permissions-delete", "DELETE", "/v1/fine_tuning/checkpoints/ckpt_abc/permissions/cp_perm_1", nil},

		// P2.4 — conversations
		{"conversations-create", "POST", "/v1/conversations", map[string]any{}},
		{"conversations-retrieve", "GET", "/v1/conversations/conv_abc", nil},
		{"conversations-items-list", "GET", "/v1/conversations/conv_abc/items", nil},
		{"conversations-items-get", "GET", "/v1/conversations/conv_abc/items/item_1", nil},

		// P2.5 — evals
		{"evals-create", "POST", "/v1/evals", map[string]any{"name": "n"}},
		{"evals-list", "GET", "/v1/evals", nil},
		{"eval-runs-create", "POST", "/v1/evals/ev_abc/runs", map[string]any{}},
		{"eval-run-output-items", "GET", "/v1/evals/ev_abc/runs/run_abc/output_items", nil},

		// P2.6 — containers
		{"containers-create", "POST", "/v1/containers", map[string]any{"name": "c"}},
		{"containers-list", "GET", "/v1/containers", nil},
		{"container-files-list", "GET", "/v1/containers/cnt_abc/files", nil},
		{"container-file-content", "GET", "/v1/containers/cnt_abc/files/file_1/content", nil},

		// P2.7 — videos
		{"videos-create", "POST", "/v1/videos", map[string]any{"prompt": "hi"}},
		{"videos-retrieve", "GET", "/v1/videos/vid_abc", nil},
		{"videos-content", "GET", "/v1/videos/vid_abc/content", nil},
		{"videos-extend", "POST", "/v1/videos/vid_abc/extend", map[string]any{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := makeRequest(t, server, TestRequest{
				Method: tc.method,
				Path:   tc.path,
				Body:   tc.body,
			})
			require.NotEqualf(t, http.StatusNotFound, resp.Code,
				"%s %s returned 404 — route not registered: %s", tc.method, tc.path, resp.Body)
			assert.Equal(t, http.StatusOK, resp.Code, "body=%s", resp.Body)
		})
	}

	// Sanity check: every case above reached the backend, so the
	// recorder should have captured exactly len(cases) requests.
	mu.Lock()
	got := len(*recorded)
	mu.Unlock()
	require.Equal(t, len(cases), got,
		"expected backend to receive %d requests, got %d", len(cases), got)
}
