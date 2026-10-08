package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOpenAI_DoesNotAutoDeploy is the access-control invariant for the
// OpenAI compat surface: a client request for a model that isn't in the
// deployed pool MUST fail with 404 model_not_found and MUST NOT side-
// effect (no auto-deploy, no auto-launch).
//
// The deployed pool is the access-control list for OpenAI clients —
// `POST /zzrouter/v1/deployments` (admin-gated) is the only on-ramp.
// If a future "helpful" refactor wires auto-deploy into /v1/*, OpenAI
// clients can escalate by sending an unknown model name and bypass the
// admin-key gate that protects what gets into the pool. This test fails
// loudly if that happens.
func TestOpenAI_DoesNotAutoDeploy(t *testing.T) {
	s := createTestNodeWithDefaults(t)

	// Snapshot the registry baseline before the request.
	require.NotNil(t, s.providers.appMgr, "test fixture must have a provider manager")
	beforeInstances := len(s.providers.appMgr.Instances().ListAll())

	cases := []struct {
		path string
		body string
	}{
		{"/v1/chat/completions", `{"model":"never-deployed-model-xyz","messages":[{"role":"user","content":"x"}]}`},
		{"/v1/completions", `{"model":"never-deployed-model-xyz","prompt":"x"}`},
		{"/v1/embeddings", `{"model":"never-deployed-model-xyz","input":"x"}`},
	}
	for _, tc := range cases {
		path, body := tc.path, tc.body
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			// No X-API-Key — the OpenAI surface is anonymous-by-default.
			w := httptest.NewRecorder()
			s.engine.ServeHTTP(w, req)

			// Must surface as the resolver's typed model_not_found, not gin's
			// NoRoute or a generic 500 — that's how SDK clients switch on
			// error.code for retry classification.
			assert.Equal(t, http.StatusNotFound, w.Code,
				"unknown model on %s must 404, got %d body=%s", path, w.Code, w.Body.String())

			var env struct {
				Error struct {
					Code string `json:"code"`
					Type string `json:"type"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env),
				"%s response must be OpenAI-shaped JSON: %s", path, w.Body.String())
			assert.Equal(t, "model_not_found", env.Error.Code,
				"%s must surface the resolver's model_not_found code (got: %s)", path, w.Body.String())
		})
	}

	// Load-bearing assertion: after every unknown-model OpenAI request,
	// the instance registry must be unchanged. No silent auto-launch.
	// If this trips, the access-control story for the OpenAI surface
	// has been compromised — the pool no longer gates what clients
	// can run.
	afterInstances := len(s.providers.appMgr.Instances().ListAll())
	assert.Equal(t, beforeInstances, afterInstances,
		"OpenAI requests for unknown models must not launch instances (before=%d after=%d)",
		beforeInstances, afterInstances)
}
