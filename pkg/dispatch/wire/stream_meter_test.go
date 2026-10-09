package wire

import (
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/observability/llm"
)

const usageStream = "data: {\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
	"data: {\"model\":\"m\",\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":7,\"total_tokens\":18}}\n\n" +
	"data: [DONE]\n\n"

func TestCopyStreamPassesBytesThrough(t *testing.T) {
	rec := llm.NewInferenceRecorder(context.Background(), "m", "mlx")
	body := strings.NewReader(usageStream)

	w := httptest.NewRecorder()
	CopyStream(t.Context(), w, body, rec, nil)
	got := w.Body.Bytes()

	assert.Equal(t, usageStream, string(got), "metering must not alter the stream")
}

func TestCopyStreamObservesUsage(t *testing.T) {
	rec := llm.NewInferenceRecorder(context.Background(), "m", "mlx")
	body := strings.NewReader(usageStream)
	time.Sleep(2 * time.Millisecond)

	CopyStream(t.Context(), httptest.NewRecorder(), body, rec, nil)

	snap := rec.Snapshot()
	assert.Equal(t, int64(7), snap.TokensOut, "terminal usage frame must reach the recorder")
	assert.Positive(t, snap.TTFTMs, "first chunk must record time-to-first-token")
}

type completionSpy struct {
	mu    sync.Mutex
	calls []llm.InferenceLogData
}

func (c *completionSpy) OnInferenceComplete(d llm.InferenceLogData) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, d)
}

func (c *completionSpy) snapshot() []llm.InferenceLogData {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]llm.InferenceLogData(nil), c.calls...)
}

func installCompletionSpy(t *testing.T) *completionSpy {
	t.Helper()
	spy := &completionSpy{}
	llm.SetInferenceLogHook(spy)
	t.Cleanup(func() { llm.SetInferenceLogHook(nil) })
	return spy
}

func TestCopyStreamPublishesCompletionOnEOF(t *testing.T) {
	spy := installCompletionSpy(t)
	rec := llm.NewInferenceRecorder(context.Background(), "m", "mlx")
	body := strings.NewReader(usageStream)

	CopyStream(t.Context(), httptest.NewRecorder(), body, rec, nil)

	calls := spy.snapshot()
	require.Len(t, calls, 1, "exactly one completion per stream")
	assert.Equal(t, int64(11), calls[0].TokensIn)
	assert.Equal(t, int64(7), calls[0].TokensOut)
}

func TestStreamMeter_FinishPublishesCompletion(t *testing.T) {
	spy := installCompletionSpy(t)
	meter := streamMeter{recorder: llm.NewInferenceRecorder(t.Context(), "m", "mlx")}
	meter.next([]byte(strings.TrimSuffix(usageStream, "data: [DONE]\n\n")))
	require.Empty(t, spy.snapshot())
	assert.Equal(t, Truncated, meter.finish(nil, false).Outcome)
	calls := spy.snapshot()
	require.Len(t, calls, 1)
	assert.Equal(t, int64(7), calls[0].TokensOut)
}

func TestStreamMeter_FinishPublishesCompletionOnce(t *testing.T) {
	spy := installCompletionSpy(t)
	meter := streamMeter{recorder: llm.NewInferenceRecorder(t.Context(), "m", "mlx")}
	meter.next([]byte(usageStream))
	meter.finish(io.EOF, false)
	meter.finish(nil, false)
	assert.Len(t, spy.snapshot(), 1)
}

func TestCopyStreamNilRecorderIsPassThrough(t *testing.T) {
	w := httptest.NewRecorder()
	result := CopyStream(t.Context(), w, strings.NewReader(usageStream), nil, nil)
	assert.Equal(t, usageStream, w.Body.String())
	assert.Equal(t, Completed, result.Outcome)
}

func TestCopyStreamMetersUsageFrameSplitAcrossReads(t *testing.T) {
	spy := installCompletionSpy(t)
	rec := llm.NewInferenceRecorder(context.Background(), "m", "mlx")

	cut := strings.Index(usageStream, `"completion_tokens"`) + 8
	body := &pieceReader{pieces: []string{usageStream[:cut], usageStream[cut:]}}

	w := httptest.NewRecorder()
	CopyStream(t.Context(), w, body, rec, nil)
	got := w.Body.Bytes()

	calls := spy.snapshot()
	require.Len(t, calls, 1, "exactly one completion per stream")
	assert.Equal(t, int64(11), calls[0].TokensIn, "a split usage frame must still meter its input tokens")
	assert.Equal(t, int64(7), calls[0].TokensOut, "a split usage frame must still meter its output tokens")
	assert.Equal(t, usageStream, string(got), "observation must not alter the stream")
}

func TestCopyStreamMetersFinalFrameWithoutTrailingNewline(t *testing.T) {
	spy := installCompletionSpy(t)
	rec := llm.NewInferenceRecorder(context.Background(), "m", "mlx")
	body := strings.NewReader(`data: {"model":"m","usage":{"prompt_tokens":4,"completion_tokens":2}}`)

	CopyStream(t.Context(), httptest.NewRecorder(), body, rec, nil)

	calls := spy.snapshot()
	require.Len(t, calls, 1)
	assert.Equal(t, int64(2), calls[0].TokensOut, "the held tail must be observed before the completion")
}

func TestStreamMeter_FinishMetersHeldTail(t *testing.T) {
	spy := installCompletionSpy(t)
	meter := streamMeter{recorder: llm.NewInferenceRecorder(t.Context(), "m", "mlx")}
	meter.next([]byte(`data: {"model":"m","usage":{"prompt_tokens":4,"completion_tokens":2}}`))
	meter.finish(nil, false)
	calls := spy.snapshot()
	require.Len(t, calls, 1)
	assert.Equal(t, int64(2), calls[0].TokensOut)
}
