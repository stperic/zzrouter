package chat

import (
	"context"

	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
)

// SendChatRequestStream sends a streaming chat completion request to the server.
func SendChatRequestStream(ctx context.Context, client *pkgClient.Client, modelName string, messages []pkgClient.ChatMessage, temperature, topP float64, maxTokens int, preferredNode string, callback func(pkgClient.StreamChunk) error) error {
	req := pkgClient.ChatRequest{
		Model:         modelName,
		Messages:      messages,
		Stream:        true,
		PreferredNode: preferredNode,
	}

	if temperature >= 0 {
		req.Temperature = &temperature
	}
	if topP >= 0 && topP <= 1.0 {
		req.TopP = &topP
	}
	if maxTokens > 0 {
		req.MaxTokens = maxTokens
	}

	return client.ChatCompletionsStream(ctx, req, callback)
}
