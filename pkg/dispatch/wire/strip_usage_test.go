package wire

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestStripUsageOnlyFrames(t *testing.T) {
	tests := []struct {
		name  string
		chunk string
		want  string
	}{
		{
			// Dropping the data line leaves its separators behind as a
			// zero-length SSE frame, which the spec says to ignore. Cheaper
			// than reasoning about which newline belongs to which frame.
			name:  "drops the usage-only frame",
			chunk: "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":3}}\n\n",
			want:  "\n",
		},
		{
			// Some providers attach usage to the last content frame. Dropping
			// that takes the caller's final token with it.
			name:  "keeps usage riding on a content frame",
			chunk: "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}],\"usage\":{\"prompt_tokens\":5}}\n\n",
			want:  "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}],\"usage\":{\"prompt_tokens\":5}}\n\n",
		},
		{
			// `"usage":null` rides along on every delta frame OpenAI and vLLM
			// emit, so the cheap substring scan is not enough on its own.
			name:  "keeps a delta frame carrying a null usage",
			chunk: "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}],\"usage\":null}\n\n",
			want:  "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}],\"usage\":null}\n\n",
		},
		{
			name:  "leaves a chunk with no usage untouched",
			chunk: "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n",
			want:  "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n",
		},
		{
			name:  "leaves the terminator alone",
			chunk: "data: [DONE]\n\n",
			want:  "data: [DONE]\n\n",
		},
		{
			// A frame split across a read boundary reaches the transform as
			// unparseable JSON. Dropping it would delete half a frame that
			// its continuation still expects.
			// Truncated PAST the "usage" key on purpose: cut before it and
			// the cheap substring scan returns early, so the case never
			// reaches the parse path it claims to cover.
			name:  "leaves a split frame alone",
			chunk: "data: {\"choices\":[],\"usage\":{\"prompt_to",
			want:  "data: {\"choices\":[],\"usage\":{\"prompt_to",
		},
		{
			name:  "drops only the usage frame out of several",
			chunk: "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\ndata: {\"choices\":[],\"usage\":{\"total_tokens\":9}}\n\ndata: [DONE]\n\n",
			want:  "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n\ndata: [DONE]\n\n",
		},
		{
			// NDJSON framing has no data: prefix. streamFrameJSON handles it,
			// so the transform must not be SSE-only.
			name:  "handles ndjson framing",
			chunk: "{\"choices\":[],\"usage\":{\"total_tokens\":9}}\n",
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, string(StripUsageOnlyFrames([]byte(tt.chunk))))
		})
	}
}

// Dropping a data line leaves its blank separator behind. That is a
// zero-length SSE frame, which the spec says to ignore, and it keeps the
// transform from having to reason about which newline belongs to which frame.
func TestStripUsageOnlyFrames_LeavesParseableSSE(t *testing.T) {
	chunk := "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\ndata: {\"choices\":[],\"usage\":{\"total_tokens\":9}}\n\ndata: [DONE]\n\n"

	out := string(StripUsageOnlyFrames([]byte(chunk)))

	assert.NotContains(t, out, "total_tokens", "the usage frame must be gone")
	assert.Contains(t, out, `"content":"a"`, "content must survive")
	assert.Contains(t, out, "[DONE]", "the terminator must survive")
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		assert.True(t, strings.HasPrefix(line, "data: "),
			"every non-blank line must still be a well-formed SSE field, got %q", line)
	}
}
