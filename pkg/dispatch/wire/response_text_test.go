package wire

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestResponseText_AcrossProviderShapes(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "openai chat",
			body: `{"choices":[{"message":{"role":"assistant","content":"Hey Eric!"}}]}`,
			want: "Hey Eric!",
		},
		{
			name: "openai completions",
			body: `{"choices":[{"text":"a completion"}]}`,
			want: "a completion",
		},
		{
			name: "ollama chat",
			body: `{"message":{"role":"assistant","content":"from ollama"},"done":true}`,
			want: "from ollama",
		},
		{
			name: "ollama generate",
			body: `{"response":"generated text","done":true}`,
			want: "generated text",
		},
		{name: "error envelope", body: `{"error":{"message":"nope"}}`},
		{name: "empty choices", body: `{"choices":[]}`},
		{name: "not json", body: `<html>502</html>`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ResponseText([]byte(tt.body)))
		})
	}
}

func TestStreamDeltaText_ReassemblesInOrder(t *testing.T) {
	chunks := []string{
		"data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\"Hey \"}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\"Eric\"}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\"!\"}}]}\n\ndata: [DONE]\n\n",
	}

	var got string
	for _, c := range chunks {
		got += StreamDeltaText([]byte(c))
	}

	assert.Equal(t, "Hey Eric!", got)
}

// One read off the wire can carry several frames.
func TestStreamDeltaText_MultipleFramesInOneChunk(t *testing.T) {
	chunk := "data: {\"choices\":[{\"delta\":{\"content\":\"one \"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"two\"}}]}\n\n"

	assert.Equal(t, "one two", StreamDeltaText([]byte(chunk)))
}

func TestStreamDeltaText_OllamaNDJSON(t *testing.T) {
	chunk := `{"message":{"content":"partial "},"done":false}` + "\n" +
		`{"message":{"content":"reply"},"done":true}` + "\n"

	assert.Equal(t, "partial reply", StreamDeltaText([]byte(chunk)))
}

func TestStreamDeltaText_IgnoresNonContentFrames(t *testing.T) {
	assert.Empty(t, StreamDeltaText([]byte("data: [DONE]\n\n")))
	assert.Empty(t, StreamDeltaText([]byte(": keep-alive\n\n")))
	assert.Empty(t, StreamDeltaText([]byte("\n\n")))
	assert.Empty(t, StreamDeltaText([]byte(`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`)))
}

// Usage-only terminal frames must not contribute text.
func TestStreamDeltaText_TerminalUsageFrame(t *testing.T) {
	frame := `data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5}}`

	assert.Empty(t, StreamDeltaText([]byte(frame)))
}
