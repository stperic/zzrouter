package harness_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stperic/zzrouter/test/e2e/harness"
)

// streamingHandler writes a canned OpenAI-shaped chat-completion
// stream. Lets each test file craft the SSE body it needs and rely
// on the same wire-format mock.
func streamingHandler(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
}

func newClient(t *testing.T, srvURL string) *harness.Client {
	t.Helper()
	node, err := harness.NewNode("t", srvURL, harness.RoleCoordinator, nil, harness.NodeKeys{
		Admin: "admin-key",
	})
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	return harness.NewClient(node, harness.TierAdmin)
}

// TestChatCompletionStream_AssemblesContent pins the basic happy
// path: multiple data chunks, role-only first, content increments,
// terminal finish_reason, [DONE].
func TestChatCompletionStream_AssemblesContent(t *testing.T) {
	body := strings.Join([]string{
		`data: {"id":"c1","model":"qwen","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		``,
		`data: {"id":"c1","model":"qwen","choices":[{"index":0,"delta":{"content":"hel"},"finish_reason":null}]}`,
		``,
		`data: {"id":"c1","model":"qwen","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":null}]}`,
		``,
		`data: {"id":"c1","model":"qwen","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		``,
		`data: [DONE]`,
		``,
		``,
	}, "\n")

	srv := httptest.NewServer(streamingHandler(body))
	defer srv.Close()

	c := newClient(t, srv.URL)
	res, err := harness.ChatCompletionStream(context.Background(), c, harness.ChatCompletionRequest{
		Model:    "qwen",
		Messages: []harness.ChatMessage{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if !res.SawDone {
		t.Error("expected [DONE]")
	}
	if res.AssembledContent != "hello" {
		t.Errorf("assembled=%q want %q", res.AssembledContent, "hello")
	}
	if res.FinalFinishReason != "stop" {
		t.Errorf("finish_reason=%q", res.FinalFinishReason)
	}
	if len(res.Chunks) != 4 {
		t.Errorf("chunks=%d want 4", len(res.Chunks))
	}
}

// TestChatCompletionStream_UsageOnFinalFrame asserts the final usage
// frame populates StreamResult.FinalUsage.
func TestChatCompletionStream_UsageOnFinalFrame(t *testing.T) {
	body := strings.Join([]string{
		`data: {"id":"c1","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		``,
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`,
		``,
		`data: {"id":"c1","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`,
		``,
		`data: [DONE]`,
		``,
		``,
	}, "\n")

	srv := httptest.NewServer(streamingHandler(body))
	defer srv.Close()

	c := newClient(t, srv.URL)
	res, err := harness.ChatCompletionStream(context.Background(), c, harness.ChatCompletionRequest{
		Model:    "qwen",
		Messages: []harness.ChatMessage{{Role: "user", Content: "hi"}},
	}, harness.IncludeUsage())
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if res.FinalUsage == nil {
		t.Fatal("FinalUsage is nil")
	}
	if res.FinalUsage.TotalTokens != 7 {
		t.Errorf("usage=%+v", res.FinalUsage)
	}
	if !res.SawDone {
		t.Error("expected [DONE]")
	}
}

// TestChatCompletionStream_DonePlacement pins that no chunks are
// recorded after [DONE]. The OpenAI contract is that [DONE] is the
// very last frame; an SSE server appending more after it would be
// drift we want to fail loudly on.
func TestChatCompletionStream_DonePlacement(t *testing.T) {
	body := strings.Join([]string{
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"a"},"finish_reason":null}]}`,
		``,
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		``,
		`data: [DONE]`,
		``,
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"LATE"},"finish_reason":null}]}`,
		``,
		``,
	}, "\n")

	srv := httptest.NewServer(streamingHandler(body))
	defer srv.Close()

	c := newClient(t, srv.URL)
	res, err := harness.ChatCompletionStream(context.Background(), c, harness.ChatCompletionRequest{
		Model:    "qwen",
		Messages: []harness.ChatMessage{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	// TailSSE returns once onFrame(SawDone) returns true; the late
	// chunk should never be observed.
	if strings.Contains(res.AssembledContent, "LATE") {
		t.Errorf("post-[DONE] chunk leaked into content: %q", res.AssembledContent)
	}
}

// TestChatCompletionStream_DeltaRoleNoLeakage is the regression pin
// from project_v1_agent_experience: openrouter pass-through used to
// repeat delta.role on every chunk. The harness exposes the raw
// chunks so a test can assert role appears on chunk[0] only.
func TestChatCompletionStream_DeltaRoleNoLeakage(t *testing.T) {
	body := strings.Join([]string{
		`data: {"id":"c1","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		``,
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"a"},"finish_reason":null}]}`,
		``,
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		``,
		`data: [DONE]`,
		``,
		``,
	}, "\n")

	srv := httptest.NewServer(streamingHandler(body))
	defer srv.Close()

	c := newClient(t, srv.URL)
	res, err := harness.ChatCompletionStream(context.Background(), c, harness.ChatCompletionRequest{
		Model:    "qwen",
		Messages: []harness.ChatMessage{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if len(res.Chunks) < 2 {
		t.Fatalf("need at least 2 chunks, got %d", len(res.Chunks))
	}
	if res.Chunks[0].Choices[0].Delta.Role != "assistant" {
		t.Error("first chunk should declare role=assistant")
	}
	for i, ch := range res.Chunks[1:] {
		if len(ch.Choices) > 0 && ch.Choices[0].Delta.Role != "" {
			t.Errorf("chunk[%d] leaked role=%q (only first chunk should carry role)",
				i+1, ch.Choices[0].Delta.Role)
		}
	}
}

// TestChatCompletionStream_RejectsNonStream surfaces the case where
// the server (mis)configures and returns JSON instead of SSE — must
// fail loudly with a content-type error rather than silently parsing
// nothing.
func TestChatCompletionStream_RejectsNonStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer srv.Close()

	c := newClient(t, srv.URL)
	_, err := harness.ChatCompletionStream(context.Background(), c, harness.ChatCompletionRequest{
		Model:    "qwen",
		Messages: []harness.ChatMessage{{Role: "user", Content: "hi"}},
	})
	if err == nil || !strings.Contains(err.Error(), "SSE") {
		t.Fatalf("expected SSE content-type error, got %v", err)
	}
}
