package wire

import (
	"context"
	"io"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/observability/llm"
)

// The two testdata/anthropic_messages_llamacpp* fixtures are verbatim
// captures from llama-server's /v1/messages (Qwen3.8-27B, 2026-10-04).
// The stream reports 4 uncached + 55 cache-read input tokens on
// message_start and 20 output tokens on message_delta.

func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	require.NoError(t, err)
	return string(b)
}

func captureCompletion(t *testing.T) *llm.InferenceLogData {
	t.Helper()
	var captured llm.InferenceLogData
	llm.SetInferenceLogHook(hookFn(func(d llm.InferenceLogData) { captured = d }))
	t.Cleanup(func() { llm.SetInferenceLogHook(nil) })
	return &captured
}

func TestParseUsageExtended_AnthropicMessage(t *testing.T) {
	u := ParseUsageExtended([]byte(readFixture(t, "anthropic_messages_llamacpp.json")))
	assert.Equal(t, UsageData{In: 59, Out: 40, Model: "Qwen3.8-27B-Q8_0"}, u)
}

// Anthropic counts cache reads and writes outside input_tokens; In must
// put them back or a cached conversation meters as nearly free.
// Fabricated from the documented Anthropic shape; no capture.
func TestParseUsageExtended_AnthropicCacheTokensCountAsInput(t *testing.T) {
	body := `{"type":"message","model":"claude-x","usage":{"input_tokens":10,` +
		`"cache_read_input_tokens":300,"cache_creation_input_tokens":50,"output_tokens":7}}`
	u := ParseUsageExtended([]byte(body))
	assert.Equal(t, int64(360), u.In)
	assert.Equal(t, int64(7), u.Out)
	assert.Equal(t, int64(300), u.CachedTokens)
}

func TestParseUsageExtended_AnthropicMessageStartNestsUsage(t *testing.T) {
	frame := `{"type":"message_start","message":{"model":"m","usage":{"cache_read_input_tokens":55,"input_tokens":4,"output_tokens":0}}}`
	u := ParseUsageExtended([]byte(frame))
	assert.Equal(t, UsageData{In: 59, CachedTokens: 55, Model: "m"}, u)
}

// The Responses API shares the input_tokens name but already counts the
// cached share inside it.
func TestParseUsageExtended_ResponsesCachedIsSubsetOfInput(t *testing.T) {
	body := `{"model":"m","usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":40},` +
		`"output_tokens":9,"output_tokens_details":{"reasoning_tokens":3}}}`
	u := ParseUsageExtended([]byte(body))
	assert.Equal(t, UsageData{In: 100, Out: 9, CachedTokens: 40, ReasoningTokens: 3, Model: "m"}, u)
}

func TestParseUsageExtended_BothVocabulariesReadAsChat(t *testing.T) {
	body := `{"usage":{"prompt_tokens":12,"completion_tokens":3,"input_tokens":999,"output_tokens":999}}`
	u := ParseUsageExtended([]byte(body))
	assert.Equal(t, int64(12), u.In)
	assert.Equal(t, int64(3), u.Out)
}

func TestCopyWithMetrics_AnthropicStreamMetersAndPassesThrough(t *testing.T) {
	stream := readFixture(t, "anthropic_messages_llamacpp_stream.sse")
	captured := captureCompletion(t)
	rec := llm.NewInferenceRecorder(context.Background(), "Qwen3.8-27B-Q8_0", "llamacpp")

	w := httptest.NewRecorder()
	w.Header().Set("Content-Type", "text/event-stream")
	// One byte per read puts message_start and message_delta in
	// separate frames, as a live upstream does.
	CopyWithMetrics(w, iotest.OneByteReader(strings.NewReader(stream)), rec, nil)

	assert.Equal(t, stream, w.Body.String(),
		"an Anthropic stream must reach the client byte for byte, with no [DONE] appended")
	assert.Equal(t, int64(59), captured.TokensIn, "input comes from message_start")
	assert.Equal(t, int64(20), captured.TokensOut, "output comes from message_delta")
	assert.Equal(t, int64(55), captured.TokensCached)
}

func TestMeteringReader_AnthropicStreamMergesStartAndDelta(t *testing.T) {
	stream := readFixture(t, "anthropic_messages_llamacpp_stream.sse")
	captured := captureCompletion(t)
	rec := llm.NewInferenceRecorder(context.Background(), "Qwen3.8-27B-Q8_0", "llamacpp")

	got, err := io.ReadAll(NewMeteringReader(io.NopCloser(iotest.OneByteReader(strings.NewReader(stream))), rec))
	require.NoError(t, err)

	assert.Equal(t, stream, string(got))
	assert.Equal(t, int64(59), captured.TokensIn)
	assert.Equal(t, int64(20), captured.TokensOut)
	assert.Equal(t, int64(55), captured.TokensCached)
}

// A stream cut before its terminal event still gets no [DONE]: the
// client speaks a protocol that has no such frame.
func TestCopyWithMetrics_NoDoneOnTruncatedNamedEventStream(t *testing.T) {
	stream := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":4}}}\n\n"
	w := httptest.NewRecorder()
	w.Header().Set("Content-Type", "text/event-stream")
	CopyWithMetrics(w, strings.NewReader(stream), nil, nil)
	assert.NotContains(t, w.Body.String(), "[DONE]")
}

// A message_delta that repeats input_tokens without the cache counts
// must not shrink the total message_start reported.
func TestMeteringReader_PartialDeltaDoesNotShrinkInput(t *testing.T) {
	stream := "event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":4,"cache_read_input_tokens":55}}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","usage":{"input_tokens":4,"output_tokens":20}}` + "\n\n"
	captured := captureCompletion(t)
	rec := llm.NewInferenceRecorder(context.Background(), "m", "llamacpp")

	_, err := io.ReadAll(NewMeteringReader(io.NopCloser(iotest.OneByteReader(strings.NewReader(stream))), rec))
	require.NoError(t, err)

	assert.Equal(t, int64(59), captured.TokensIn)
	assert.Equal(t, int64(20), captured.TokensOut)
}

func TestParseUsageExtended_CompletionOnlyChatFrameKeepsOutput(t *testing.T) {
	u := ParseUsageExtended([]byte(`{"usage":{"completion_tokens":8}}`))
	assert.Equal(t, int64(8), u.Out)
}

// A Responses-shaped body that also echoes an Anthropic cache count is
// read as Responses; the cached share is already inside input_tokens.
func TestParseUsageExtended_ResponsesIgnoresAnthropicCacheEcho(t *testing.T) {
	body := `{"usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":40},"cache_read_input_tokens":40,"output_tokens":1}}`
	u := ParseUsageExtended([]byte(body))
	assert.Equal(t, int64(100), u.In)
	assert.Equal(t, int64(40), u.CachedTokens)
}
