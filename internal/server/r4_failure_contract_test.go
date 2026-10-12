package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestColdLoadMemoryDiagnosticExtensionsContract(t *testing.T) {
	cause := &process.MemoryFitError{Parameter: "budget", Device: "GPU-A", RequiredMiB: 9500, FreeMiB: 6000}
	wrapped := fmt.Errorf("cold load: %w", cause)
	wire := loadFailureWire(wrapped)
	require.Equal(t, http.StatusConflict, wire.Status)
	require.Contains(t, wire.Extra, "errors", "inference and management must retain the same typed memory diagnostic")
	management := failureWire(wrapped, http.StatusInternalServerError)
	assert.Equal(t, management.Extra, wire.Extra)
	assert.Equal(t, management.Title, wire.Title)
}

func TestColdLoadExplicitWireExtensionsContract(t *testing.T) {
	want := httperr.Error{Status: http.StatusServiceUnavailable, Type: "api_error", Code: "specific_failure", Message: "try later", Title: "Specific failure", RetryAfter: 1500 * time.Millisecond, Extra: map[string]any{"run_id": "r", "job_id": "j"}}
	got := loadFailureWire(fmt.Errorf("wrapped: %w", want))
	assert.Equal(t, want, got, "explicit wire facts must survive the cold-load edge")
}

func TestOpenRunCanceledCallerWritesNothingContract(t *testing.T) {
	for _, reason := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(reason.Error(), func(t *testing.T) {
			s := &Server{}
			run := instance.NewInstance("r", "p", "m", 0, time.Minute, 1)
			run.MarkRunning(false, nil)
			ctx, cancel := context.WithCancel(t.Context())
			if reason == context.DeadlineExceeded {
				cancel()
				ctx, cancel = context.WithDeadline(t.Context(), time.Unix(0, 0))
			}
			cancel()
			require.ErrorIs(t, ctx.Err(), reason)
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx)
			called := false
			lease, target, _, ok := s.openRun(req, run, nil, func(httperr.Error) { called = true })
			assert.False(t, ok)
			assert.False(t, called)
			assert.Nil(t, lease)
			assert.Nil(t, target)
			assert.Zero(t, run.GetActiveRequests())
		})
	}
}

func TestWarmingExtensionsAcrossDialectsContract(t *testing.T) {
	run := instance.NewInstance("r", "p", "m", 0, 0, 1)
	run.StreamJobID = "j"
	wire := loadFailureWire(fmt.Errorf("outer: %w", loadStillWarming("warming").forRun(run)))
	assert.Equal(t, "warming", wire.Message, "synthetic state keeps its stored message through wrapping")
	for _, path := range []string{"/v1/chat/completions", "/v1/messages", "/api/generate", "/zzrouter/v1/runs"} {
		t.Run(path, func(t *testing.T) {
			w := httptest.NewRecorder()
			writeError(w, routedRequest(httptest.NewRequest(http.MethodPost, path, nil)), wire)
			require.Equal(t, http.StatusServiceUnavailable, w.Code)
			assert.Equal(t, "5", w.Header().Get("Retry-After"))
			var body map[string]any
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
			assert.Equal(t, "r", body["run_id"])
			assert.Equal(t, "j", body["job_id"])
		})
	}
}

func TestAdmissionUnknownReasonContract(t *testing.T) {
	for _, reason := range []error{context.Canceled, context.DeadlineExceeded, errors.New("unknown reason")} {
		wire, ok := knownFailure(fmt.Errorf("wrapped: %w", &instance.AdmissionError{Reason: reason, RetryAfter: 1500 * time.Millisecond}))
		require.True(t, ok)
		assert.Equal(t, http.StatusServiceUnavailable, wire.Status)
		assert.Equal(t, "capacity_exceeded", wire.Code)
		assert.Equal(t, 1500*time.Millisecond, wire.RetryAfter)
	}
}

func TestLoadCauseAcrossProducerAndChainContract(t *testing.T) {
	s := variantNode(t)
	sentinel := errors.New("catalog cause")
	cause := fmt.Errorf("%w: %w", prov_apps.ErrModelIncomplete, sentinel)
	mgr, err := prov_apps.NewProviderAppManager(s.configStore.Config(), prov_apps.WithModelValidator(func(context.Context, string) error { return cause }))
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, mgr.Stop(context.Background())) })
	s.providers.appMgr = mgr
	_, err = s.ensureLocalRun(prov_apps.EnsureRequest{Provider: "llamacpp", Model: qwenModel})
	assert.ErrorIs(t, err, sentinel)
	assert.Equal(t, http.StatusConflict, loadFailureWire(err).Status)
	_, err = newChainServerDeps(s).LocalRun(qwenModel, "llamacpp")
	assert.ErrorIs(t, err, sentinel)
	assert.Equal(t, "model_incomplete", loadFailureWire(err).Code)
}

func TestColdLoadEmptyCodeExplicitWireContract(t *testing.T) {
	for _, status := range []int{http.StatusBadGateway, http.StatusServiceUnavailable} {
		want := httperr.Error{Status: status, Type: "api_error", Message: "explicit message", Extra: map[string]any{"run_id": "r"}, RetryAfter: time.Second}
		assert.Equal(t, want, loadFailureWire(fmt.Errorf("outer: %w", want)), "explicit missing code is not an unnamed load error")
	}
}

func TestLoadStateRetainsReasonAndCauseContract(t *testing.T) {
	cause := errors.New("transport cause")
	state := loadStillWarming("still loading").withCause(cause)
	assert.ErrorIs(t, state, cause)
	assert.ErrorIs(t, state, errLoadWarming)
	wire := loadFailureWire(fmt.Errorf("outer: %w", state))
	assert.Equal(t, http.StatusServiceUnavailable, wire.Status)
	assert.Equal(t, "model_load_timeout", wire.Code)
	assert.Equal(t, "still loading", wire.Message)
	assert.Equal(t, modelWarmupRetryAfter, wire.RetryAfter)
}

func TestCommittedStreamRetainsSupportedFieldsContract(t *testing.T) {
	run := instance.NewInstance("r", "p", "m", 0, 0, 1)
	run.StreamJobID = "j"
	wire := loadFailureWire(loadStillWarming("warming").forRun(run))
	header := httptest.NewRecorder()
	writeError(header, nil, wire)
	assert.Contains(t, header.Body.String(), "job_id")
	assert.Equal(t, "5", header.Header().Get("Retry-After"))
	stream := httptest.NewRecorder()
	dialectOf(nil).(httperr.InBandStreamer).StreamError(stream, wire)
	assert.Contains(t, stream.Body.String(), "model_load_timeout")
	assert.NotContains(t, stream.Body.String(), "job_id")
	assert.NotContains(t, stream.Body.String(), "run_id")
	assert.Empty(t, stream.Header().Get("Retry-After"))
	assert.Equal(t, 1, strings.Count(stream.Body.String(), "data: [DONE]"))
}

func TestInventoryAdmissionExplicitEmptyCodeContract(t *testing.T) {
	for _, status := range []int{http.StatusBadGateway, http.StatusServiceUnavailable} {
		want := httperr.Error{Status: status, Type: "api_error", Message: "explicit inventory failure", Extra: map[string]any{"run_id": "r"}, RetryAfter: time.Second}
		w := httptest.NewRecorder()
		writeModelAdmissionError(w, routedRequest(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)), fmt.Errorf("outer: %w", want))
		assert.Equal(t, status, w.Code)
		assert.Equal(t, "1", w.Header().Get("Retry-After"))
		var body map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		assert.Equal(t, "r", body["run_id"])
		e := body["error"].(map[string]any)
		assert.Empty(t, e["code"])
		assert.Equal(t, want.Message, e["message"])
		assert.Equal(t, want.Type, e["type"])
	}
}
