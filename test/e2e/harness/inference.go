package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// ChatMessage is a single turn in a chat completion request. Matches
// the OpenAI-compatible shape exactly — zzrouter forwards verbatim
// to the underlying provider.
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatCompletionRequest mirrors the OpenAI v1 chat-completion shape
// the harness needs for Ring 2. Only the fields tests actually drive
// are typed here; raw bodies should use Client.POST + RawBody.
type ChatCompletionRequest struct {
	Model       string        `json:"model"`
	Messages    []ChatMessage `json:"messages"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Temperature float64       `json:"temperature,omitempty"`
	Stream      bool          `json:"stream,omitempty"`
}

// ChatCompletionUsage is the {prompt,completion,total}_tokens triple.
// Cost is the OpenRouter-style per-request charge (USD); 0 on :free
// tier or when the upstream doesn't report cost. Other providers may
// omit the field entirely (decodes as 0).
type ChatCompletionUsage struct {
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	TotalTokens      int     `json:"total_tokens"`
	Cost             float64 `json:"cost,omitempty"`
}

// ChatCompletionChoice is one alternative in the response. message is
// always populated for non-streaming; finish_reason is one of
// {stop, length, tool_calls, content_filter, ...}.
type ChatCompletionChoice struct {
	Index        int         `json:"index"`
	Message      ChatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

// ChatCompletionResponse mirrors the canonical non-streaming reply.
// ZZRouterAttribution is the provider-agnostic block memory flagged
// as added in main@ca07528d ("provider-agnostic zzrouter block on
// every inference response"); typed here so cost-truth assertions
// in later slices can pin its presence.
type ChatCompletionResponse struct {
	ID                  string                 `json:"id"`
	Object              string                 `json:"object"`
	Created             int64                  `json:"created"`
	Model               string                 `json:"model"`
	Choices             []ChatCompletionChoice `json:"choices"`
	Usage               ChatCompletionUsage    `json:"usage"`
	ZZRouterAttribution map[string]any         `json:"zzrouter,omitempty"`
}

// OllamaChatMessage mirrors Ollama's /api/chat message shape.
type OllamaChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// OllamaChatRequest mirrors POST /api/chat. Stream defaults to false.
// Options is the loose Ollama params bag (num_predict, temperature, …).
type OllamaChatRequest struct {
	Model    string              `json:"model"`
	Messages []OllamaChatMessage `json:"messages"`
	Stream   bool                `json:"stream"`
	Options  map[string]any      `json:"options,omitempty"`
}

// OllamaChatResponse is the non-streaming /api/chat reply.
type OllamaChatResponse struct {
	Model      string            `json:"model"`
	CreatedAt  string            `json:"created_at"`
	Message    OllamaChatMessage `json:"message"`
	Done       bool              `json:"done"`
	DoneReason string            `json:"done_reason,omitempty"`
	EvalCount  int               `json:"eval_count"`
}

// OllamaChat sends POST /api/chat (non-streaming) and decodes the body.
func OllamaChat(ctx context.Context, c *Client, req OllamaChatRequest) (*OllamaChatResponse, error) {
	if req.Model == "" {
		return nil, fmt.Errorf("OllamaChat: model is required")
	}
	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("OllamaChat: at least one message is required")
	}
	resp, err := c.POST(ctx, "/api/chat", req)
	if err != nil {
		return nil, fmt.Errorf("POST /api/chat: %w", err)
	}
	if resp.Status != http.StatusOK {
		return nil, fmt.Errorf("/api/chat: status %d body=%s", resp.Status, resp.Body)
	}
	out := &OllamaChatResponse{}
	if err := resp.JSON(out); err != nil {
		return nil, fmt.Errorf("decode ollama chat: %w", err)
	}
	return out, nil
}

// OllamaGenerateRequest mirrors POST /api/generate.
type OllamaGenerateRequest struct {
	Model   string         `json:"model"`
	Prompt  string         `json:"prompt"`
	Stream  bool           `json:"stream"`
	Options map[string]any `json:"options,omitempty"`
}

// OllamaGenerateResponse is the non-streaming /api/generate reply.
type OllamaGenerateResponse struct {
	Model      string `json:"model"`
	CreatedAt  string `json:"created_at"`
	Response   string `json:"response"`
	Done       bool   `json:"done"`
	DoneReason string `json:"done_reason,omitempty"`
	EvalCount  int    `json:"eval_count"`
}

// OllamaGenerate sends POST /api/generate (non-streaming).
func OllamaGenerate(ctx context.Context, c *Client, req OllamaGenerateRequest) (*OllamaGenerateResponse, error) {
	if req.Model == "" {
		return nil, fmt.Errorf("OllamaGenerate: model is required")
	}
	if req.Prompt == "" {
		return nil, fmt.Errorf("OllamaGenerate: prompt is required")
	}
	resp, err := c.POST(ctx, "/api/generate", req)
	if err != nil {
		return nil, fmt.Errorf("POST /api/generate: %w", err)
	}
	if resp.Status != http.StatusOK {
		return nil, fmt.Errorf("/api/generate: status %d body=%s", resp.Status, resp.Body)
	}
	out := &OllamaGenerateResponse{}
	if err := resp.JSON(out); err != nil {
		return nil, fmt.Errorf("decode ollama generate: %w", err)
	}
	return out, nil
}

// OllamaEmbedRequest mirrors POST /api/embed (the modern alias for
// /api/embeddings). Input is `any` to accept string OR []string.
type OllamaEmbedRequest struct {
	Model string `json:"model"`
	Input any    `json:"input"`
}

// OllamaEmbedResponse is the /api/embed reply.
type OllamaEmbedResponse struct {
	Model      string      `json:"model"`
	Embeddings [][]float64 `json:"embeddings"`
}

// OllamaEmbed sends POST /api/embed and decodes the response.
func OllamaEmbed(ctx context.Context, c *Client, req OllamaEmbedRequest) (*OllamaEmbedResponse, error) {
	if req.Model == "" {
		return nil, fmt.Errorf("OllamaEmbed: model is required")
	}
	if req.Input == nil {
		return nil, fmt.Errorf("OllamaEmbed: input is required")
	}
	resp, err := c.POST(ctx, "/api/embed", req)
	if err != nil {
		return nil, fmt.Errorf("POST /api/embed: %w", err)
	}
	if resp.Status != http.StatusOK {
		return nil, fmt.Errorf("/api/embed: status %d body=%s", resp.Status, resp.Body)
	}
	out := &OllamaEmbedResponse{}
	if err := resp.JSON(out); err != nil {
		return nil, fmt.Errorf("decode ollama embed: %w", err)
	}
	return out, nil
}

// OllamaPull triggers POST /api/pull and drains the NDJSON status
// stream until it sees `{"status":"success"}` or ctx fires. Returns
// nil on success. Idempotent — pulling an already-cached model returns
// near-instantly with a single success line.
func OllamaPull(ctx context.Context, c *Client, model string) error {
	if model == "" {
		return fmt.Errorf("OllamaPull: model is required")
	}
	body := map[string]any{"model": model, "stream": true}
	resp, err := c.POST(ctx, "/api/pull", body)
	if err != nil {
		return fmt.Errorf("POST /api/pull: %w", err)
	}
	if resp.Status != http.StatusOK {
		return fmt.Errorf("/api/pull: status %d body=%s", resp.Status, resp.Body)
	}
	// Body is the full NDJSON stream (Client buffers responses). Walk
	// each line; success comes as the trailing object.
	for _, line := range bytes.Split(resp.Body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var ev struct {
			Status string `json:"status"`
			Error  string `json:"error"`
		}
		if jerr := json.Unmarshal(line, &ev); jerr != nil {
			continue
		}
		if ev.Error != "" {
			return fmt.Errorf("/api/pull error: %s", ev.Error)
		}
		if ev.Status == "success" {
			return nil
		}
	}
	return fmt.Errorf("/api/pull stream ended without success status")
}

// OllamaTagsModel is one entry from GET /api/tags.
type OllamaTagsModel struct {
	Name  string `json:"name"`
	Model string `json:"model"`
}

// OllamaTagsResponse is GET /api/tags.
type OllamaTagsResponse struct {
	Models []OllamaTagsModel `json:"models"`
}

// OllamaTags fetches GET /api/tags.
func OllamaTags(ctx context.Context, c *Client) (*OllamaTagsResponse, error) {
	resp, err := c.GET(ctx, "/api/tags")
	if err != nil {
		return nil, fmt.Errorf("GET /api/tags: %w", err)
	}
	if resp.Status != http.StatusOK {
		return nil, fmt.Errorf("/api/tags: status %d body=%s", resp.Status, resp.Body)
	}
	out := &OllamaTagsResponse{}
	if err := resp.JSON(out); err != nil {
		return nil, fmt.Errorf("decode ollama tags: %w", err)
	}
	return out, nil
}

// CompletionRequest mirrors the legacy OpenAI v1 completions shape
// the harness needs for Ring 2 — a thin subset of the spec.
type CompletionRequest struct {
	Model       string  `json:"model"`
	Prompt      string  `json:"prompt"`
	MaxTokens   int     `json:"max_tokens,omitempty"`
	Temperature float64 `json:"temperature,omitempty"`
}

// CompletionChoice is one alternative in a /v1/completions response.
type CompletionChoice struct {
	Index        int    `json:"index"`
	Text         string `json:"text"`
	FinishReason string `json:"finish_reason"`
}

// CompletionResponse mirrors the canonical /v1/completions reply.
type CompletionResponse struct {
	ID                  string              `json:"id"`
	Object              string              `json:"object"`
	Created             int64               `json:"created"`
	Model               string              `json:"model"`
	Choices             []CompletionChoice  `json:"choices"`
	Usage               ChatCompletionUsage `json:"usage"`
	ZZRouterAttribution map[string]any      `json:"zzrouter,omitempty"`
}

// Completion sends POST /v1/completions and decodes the response.
func Completion(ctx context.Context, c *Client, req CompletionRequest) (*CompletionResponse, error) {
	if req.Model == "" {
		return nil, fmt.Errorf("Completion: model is required")
	}
	if req.Prompt == "" {
		return nil, fmt.Errorf("Completion: prompt is required")
	}
	resp, err := c.POST(ctx, "/v1/completions", req)
	if err != nil {
		return nil, fmt.Errorf("POST /v1/completions: %w", err)
	}
	if resp.Status != http.StatusOK {
		return nil, fmt.Errorf("/v1/completions: status %d body=%s", resp.Status, resp.Body)
	}
	out := &CompletionResponse{}
	if err := resp.JSON(out); err != nil {
		return nil, fmt.Errorf("decode completion: %w", err)
	}
	return out, nil
}

// EmbeddingsRequest mirrors the OpenAI v1 embeddings shape. Input is
// `any` because the spec accepts string, []string, []int (token ids),
// or [][]int — tests that need anything fancier should drop to
// Client.POST + RawBody.
type EmbeddingsRequest struct {
	Model string `json:"model"`
	Input any    `json:"input"`
}

// EmbeddingsDatum is one entry in an embeddings response.
type EmbeddingsDatum struct {
	Object    string    `json:"object"`
	Index     int       `json:"index"`
	Embedding []float64 `json:"embedding"`
}

// EmbeddingsResponse mirrors the OpenAI v1 embeddings reply.
// ZZRouterAttribution pins the provider-agnostic vendor block landed
// in main@ca07528d so a regression surfaces as a structural failure.
type EmbeddingsResponse struct {
	Object              string              `json:"object"`
	Model               string              `json:"model"`
	Data                []EmbeddingsDatum   `json:"data"`
	Usage               ChatCompletionUsage `json:"usage"`
	ZZRouterAttribution map[string]any      `json:"zzrouter,omitempty"`
}

// Embeddings sends POST /v1/embeddings and decodes the response.
func Embeddings(ctx context.Context, c *Client, req EmbeddingsRequest) (*EmbeddingsResponse, error) {
	if req.Model == "" {
		return nil, fmt.Errorf("Embeddings: model is required")
	}
	if req.Input == nil {
		return nil, fmt.Errorf("Embeddings: input is required")
	}
	resp, err := c.POST(ctx, "/v1/embeddings", req)
	if err != nil {
		return nil, fmt.Errorf("POST /v1/embeddings: %w", err)
	}
	if resp.Status != http.StatusOK {
		return nil, fmt.Errorf("/v1/embeddings: status %d body=%s", resp.Status, resp.Body)
	}
	out := &EmbeddingsResponse{}
	if err := resp.JSON(out); err != nil {
		return nil, fmt.Errorf("decode embeddings: %w", err)
	}
	return out, nil
}

// ChatCompletion sends POST /v1/chat/completions and decodes the
// non-streaming response. Streaming uses TailSSE on a raw POST.
//
// Caller-controlled context governs total request timeout — chat
// against a cold worker can take seconds before the first byte; tests
// pass a generous deadline (30s+).
func ChatCompletion(ctx context.Context, c *Client, req ChatCompletionRequest, opts ...ReqOpt) (*ChatCompletionResponse, error) {
	if req.Model == "" {
		return nil, fmt.Errorf("ChatCompletion: model is required")
	}
	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("ChatCompletion: at least one message is required")
	}
	resp, err := c.POST(ctx, "/v1/chat/completions", req, opts...)
	if err != nil {
		return nil, fmt.Errorf("POST /v1/chat/completions: %w", err)
	}
	if resp.Status != http.StatusOK {
		return nil, fmt.Errorf("/v1/chat/completions: status %d body=%s", resp.Status, resp.Body)
	}
	out := &ChatCompletionResponse{}
	if err := resp.JSON(out); err != nil {
		return nil, fmt.Errorf("decode chat completion: %w", err)
	}
	return out, nil
}
