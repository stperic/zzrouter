package wire

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/observability/llm"
)

func streamOutcomeCases() []struct {
	name    string
	frame   string
	outcome Outcome
} {
	return []struct {
		name, frame string
		outcome     Outcome
	}{
		{"openai", "data: [DONE]\n\n", Completed},
		{"finish_reason", "data: {\"choices\":[{\"finish_reason\":\"stop\"}]}\n\n", Completed},
		{"anthropic", "event: message_stop\n\n", Completed},
		{"anthropic_json", "data: {\"type\":\"message_stop\"}\n\n", Completed},
		{"ollama", "{\"done\":true}\n", Completed},
		{"ollama_pull", "{\"status\":\"success\"}\n", Completed},
		{"ollama_progress", "{\"status\":\"pulling\"}\n", Truncated},
		{"responses", "data: {\"type\":\"response.completed\"}\n\n", Completed},
		{"responses_event", "event: response.completed\n\n", Completed},
		{"responses_incomplete", "data: {\"type\":\"response.incomplete\"}\n\n", Completed},
		{"responses_incomplete_event", "event: response.incomplete\n\n", Completed},
		{"openai_error", "data: {\"error\":{\"message\":\"failed\"}}\n\n", UpstreamFailed},
		{"ollama_error", "{\"error\":\"failed\"}\n", UpstreamFailed},
		{"anthropic_error", "event: error\n\n", UpstreamFailed},
		{"responses_error", "data: {\"type\":\"response.failed\"}\n\n", UpstreamFailed},
		{"responses_error_event", "event: response.failed\n\n", UpstreamFailed},
		{"failure_then_done", "data: {\"error\":\"failed\"}\n\ndata: [DONE]\n\n", UpstreamFailed},
		{"error_with_unexpected_done", "data: {\"error\":\"failed\",\"done\":\"unknown\"}\n\n", UpstreamFailed},
		{"failure_with_unexpected_choices", "data: {\"type\":\"response.failed\",\"choices\":{}}\n\n", UpstreamFailed},
		{"top_level_null_error_is_no_error", "data: {\"error\":null}\n\n", Truncated},
		{"empty", "", Truncated},
		{"nonterminal_event", "event: message_delta\n\n", Truncated},
		{"null_finish_reason", "data: {\"choices\":[{\"finish_reason\":null}]}\n\n", Truncated},
		{"nested_error", "data: {\"content\":{\"error\":\"text\"}}\n\n", Truncated},
		{"done_in_content", "data: {\"content\":\"[DONE]\"}\n\n", Truncated},
		{"event_prefix", "event: message_stop_extra\n\n", Truncated},
		{"malformed_json", "data: {\"done\":true\n", Truncated},
		{"no_newline", "data: [DONE]", Completed},
	}
}

func TestCopyStream_ReadErrorIsTruncatedAndWritesNoDone(t *testing.T) {
	upstream := "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"
	w := httptest.NewRecorder()
	result := CopyStream(context.Background(), w, newCutReader(upstream), nil, nil)
	assert.Equal(t, Truncated, result.Outcome)
	assert.ErrorIs(t, result.Err, io.ErrUnexpectedEOF)
	assert.Equal(t, upstream, w.Body.String())
	assert.NotContains(t, w.Body.String(), "[DONE]")
}

func TestCopyStream_FinishReasonWithoutDoneIsCompleted(t *testing.T) {
	w := httptest.NewRecorder()
	result := CopyStream(context.Background(), w, strings.NewReader("data: {\"choices\":[{\"finish_reason\":\"stop\"}]}\n\n"), nil, nil)
	assert.Equal(t, Completed, result.Outcome)
	assert.NoError(t, result.Err)
	assert.NotContains(t, w.Body.String(), "[DONE]")
}

func TestCopyStream_TerminalFramesPerDialect(t *testing.T) {
	for _, tc := range streamOutcomeCases() {
		if tc.outcome != Completed {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			result := CopyStream(context.Background(), w, strings.NewReader(tc.frame), nil, nil)
			assert.Equal(t, Completed, result.Outcome)
			assert.Equal(t, tc.frame, w.Body.String())
		})
	}
}

func TestCopyStream_InBandErrorIsUpstreamFailed(t *testing.T) {
	for _, tc := range streamOutcomeCases() {
		if tc.outcome != UpstreamFailed {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			result := CopyStream(context.Background(), w, strings.NewReader(tc.frame), nil, nil)
			assert.Equal(t, UpstreamFailed, result.Outcome)
			assert.NoError(t, result.Err)
			assert.Equal(t, tc.frame, w.Body.String())
		})
	}
}

func TestResult_FailureOnlyForTruncated(t *testing.T) {
	for _, outcome := range []Outcome{Completed, Truncated, UpstreamFailed, ClientGone} {
		failure, ok := (Result{Outcome: outcome, Err: io.ErrUnexpectedEOF}).Failure()
		assert.Equal(t, outcome == Truncated, ok)
		if ok {
			assert.Equal(t, http.StatusBadGateway, failure.Status)
			assert.Equal(t, "api_error", failure.Type)
			assert.Equal(t, string(httperr.CodeUpstreamStreamInterrupted), failure.Code)
			assert.Equal(t, "The upstream stream ended before completing.", failure.Message)
		} else {
			assert.Equal(t, httperr.Error{}, failure)
		}
	}
}

func TestStreamMeter_AgreesWithCopyStream(t *testing.T) {
	for _, tc := range streamOutcomeCases() {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			result := CopyStream(t.Context(), w, iotest.OneByteReader(strings.NewReader(tc.frame)), nil, nil)
			meter := streamMeter{}
			for i := range len(tc.frame) {
				meter.next([]byte(tc.frame[i : i+1]))
			}
			assert.Equal(t, tc.outcome, result.Outcome)
			assert.Equal(t, result, meter.finish(io.EOF, false))
			assert.Equal(t, tc.frame, w.Body.String())
		})
	}
}

func TestStreamMeter_FinishBeforeEOF(t *testing.T) {
	unterminated := strings.TrimSuffix(usageStream, "data: [DONE]\n\n")
	for _, tc := range []struct {
		name, stream string
		canceled     bool
		want         Outcome
	}{
		{"live", unterminated, false, Truncated},
		{"canceled", unterminated, true, ClientGone},
		{"terminal_then_finish", usageStream, false, Completed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			meter := streamMeter{}
			meter.next([]byte(tc.stream))
			assert.Equal(t, tc.want, meter.finish(nil, tc.canceled).Outcome)
		})
	}
}

func TestStreamMeter_UsageRecordedOnceOnEveryOutcome(t *testing.T) {
	usage := "data: {\"model\":\"m\",\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":7,\"prompt_tokens_details\":{\"cached_tokens\":3},\"completion_tokens_details\":{\"reasoning_tokens\":2},\"cost\":0.25}}\n\n"
	for _, tc := range []struct {
		name      string
		outcome   Outcome
		errorType string
	}{
		{"completed", Completed, ""}, {"eof", Truncated, "upstream_stream_truncated"},
		{"read_error", Truncated, "upstream_stream_error"}, {"failed", UpstreamFailed, "upstream_stream_failed"},
		{"client", ClientGone, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spy := installCompletionSpy(t)
			rec := llm.NewInferenceRecorder(t.Context(), "m", "openai")
			stream := usage
			if tc.outcome == Completed {
				stream += "data: [DONE]\n\n"
			}
			if tc.outcome == UpstreamFailed {
				stream += "event: error\n\n"
			}
			var reader io.Reader = strings.NewReader(stream)
			if tc.name == "read_error" {
				reader = newCutReader(stream)
			}
			var w http.ResponseWriter = httptest.NewRecorder()
			if tc.outcome == ClientGone {
				w = &failingWriter{buf: &bytes.Buffer{}}
			}
			result := CopyStream(t.Context(), w, reader, rec, nil)
			assert.Equal(t, tc.outcome, result.Outcome)
			calls := spy.snapshot()
			require.Len(t, calls, 1)
			assert.Equal(t, int64(11), calls[0].TokensIn)
			assert.Equal(t, int64(7), calls[0].TokensOut)
			assert.Equal(t, int64(3), calls[0].TokensCached)
			assert.Equal(t, int64(2), calls[0].TokensReasoning)
			assert.Equal(t, 0.25, calls[0].Cost)
			assert.Equal(t, tc.errorType, calls[0].ErrorType)
		})
	}
}

func TestCopyStream_OutcomeUsesRawFrames(t *testing.T) {
	for _, tc := range []struct {
		input, output string
		want          Outcome
	}{
		{"data: [DONE]\n\n", "data: {}\n\n", Completed},
		{"data: {}\n\n", "data: [DONE]\n\n", Truncated},
		{"event: error\n\n", "data: [DONE]\n\n", UpstreamFailed},
	} {
		w := httptest.NewRecorder()
		result := CopyStream(context.Background(), w, strings.NewReader(tc.input), nil, func([]byte) []byte { return []byte(tc.output) })
		assert.Equal(t, tc.want, result.Outcome)
		assert.Equal(t, tc.output, w.Body.String())
	}
}

type shortWriter struct{ http.ResponseWriter }

func (w shortWriter) Write(b []byte) (int, error) { return len(b) - 1, nil }

func TestCopyStream_NoFlusherAndShortWrite(t *testing.T) {
	w := shortWriter{httptest.NewRecorder()}
	result := CopyStream(context.Background(), w, strings.NewReader("data: [DONE]\n\n"), nil, nil)
	assert.Equal(t, ClientGone, result.Outcome)
	assert.ErrorIs(t, result.Err, io.ErrShortWrite)
}

type blockingBody struct {
	started chan struct{}
	closed  chan struct{}
}

func (b *blockingBody) Read([]byte) (int, error) {
	close(b.started)
	<-b.closed
	return 0, io.ErrClosedPipe
}
func (b *blockingBody) Close() error { close(b.closed); return nil }

func TestCopyStream_ClosedBodyUnblocksRead(t *testing.T) {
	spy := installCompletionSpy(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	source := &blockingBody{started: make(chan struct{}), closed: make(chan struct{})}
	result := make(chan Result, 1)
	go func() {
		result <- CopyStream(ctx, httptest.NewRecorder(), source, llm.NewInferenceRecorder(ctx, "m", "openai"), nil)
	}()
	<-source.started
	cancel()
	require.NoError(t, source.Close())
	assert.Equal(t, ClientGone, (<-result).Outcome)
	assert.Len(t, spy.snapshot(), 1)
}

func TestStreamMeter_TTFTWaitsForWholeFrame(t *testing.T) {
	rec := llm.NewInferenceRecorder(t.Context(), "m", "openai")
	time.Sleep(2 * time.Millisecond)
	reader := &checkingReader{
		reader: &pieceReader{pieces: []string{"data:", " [DONE]\n\n"}},
		beforeRead: func(reads int) {
			if reads == 1 {
				assert.Zero(t, rec.Snapshot().TTFTMs)
			}
			if reads == 2 {
				assert.Positive(t, rec.Snapshot().TTFTMs)
			}
		},
	}
	result := CopyStream(t.Context(), httptest.NewRecorder(), reader, rec, nil)
	assert.Equal(t, Completed, result.Outcome)
}

type checkingReader struct {
	reader     io.Reader
	beforeRead func(int)
	reads      int
}

func (r *checkingReader) Read(p []byte) (int, error) {
	r.beforeRead(r.reads)
	r.reads++
	return r.reader.Read(p)
}

func TestStreamMeter_DataAndErrorInSameRead(t *testing.T) {
	for _, readErr := range []error{io.EOF, io.ErrUnexpectedEOF} {
		spy := installCompletionSpy(t)
		rec := llm.NewInferenceRecorder(t.Context(), "m", "openai")
		reader := &finalRead{data: []byte(usageStream), err: readErr}
		w := httptest.NewRecorder()
		result := CopyStream(t.Context(), w, reader, rec, nil)
		assert.Equal(t, usageStream, w.Body.String())
		assert.Equal(t, Completed, result.Outcome)
		calls := spy.snapshot()
		require.Len(t, calls, 1)
		assert.Equal(t, int64(7), calls[0].TokensOut)
	}
}

type finalRead struct {
	data []byte
	err  error
}

func (r *finalRead) Read(p []byte) (int, error) {
	n := copy(p, r.data)
	r.data = r.data[n:]
	if len(r.data) == 0 {
		return n, r.err
	}
	return n, nil
}

func TestCopyStream_ClientCancelWhileUpstreamQuietIsClientGone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	result := CopyStream(ctx, httptest.NewRecorder(), newStallReader(ctx, "data: {\"choices\":[]}\n\n"), nil, nil)
	assert.Equal(t, ClientGone, result.Outcome)
	_, failed := result.Failure()
	assert.False(t, failed, "nobody is left to receive an error ending")
}

func TestResult_ZeroIsNotCompleted(t *testing.T) {
	assert.Equal(t, Unknown, Result{}.Outcome)
}

func TestTerminal_EmptyFinishReasonIsNotTerminal(t *testing.T) {
	done, failed := terminal([]byte(`data: {"choices":[{"finish_reason":""}]}`))
	assert.False(t, done)
	assert.False(t, failed)
}
