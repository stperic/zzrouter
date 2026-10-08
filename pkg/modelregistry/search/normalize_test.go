package search

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNormalizeCloudResults(t *testing.T) {
	tests := []struct {
		name            string
		input           map[string]any
		wantAPIID       string
		wantDisplayName string
	}{
		{
			name:            "OpenRouter: id=API, name=display",
			input:           map[string]any{"id": "qwen/qwen3.6-plus:free", "name": "Qwen: Qwen3.6 Plus (free)"},
			wantAPIID:       "qwen/qwen3.6-plus:free",
			wantDisplayName: "Qwen: Qwen3.6 Plus (free)",
		},
		{
			name:            "Cloudflare: id=UUID, name=API",
			input:           map[string]any{"id": "d7948af7-749a-4fa3-8480-2a9f4215f427", "name": "@cf/moonshotai/kimi-k2.5"},
			wantAPIID:       "@cf/moonshotai/kimi-k2.5",
			wantDisplayName: "@cf/moonshotai/kimi-k2.5",
		},
		{
			name:            "Google Gemini: id with models/ prefix, no name",
			input:           map[string]any{"id": "models/gemini-2.5-flash"},
			wantAPIID:       "gemini-2.5-flash",
			wantDisplayName: "gemini-2.5-flash",
		},
		{
			name:            "Google Gemini: id with models/ prefix, display_name from provider",
			input:           map[string]any{"id": "models/gemini-2.5-flash", "display_name": "Gemini 2.5 Flash"},
			wantAPIID:       "gemini-2.5-flash",
			wantDisplayName: "Gemini 2.5 Flash",
		},
		{
			name:            "OpenAI: id=API, no name",
			input:           map[string]any{"id": "gpt-4o"},
			wantAPIID:       "gpt-4o",
			wantDisplayName: "gpt-4o",
		},
		{
			name:            "Anthropic: id=API, display_name already set",
			input:           map[string]any{"id": "claude-3-sonnet-20240229", "display_name": "Claude 3 Sonnet"},
			wantAPIID:       "claude-3-sonnet-20240229",
			wantDisplayName: "Claude 3 Sonnet",
		},
		{
			name:            "Groq: id=API, no name",
			input:           map[string]any{"id": "llama-3.1-8b-instant"},
			wantAPIID:       "llama-3.1-8b-instant",
			wantDisplayName: "llama-3.1-8b-instant",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			results := []any{tt.input}
			NormalizeCloudResults(results)

			m, ok := results[0].(map[string]any)
			assert.True(t, ok)
			assert.Equal(t, tt.wantAPIID, m["api_id"], "api_id")
			assert.Equal(t, tt.wantDisplayName, m["display_name"], "display_name")
		})
	}
}

func TestIsUUID(t *testing.T) {
	assert.True(t, isUUID("d7948af7-749a-4fa3-8480-2a9f4215f427"))
	assert.True(t, isUUID("01564C52-8717-47DC-8EFD-907A2CA18301"))
	assert.False(t, isUUID("gpt-4o"))
	assert.False(t, isUUID("qwen/qwen3.6-plus:free"))
	assert.False(t, isUUID("@cf/meta/llama-3"))
	assert.False(t, isUUID("models/gemini-2.5-flash"))
	assert.False(t, isUUID(""))
}
