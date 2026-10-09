package wire

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

func TestStreamMeter_BothEntryPointsAgree(t *testing.T) {
	for _, tc := range streamOutcomeCases() {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			copyResult := CopyStream(context.Background(), w, iotest.OneByteReader(strings.NewReader(tc.frame)), nil, nil)
			body := NewMeteredBody(context.Background(), io.NopCloser(iotest.OneByteReader(strings.NewReader(tc.frame))), nil)
			got, err := io.ReadAll(body)
			require.NoError(t, err)
			require.NoError(t, body.Close())
			assert.Equal(t, tc.outcome, copyResult.Outcome)
			assert.Equal(t, copyResult, body.Result())
			assert.Equal(t, tc.frame, string(got))
			assert.Equal(t, tc.frame, w.Body.String())
		})
	}
}

func TestMeteredBody_CloseBeforeEOF(t *testing.T) {
	unterminated := strings.TrimSuffix(usageStream, "data: [DONE]\n\n")
	for _, tc := range []struct {
		name     string
		stream   string
		canceled bool
		want     Outcome
	}{
		{"live", unterminated, false, Truncated},
		{"canceled", unterminated, true, ClientGone},
		{"terminal_then_close", usageStream, false, Completed},
	} {
		canceled := tc.canceled
		t.Run(tc.name, func(t *testing.T) {
			want := tc.want
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			body := NewMeteredBody(ctx, io.NopCloser(newStallReader(ctx, tc.stream)), nil)
			_, err := io.ReadFull(body, make([]byte, len(tc.stream)))
			require.NoError(t, err)
			if canceled {
				cancel()
			}
			require.NoError(t, body.Close())
			assert.Equal(t, want, body.Result().Outcome)
		})
	}
}

func TestStreamMeter_UsageRecordedOnceOnEveryOutcome(t *testing.T) {
	usage := "data: {\"model\":\"m\",\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":7,\"prompt_tokens_details\":{\"cached_tokens\":3},\"completion_tokens_details\":{\"reasoning_tokens\":2},\"cost\":0.25}}\n\n"
	for _, entry := range []string{"copy", "body"} {
		for _, tc := range []struct {
			name      string
			outcome   Outcome
			errorType string
		}{
			{"completed", Completed, ""},
			{"eof", Truncated, "upstream_stream_truncated"},
			{"read_error", Truncated, "upstream_stream_error"},
			{"failed", UpstreamFailed, "upstream_stream_failed"},
			{"client", ClientGone, ""},
		} {
			t.Run(entry+"/"+tc.name, func(t *testing.T) {
				spy := installCompletionSpy(t)
				rec := llm.NewInferenceRecorder(context.Background(), "m", "openai")
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
				var result Result
				if entry == "copy" {
					var w http.ResponseWriter = httptest.NewRecorder()
					if tc.outcome == ClientGone {
						w = &failingWriter{buf: &bytes.Buffer{}}
					}
					result = CopyStream(context.Background(), w, reader, rec, nil)
				} else {
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					body := NewMeteredBody(ctx, io.NopCloser(reader), rec)
					if tc.outcome == ClientGone {
						_, err := io.ReadFull(body, make([]byte, len(usage)))
						require.NoError(t, err)
						cancel()
					} else {
						_, err := io.ReadAll(body)
						if tc.name == "read_error" {
							require.ErrorIs(t, err, io.ErrUnexpectedEOF)
						} else {
							require.NoError(t, err)
						}
					}
					require.NoError(t, body.Close())
					require.NoError(t, body.Close())
					result = body.Result()
				}
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
	once    sync.Once
}

func (b *blockingBody) Read([]byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	<-b.closed
	return 0, io.ErrClosedPipe
}
func (b *blockingBody) Close() error { close(b.closed); return nil }

func TestMeteredBody_CloseUnblocksRead(t *testing.T) {
	spy := installCompletionSpy(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	source := &blockingBody{started: make(chan struct{}), closed: make(chan struct{})}
	body := NewMeteredBody(ctx, source, llm.NewInferenceRecorder(ctx, "m", "openai"))
	done := make(chan struct{})
	go func() { defer close(done); _, _ = body.Read(make([]byte, 1)) }()
	<-source.started
	cancel()
	require.NoError(t, body.Close())
	<-done
	assert.Equal(t, ClientGone, body.Result().Outcome)
	assert.Len(t, spy.snapshot(), 1)
}

func TestStreamMeter_TTFTWaitsForWholeFrame(t *testing.T) {
	for _, entry := range []string{"copy", "body"} {
		t.Run(entry, func(t *testing.T) {
			rec := llm.NewInferenceRecorder(context.Background(), "m", "openai")
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
			if entry == "copy" {
				result := CopyStream(context.Background(), httptest.NewRecorder(), reader, rec, nil)
				assert.Equal(t, Completed, result.Outcome)
			} else {
				body := NewMeteredBody(context.Background(), io.NopCloser(reader), rec)
				_, err := io.ReadAll(body)
				require.NoError(t, err)
				require.NoError(t, body.Close())
				assert.Equal(t, Completed, body.Result().Outcome)
			}
		})
	}
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
	for _, entry := range []string{"copy", "body"} {
		for _, readErr := range []error{io.EOF, io.ErrUnexpectedEOF} {
			spy := installCompletionSpy(t)
			rec := llm.NewInferenceRecorder(context.Background(), "m", "openai")
			reader := &finalRead{data: []byte(usageStream), err: readErr}
			var result Result
			if entry == "copy" {
				w := httptest.NewRecorder()
				result = CopyStream(context.Background(), w, reader, rec, nil)
				assert.Equal(t, usageStream, w.Body.String())
			} else {
				body := NewMeteredBody(context.Background(), io.NopCloser(reader), rec)
				got, err := io.ReadAll(body)
				if readErr == io.EOF {
					require.NoError(t, err)
				} else {
					require.ErrorIs(t, err, readErr)
				}
				require.NoError(t, body.Close())
				result = body.Result()
				assert.Equal(t, usageStream, string(got))
			}
			// The terminal frame arrived before the error, so the answer is complete.
			assert.Equal(t, Completed, result.Outcome)
			calls := spy.snapshot()
			require.Len(t, calls, 1)
			assert.Equal(t, int64(7), calls[0].TokensOut)
		}
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
