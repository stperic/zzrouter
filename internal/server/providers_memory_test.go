package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMemoryStartupGateInternalLoadKeepsUnavailableStatus(t *testing.T) {
	s := variantNode(t)
	executor := s.newLoadExecutor()
	executor.launchOnDemand = func(context.Context, string, string, string, map[string]string, map[string]string) (*instance.Instance, error) {
		return nil, fmt.Errorf("another GPU-budgeted run is starting: %w", prov_apps.ErrAtCapacity)
	}
	c, response := testGinContext("POST", "/zzrouter/v1/internal/runs/load")
	c.Request = httptest.NewRequest("POST", "/zzrouter/v1/internal/runs/load", strings.NewReader(`{"model_name":"`+qwenModel+`","provider":"llamacpp"}`))
	executor.HandleInternalLoadModel(c)
	require.Equal(t, 503, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), "another GPU-budgeted run is starting")
}

func TestMemoryRefusalCarriesBudgetAndFreeInsideProblem(t *testing.T) {
	c, response := testGinContext("POST", "/zzrouter/v1/internal/runs")
	refusal := &process.MemoryFitError{Parameter: "budget", Device: "GPU-A", RequiredMiB: 9500, FreeMiB: 6000, Holders: []process.MemoryHolder{}}
	respondProviderErr(c, fmt.Errorf("launch refused: %w", refusal))
	require.Equal(t, 409, response.Code)
	assert.Equal(t, "application/problem+json", response.Header().Get("Content-Type"))
	var problem utils.ProblemDetails
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &problem))
	assert.Equal(t, "out_of_range", problem.Code)
	assert.Equal(t, 409, extractStatusCode(refusal))
	require.Len(t, problem.Errors, 1)
	got := problem.Errors[0].Got.(map[string]any)
	assert.Equal(t, float64(9500), got["required_mib"])
	assert.Equal(t, float64(6000), got["free_mib"])
	assert.Contains(t, problem.Errors[0].Hint, "PATCH")
	assert.Equal(t, 503, extractStatusCode(fmt.Errorf("observe: %w", process.ErrMemoryObservation)))
	assert.Equal(t, 400, extractStatusCode(fmt.Errorf("budget: %w", process.ErrMemoryBudgetInvalid)))
	classified := classifyLoadFailure(fmt.Errorf("cold load: %w", refusal))
	assert.Equal(t, 409, classified.status)
	assert.Equal(t, "out_of_range", classified.code)
	assert.Contains(t, classified.message, "required 9500 MiB, currently free 6000 MiB")
	wire := httptest.NewRecorder()
	writeError(wire, nil, httperr.Error{Status: classified.status, Type: classified.errType, Code: classified.code, Message: classified.message})
	assert.Equal(t, 409, wire.Code)
	assert.Contains(t, wire.Body.String(), "required 9500 MiB, currently free 6000 MiB")
	assert.Equal(t, 503, classifyLoadFailure(process.ErrMemoryObservation).status)
	assert.Equal(t, 400, classifyLoadFailure(process.ErrMemoryBudgetInvalid).status)
}
