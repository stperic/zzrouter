package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/preflight"
)

// The report knows which check failed and how to fix it. Flattening that
// into one prose sentence left a caller parsing English to discover it
// needed curl, and threw the remedy away entirely.
func TestPreflightFailureCarriesTheFailedChecks(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/zzrouter/v1/providers/vllm/install", nil)

	respondPreflightFailed(c, &preflight.Report{
		Provider: "vllm",
		AllOK:    false,
		Results: []preflight.Result{
			{Check: "disk-space", Passed: true, Message: ">= 500MB available"},
			{Check: "curl", Passed: false, Message: "curl not found", Hint: "install curl"},
			{Check: "nvidia-gpu", Passed: false, Message: "no NVIDIA GPU detected"},
		},
	})

	require.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Header().Get("Content-Type"), "application/problem+json")

	var problem struct {
		Code   string `json:"code"`
		Detail string `json:"detail"`
		Errors []struct {
			Key, Code, Message, Hint string
		} `json:"errors"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &problem))

	assert.Equal(t, "preflight_failed", problem.Code,
		"distinguishable from any other 400 without reading detail")
	require.Len(t, problem.Errors, 2, "passing checks are not failures")

	assert.Equal(t, "curl", problem.Errors[0].Key)
	assert.Equal(t, "preflight_failed", problem.Errors[0].Code,
		"the per-key code is drawn from the same closed vocabulary as every other per-key failure")
	assert.Equal(t, "install curl", problem.Errors[0].Hint)

	// A check with no known remedy simply omits the hint rather than
	// inventing one.
	assert.Equal(t, "nvidia-gpu", problem.Errors[1].Key)
	assert.Empty(t, problem.Errors[1].Hint)

	// The prose summary survives for a human reading the log.
	assert.Contains(t, problem.Detail, "curl not found")
}

// A completed install deletes its download archive, so re-checking that
// step's file_exists said the install was broken when it was fine — and
// the cascade buried every step that actually bears on intactness.
func TestVerifyAllToleratesConsumedArtifacts(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "never-written.tar.gz")
	present := t.TempDir()

	plan := &install.Plan{
		Provider: "llamacpp",
		Steps: []install.Step{
			{Number: 1, Verify: install.StepVerify{Type: "dir_exists", Path: present}},
			{Number: 2, Verify: install.StepVerify{Type: "file_exists", Path: missing, Transient: true}},
			{Number: 3, Verify: install.StepVerify{Type: "dir_exists", Path: present}},
		},
	}

	got := plan.VerifyAll()

	assert.True(t, got.AllOK, "a consumed archive is not damage")
	assert.True(t, got.Steps[1].Passed)
	assert.Contains(t, got.Steps[1].Message, "consumed")
	assert.True(t, got.Steps[2].Passed, "later steps must still be evaluated, not skipped")

	// A durable check that fails is still a failure, and still cascades.
	plan.Steps[2].Verify = install.StepVerify{Type: "file_exists", Path: missing}
	got = plan.VerifyAll()
	assert.False(t, got.AllOK)

	// VerifyStep keeps literal semantics: a step-by-step caller asking
	// "did the download land?" needs the real answer.
	assert.False(t, install.VerifyStep(plan.Steps[1]).Passed)
}
