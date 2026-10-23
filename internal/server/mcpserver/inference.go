package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	commonmcp "github.com/stperic/common/mcp"
	"github.com/stperic/zzrouter/pkg/version"
)

const inferenceInstructions = "Use zzrouter_inference_models to find a model ID, then zzrouter_chat or zzrouter_embeddings. These tools call the existing inference API with your user key, including its model access and quota rules."

type inferenceMessage struct {
	Role    string `json:"role" jsonschema:"Message role: system, user or assistant."`
	Content string `json:"content" jsonschema:"Message text."`
}

type inferenceChatInput struct {
	Model          string             `json:"model" jsonschema:"Model ID from zzrouter_inference_models, optionally qualified with @node."`
	Messages       []inferenceMessage `json:"messages" jsonschema:"Conversation messages in order."`
	MaxTokens      *int               `json:"max_tokens,omitempty" jsonschema:"Optional maximum output tokens."`
	Temperature    *float64           `json:"temperature,omitempty" jsonschema:"Optional sampling temperature."`
	IdempotencyKey string             `json:"idempotency_key,omitempty" jsonschema:"Stable key to reuse only when retrying this same chat request."`
}

type inferenceEmbeddingInput struct {
	Model string   `json:"model" jsonschema:"Embedding model ID from zzrouter_inference_models, optionally qualified with @node."`
	Input []string `json:"input" jsonschema:"Texts to embed."`
}

type inferenceOutput map[string]any

func (out inferenceOutput) summary() string {
	// JSON decoded by dispatcher.call is safe to encode again. Keep the full
	// REST result available to MCP clients that only read text content.
	body, err := json.Marshal(out)
	if err != nil {
		return "Inference API response could not be encoded as text."
	}
	return string(body)
}

// NewInference builds the user-keyed MCP handler from the existing inference API.
func NewInference(cfg Config) http.Handler {
	s := commonmcp.NewServer(commonmcp.Options{
		Name: "zzrouter-inference", Version: version.Current.String(), Instructions: inferenceInstructions, ToolsListTTL: toolsListTTL,
	})
	bindings := map[string][]string{}
	read := &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: new(bool)}
	write := &mcp.ToolAnnotations{DestructiveHint: new(bool)}
	registerTool(s, cfg, bindings, &mcp.Tool{Name: "zzrouter_inference_models", Description: "List model IDs and capabilities through the user inference API.", Annotations: read}, nil,
		func(d dispatcher, ctx context.Context, _ struct{}) (inferenceOutput, error) {
			return inferenceCall(d, ctx, request{method: http.MethodGet, path: requestPath{"/v1/models"}})
		})
	registerTool(s, cfg, bindings, &mcp.Tool{Name: "zzrouter_chat", Description: "Send a non-streaming chat request through the user inference API, with its access and quota checks.", Annotations: write}, nil,
		func(d dispatcher, ctx context.Context, in inferenceChatInput) (inferenceOutput, error) {
			body := map[string]any{"model": in.Model, "messages": in.Messages, "stream": false}
			if in.MaxTokens != nil {
				body["max_tokens"] = *in.MaxTokens
			}
			if in.Temperature != nil {
				body["temperature"] = *in.Temperature
			}
			return inferenceCall(d, ctx, request{method: http.MethodPost, path: requestPath{"/v1/chat/completions"}, jsonBody: body, contentType: "application/json", idempotencyKey: in.IdempotencyKey})
		})
	registerTool(s, cfg, bindings, &mcp.Tool{Name: "zzrouter_embeddings", Description: "Embed texts through the user inference API, with its access and quota checks.", Annotations: write}, nil,
		func(d dispatcher, ctx context.Context, in inferenceEmbeddingInput) (inferenceOutput, error) {
			return inferenceCall(d, ctx, request{method: http.MethodPost, path: requestPath{"/v1/embeddings"}, jsonBody: map[string]any{"model": in.Model, "input": in.Input}, contentType: "application/json"})
		})
	return httpHandler(s)
}

func inferenceCall(d dispatcher, ctx context.Context, req request) (inferenceOutput, error) {
	var out inferenceOutput
	err := d.call(ctx, req, &out)
	return out, err
}
