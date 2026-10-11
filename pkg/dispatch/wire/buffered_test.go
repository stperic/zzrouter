package wire

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/observability/llm"
)

func largeBufferedJSON() string {
	return `{"content":"` + strings.Repeat("x", 3<<20) + `","usage":{"prompt_tokens":11,"completion_tokens":7}}`
}

func TestRelayBuffered_ChunkedBodyOver1MiBIsComplete(t *testing.T) {
	body := largeBufferedJSON()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()
	resp, err := srv.Client().Get(srv.URL)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, int64(-1), resp.ContentLength)
	require.Equal(t, []string{"chunked"}, resp.TransferEncoding)
	require.Empty(t, resp.Header.Get("Transfer-Encoding"))
	require.False(t, IsStreaming(resp))
	w := httptest.NewRecorder()
	result := RelayBuffered(w, resp, nil, nil)
	assert.Equal(t, Completed, result.Outcome)
	assert.Equal(t, body, w.Body.String())
	assert.Equal(t, strconv.Itoa(len(body)), w.Header().Get("Content-Length"))
}

func TestRelayBuffered_UnknownLengthOver1MiBIsComplete(t *testing.T) {
	body := largeBufferedJSON()
	resp := &http.Response{StatusCode: http.StatusOK, ContentLength: -1, Body: io.NopCloser(strings.NewReader(body))}
	w := httptest.NewRecorder()
	assert.Equal(t, Completed, RelayBuffered(w, resp, nil, nil).Outcome)
	assert.Equal(t, body, w.Body.String())
}

func TestRelayBuffered_LargeKnownLengthRecordsCompletion(t *testing.T) {
	spy := installCompletionSpy(t)
	body := largeBufferedJSON()
	resp := &http.Response{StatusCode: http.StatusOK, ContentLength: int64(len(body)), Body: io.NopCloser(strings.NewReader(body))}
	w := httptest.NewRecorder()
	result := RelayBuffered(w, resp, nil, llm.NewInferenceRecorder(t.Context(), "m", "p"))
	assert.Equal(t, Completed, result.Outcome)
	assert.Equal(t, body, w.Body.String())
	calls := spy.snapshot()
	require.Len(t, calls, 1)
	assert.Equal(t, int64(11), calls[0].TokensIn)
	assert.Equal(t, int64(7), calls[0].TokensOut)
}

func TestRelayBuffered_MidBodyReadErrorIsTruncatedAndWritesNothing(t *testing.T) {
	spy := installCompletionSpy(t)
	resp := &http.Response{StatusCode: http.StatusOK, ContentLength: -1, Body: io.NopCloser(newCutReader(`{"choices":[`))}
	w := &bufferedWriteSpy{ResponseRecorder: httptest.NewRecorder()}
	shaped := false
	result := RelayBuffered(w, resp, func(b []byte) []byte { shaped = true; return b }, llm.NewInferenceRecorder(t.Context(), "m", "p"))
	assert.Equal(t, Truncated, result.Outcome)
	assert.ErrorIs(t, result.Err, io.ErrUnexpectedEOF)
	assert.False(t, shaped)
	assert.Zero(t, w.headers)
	assert.Empty(t, w.Body.Bytes())
	assert.Empty(t, w.Header())
	calls := spy.snapshot()
	require.Len(t, calls, 1)
	assert.Equal(t, "upstream_body_error", calls[0].ErrorType)
}

func TestRelayBuffered_ShortBodyUnderContentLength(t *testing.T) {
	for _, reader := range []struct {
		name string
		body io.Reader
	}{
		{"unexpected_eof", newCutReader(`{"usage":`)},
		{"plain_eof", strings.NewReader(`{"usage":`)},
	} {
		t.Run(reader.name, func(t *testing.T) {
			resp := &http.Response{StatusCode: http.StatusOK, ContentLength: 100, Body: io.NopCloser(reader.body)}
			w := &bufferedWriteSpy{ResponseRecorder: httptest.NewRecorder()}
			result := RelayBuffered(w, resp, nil, nil)
			assert.Equal(t, Truncated, result.Outcome)
			assert.ErrorIs(t, result.Err, io.ErrUnexpectedEOF)
			assert.Zero(t, w.headers)
			assert.Empty(t, w.Body.Bytes())
		})
	}
}

func TestRelayBuffered_ShapeAppliedAndLengthMatches(t *testing.T) {
	const raw = `{"usage":{"prompt_tokens":11,"completion_tokens":7}}`
	const shaped = `{"shaped":true}`
	resp := &http.Response{StatusCode: http.StatusCreated, ContentLength: int64(len(raw)), Body: io.NopCloser(strings.NewReader(raw))}
	w := httptest.NewRecorder()
	w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
	w.Header().Set("Transfer-Encoding", "chunked")
	w.Header().Set("Content-Encoding", "gzip")
	spy := installCompletionSpy(t)
	result := RelayBuffered(w, resp, func(body []byte) []byte {
		assert.Equal(t, raw, string(body))
		require.Len(t, spy.snapshot(), 1)
		return []byte(shaped)
	}, llm.NewInferenceRecorder(t.Context(), "m", "p"))
	assert.Equal(t, Completed, result.Outcome)
	assert.Equal(t, http.StatusCreated, w.Code)
	assert.Equal(t, shaped, w.Body.String())
	assert.Equal(t, strconv.Itoa(len(shaped)), w.Header().Get("Content-Length"))
	assert.Empty(t, w.Header().Get("Transfer-Encoding"))
	assert.Empty(t, w.Header().Get("Content-Encoding"))
	assert.Equal(t, int64(7), spy.snapshot()[0].TokensOut)
}

func TestRelayBuffered_RecordsCompletionOnce(t *testing.T) {
	for _, body := range []string{"", `{"usage":{"prompt_tokens":11,"completion_tokens":7}}`, `{"done":true,"prompt_eval_count":11,"eval_count":7}`} {
		t.Run(body, func(t *testing.T) {
			spy := installCompletionSpy(t)
			resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}
			result := RelayBuffered(httptest.NewRecorder(), resp, nil, llm.NewInferenceRecorder(t.Context(), "m", "p"))
			assert.Equal(t, Completed, result.Outcome)
			assert.Len(t, spy.snapshot(), 1)
		})
	}
}

type bufferedWriteSpy struct {
	*httptest.ResponseRecorder
	headers int
	err     error
	short   bool
}

func (w *bufferedWriteSpy) WriteHeader(status int) {
	w.headers++
	w.ResponseRecorder.WriteHeader(status)
}

func (w *bufferedWriteSpy) Write(b []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	if w.short {
		return len(b) - 1, nil
	}
	return w.ResponseRecorder.Write(b)
}

func TestRelayBuffered_ClientWriteError(t *testing.T) {
	for _, short := range []bool{false, true} {
		t.Run(strconv.FormatBool(short), func(t *testing.T) {
			spy := installCompletionSpy(t)
			w := &bufferedWriteSpy{ResponseRecorder: httptest.NewRecorder(), short: short}
			want := io.ErrShortWrite
			if !short {
				w.err = io.ErrClosedPipe
				want = w.err
			}
			resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{}`))}
			result := RelayBuffered(w, resp, nil, llm.NewInferenceRecorder(t.Context(), "m", "p"))
			assert.Equal(t, ClientGone, result.Outcome)
			assert.ErrorIs(t, result.Err, want)
			assert.Len(t, spy.snapshot(), 1)
		})
	}
}

func TestRelayBuffered_NoBodyResponses(t *testing.T) {
	for _, tc := range []struct {
		method string
		status int
	}{{http.MethodHead, 200}, {http.MethodGet, 204}, {http.MethodGet, 304}} {
		t.Run(tc.method+strconv.Itoa(tc.status), func(t *testing.T) {
			resp := &http.Response{StatusCode: tc.status, ContentLength: 100, Body: http.NoBody, Request: httptest.NewRequest(tc.method, "/", nil)}
			assert.Equal(t, Completed, RelayBuffered(httptest.NewRecorder(), resp, nil, nil).Outcome)
		})
	}
}
