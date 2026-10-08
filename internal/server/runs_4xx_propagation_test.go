package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRuns_LaunchRun_4xxPropagation pins POST /zzrouter/v1/runs: an
// internal-executor 4xx (e.g. "unsupported provider") propagates as 400
// Problem Details verbatim — not 502 Bad Gateway from broadcast-envelope
// unmarshal failure (the pre-418aac33 bug) and not 500 from a stringified
// status-code error.
func TestRuns_LaunchRun_4xxPropagation(t *testing.T) {
	s := createTestNodeWithDefaults(t)

	body := []byte(`{"model_name":"x","provider":"definitely-not-a-real-provider"}`)
	req := httptest.NewRequest(http.MethodPost, "/zzrouter/v1/runs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", TestAdminKey)

	w := httptest.NewRecorder()
	s.engine.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code,
		"unsupported provider must propagate 400 from internal to public, got %d body=%s", w.Code, w.Body.String())
	assert.Contains(t, w.Header().Get("Content-Type"), "application/problem+json",
		"4xx must round-trip as Problem Details, got %q", w.Header().Get("Content-Type"))

	var pd struct {
		Status int    `json:"status"`
		Title  string `json:"title"`
		Detail string `json:"detail"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &pd),
		"public 4xx must be valid Problem Details JSON: %s", w.Body.String())
	assert.Equal(t, http.StatusBadRequest, pd.Status, "Problem Details status must match HTTP code")
	assert.NotEmpty(t, pd.Detail, "Problem Details detail must be populated")
}

// TestRuns_StopRun_4xxPropagation pins DELETE /zzrouter/v1/runs/:id —
// previously RunsService.StopRun stringified upstream 4xx as
// `fmt.Errorf("internal API error: status %d", ...)`, which lost the
// status code and surfaced as 500 at the public hop. parseRoutedError
// now preserves it.
func TestRuns_StopRun_4xxPropagation(t *testing.T) {
	s := createTestNodeWithDefaults(t)

	// Unknown run ID — internal handler returns 404 Problem Details. The
	// public surface must forward 404, not 500.
	req := httptest.NewRequest(http.MethodDelete, "/zzrouter/v1/runs/no-such-run", nil)
	req.Header.Set("X-API-Key", TestAdminKey)

	w := httptest.NewRecorder()
	s.engine.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code,
		"DELETE on missing run must propagate 404, got %d body=%s", w.Code, w.Body.String())
}
