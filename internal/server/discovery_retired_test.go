package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Discovery is what an agent enumerates to decide what it can call, so a
// route that only ever answers 410 is a guaranteed dead end.
func TestAPIDiscoveryOmitsRetiredRoutes(t *testing.T) {
	s := createTestNodeWithDefaults(t)

	// Establish that the retired routes really are mounted — otherwise
	// this test would pass for the wrong reason.
	var mounted []string
	for _, r := range s.engine.Routes() {
		if isRetiredRoute(r.Handler) {
			// Compare in the form discovery publishes, or this assertion
			// passes for the wrong reason.
			mounted = append(mounted, r.Method+" "+uriTemplatePath(r.Path))
		}
	}
	require.NotEmpty(t, mounted, "expected retired routes to be registered")

	resp := makeAuthRequest(t, s, http.MethodGet, "/zzrouter/v1", TestAdminKey, nil)
	require.Equal(t, http.StatusOK, resp.Code)

	var body struct {
		Data struct {
			EndpointGroups map[string][]string `json:"endpoint_groups"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(resp.Body, &body))

	advertised := map[string]bool{}
	for _, entries := range body.Data.EndpointGroups {
		for _, e := range entries {
			advertised[e] = true
		}
	}
	require.NotEmpty(t, advertised, "discovery should list something")

	for _, m := range mounted {
		assert.False(t, advertised[m], "discovery advertises retired route %s", m)
	}

	// The live surface is still there — this must not have emptied the group.
	assert.True(t, advertised["PATCH /zzrouter/v1/providers/{name}/parameters"],
		"the replacement mutator must still be advertised")
}

// Every other management failure answers in the Problem envelope, and a
// caller is told to branch on the top-level code.
func TestRetiredRouteAnswersInTheProblemEnvelope(t *testing.T) {
	s := createTestNodeWithDefaults(t)

	resp := makeAuthRequest(t, s, http.MethodPut,
		"/zzrouter/v1/providers/llamacpp/parameters", TestAdminKey, map[string]any{})

	require.Equal(t, http.StatusGone, resp.Code)
	assert.Contains(t, resp.Headers.Get("Content-Type"), "application/problem+json")

	var problem map[string]any
	require.NoError(t, json.Unmarshal(resp.Body, &problem))
	assert.Equal(t, "retired", problem["code"])
	assert.Equal(t, float64(http.StatusGone), problem["status"])
	// The detail has to name the replacement, or a caller is stuck.
	detail, _ := problem["detail"].(string)
	assert.True(t, strings.Contains(detail, "PATCH"), "detail should name the replacement: %q", detail)
	assert.Contains(t, detail, "merge-patch+json")
}
