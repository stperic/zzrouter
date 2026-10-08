package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/httperr"
)

// A load failure has to survive the seam between the executor, which is
// the only layer that knows why the load did not happen, and the
// handler, which is the only layer that can put it on the wire. Every
// cause used to arrive as 500 model_load_failed.
func TestClassifyLoadFailure(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		status  int
		errType string
		code    string
		message string
	}{
		{
			name:    "model was never downloaded is the caller's problem, not a server fault",
			err:     newLoadFailure(http.StatusNotFound, "Model file not found"),
			message: "Model file not found",
			status:  http.StatusNotFound,
			errType: "invalid_request_error",
			code:    "model_not_found",
		},
		{
			name:    "rejected request keeps its 400",
			err:     newLoadFailure(http.StatusBadRequest, "unknown provider"),
			message: "unknown provider",
			status:  http.StatusBadRequest,
			errType: "invalid_request_error",
			code:    "invalid_request",
		},
		{
			name:    "provider unavailable stays retryable",
			err:     newLoadFailure(http.StatusServiceUnavailable, "provider down"),
			message: "provider down",
			status:  http.StatusServiceUnavailable,
			errType: "api_error",
			code:    "provider_unavailable",
		},
		{
			name:    "engine that died on startup is a bad gateway",
			err:     newLoadFailure(http.StatusBadGateway, "instance 'x' failed to start: boom"),
			message: "instance 'x' failed to start: boom",
			status:  http.StatusBadGateway,
			errType: "api_error",
			code:    "model_load_failed",
		},
		{
			name:    "a model still coming up is retryable, not broken",
			err:     loadStillWarming("instance 'x' is still loading"),
			message: "instance 'x' is still loading",
			status:  http.StatusServiceUnavailable,
			errType: "api_error",
			code:    "model_load_timeout",
		},
		{
			name:    "losing track of our own instance is genuinely our fault",
			err:     loadInternal("instance 'x' disappeared while waiting"),
			message: "instance 'x' disappeared while waiting",
			status:  http.StatusInternalServerError,
			errType: "server_error",
			code:    "model_load_failed",
		},
		{
			name:    "an unclassified error still reaches the caller",
			err:     errors.New("something else"),
			message: "failed to load model: something else",
			status:  http.StatusBadGateway,
			errType: "api_error",
			code:    "model_load_failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lf := classifyLoadFailure(tt.err)
			require.NotNil(t, lf)
			assert.Equal(t, tt.status, lf.status)
			assert.Equal(t, tt.errType, lf.errType)
			assert.Equal(t, tt.code, lf.code)
			assert.Equal(t, tt.message, lf.message,
				"the message is part of the wire contract; a swapped positional field would show up here")
		})
	}
}

// Retrying is only advertised where it can work: a model still coming
// up says come back, a model that cannot load says nothing of the kind.
func TestRetryAfterOnlyWhereRetryingHelps(t *testing.T) {
	assert.Positive(t, classifyLoadFailure(loadStillWarming("warming")).retryAfterSecs,
		"a still-loading model must tell the caller to come back")

	for name, err := range map[string]error{
		"engine died":   newLoadFailure(http.StatusBadGateway, "failed to start"),
		"never pulled":  newLoadFailure(http.StatusNotFound, "Model file not found"),
		"our own fault": loadInternal("instance disappeared"),
	} {
		t.Run(name, func(t *testing.T) {
			assert.Zero(t, classifyLoadFailure(err).retryAfterSecs,
				"retrying this cannot succeed, so do not invite it")
		})
	}
}

// classifyLoadFailure has to see through wrapping, or a failure that
// picks up context on the way out silently falls back to 502.
func TestClassifyLoadFailureUnwraps(t *testing.T) {
	wrapped := fmt.Errorf("loading %q: %w", "some-model",
		newLoadFailure(http.StatusNotFound, "Model file not found"))

	lf := classifyLoadFailure(wrapped)

	assert.Equal(t, http.StatusNotFound, lf.status)
	assert.Equal(t, "model_not_found", lf.code)
}

// The whole claim of this arc is that a load failure reads the same
// whether the caller streamed or not. Nothing tested that the two
// branches agree, only that each was individually reasonable.
func TestStreamedAndHeaderFailuresAgree(t *testing.T) {
	for _, cause := range []error{
		newLoadFailure(http.StatusNotFound, "Model file not found"),
		newLoadFailure(http.StatusBadGateway, "instance 'x' failed to start: boom"),
		loadStillWarming("instance 'x' is still loading"),
		loadInternal("instance 'x' disappeared"),
		errors.New("something unclassified"),
	} {
		lf := classifyLoadFailure(cause)

		t.Run(lf.code+"/"+lf.message, func(t *testing.T) {
			failure := httperr.Error{Status: lf.status, Type: lf.errType, Message: lf.message, Code: lf.code}

			// Header path.
			hdr := httptest.NewRecorder()
			writeError(hdr, nil, failure)

			// Streamed path, once the response is already committed.
			sse := httptest.NewRecorder()
			dialectOf(nil).(httperr.InBandStreamer).StreamError(sse, failure)

			var fromHeader, fromStream struct {
				Error struct {
					Message string  `json:"message"`
					Type    string  `json:"type"`
					Code    *string `json:"code"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(hdr.Body.Bytes(), &fromHeader))
			frame, _, _ := strings.Cut(sse.Body.String(), "\n\n")
			payload := strings.TrimPrefix(frame, "data: ")
			require.NoError(t, json.Unmarshal([]byte(payload), &fromStream))

			assert.Equal(t, fromHeader.Error.Type, fromStream.Error.Type,
				"error.type must not depend on whether the caller streamed")
			assert.Equal(t, fromHeader.Error.Message, fromStream.Error.Message,
				"error.message must not depend on whether the caller streamed")
			require.NotNil(t, fromStream.Error.Code, "the streamed frame must carry a code")
			require.NotNil(t, fromHeader.Error.Code)
			assert.Equal(t, *fromHeader.Error.Code, *fromStream.Error.Code,
				"error.code must not depend on whether the caller streamed")
		})
	}
}

// A classified failure must not sever the chain it was built from.
func TestLoadFailureKeepsItsCause(t *testing.T) {
	sentinel := errors.New("transport exploded")

	lf := classifyLoadFailure(sentinel)

	assert.ErrorIs(t, lf, sentinel, "errors.Is must still reach the original cause")
}
