package wire

import (
	"context"
	"io"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/stperic/zzrouter/pkg/observability/llm"
)

func TestContentBearing(t *testing.T) {
	tests := []struct {
		name string
		line string
		want bool
	}{
		// OpenAI chat chunks
		{"openai content", `data: {"choices":[{"delta":{"content":"hi"}}]}`, true},
		{"openai reasoning_content", `data: {"choices":[{"delta":{"reasoning_content":"hmm"}}]}`, true},
		{"openai reasoning", `data: {"choices":[{"delta":{"reasoning":"hmm"}}]}`, true},
		{"openai tool_calls", `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{"}}]}}]}`, true},
		{"openai bare json", `{"choices":[{"delta":{"content":"hi"}}]}`, true},

		// OpenAI legacy completions
		{"legacy completion text", `data: {"choices":[{"text":"hi","index":0}]}`, true},

		// Anthropic
		{"anthropic content_block_delta", `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`, true},
		{"anthropic delta without prefix", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"x"}}`, true},

		// Responses
		{"responses output_text.delta", `data: {"type":"response.output_text.delta","delta":"hi"}`, true},
		{"responses reasoning_text.delta", `data: {"type":"response.reasoning_text.delta","delta":"hm"}`, true},
		{"responses function_call_arguments.delta", `data: {"type":"response.function_call_arguments.delta","delta":"{"}`, true},

		// Ollama NDJSON
		{"ollama message content", `{"model":"m","message":{"role":"assistant","content":"hi"},"done":false}`, true},
		{"ollama message thinking", `{"model":"m","message":{"role":"assistant","thinking":"hm"},"done":false}`, true},
		{"ollama message tool_calls", `{"model":"m","message":{"role":"assistant","tool_calls":[{"function":{"name":"f"}}]},"done":false}`, true},
		{"ollama generate response", `{"model":"m","response":"hi","done":false}`, true},

		// Negatives
		{"openai role-only delta", `data: {"choices":[{"delta":{"role":"assistant"}}]}`, false},
		{"openai empty content", `data: {"choices":[{"delta":{"content":""}}]}`, false},
		{"openai empty tool_calls", `data: {"choices":[{"delta":{"tool_calls":[]}}]}`, false},
		{"openai null tool_calls", `data: {"choices":[{"delta":{"tool_calls":null}}]}`, false},
		{"openai usage-only chunk", `data: {"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7}}`, false},
		{"anthropic message_start", `data: {"type":"message_start","message":{"id":"m","content":[]}}`, false},
		{"anthropic content_block_start", `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`, false},
		{"anthropic event line", `event: content_block_delta`, false},
		{"anthropic event message_start", `event: message_start`, false},
		{"responses response.created", `data: {"type":"response.created","response":{"id":"r"}}`, false},
		{"ollama empty content", `{"model":"m","message":{"role":"assistant","content":""},"done":false}`, false},
		{"ollama done frame", `{"model":"m","response":"","done":true}`, false},
		{"done sentinel", `data: [DONE]`, false},
		{"done sentinel bare", `[DONE]`, false},
		{"sse comment", `: ping`, false},
		{"blank line", ``, false},
		{"malformed json", `data: {"choices":[`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, contentBearing([]byte(tt.line)))
		})
	}
}

// pacedStep is one Read of a stream: the reader sleeps for wait, then delivers data.
type pacedStep struct {
	wait time.Duration
	data string
}

// pacedStream delivers its steps one Read at a time, so a test can put real time between frames.
type pacedStream struct {
	steps []pacedStep
	cur   []byte
}

func (p *pacedStream) Read(b []byte) (int, error) {
	if len(p.cur) == 0 {
		if len(p.steps) == 0 {
			return 0, io.EOF
		}
		step := p.steps[0]
		p.steps = p.steps[1:]
		time.Sleep(step.wait)
		p.cur = []byte(step.data)
	}
	n := copy(b, p.cur)
	p.cur = p.cur[n:]
	return n, nil
}

// ttftFor copies a paced stream through the meter and returns the recorded TTFT in milliseconds.
func ttftFor(t *testing.T, steps ...pacedStep) int64 {
	t.Helper()
	rec := llm.NewInferenceRecorder(context.Background(), "m", "mlx")
	CopyStream(t.Context(), httptest.NewRecorder(), &pacedStream{steps: steps}, rec, nil)
	return rec.Snapshot().TTFTMs
}

// Each gap is well above the millisecond rounding of TTFTMs, so a lower bound
// shows which frame stamped the first token.
const ttftGap = 30 * time.Millisecond

func TestCopyStreamTTFTStampsOnContentNotRole(t *testing.T) {
	ttft := ttftFor(t,
		pacedStep{data: "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n"},
		pacedStep{wait: ttftGap, data: "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"},
		pacedStep{data: "data: [DONE]\n\n"},
	)
	assert.GreaterOrEqual(t, ttft, ttftGap.Milliseconds()-5,
		"TTFT must be stamped at the content frame, not the role-only frame before it")
}

func TestCopyStreamTTFTStampsOnceAtFirstContent(t *testing.T) {
	// A second content frame arrives later; a restamp would move TTFT past the first one.
	ttft := ttftFor(t,
		pacedStep{data: "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n"},
		pacedStep{wait: ttftGap, data: "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n"},
		pacedStep{wait: ttftGap, data: "data: {\"choices\":[{\"delta\":{\"content\":\"b\"}}]}\n\n"},
	)
	assert.GreaterOrEqual(t, ttft, ttftGap.Milliseconds()-5)
	assert.Less(t, ttft, 2*ttftGap.Milliseconds(), "a second content frame must not restamp TTFT")
}

func TestCopyStreamTTFTFromReasoningOnly(t *testing.T) {
	ttft := ttftFor(t,
		pacedStep{data: "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n"},
		pacedStep{wait: ttftGap, data: "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"hmm\"}}]}\n\n"},
		pacedStep{data: "data: [DONE]\n\n"},
	)
	assert.GreaterOrEqual(t, ttft, ttftGap.Milliseconds()-5, "reasoning-only stream must record TTFT")
}

func TestCopyStreamNoContentRecordsNoTTFT(t *testing.T) {
	// The first frame is late, so a meter that stamps on any frame would show a TTFT here.
	ttft := ttftFor(t,
		pacedStep{wait: ttftGap, data: "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n"},
		pacedStep{data: "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":0}}\n\n"},
		pacedStep{data: "data: [DONE]\n\n"},
	)
	assert.Zero(t, ttft, "a stream with no content must not record TTFT")
}
