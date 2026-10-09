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

func TestCopyStream_EOFWithoutTerminalIsTruncated(t *testing.T) {
	upstream := `data: {"choices":[{"delta":{"content":"hi"}}]}` + "\n\n"
	w := httptest.NewRecorder()
	result := CopyStream(context.Background(), w, strings.NewReader(upstream), nil, nil)
	assert.Equal(t, Truncated, result.Outcome)
	assert.NoError(t, result.Err)
	assert.Equal(t, upstream, w.Body.String())
	assert.NotContains(t, w.Body.String(), "[DONE]")
}

func TestCopyStream_NDJSONPassesThrough(t *testing.T) {
	upstream := `{"model":"x","done":false,"message":{"role":"assistant","content":"hi"}}` + "\n" +
		`{"model":"x","done":true}` + "\n"
	w := httptest.NewRecorder()
	result := CopyStream(context.Background(), w, strings.NewReader(upstream), nil, nil)
	assert.Equal(t, Completed, result.Outcome)
	assert.Equal(t, upstream, w.Body.String())
}

func TestCopyStream_PreservesExistingDone(t *testing.T) {
	upstream := `data: {"choices":[{"delta":{"content":"hi"}}]}` + "\n\n" + "data: [DONE]\n\n"
	w := httptest.NewRecorder()
	result := CopyStream(context.Background(), w, strings.NewReader(upstream), nil, nil)
	assert.Equal(t, Completed, result.Outcome)
	assert.Equal(t, upstream, w.Body.String())
}

func TestCopyStream_ClientGone(t *testing.T) {
	w := &failingWriter{buf: &bytes.Buffer{}, failAfter: 0}
	result := CopyStream(context.Background(), w, strings.NewReader(usageStream), nil, nil)
	assert.Equal(t, ClientGone, result.Outcome)
	assert.ErrorIs(t, result.Err, io.ErrClosedPipe)
	assert.Equal(t, 1, w.writes)
	assert.Empty(t, w.buf.String())
}

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

func (f *failingWriter) Flush() {}

func (f *failingWriter) Write(b []byte) (int, error) {
	f.writes++
	if f.writes > f.failAfter {
		return 0, io.ErrClosedPipe
	}
	return f.buf.Write(b)
}

func TestCopyStream_PopulatesResponseModel(t *testing.T) {
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
	CopyStream(context.Background(), w, strings.NewReader(upstream), rec, nil)
	rec.RecordCompletion(0, 0) // fire the hook so we see the captured payload

	assert.Equal(t, "gpt-4-turbo-2024-04-09", captured.ResponseModel,
		"streaming path must populate ResponseModel from any model-bearing chunk")
}

func TestCopyStream_ResponseModelOnDeltaOnly(t *testing.T) {
	upstream := `data: {"id":"1","model":"gpt-4-turbo-2024-04-09","choices":[{"delta":{"content":"hi"}}]}` + "\n\n"

	rec := llm.NewInferenceRecorder(context.Background(), "gpt-4-turbo", "openai")

	var captured llm.InferenceLogData
	llm.SetInferenceLogHook(hookFn(func(d llm.InferenceLogData) { captured = d }))
	defer llm.SetInferenceLogHook(nil)

	w := httptest.NewRecorder()
	w.Header().Set("Content-Type", "text/event-stream")
	CopyStream(context.Background(), w, strings.NewReader(upstream), rec, nil)

	assert.Equal(t, "gpt-4-turbo-2024-04-09", captured.ResponseModel,
		"delta chunks alone must populate ResponseModel — the gate on usage>0 dropped this case before the cold-review fix")
}

func TestCopyStream_ReassemblesStreamedReply(t *testing.T) {
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
	CopyStream(context.Background(), w, strings.NewReader(upstream), rec, nil)

	assert.Equal(t, "Hey Eric!", captured.ResponseText)
	assert.Contains(t, w.Body.String(), "Hey ", "capture must not disturb what the client receives")
}

// With capture off the stream is untouched and nothing is retained.
func TestCopyStream_ReplyNotCapturedWhenGated(t *testing.T) {
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
	CopyStream(context.Background(), w, strings.NewReader(upstream), rec, nil)

	assert.Empty(t, captured.ResponseText)
	assert.Contains(t, w.Body.String(), "secret", "the client still gets the full stream")
}

const splitStream = "data: {\"model\":\"engine-7b\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
	"data: {\"model\":\"engine-7b\",\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":7}}\n\n" +
	"data: [DONE]\n\n"

func TestCopyStream_MetersUsageFrameSplitAcrossReads(t *testing.T) {
	spy := installCompletionSpy(t)
	rec := llm.NewInferenceRecorder(context.Background(), "gpt-4", "openai")

	cut := strings.Index(splitStream, `"prompt_tokens"`) + 5
	body := &pieceReader{pieces: []string{splitStream[:cut], splitStream[cut:]}}

	w := httptest.NewRecorder()
	w.Header().Set("Content-Type", "text/event-stream")
	CopyStream(context.Background(), w, body, rec, nil)

	calls := spy.snapshot()
	require.Len(t, calls, 1, "exactly one completion per stream")
	assert.Equal(t, int64(11), calls[0].TokensIn, "a split usage frame must still meter its input tokens")
	assert.Equal(t, int64(7), calls[0].TokensOut, "a split usage frame must still meter its output tokens")
	assert.Equal(t, splitStream, w.Body.String(), "framing must not change what the client receives")
}

func TestCopyStream_TransformSeesWholeFrameSplitAcrossReads(t *testing.T) {
	cut := strings.Index(splitStream, `"engine-7b","choices":[]`) + 4
	body := &pieceReader{pieces: []string{splitStream[:cut], splitStream[cut:]}}

	w := httptest.NewRecorder()
	w.Header().Set("Content-Type", "text/event-stream")
	CopyStream(context.Background(), w, body, nil, func(chunk []byte) []byte {
		return RewriteModelInStreamChunk(chunk, "my-alias")
	})

	out := w.Body.String()
	assert.NotContains(t, out, "engine-7b", "every frame must reach the transform whole")
	assert.Equal(t, 2, strings.Count(out, `"model":"my-alias"`), "both model-bearing frames must be rewritten")
}

func TestCopyStream_MetersFinalFrameWithoutTrailingNewline(t *testing.T) {
	spy := installCompletionSpy(t)
	rec := llm.NewInferenceRecorder(context.Background(), "gpt-4", "openai")
	upstream := `data: {"choices":[],"usage":{"prompt_tokens":4,"completion_tokens":2}}`

	w := httptest.NewRecorder()
	w.Header().Set("Content-Type", "text/event-stream")
	CopyStream(context.Background(), w, strings.NewReader(upstream), rec, nil)

	calls := spy.snapshot()
	require.Len(t, calls, 1)
	assert.Equal(t, int64(2), calls[0].TokensOut, "the held tail must be observed before the completion")
	assert.Contains(t, w.Body.String(), upstream, "the held tail must still reach the client")
}
