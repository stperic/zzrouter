package wire

import (
	"context"
	"io"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/observability/llm"
	"github.com/stperic/zzrouter/pkg/utils"
)

const usageStream = "data: {\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
	"data: {\"model\":\"m\",\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":7,\"total_tokens\":18}}\n\n" +
	"data: [DONE]\n\n"

func TestCopyStreamPassesBytesThrough(t *testing.T) {
	rec := llm.NewInferenceRecorder(context.Background(), "m", "mlx")
	body := strings.NewReader(usageStream)

	w := httptest.NewRecorder()
	CopyStream(t.Context(), w, body, RecorderObserver(rec), nil)
	got := w.Body.Bytes()

	assert.Equal(t, usageStream, string(got), "metering must not alter the stream")
}

func TestCopyStreamObservesUsage(t *testing.T) {
	rec := llm.NewInferenceRecorder(context.Background(), "m", "mlx")
	body := strings.NewReader(usageStream)
	time.Sleep(2 * time.Millisecond)

	CopyStream(t.Context(), httptest.NewRecorder(), body, RecorderObserver(rec), nil)

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

	CopyStream(t.Context(), httptest.NewRecorder(), body, RecorderObserver(rec), nil)

	calls := spy.snapshot()
	require.Len(t, calls, 1, "exactly one completion per stream")
	assert.Equal(t, int64(11), calls[0].TokensIn)
	assert.Equal(t, int64(7), calls[0].TokensOut)
}

func TestStreamMeter_FinishPublishesCompletion(t *testing.T) {
	spy := installCompletionSpy(t)
	meter := streamMeter{observer: RecorderObserver(llm.NewInferenceRecorder(t.Context(), "m", "mlx"))}
	meter.next([]byte(strings.TrimSuffix(usageStream, "data: [DONE]\n\n")))
	require.Empty(t, spy.snapshot())
	assert.Equal(t, Truncated, meter.finish(nil, false).Outcome)
	calls := spy.snapshot()
	require.Len(t, calls, 1)
	assert.Equal(t, int64(7), calls[0].TokensOut)
}

func TestStreamMeter_FinishPublishesCompletionOnce(t *testing.T) {
	spy := installCompletionSpy(t)
	meter := streamMeter{observer: RecorderObserver(llm.NewInferenceRecorder(t.Context(), "m", "mlx"))}
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
	CopyStream(t.Context(), w, body, RecorderObserver(rec), nil)
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

	CopyStream(t.Context(), httptest.NewRecorder(), body, RecorderObserver(rec), nil)

	calls := spy.snapshot()
	require.Len(t, calls, 1)
	assert.Equal(t, int64(2), calls[0].TokensOut, "the held tail must be observed before the completion")
}

func TestStreamMeter_FinishMetersHeldTail(t *testing.T) {
	spy := installCompletionSpy(t)
	meter := streamMeter{observer: RecorderObserver(llm.NewInferenceRecorder(t.Context(), "m", "mlx"))}
	meter.next([]byte(`data: {"model":"m","usage":{"prompt_tokens":4,"completion_tokens":2}}`))
	meter.finish(nil, false)
	calls := spy.snapshot()
	require.Len(t, calls, 1)
	assert.Equal(t, int64(2), calls[0].TokensOut)
}

func TestStreamObserversAgreeOnRecordedChat(t *testing.T) {
	recorded, err := os.ReadFile("../../protocol/anthropic/testdata/chat_vllm_0_29_0.sse")
	require.NoError(t, err)
	var steps []pacedStep
	for frame := range strings.SplitAfterSeq(string(recorded), "\n\n") {
		if frame != "" {
			steps = append(steps, pacedStep{wait: time.Millisecond, data: frame})
		}
	}
	synctest.Test(t, func(t *testing.T) {
		spy := installCompletionSpy(t)
		rec := llm.NewInferenceRecorder(t.Context(), "client-model", "vllm")
		public := httptest.NewRecorder()
		result := CopyStream(t.Context(), public, &pacedStream{steps: steps}, RecorderObserver(rec), nil)
		assert.Equal(t, Completed, result.Outcome)
		assert.Equal(t, string(recorded), public.Body.String())

		clock := func() time.Time { return utils.Now().Add(time.Hour) }
		start := clock()
		measured, err := NewStreamMeasurement(clock)
		require.NoError(t, err)
		assert.Equal(t, result, CopyStream(t.Context(), io.Discard, &pacedStream{steps: steps}, measured, nil))
		calls := spy.snapshot()
		require.Len(t, calls, 1)
		assert.Equal(t, "Qwen/Qwen2.5-0.5B", calls[0].ResponseModel)
		require.NotNil(t, measured.InputTokens)
		require.NotNil(t, measured.OutputTokens)
		assert.EqualValues(t, calls[0].TokensIn, *measured.InputTokens)
		assert.EqualValues(t, calls[0].TokensOut, *measured.OutputTokens)
		assert.Equal(t, 58, *measured.InputTokens)
		assert.Equal(t, 32, *measured.OutputTokens)
		assert.Equal(t, start.Add(2*time.Millisecond), measured.FirstContentToken)
		assert.Equal(t, time.Duration(calls[0].TTFTNs), measured.FirstContentToken.Sub(start))
		assert.Equal(t, start.Add(time.Duration(len(steps)-2)*time.Millisecond), measured.LastContentToken)
		assert.Empty(t, measured.ErrorCode)
	})
}

func TestStreamMeasurementPreservesAbsentAndZeroUsage(t *testing.T) {
	for _, tc := range []struct {
		name, frame   string
		input, output *int
	}{
		{name: "missing"},
		{name: "null usage", frame: `data: {"model":"engine","usage":null}`},
		{name: "zero output", frame: `data: {"usage":{"completion_tokens":0}}`, output: new(0)},
		{name: "zero input", frame: `data: {"usage":{"prompt_tokens":0}}`, input: new(0)},
		{name: "mlx usage object", frame: `data: {"object":"chat.completion","usage":{"prompt_tokens":3,"completion_tokens":0}}`, input: new(3), output: new(0)},
		{name: "ollama zero counts", frame: `{"done":true,"prompt_eval_count":0,"eval_count":0}`, input: new(0), output: new(0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := NewStreamMeasurement(func() time.Time { t.Fatal("no frame carried content"); return time.Time{} })
			require.NoError(t, err)
			result := CopyStream(t.Context(), io.Discard, strings.NewReader(tc.frame+"\n\ndata: [DONE]\n\n"), m, nil)
			assert.Equal(t, Completed, result.Outcome)
			assert.Equal(t, tc.input, m.InputTokens)
			assert.Equal(t, tc.output, m.OutputTokens)
			assert.True(t, m.FirstContentToken.IsZero())
			assert.True(t, m.LastContentToken.IsZero())
		})
	}
}

func TestStreamMeasurementRetainsWireFailureCode(t *testing.T) {
	for _, tc := range []struct {
		name, frame, code, recorderCode string
		canceled                        bool
		readError                       bool
	}{
		{name: "truncated EOF", code: "upstream_stream_interrupted", recorderCode: "upstream_stream_truncated"},
		{name: "read error", code: "upstream_stream_interrupted", recorderCode: "upstream_stream_error", readError: true},
		{name: "in-band failure", frame: "event: error\n\n", code: "upstream_stream_failed", recorderCode: "upstream_stream_failed"},
		{name: "ctx interrupted", canceled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.canceled {
				cancel()
			}
			body := func() io.Reader {
				if tc.readError {
					return newCutReader(tc.frame)
				}
				return strings.NewReader(tc.frame)
			}
			m, err := NewStreamMeasurement(utils.Now)
			require.NoError(t, err)
			result := CopyStream(ctx, io.Discard, body(), m, nil)
			assert.Equal(t, tc.code, m.ErrorCode)
			if failure, ok := result.Failure(); ok {
				assert.Equal(t, failure.Code, m.ErrorCode)
			}
			spy := installCompletionSpy(t)
			recorder := llm.NewInferenceRecorder(ctx, "m", "vllm")
			assert.Equal(t, result.Outcome, CopyStream(ctx, io.Discard, body(), RecorderObserver(recorder), nil).Outcome)
			calls := spy.snapshot()
			require.Len(t, calls, 1)
			assert.Equal(t, tc.recorderCode, calls[0].ErrorType)
		})
	}
}

func TestStreamMeasurementRequiresClock(t *testing.T) {
	m, err := NewStreamMeasurement(nil)
	require.Error(t, err)
	assert.Nil(t, m)
}

func TestStreamMeasurementZeroValuePanicsOnContent(t *testing.T) {
	var measurement StreamMeasurement
	assert.Panics(t, func() { measurement.ContentToken() })
}

func TestRecorderObserverKeepsLegacyUsageGate(t *testing.T) {
	for _, tc := range []struct {
		name, frame, responseModel string
		usageCandidate             bool
	}{
		{"vLLM zero stats", `data: {"model":"engine","usage":{"prompt_tokens":0,"completion_tokens":0,"prompt_tokens_details":{"cached_tokens":2},"completion_tokens_details":{"reasoning_tokens":3}}}`, "engine", true},
		{"Ollama zero counts", `{"model":"ignored","done":true,"prompt_eval_count":0,"eval_count":0}`, "original", true},
		{"substring in text", `{"response":"mentions eval_count in text","done":false}`, "original", false},
		{"model-less Ollama counts", `{"done":true,"prompt_eval_count":4,"eval_count":2}`, "original", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spy := installCompletionSpy(t)
			rec := llm.NewInferenceRecorder(t.Context(), "m", "vllm")
			rec.SetResponseModel("original")
			rec.SetExtendedUsage(3, 4, 0.125, "provider")
			rec.SetTokensOut(9)

			assert.Equal(t, tc.usageCandidate, streamUsageCandidate([]byte(tc.frame), false))
			CopyStream(t.Context(), io.Discard, strings.NewReader(tc.frame+"\n"), RecorderObserver(rec), func(frame []byte) []byte {
				snap := rec.Snapshot()
				assert.EqualValues(t, 9, snap.TokensOut)
				assert.Equal(t, 0.125, snap.Cost)
				assert.Equal(t, "provider", snap.CostSource)
				return frame
			})
			calls := spy.snapshot()
			require.Len(t, calls, 1)
			assert.Zero(t, calls[0].TokensIn)
			assert.Zero(t, calls[0].TokensOut)
			assert.Zero(t, calls[0].TokensCached)
			assert.Zero(t, calls[0].TokensReasoning)
			assert.Equal(t, tc.responseModel, calls[0].ResponseModel)
		})
	}
}
