package wire

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSetModelIfPresent(t *testing.T) {
	t.Run("replaces an existing model", func(t *testing.T) {
		out := SetModelIfPresent([]byte(`{"model":"/weights/org/model","choices":[]}`), "org/model")
		assert.Equal(t, "org/model", ModelFromBody(out))
	})

	t.Run("never invents the field", func(t *testing.T) {
		in := []byte(`{"choices":[]}`)
		assert.Equal(t, in, SetModelIfPresent(in, "org/model"))
	})

	t.Run("non-JSON passes through", func(t *testing.T) {
		in := []byte("not json")
		assert.Equal(t, in, SetModelIfPresent(in, "org/model"))
	})
}

func TestRewriteModelInStreamChunk(t *testing.T) {
	t.Run("rewrites every frame carrying a model", func(t *testing.T) {
		chunk := []byte("data: {\"model\":\"/weights/org/model\",\"choices\":[]}\n\n" +
			"data: {\"model\":\"/weights/org/model\",\"choices\":[]}\n\n")
		out := string(RewriteModelInStreamChunk(chunk, "org/model"))
		assert.NotContains(t, out, "/weights/org/model")
		assert.Equal(t, 2, countSubstr(out, `"model":"org/model"`))
	})

	t.Run("leaves framing and terminator alone", func(t *testing.T) {
		chunk := []byte("data: {\"model\":\"/w/m\"}\n\ndata: [DONE]\n\n")
		out := string(RewriteModelInStreamChunk(chunk, "org/model"))
		assert.Contains(t, out, "data: [DONE]")
		assert.True(t, len(out) > 0 && out[len(out)-2:] == "\n\n", "trailing framing preserved")
	})

	t.Run("chunks without a model are untouched", func(t *testing.T) {
		chunk := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		assert.Equal(t, chunk, RewriteModelInStreamChunk(chunk, "org/model"))
	})
}

func countSubstr(s, sub string) int {
	n := 0
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			n++
		}
	}
	return n
}

// Ollama streams NDJSON — bare objects, no `data: ` prefix — and
// wire.IsStreaming counts application/x-ndjson as streaming. An
// SSE-only rewriter therefore left every /api/* stream naming the
// replica while the same request with stream:false was rewritten.
func TestRewriteModelInStreamChunk_NDJSON(t *testing.T) {
	t.Run("bare objects are rewritten", func(t *testing.T) {
		chunk := []byte(`{"model":"m","done":false}` + "\n" + `{"model":"m","done":true}` + "\n")
		out := string(RewriteModelInStreamChunk(chunk, "route-x"))
		assert.Equal(t, 2, strings.Count(out, `"model":"route-x"`))
		assert.NotContains(t, out, `"model":"m"`)
	})

	t.Run("mixed framing in one chunk", func(t *testing.T) {
		chunk := []byte("data: {\"model\":\"m\"}\n{\"model\":\"m\"}\ndata: [DONE]\n")
		out := string(RewriteModelInStreamChunk(chunk, "route-x"))
		assert.Contains(t, out, "data: {\"model\":\"route-x\"}")
		assert.Contains(t, out, "\n{\"model\":\"route-x\"}")
		assert.Contains(t, out, "data: [DONE]", "the sentinel must survive untouched")
	})

	t.Run("non-JSON lines are left alone", func(t *testing.T) {
		chunk := []byte(": keep-alive\n\ndata: [DONE]\n")
		assert.Equal(t, chunk, RewriteModelInStreamChunk(chunk, "route-x"))
	})
}

// A frame split across a read boundary arrives here as unparseable JSON.
// It must come out byte-identical: the continuation is appended to it by
// the client, so a swallowed trailing space silently joins two tokens.
// This is the property the call-site comment in commit.go claims, and it
// did not hold while the line was rebuilt from its trimmed form.
func TestRewriteModelInStreamChunk_LeavesUnrewritableLinesByteIdentical(t *testing.T) {
	tests := []struct {
		name  string
		chunk string
	}{
		{
			name:  "truncated mid-string, trailing space is load-bearing",
			chunk: `data: {"model":"m","choices":[{"delta":{"content":"hello `,
		},
		{
			name:  "truncated mid-key",
			chunk: `data: {"model":"m","choi`,
		},
		{
			name:  "already the caller's id",
			chunk: `data: {"model":"route-x","choices":[]}` + "\n\n",
		},
		{
			name:  "NDJSON split across the boundary",
			chunk: `{"model":"m","message":{"content":"tok `,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := []byte(tt.chunk)
			assert.Equal(t, string(in), string(RewriteModelInStreamChunk(in, "route-x")))
		})
	}
}

// A rewrite must replace the payload and nothing else — the framing bytes
// around it belong to the transport.
func TestRewriteModelInStreamChunk_PreservesFramingBytes(t *testing.T) {
	t.Run("CRLF survives", func(t *testing.T) {
		out := string(RewriteModelInStreamChunk(
			[]byte("data: {\"model\":\"m\",\"choices\":[]}\r\n\r\n"), "route-x"))
		assert.Contains(t, out, `"model":"route-x"`)
		assert.True(t, strings.HasSuffix(out, "\r\n\r\n"),
			"frame terminator was rewritten: %q", out)
	})

	t.Run("leading indent survives", func(t *testing.T) {
		out := string(RewriteModelInStreamChunk(
			[]byte("  data: {\"model\":\"m\"}\n"), "route-x"))
		assert.True(t, strings.HasPrefix(out, "  data: "), "got %q", out)
	})
}
