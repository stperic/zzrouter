package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
)

// A model still loading past the request's budget names the run and job
// the caller can follow, so an agent watches the load instead of retrying
// blind.
func TestStillWarmingNamesTheRunToFollow(t *testing.T) {
	run := instance.NewInstance("run-1", "llamacpp", "m", 8080, time.Minute, 0)
	run.StreamJobID = "run_abc"

	wire := loadFailureWire(loadStillWarming("still loading").forRun(run))

	assert.Equal(t, http.StatusServiceUnavailable, wire.Status)
	assert.Equal(t, "model_load_timeout", wire.Code)
	assert.Positive(t, wire.RetryAfter)
	assert.Equal(t, map[string]any{"run_id": "run-1", "job_id": "run_abc"}, wire.Extra)
	assert.Equal(t, wire.Extra, loadFailureWire(fmt.Errorf("load: %w", loadStillWarming("still loading").forRun(run))).Extra)

	w := httptest.NewRecorder()
	writeError(w, routedRequest(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)), wire)
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "run_abc", body["job_id"])
	assert.Equal(t, "5", w.Header().Get("Retry-After"))
}

// Inference admission reads the same table as management routes; only an
// error no row names keeps admission's own 503.
func TestModelAdmissionAnswersFromTheFailureTable(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{fmt.Errorf("admit: %w", prov_apps.ErrModelNotLocal), http.StatusNotFound, "model_not_found"},
		{fmt.Errorf("admit: %w", prov_apps.ErrModelIncomplete), http.StatusConflict, "model_incomplete"},
		{fmt.Errorf("admit: %w", prov_apps.ErrInstanceAlreadyExists), http.StatusConflict, "conflict"},
		// Defensive: both validators pass only refusals, but an unnamed error still needs an answer.
		{errors.New("catalog unreachable"), http.StatusServiceUnavailable, "backend_unavailable"},
	} {
		w := httptest.NewRecorder()
		writeModelAdmissionError(w, routedRequest(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)), tc.err)
		assert.Equal(t, tc.status, w.Code, tc.err.Error())
		var body struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		assert.Equal(t, tc.code, body.Error.Code, tc.err.Error())
	}
}

// A routed failure keeps the document its node wrote; an error the table
// does not name is a 500 that still says what failed, never a bare
// "failed to ..." with the cause dropped.
func TestRespondToError(t *testing.T) {
	respond := func(err error) (*httptest.ResponseRecorder, map[string]any) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/zzrouter/v1/internal/runs/preview", nil)
		RespondToError(c, err)
		var body map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		return w, body
	}

	w, body := respond(newRouteProblemError(http.StatusConflict, "Conflict", "held", "model_node_ambiguous", "localai"))
	assert.Equal(t, http.StatusConflict, w.Code)
	assert.Equal(t, "model_node_ambiguous", body["code"])

	w, _ = respond(fmt.Errorf("allocate: %w", prov_apps.ErrPortUnavailable))
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)

	w, body = respond(errors.New(`no port pool for provider "ollama"`))
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, body["detail"], "no port pool")
}

// answeredStatus is the status a management route answers err with.
func answeredStatus(err error) int {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = routedRequest(httptest.NewRequest(http.MethodPost, "/zzrouter/v1/runs", nil))
	RespondToError(c, err)
	return w.Code
}

// Each row answers with its decided status, through wrapping, on the real responder.
func TestFailureTableRowsAnswerTheirStatus(t *testing.T) {
	for _, row := range failureTable {
		if row.sentinel == nil {
			continue
		} // Typed rows have payload-specific contract tests.
		cause := row.sentinel
		switch row.sentinel {
		case instance.ErrQueueWaitExpired, instance.ErrStopping:
			cause = &instance.AdmissionError{Reason: row.sentinel, RetryAfter: instance.AdmissionRetryAfter}
		case errLoadWarming, errLoadInternal, errLoadFailed:
			cause = &loadState{cause: row.sentinel, message: "load state"}
		}
		wrapped := fmt.Errorf("launch: %w", cause)
		assert.Equal(t, row.status, answeredStatus(wrapped), row.sentinel.Error())
	}
}

// The decisions this table records, pinned as values so a changed status is a
// visible test change, not a silent one.
func TestFailureTableDecisions(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{prov_apps.ErrProviderNotFound, http.StatusNotFound, ""},
		{prov_apps.ErrExternalService, http.StatusForbidden, ""},
		{install.ErrStalePlan, http.StatusConflict, ""},
		{prov_apps.ErrModelIncomplete, http.StatusConflict, "model_incomplete"},
		{prov_apps.ErrModelNotLocal, http.StatusNotFound, ""},
		{prov_apps.ErrAtCapacity, http.StatusServiceUnavailable, ""},
	} {
		wire, ok := knownFailure(fmt.Errorf("op: %w", tc.err))
		require.True(t, ok, tc.err.Error())
		assert.Equal(t, tc.status, wire.Status, tc.err.Error())
		assert.Equal(t, tc.code, wire.Code, tc.err.Error())
	}
}
