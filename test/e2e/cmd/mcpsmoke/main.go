// Command mcpsmoke checks the coordinator's admin and user MCP endpoints.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type keyTransport struct {
	base http.RoundTripper
	key  string
}

func (t keyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	copy := req.Clone(req.Context())
	copy.Header.Set("X-API-Key", t.key)
	return t.base.RoundTrip(copy)
}

func connect(ctx context.Context, url, key string) (*mcp.ClientSession, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: "zzrouter-mcpsmoke", Version: "1"}, nil)
	return client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   url,
		HTTPClient: &http.Client{Transport: keyTransport{base: http.DefaultTransport, key: key}},
	}, nil)
}

func call(ctx context.Context, session *mcp.ClientSession, name string, args map[string]any) (map[string]any, error) {
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if result.IsError {
		if result.StructuredContent != nil {
			body, _ := json.Marshal(result.StructuredContent)
			return nil, fmt.Errorf("%s returned a tool error: %s", name, body)
		}
		if len(result.Content) > 0 {
			if content, ok := result.Content[0].(*mcp.TextContent); ok {
				return nil, fmt.Errorf("%s returned a tool error: %s", name, content.Text)
			}
		}
		return nil, fmt.Errorf("%s returned a tool error", name)
	}
	out, ok := result.StructuredContent.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s returned no structured object", name)
	}
	return out, nil
}

func requireArray(name string, out map[string]any, field string) error {
	if _, ok := out[field].([]any); !ok {
		return fmt.Errorf("%s response has no %s array", name, field)
	}
	return nil
}

func requireItems(name string, out map[string]any, field string) error {
	items, ok := out[field].([]any)
	if !ok {
		return fmt.Errorf("%s response has no %s array", name, field)
	}
	if len(items) == 0 {
		return fmt.Errorf("%s response has an empty %s array", name, field)
	}
	return nil
}

func requireChatReply(out map[string]any) error {
	if err := requireItems("zzrouter_chat", out, "choices"); err != nil {
		return err
	}
	choice, ok := out["choices"].([]any)[0].(map[string]any)
	if !ok {
		return errors.New("zzrouter_chat returned an invalid choice")
	}
	message, ok := choice["message"].(map[string]any)
	if !ok {
		return errors.New("zzrouter_chat returned no assistant message")
	}
	content, ok := message["content"].(string)
	if !ok || content == "" {
		return errors.New("zzrouter_chat returned no assistant text")
	}
	return nil
}

func requireEmbedding(out map[string]any) error {
	if err := requireItems("zzrouter_embeddings", out, "data"); err != nil {
		return err
	}
	entry, ok := out["data"].([]any)[0].(map[string]any)
	if !ok {
		return errors.New("zzrouter_embeddings returned an invalid item")
	}
	vector, ok := entry["embedding"].([]any)
	if !ok || len(vector) == 0 {
		return errors.New("zzrouter_embeddings returned no vector")
	}
	for _, value := range vector {
		if _, ok := value.(float64); !ok {
			return errors.New("zzrouter_embeddings returned a nonnumeric vector")
		}
	}
	return nil
}

func requireTools(ctx context.Context, session *mcp.ClientSession, names ...string) error {
	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		return err
	}
	found := make(map[string]bool, len(listed.Tools))
	for _, tool := range listed.Tools {
		found[tool.Name] = true
	}
	for _, name := range names {
		if !found[name] {
			return fmt.Errorf("MCP tool %s is missing", name)
		}
	}
	return nil
}

func smoke(ctx context.Context, adminURL, userURL, adminKey, userKey, chatModel, embeddingModel string) error {
	if adminKey == "" || userKey == "" || adminKey == userKey {
		return errors.New("distinct admin and user keys are required")
	}
	admin, err := connect(ctx, adminURL, adminKey)
	if err != nil {
		return fmt.Errorf("admin MCP connect: %w", err)
	}
	defer admin.Close()
	if err := requireTools(ctx, admin, "zz_overview", "zz_nodes", "zz_providers", "zz_run_preview", "zz_run_start", "zz_run_stop"); err != nil {
		return fmt.Errorf("admin MCP catalog: %w", err)
	}
	for _, name := range []string{"zz_overview", "zz_nodes", "zz_providers"} {
		if _, err := call(ctx, admin, name, map[string]any{}); err != nil {
			return err
		}
	}
	fmt.Println("admin MCP: catalog and cluster inventory passed")

	user, err := connect(ctx, userURL, userKey)
	if err != nil {
		return fmt.Errorf("user MCP connect: %w", err)
	}
	defer user.Close()
	if err := requireTools(ctx, user, "zzrouter_inference_models", "zzrouter_chat", "zzrouter_embeddings"); err != nil {
		return fmt.Errorf("user MCP catalog: %w", err)
	}
	models, err := call(ctx, user, "zzrouter_inference_models", map[string]any{})
	if err != nil {
		return err
	}
	if err := requireArray("zzrouter_inference_models", models, "data"); err != nil {
		return err
	}
	if chatModel != "" {
		chat, err := call(ctx, user, "zzrouter_chat", map[string]any{
			"model": chatModel, "messages": []map[string]string{{"role": "user", "content": "Reply with OK."}}, "max_tokens": 128,
		})
		if err != nil {
			return err
		}
		if err := requireChatReply(chat); err != nil {
			return err
		}
	}
	if embeddingModel != "" {
		embedding, err := call(ctx, user, "zzrouter_embeddings", map[string]any{"model": embeddingModel, "input": []string{"MCP smoke test"}})
		if err != nil {
			return err
		}
		if err := requireEmbedding(embedding); err != nil {
			return err
		}
	}
	fmt.Println("user MCP: catalog and requested inference passed")
	return nil
}

func main() {
	adminURL := flag.String("admin-url", "http://127.0.0.1:9090/zzrouter/v1/mcp", "coordinator admin MCP endpoint")
	userURL := flag.String("user-url", "http://127.0.0.1:9090/inference/mcp", "coordinator user MCP endpoint")
	chatModel := flag.String("chat-model", "", "optional model ID for user chat")
	embeddingModel := flag.String("embedding-model", "", "optional model ID for user embeddings")
	flag.Parse()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := smoke(ctx, *adminURL, *userURL, os.Getenv("ZZROUTER_ADMIN_API_KEY"), os.Getenv("ZZROUTER_API_KEY"), *chatModel, *embeddingModel); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
