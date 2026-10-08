package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// ChatStreamDelta is the delta object inside a streaming choice.
// Role appears only in the first chunk for an assistant turn; subsequent
// chunks carry content increments. ToolCalls follow the same first-only
// role rule but with a richer shape — kept as raw JSON until a slice
// actually drives tool-use streaming.
type ChatStreamDelta struct {
	Role      string          `json:"role,omitempty"`
	Content   string          `json:"content,omitempty"`
	ToolCalls json.RawMessage `json:"tool_calls,omitempty"`
}

// ChatStreamChoice is one alternative in a streaming chunk.
// finish_reason is null until the terminal chunk, where it carries
// stop|length|tool_calls|content_filter.
type ChatStreamChoice struct {
	Index        int             `json:"index"`
	Delta        ChatStreamDelta `json:"delta"`
	FinishReason *string         `json:"finish_reason"`
}

// ChatStreamChunk is one parsed `data: {...}` payload from the SSE
// stream. The terminal `data: [DONE]` sentinel produces no chunk —
// see StreamResult.SawDone.
type ChatStreamChunk struct {
	ID      string               `json:"id"`
	Object  string               `json:"object"`
	Created int64                `json:"created"`
	Model   string               `json:"model"`
	Choices []ChatStreamChoice   `json:"choices"`
	Usage   *ChatCompletionUsage `json:"usage,omitempty"`
	Raw     map[string]any       `json:"-"`
}

// StreamResult is the assembled view of one streaming chat call.
//
// Chunks holds every parsed `data: {...}` payload in arrival order.
// SawDone records whether the terminal `data: [DONE]` sentinel was
// observed; OpenAI semantics require it after the final usage frame.
// AssembledContent concatenates choices[0].delta.content across
// chunks for the canonical "what did the model say" view.
//
// FinalFinishReason is the first non-nil finish_reason seen on
// choices[0]; usage frames typically carry empty choices, so this
// field captures the actual termination signal independently of
// where it lands in the stream.
type StreamResult struct {
	Chunks            []ChatStreamChunk
	SawDone           bool
	AssembledContent  string
	FinalFinishReason string
	FinalUsage        *ChatCompletionUsage
}

// ChatCompletionStream POSTs /v1/chat/completions with stream=true,
// tails the SSE stream, and returns the assembled view. The request
// must NOT set Stream=false explicitly — this helper sets it.
//
// OpenAI's `stream_options.include_usage=true` triggers a final usage
// frame before [DONE]; the harness exposes it via the StreamUsage
// option so tests can pin both behaviors (with and without usage).
//
// Streaming sends one HTTP request whose body remains open for the
// duration; ctx governs total stream lifetime including [DONE]. A
// typical chat completion takes 5–30s; pass a generous deadline.
func ChatCompletionStream(
	ctx context.Context,
	c *Client,
	req ChatCompletionRequest,
	opts ...StreamOpt,
) (*StreamResult, error) {
	if req.Model == "" {
		return nil, fmt.Errorf("ChatCompletionStream: model is required")
	}
	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("ChatCompletionStream: at least one message is required")
	}
	req.Stream = true

	// Apply stream-only options. We can't put stream_options into
	// ChatCompletionRequest without polluting the non-streaming path,
	// so we serialize the request to a map and merge.
	body, err := buildStreamBody(req, opts)
	if err != nil {
		return nil, fmt.Errorf("build body: %w", err)
	}

	url := c.Node().BaseURL() + "/v1/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Content-Type", "application/json")
	c.applyAuth(httpReq, "")

	httpResp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("post: %w", err)
	}
	if httpResp.StatusCode != http.StatusOK {
		// On non-200 the body usually carries an RFC 9457 problem;
		// surface as much as fits without burying the test in noise.
		raw := readLimitedBody(httpResp)
		_ = httpResp.Body.Close()
		return nil, fmt.Errorf("/v1/chat/completions stream: status %d body=%s",
			httpResp.StatusCode, raw)
	}

	out := &StreamResult{}
	var assembled bytes.Buffer

	err = TailSSE(ctx, httpResp, func(f SSEFrame) bool {
		// `data: [DONE]` is the canonical OpenAI terminator.
		if strings.TrimSpace(f.Data) == "[DONE]" {
			out.SawDone = true
			return true
		}
		if f.Data == "" {
			return false
		}
		var chunk ChatStreamChunk
		if err := json.Unmarshal([]byte(f.Data), &chunk); err != nil {
			// Malformed chunk — record raw via the Raw map so a test
			// can still inspect, but don't kill the stream over one
			// bad frame.
			return false
		}
		_ = json.Unmarshal([]byte(f.Data), &chunk.Raw)
		out.Chunks = append(out.Chunks, chunk)

		if len(chunk.Choices) > 0 {
			ch := chunk.Choices[0]
			assembled.WriteString(ch.Delta.Content)
			if ch.FinishReason != nil && *ch.FinishReason != "" && out.FinalFinishReason == "" {
				out.FinalFinishReason = *ch.FinishReason
			}
		}
		if chunk.Usage != nil {
			out.FinalUsage = chunk.Usage
		}
		return false
	})
	if err != nil {
		return out, fmt.Errorf("tail sse: %w", err)
	}

	out.AssembledContent = assembled.String()
	return out, nil
}

// StreamOpt mutates the streaming request body in ways
// ChatCompletionRequest doesn't expose directly.
type StreamOpt func(map[string]any)

// IncludeUsage sets stream_options.include_usage=true. Triggers a
// final usage frame before [DONE].
func IncludeUsage() StreamOpt {
	return func(m map[string]any) {
		opts, _ := m["stream_options"].(map[string]any)
		if opts == nil {
			opts = map[string]any{}
		}
		opts["include_usage"] = true
		m["stream_options"] = opts
	}
}

// buildStreamBody serializes ChatCompletionRequest with stream=true
// then applies stream-only opts via map mutation. Avoids growing the
// request struct with fields the non-streaming path can't use.
func buildStreamBody(req ChatCompletionRequest, opts []StreamOpt) ([]byte, error) {
	raw, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if len(opts) == 0 {
		return raw, nil
	}
	m := map[string]any{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	for _, o := range opts {
		o(m)
	}
	return json.Marshal(m)
}

// readLimitedBody reads up to 4KB of an error response. Limits avoid
// turning a runaway response into an unbounded test failure dump.
func readLimitedBody(resp *http.Response) []byte {
	const limit = 4 * 1024
	if resp == nil || resp.Body == nil {
		return nil
	}
	buf := make([]byte, limit)
	n, _ := resp.Body.Read(buf)
	if n <= 0 {
		return nil
	}
	return buf[:n]
}

// ErrEmptyStream is returned (wrapped) when a streaming call closes
// cleanly with zero data chunks. Distinct from a malformed frame.
var ErrEmptyStream = errors.New("stream closed with no chunks")
