package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Agents are instructed to branch on `code` instead of parsing `detail`
// (docs/agent_error_codes.md). That only works if every management failure
// carries one, so this walks representative error paths end to end rather
// than trusting the constructor in isolation.
func TestManagementErrorsAlwaysCarryACode(t *testing.T) {
	s := createTestNodeWithDefaults(t)

	cases := []struct {
		name     string
		method   string
		path     string
		body     any
		wantCode string
	}{
		{"unknown provider", http.MethodGet, "/zzrouter/v1/providers/nope/versions", nil, "not_found"},
		{"unknown job", http.MethodGet, "/zzrouter/v1/jobs/job_nope", nil, "not_found"},
		{"unknown inference log", http.MethodGet, "/zzrouter/v1/inference-logs/nope", nil, "not_found"},
		{"unknown model usage", http.MethodGet, "/zzrouter/v1/usage/models/nope", nil, "not_found"},
		{"missing key", http.MethodGet, "/zzrouter/v1/providers", nil, "unauthorized"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key := TestAdminKey
			if tc.wantCode == "unauthorized" {
				key = ""
			}
			resp := makeAuthRequest(t, s, tc.method, tc.path, key, tc.body)
			require.GreaterOrEqual(t, resp.Code, 400, "expected an error status")

			var problem map[string]any
			require.NoError(t, json.Unmarshal(resp.Body, &problem), "body: %s", resp.Body)

			assert.Equal(t, tc.wantCode, problem["code"],
				"every management error needs a code to branch on; body: %s", resp.Body)
			// type carries the same identity as a URI, so the two must agree.
			assert.Contains(t, problem["type"], "problems/")
		})
	}
}
