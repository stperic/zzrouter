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
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
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
			err:     fmt.Errorf("Model file not found: %w", prov_apps.ErrModelNotLocal),
			message: "Model file not found: " + prov_apps.ErrModelNotLocal.Error() + "; download or register it before starting; use POST /zzrouter/v1/deployments for a cloud model",
			status:  http.StatusNotFound,
			errType: "not_found_error",
			code:    "model_not_found",
		},
		{
			name:    "rejected request keeps its 400",
			err:     fmt.Errorf("unknown provider: %w", prov_apps.ErrParameterValidation),
			message: "unknown provider: " + prov_apps.ErrParameterValidation.Error(),
			status:  http.StatusBadRequest,
			errType: "invalid_request_error",
			code:    "invalid_request",
		},
		{
			name:    "provider unavailable stays retryable",
			err:     fmt.Errorf("provider down: %w", prov_apps.ErrShutdown),
			message: "provider down: " + prov_apps.ErrShutdown.Error(),
			status:  http.StatusServiceUnavailable,
			errType: "api_error",
			code:    "provider_unavailable",
		},
		{
			name:    "engine that died on startup is a bad gateway",
			err:     loadEngineFailed("instance 'x' failed to start: boom"),
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
			lf := loadFailureWire(tt.err)
			assert.Equal(t, tt.status, lf.Status)
			assert.Equal(t, tt.errType, lf.Type)
			assert.Equal(t, tt.code, lf.Code)
			assert.Equal(t, tt.message, lf.Message,
				"the message is part of the wire contract; a swapped positional field would show up here")
		})
	}
}

// Retrying is only advertised where it can work: a model still coming
// up says come back, a model that cannot load says nothing of the kind.
func TestRetryAfterOnlyWhereRetryingHelps(t *testing.T) {
	assert.Positive(t, loadFailureWire(loadStillWarming("warming")).RetryAfter,
		"a still-loading model must tell the caller to come back")

	for name, err := range map[string]error{
		"engine died":   loadEngineFailed("failed to start"),
		"never pulled":  fmt.Errorf("Model file not found: %w", prov_apps.ErrModelNotLocal),
		"our own fault": loadInternal("instance disappeared"),
	} {
		t.Run(name, func(t *testing.T) {
			assert.Zero(t, loadFailureWire(err).RetryAfter,
				"retrying this cannot succeed, so do not invite it")
		})
	}
}

// loadFailureWire has to see through wrapping, or a failure that
// picks up context on the way out silently falls back to 502.
func TestClassifyLoadFailureUnwraps(t *testing.T) {
	wrapped := fmt.Errorf("loading %q: %w", "some-model",
		fmt.Errorf("Model file not found: %w", prov_apps.ErrModelNotLocal))

	lf := loadFailureWire(wrapped)

	assert.Equal(t, http.StatusNotFound, lf.Status)
	assert.Equal(t, "model_not_found", lf.Code)
}

// The whole claim of this arc is that a load failure reads the same
// whether the caller streamed or not. Nothing tested that the two
// branches agree, only that each was individually reasonable.
func TestStreamedAndHeaderFailuresAgree(t *testing.T) {
	for _, cause := range []error{
		fmt.Errorf("Model file not found: %w", prov_apps.ErrModelNotLocal),
		loadEngineFailed("instance 'x' failed to start: boom"),
		fmt.Errorf("instance x failed: %w", &httperr.EngineFailure{Class: httperr.FailureMemory, Message: "allocation failed"}),
		loadStillWarming("instance 'x' is still loading"),
		loadInternal("instance 'x' disappeared"),
		errors.New("something unclassified"),
	} {
		lf := loadFailureWire(cause)

		t.Run(lf.Code+"/"+lf.Message, func(t *testing.T) {
			failure := lf

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
			frame, _, _ := strings.Cut(strings.TrimLeft(sse.Body.String(), "\n"), "\n\n")
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
	s := variantNode(t)
	sentinel := errors.New("transport exploded")
	run := instance.NewInstance("failed", "llamacpp", qwenModel, 0, 0, 1)
	run.MarkFailedError(sentinel)
	_, err := s.waitForLocalInstance(run)
	assert.ErrorIs(t, err, sentinel, "readiness producer retains the original cause")
	wire := loadFailureWire(err)
	assert.Equal(t, http.StatusBadGateway, wire.Status)
	assert.Contains(t, wire.Message, sentinel.Error())
}

func TestClassifyLoadFailureEngineClass(t *testing.T) {
	for _, class := range []httperr.FailureClass{httperr.FailureMemory, httperr.FailureUnsupportedInput, httperr.FailureChatTemplateRejected, httperr.FailureToolCallingNotEnabled} {
		cause := &httperr.EngineFailure{Class: class, Message: "declared failure"}
		for _, wrapped := range []error{cause, loadEngineFailed(cause.Error()).withCause(cause), fmt.Errorf("cold load: %w", cause)} {
			failure := loadFailureWire(wrapped)
			want := (&httperr.EngineFailure{Class: class, Message: wrapped.Error()}).HTTPError()
			assert.Equal(t, want.Status, failure.Status)
			assert.Equal(t, want.Code, failure.Code)
			assert.Equal(t, want.Message, failure.Message)
			assert.Zero(t, failure.RetryAfter)
			assert.ErrorIs(t, wrapped, cause, "producer wrappers retain typed causes before rendering")
		}
	}
	assert.Equal(t, http.StatusBadGateway, loadFailureWire(errors.New("CUDA out of memory")).Status)
}
