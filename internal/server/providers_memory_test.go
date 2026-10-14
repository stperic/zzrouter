package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMemoryRefusalCarriesBudgetAndFreeInsideProblem(t *testing.T) {
	c, response := testGinContext("POST", "/zzrouter/v1/internal/runs")
	refusal := &process.MemoryFitError{Parameter: "budget", Device: "GPU-A", RequiredMiB: 9500, FreeMiB: 6000,
		Holders: []process.MemoryHolder{{Run: "comfy-run", Provider: "comfyui", Ownership: "zzrouter", MemoryMiB: new(int64(31966)), UnusedCommitmentMiB: 31966, ReclaimSkippedReason: "busy"}}}
	RespondToError(c, fmt.Errorf("launch refused: %w", refusal))
	require.Equal(t, 409, response.Code)
	assert.Equal(t, "application/problem+json", response.Header().Get("Content-Type"))
	var problem utils.ProblemDetails
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &problem))
	assert.Equal(t, "out_of_range", problem.Code)
	assert.Equal(t, 409, answeredStatus(refusal))
	require.Len(t, problem.Errors, 1)
	got := problem.Errors[0].Got.(map[string]any)
	assert.Equal(t, float64(9500), got["required_mib"])
	assert.Equal(t, float64(6000), got["free_mib"])
	holder := got["holders"].([]any)[0].(map[string]any)
	assert.Equal(t, "busy", holder["reclaim_skipped_reason"])
	assert.NotContains(t, holder, "pid")
	assert.Contains(t, problem.Detail, "zzRouter run comfy-run provider comfyui (31966 MiB unused commitment; reclaim skipped: busy)")
	assert.Contains(t, problem.Errors[0].Hint, "PATCH")
	assert.Equal(t, 503, answeredStatus(fmt.Errorf("observe: %w", process.ErrMemoryObservation)))
	assert.Equal(t, 400, answeredStatus(fmt.Errorf("budget: %w", process.ErrMemoryBudgetInvalid)))
	classified := loadFailureWire(fmt.Errorf("cold load: %w", refusal))
	assert.Equal(t, 409, classified.Status)
	assert.Equal(t, "out_of_range", classified.Code)
	assert.Contains(t, classified.Message, "required 9500 MiB, currently free 6000 MiB")
	assert.Contains(t, classified.Message, "reclaim skipped: busy")
	wire := httptest.NewRecorder()
	writeError(wire, nil, classified)
	assert.Equal(t, 409, wire.Code)
	assert.Contains(t, wire.Body.String(), "required 9500 MiB, currently free 6000 MiB")
	assert.Equal(t, 503, loadFailureWire(process.ErrMemoryObservation).Status)
	assert.Equal(t, 400, loadFailureWire(process.ErrMemoryBudgetInvalid).Status)
}

// A cold load that queued and was then refused answers like an inline
// refusal: the run keeps its refusal, not just the text.
func TestQueuedRefusalKeepsItsStatusForColdLoad(t *testing.T) {
	s := variantNode(t)
	inst := instance.NewInstance("queued-run", "llamacpp", qwenModel, 8640, time.Minute, 0)
	require.NoError(t, s.providers.appMgr.Instances().Register(inst))
	t.Cleanup(func() { _ = s.providers.appMgr.Instances().Remove(inst.ID) })
	inst.MarkFailedError(fmt.Errorf("admission: %w", &process.MemoryFitError{Parameter: "budget", Device: "GPU-A", RequiredMiB: 9500, FreeMiB: 6000}))
	_, err := s.waitForLocalInstance(inst)
	var fit *process.MemoryFitError
	require.ErrorAs(t, err, &fit)
	lf := loadFailureWire(err)
	assert.Equal(t, 409, lf.Status)
	assert.Equal(t, "out_of_range", lf.Code)
}

func TestEngineFailureSurvivesColdLoad(t *testing.T) {
	s := variantNode(t)
	inst := instance.NewInstance("failed-run", "llamacpp", qwenModel, 8640, time.Minute, 0)
	require.NoError(t, s.providers.appMgr.Instances().Register(inst))
	t.Cleanup(func() { _ = s.providers.appMgr.Instances().Remove(inst.ID) })
	cause := &httperr.EngineFailure{Class: httperr.FailureMemory, Message: "device allocation failed"}
	inst.MarkFailedError(cause)
	_, err := s.waitForLocalInstance(inst)
	require.ErrorIs(t, err, cause)
	lf := loadFailureWire(err)
	assert.Equal(t, http.StatusConflict, lf.Status)
	assert.Equal(t, string(httperr.CodeOutOfRange), lf.Code)
	assert.Contains(t, lf.Message, inst.ID)
}
