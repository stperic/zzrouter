package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	commonmcp "github.com/stperic/common/mcp"
	"github.com/stperic/zzrouter/pkg/version"
)

const inferenceInstructions = "Use zzrouter_inference_models to find a model ID, then zzrouter_chat or zzrouter_embeddings. These tools call the existing inference API with your user key, including its model access and quota rules. Preserve native JSON fields through options. Transport is non-streaming JSON with a 1 MiB request/result cap. Discover native operation summaries with zzrouter_operations, fetch schemas with zzrouter_operation, and read ordinary native/routed metadata with zzrouter_connections. The current native diagnostic adapter is administrator-only."

type inferenceChatInput struct {
	Model          string                     `json:"model" jsonschema:"Model ID from zzrouter_inference_models, optionally qualified with @node."`
	Messages       []json.RawMessage          `json:"messages" jsonschema:"Native conversation message objects in order, including content parts, assistant tool_calls and tool results."`
	MaxTokens      *int                       `json:"max_tokens,omitempty" jsonschema:"Optional maximum output tokens."`
	Temperature    *float64                   `json:"temperature,omitempty" jsonschema:"Optional sampling temperature."`
	IdempotencyKey string                     `json:"idempotency_key,omitempty" jsonschema:"Stable key to reuse only when retrying this same chat request."`
	Options        map[string]json.RawMessage `json:"options,omitempty" jsonschema:"Additional native JSON fields, such as tools, tool_choice, response_format and provider options. Common fields and stream/stream_options are refused here."`
}

type inferenceEmbeddingInput struct {
	Model   string                     `json:"model" jsonschema:"Embedding model ID from zzrouter_inference_models, optionally qualified with @node."`
	Input   json.RawMessage            `json:"input" jsonschema:"Native embedding input: one string, strings, token IDs, or arrays of token IDs as supported by the selected API."`
	Options map[string]json.RawMessage `json:"options,omitempty" jsonschema:"Additional native JSON fields, such as dimensions, encoding_format, user and provider options. model/input and stream/stream_options are refused here."`
}

// NewInference builds the user-keyed MCP handler from the existing inference API.
func NewInference(cfg Config) http.Handler {
	s := commonmcp.NewServer(commonmcp.Options{
		Name: "zzrouter-inference", Version: version.Current.String(), Instructions: inferenceInstructions, ToolsListTTL: toolsListTTL,
	})
	registerInferenceTools(s, cfg)
	return httpHandler(s)
}

// InferenceOperationBindings declares the REST operations served by user MCP tools.
func InferenceOperationBindings() map[string][]string { return registerInferenceTools(nil, Config{}) }

func registerInferenceTools(s *mcp.Server, cfg Config) map[string][]string {
	bindings := map[string][]string{}
	read := &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: new(bool)}
	write := &mcp.ToolAnnotations{DestructiveHint: new(bool)}
	registerTool(s, cfg, bindings, &mcp.Tool{Name: "zzrouter_inference_models", Description: "List model IDs and capabilities through the user inference API.", Annotations: read}, nil,
		func(d dispatcher, ctx context.Context, _ struct{}) (jsonOutput, error) {
			return jsonCall(d, ctx, request{method: http.MethodGet, path: requestPath{"/v1/models"}})
		})
	registerTool(s, cfg, bindings, &mcp.Tool{Name: "zzrouter_chat", Description: "Send a non-streaming chat request through the user inference API, with its access and quota checks.", Annotations: write}, nil,
		func(d dispatcher, ctx context.Context, in inferenceChatInput) (jsonOutput, error) {
			body := map[string]any{"model": in.Model, "messages": in.Messages, "stream": false}
			if in.MaxTokens != nil {
				body["max_tokens"] = *in.MaxTokens
			}
			if in.Temperature != nil {
				body["temperature"] = *in.Temperature
			}
			encoded, err := inferenceJSON(body, in.Options, "model", "messages", "max_tokens", "temperature")
			if err != nil {
				return nil, err
			}
			return jsonCall(d, ctx, request{method: http.MethodPost, path: requestPath{"/v1/chat/completions"}, body: encoded, contentType: "application/json", idempotencyKey: in.IdempotencyKey})
		})
	registerTool(s, cfg, bindings, &mcp.Tool{Name: "zzrouter_embeddings", Description: "Embed texts through the user inference API, with its access and quota checks.", Annotations: write}, nil,
		func(d dispatcher, ctx context.Context, in inferenceEmbeddingInput) (jsonOutput, error) {
			encoded, err := inferenceJSON(map[string]any{"model": in.Model, "input": in.Input}, in.Options, "model", "input")
			if err != nil {
				return nil, err
			}
			return jsonCall(d, ctx, request{method: http.MethodPost, path: requestPath{"/v1/embeddings"}, body: encoded, contentType: "application/json"})
		})
	registerProviderTools(s, cfg, bindings, true)
	return bindings
}
