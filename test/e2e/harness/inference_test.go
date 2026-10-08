package harness_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stperic/zzrouter/test/e2e/harness"
)

// TestChatCompletion_HappyPath drives the typed wrapper against an
// httptest server returning the canonical OpenAI shape. Pins the
// non-streaming decode path.
func TestChatCompletion_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      "cmpl-1",
			"object":  "chat.completion",
			"created": 1,
			"model":   req["model"],
			"choices": []map[string]any{{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": "hello back"},
				"finish_reason": "stop",
			}},
			"usage":    map[string]any{"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8},
			"zzrouter": map[string]any{"served_by": "worker-1"},
		})
	}))
	defer srv.Close()

	node, err := harness.NewNode("test", srv.URL, harness.RoleCoordinator, nil, harness.NodeKeys{
		Admin: "admin-key",
	})
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	c := harness.NewClient(node, harness.TierAdmin)

	resp, err := harness.ChatCompletion(context.Background(), c, harness.ChatCompletionRequest{
		Model:    "qwen2.5-1.5b",
		Messages: []harness.ChatMessage{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}
	if len(resp.Choices) != 1 || resp.Choices[0].FinishReason != "stop" {
		t.Errorf("unexpected response: %+v", resp)
	}
	if resp.Usage.TotalTokens != 8 {
		t.Errorf("usage: %+v", resp.Usage)
	}
	if served, _ := resp.ZZRouterAttribution["served_by"].(string); served != "worker-1" {
		t.Errorf("attribution missing served_by: %+v", resp.ZZRouterAttribution)
	}
}

// TestChatCompletion_RequiresModel pins the input validation — a
// missing model is a programming error, not a server-side failure.
func TestChatCompletion_RequiresModel(t *testing.T) {
	node, _ := harness.NewNode("t", "http://example.invalid", harness.RoleCoordinator, nil, harness.NodeKeys{})
	c := harness.NewClient(node, harness.TierAdmin)
	_, err := harness.ChatCompletion(context.Background(), c, harness.ChatCompletionRequest{
		Messages: []harness.ChatMessage{{Role: "user", Content: "hi"}},
	})
	if err == nil || !strings.Contains(err.Error(), "model") {
		t.Fatalf("expected model-required error, got %v", err)
	}
}
