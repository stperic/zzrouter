package wire

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/observability/llm"
)

// TestCopyWithMetrics_SynthesizesDoneIfMissing enforces invariant #7:
// [DONE] is always the last frame on the wire. When the upstream reader
// returns EOF before emitting data: [DONE], CopyWithMetrics must
// synthesize the terminator so SDK iterators on the client side release.
func TestCopyWithMetrics_SynthesizesDoneIfMissing(t *testing.T) {
	upstream := `data: {"choices":[{"delta":{"content":"hi"}}]}` + "\n\n"

	w := httptest.NewRecorder()
	w.Header().Set("Content-Type", "text/event-stream")
	r := strings.NewReader(upstream)

	CopyWithMetrics(w, r, nil, nil)

	out := w.Body.String()
	assert.True(t, strings.HasSuffix(out, "data: [DONE]\n\n"),
		"CopyWithMetrics must synthesize [DONE] when upstream EOFed early; got tail %q",
		tail(out, 40))
}

// TestCopyWithMetrics_NoDoneSynthesisOnNDJSON pins the Ollama-side
// regression: ollama-python's chat() iterator does
// `for line in r.iter_lines(): part = json.loads(line)` with no
// skip-empty + no skip-non-JSON guard. Appending `data: [DONE]\n\n`
// to an NDJSON stream crashes the SDK with
// "JSONDecodeError: Expecting value: line 1 column 1 (char 0)" on
// the trailing empty line. The Ollama protocol's terminator is
// `done: true` on the last JSON object — no extra sentinel.
func TestCopyWithMetrics_NoDoneSynthesisOnNDJSON(t *testing.T) {
	upstream := `{"model":"x","done":false,"message":{"role":"assistant","content":"hi"}}` + "\n" +
		`{"model":"x","done":true}` + "\n"

	w := httptest.NewRecorder()
	w.Header().Set("Content-Type", "application/x-ndjson")
	r := strings.NewReader(upstream)

	CopyWithMetrics(w, r, nil, nil)

	out := w.Body.String()
	assert.NotContains(t, out, "[DONE]",
		"CopyWithMetrics must NOT inject [DONE] on NDJSON streams; got: %q", out)
	// Body should be the upstream verbatim — every byte preserved.
	assert.Equal(t, upstream, out,
		"NDJSON stream must round-trip byte-for-byte (no synthesis, no rewrite)")
}

// TestCopyWithMetrics_PreservesExistingDone verifies no duplicate
// terminator is emitted when upstream already sent [DONE].
func TestCopyWithMetrics_PreservesExistingDone(t *testing.T) {
	upstream := `data: {"choices":[{"delta":{"content":"hi"}}]}` + "\n\n" +
		`data: [DONE]` + "\n\n"

	w := httptest.NewRecorder()
	r := strings.NewReader(upstream)

	CopyWithMetrics(w, r, nil, nil)

	out := w.Body.String()
	// Exactly one [DONE] frame.
	assert.Equal(t, 1, strings.Count(out, "data: [DONE]"),
		"exactly one [DONE] frame must appear on the wire; got output: %q", out)
}

// TestCopyWithMetrics_ClientDisconnectSkipsSyntheticDone verifies that
// when the first write to the client fails (mid-stream disconnect),
// CopyWithMetrics flips its clientGone flag and does NOT call
// SendSSEDone afterwards.
//
// The writer implements http.Flusher so canFlush=true forces the slow
// path (the fast path io.Copy would never exercise the clientGone
// gate). failAfter:0 makes every Write return io.ErrClosedPipe, so the
// first chunk attempt fails and we can count attempted writes — if
// SendSSEDone were called post-loop, we'd observe 2 writes; the
// invariant requires exactly 1.
func TestCopyWithMetrics_ClientDisconnectSkipsSyntheticDone(t *testing.T) {
	upstream := `data: {"choices":[{"delta":{"content":"hi"}}]}` + "\n\n"

	w := &failingWriter{buf: &bytes.Buffer{}, failAfter: 0}
	r := strings.NewReader(upstream)

	CopyWithMetrics(w, r, nil, nil)

	// Exactly one write attempt (the upstream chunk). If SendSSEDone had
	// fired post-loop, writes would be 2.
	assert.Equal(t, 1, w.writes,
		"expected exactly one write attempt; any more means SendSSEDone ran after clientGone")
	assert.Empty(t, w.buf.String(),
		"failing writer captured bytes despite ErrClosedPipe on every write")
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// failingWriter accepts the first failAfter writes and returns io.ErrClosedPipe
// on subsequent writes. Used to simulate a mid-stream client disconnect.
type failingWriter struct {
	buf       *bytes.Buffer
	failAfter int
	writes    int
	hdr       http.Header
}

func (f *failingWriter) Header() http.Header {
	if f.hdr == nil {
		f.hdr = http.Header{}
	}
	return f.hdr
}
func (f *failingWriter) WriteHeader(_ int) {}

// Flush satisfies http.Flusher so CopyWithMetrics takes the slow path
// (the fast path io.Copy skips the clientGone gate).
func (f *failingWriter) Flush() {}

func (f *failingWriter) Write(b []byte) (int, error) {
	f.writes++
	if f.writes > f.failAfter {
		return 0, io.ErrClosedPipe
	}
	return f.buf.Write(b)
}

// TestCopyWithMetrics_PopulatesResponseModel pins the streaming-path
// gen_ai.response.model integration. The OpenAI SSE wire shape carries
// `model` on every chunk but `usage` only on the terminal frame —
// SetResponseModel must run on the first model-bearing chunk so a
// stream that errors before the terminal frame still surfaces the
// upstream-served model. Regression for the should-fix #2 from the
// 2661d2d6 cold review.
func TestCopyWithMetrics_PopulatesResponseModel(t *testing.T) {
	// Two chunks: one delta-only carrying model, one terminal usage frame.
	upstream := `data: {"id":"1","model":"gpt-4-turbo-2024-04-09","choices":[{"delta":{"content":"hi"}}]}` + "\n\n" +
		`data: {"id":"1","model":"gpt-4-turbo-2024-04-09","usage":{"prompt_tokens":10,"completion_tokens":3}}` + "\n\n" +
		`data: [DONE]` + "\n\n"

	rec := llm.NewInferenceRecorder(context.Background(), "gpt-4-turbo", "openai")

	var captured llm.InferenceLogData
	llm.SetInferenceLogHook(hookFn(func(d llm.InferenceLogData) { captured = d }))
	defer llm.SetInferenceLogHook(nil)

	w := httptest.NewRecorder()
	w.Header().Set("Content-Type", "text/event-stream")
	CopyWithMetrics(w, strings.NewReader(upstream), rec, nil)
	rec.RecordCompletion(0, 0) // fire the hook so we see the captured payload

	assert.Equal(t, "gpt-4-turbo-2024-04-09", captured.ResponseModel,
		"streaming path must populate ResponseModel from any model-bearing chunk")
}

// TestCopyWithMetrics_ResponseModelOnDeltaOnly pins the regression case
// directly: a stream that only ever sends model-bearing delta chunks
// (no terminal usage frame, e.g. mid-stream upstream error) still
// surfaces response.model on the recorder.
func TestCopyWithMetrics_ResponseModelOnDeltaOnly(t *testing.T) {
	upstream := `data: {"id":"1","model":"gpt-4-turbo-2024-04-09","choices":[{"delta":{"content":"hi"}}]}` + "\n\n"

	rec := llm.NewInferenceRecorder(context.Background(), "gpt-4-turbo", "openai")

	var captured llm.InferenceLogData
	llm.SetInferenceLogHook(hookFn(func(d llm.InferenceLogData) { captured = d }))
	defer llm.SetInferenceLogHook(nil)

	w := httptest.NewRecorder()
	w.Header().Set("Content-Type", "text/event-stream")
	CopyWithMetrics(w, strings.NewReader(upstream), rec, nil)
	rec.RecordCompletion(0, 0)

	assert.Equal(t, "gpt-4-turbo-2024-04-09", captured.ResponseModel,
		"delta chunks alone must populate ResponseModel — the gate on usage>0 dropped this case before the cold-review fix")
}

// A streamed reply must reach the log reassembled, not chunk by chunk
// and not at all — the gap that made an operator read the next entry's
// history to find out what the model said.
func TestCopyWithMetrics_ReassemblesStreamedReply(t *testing.T) {
	upstream := `data: {"choices":[{"delta":{"role":"assistant"}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{"content":"Hey "}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{"content":"Eric!"}}]}` + "\n\n" +
		`data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":3}}` + "\n\n" +
		`data: [DONE]` + "\n\n"

	prev := llm.CaptureResponses()
	llm.SetCaptureResponses(true)
	defer llm.SetCaptureResponses(prev)

	rec := llm.NewInferenceRecorder(context.Background(), "gpt-4", "openai")
	var captured llm.InferenceLogData
	llm.SetInferenceLogHook(hookFn(func(d llm.InferenceLogData) { captured = d }))
	defer llm.SetInferenceLogHook(nil)

	w := httptest.NewRecorder()
	w.Header().Set("Content-Type", "text/event-stream")
	CopyWithMetrics(w, strings.NewReader(upstream), rec, nil)
	rec.RecordCompletion(0, 0)

	assert.Equal(t, "Hey Eric!", captured.ResponseText)
	assert.Contains(t, w.Body.String(), "Hey ", "capture must not disturb what the client receives")
}

// With capture off the stream is untouched and nothing is retained.
func TestCopyWithMetrics_ReplyNotCapturedWhenGated(t *testing.T) {
	upstream := `data: {"choices":[{"delta":{"content":"secret"}}]}` + "\n\n" + `data: [DONE]` + "\n\n"

	prev := llm.CaptureResponses()
	llm.SetCaptureResponses(false)
	defer llm.SetCaptureResponses(prev)

	rec := llm.NewInferenceRecorder(context.Background(), "gpt-4", "openai")
	var captured llm.InferenceLogData
	llm.SetInferenceLogHook(hookFn(func(d llm.InferenceLogData) { captured = d }))
	defer llm.SetInferenceLogHook(nil)

	w := httptest.NewRecorder()
	w.Header().Set("Content-Type", "text/event-stream")
	CopyWithMetrics(w, strings.NewReader(upstream), rec, nil)
	rec.RecordCompletion(0, 0)

	assert.Empty(t, captured.ResponseText)
	assert.Contains(t, w.Body.String(), "secret", "the client still gets the full stream")
}

// splitStream is the fixture for the read-boundary cases: the terminal
// usage frame is what carries the token counts, so cutting it in half is
// the difference between a metered request and a free one.
const splitStream = "data: {\"model\":\"engine-7b\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
	"data: {\"model\":\"engine-7b\",\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":7}}\n\n" +
	"data: [DONE]\n\n"

// A usage frame delivered in two reads must still be metered. Before
// line framing each half was invalid JSON, so the stream billed as zero
// tokens with no error anywhere — the silent undercount.
func TestCopyWithMetrics_MetersUsageFrameSplitAcrossReads(t *testing.T) {
	spy := installCompletionSpy(t)
	rec := llm.NewInferenceRecorder(context.Background(), "gpt-4", "openai")

	// Cut inside the usage object, which is where a real segment
	// boundary hurts.
	cut := strings.Index(splitStream, `"prompt_tokens"`) + 5
	body := &pieceReader{pieces: []string{splitStream[:cut], splitStream[cut:]}}

	w := httptest.NewRecorder()
	w.Header().Set("Content-Type", "text/event-stream")
	CopyWithMetrics(w, body, rec, nil)

	calls := spy.snapshot()
	require.Len(t, calls, 1, "exactly one completion per stream")
	assert.Equal(t, int64(11), calls[0].TokensIn, "a split usage frame must still meter its input tokens")
	assert.Equal(t, int64(7), calls[0].TokensOut, "a split usage frame must still meter its output tokens")
	assert.Equal(t, splitStream, w.Body.String(), "framing must not change what the client receives")
}

// The transforms parse a line at a time too, so a split frame used to
// reach them as truncated JSON and pass through carrying the engine's
// model id instead of the caller's. Framing closes that with the same
// mechanism, not a second one.
func TestCopyWithMetrics_TransformSeesWholeFrameSplitAcrossReads(t *testing.T) {
	cut := strings.Index(splitStream, `"engine-7b","choices":[]`) + 4
	body := &pieceReader{pieces: []string{splitStream[:cut], splitStream[cut:]}}

	w := httptest.NewRecorder()
	w.Header().Set("Content-Type", "text/event-stream")
	CopyWithMetrics(w, body, nil, func(chunk []byte) []byte {
		return RewriteModelInStreamChunk(chunk, "my-alias")
	})

	out := w.Body.String()
	assert.NotContains(t, out, "engine-7b", "every frame must reach the transform whole")
	assert.Equal(t, 2, strings.Count(out, `"model":"my-alias"`), "both model-bearing frames must be rewritten")
}

// An upstream that ends without a trailing newline still has to be
// metered: at EOF that last frame is complete.
func TestCopyWithMetrics_MetersFinalFrameWithoutTrailingNewline(t *testing.T) {
	spy := installCompletionSpy(t)
	rec := llm.NewInferenceRecorder(context.Background(), "gpt-4", "openai")
	upstream := `data: {"choices":[],"usage":{"prompt_tokens":4,"completion_tokens":2}}`

	w := httptest.NewRecorder()
	w.Header().Set("Content-Type", "text/event-stream")
	CopyWithMetrics(w, strings.NewReader(upstream), rec, nil)

	calls := spy.snapshot()
	require.Len(t, calls, 1)
	assert.Equal(t, int64(2), calls[0].TokensOut, "the held tail must be observed before the completion")
	assert.Contains(t, w.Body.String(), upstream, "the held tail must still reach the client")
}
