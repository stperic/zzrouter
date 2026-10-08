package integration_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	srv "github.com/stperic/zzrouter/internal/server"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIntegration_ProblemEnvelope_Conformance pins the RFC 9457 Problem
// Details contract for /zzrouter/v1/* admin 4xx responses. Every error
// must carry type/title/status/instance/request_id and the Content-Type
// must be application/problem+json. Closes drift surfaced by Arc B (#1)
// where a raw Go error string leaked through a 503 detail.
//
// Out of scope: /v1/* (OpenAI error shape) and /api/* (Ollama error
// shape) — both use protocol-native error envelopes by design.
func TestIntegration_ProblemEnvelope_Conformance(t *testing.T) {
	cfg := srv.DefaultTestNodeConfig()
	cfg.UserKey = srv.TestUserKey
	server := srv.NewTestNode(t, cfg)

	tests := []struct {
		name       string
		method     string
		path       string
		apiKey     string
		body       any
		wantStatus int
	}{
		{
			name:       "missing_key_401",
			method:     "GET",
			path:       "/zzrouter/v1/models",
			apiKey:     "",
			wantStatus: http.StatusUnauthorized,
		},
		{ //nolint:gosec // Synthetic invalid key exercises authentication rejection.
			name:       "wrong_key_401",
			method:     "GET",
			path:       "/zzrouter/v1/models",
			apiKey:     "mx-zzr-it-bogus-9q3vR8mTnYfLcDxBh4",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "reader_key_forbidden_403",
			method:     "POST",
			path:       "/zzrouter/v1/teams",
			apiKey:     srv.TestUserKey,
			body:       map[string]any{"id": "t1", "name": "team1"},
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "validation_400_missing_param",
			method:     "GET",
			path:       "/zzrouter/v1/models/show",
			apiKey:     srv.TestAdminKey,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "not_found_404_unknown_provider",
			method:     "GET",
			path:       "/zzrouter/v1/providers/nonexistent-xyz",
			apiKey:     srv.TestAdminKey,
			wantStatus: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := srv.MakeAuthRequest(t, server, tt.method, tt.path, tt.apiKey, tt.body)
			require.Equal(t, tt.wantStatus, resp.Code,
				"unexpected status; body=%s", string(resp.Body))
			assertProblemEnvelope(t, resp, tt.wantStatus)
		})
	}
}

// assertProblemEnvelope verifies that resp matches RFC 9457 Problem
// Details. Required: type (URL with our problem prefix), title, status
// (matches HTTP code), instance, request_id. detail is omitempty so we
// don't require it.
func assertProblemEnvelope(t *testing.T, resp *srv.TestResponse, wantStatus int) {
	t.Helper()

	ct := resp.Headers.Get("Content-Type")
	assert.Truef(t,
		strings.HasPrefix(ct, "application/problem+json"),
		"Content-Type must be application/problem+json, got %q; body=%s",
		ct, string(resp.Body))

	var p map[string]any
	require.NoError(t, json.Unmarshal(resp.Body, &p),
		"body is not JSON: %s", string(resp.Body))

	typeStr, _ := p["type"].(string)
	assert.Truef(t,
		strings.HasPrefix(typeStr, "https://api.zzrouter.com/problems/"),
		"type must use the zzrouter problems URI prefix, got %q", typeStr)

	title, _ := p["title"].(string)
	assert.NotEmptyf(t, title, "title must be non-empty")

	statusF, ok := p["status"].(float64)
	assert.Truef(t, ok, "status must be a JSON number, got %T", p["status"])
	assert.Equalf(t, wantStatus, int(statusF),
		"status field must match HTTP status code")

	instance, _ := p["instance"].(string)
	assert.NotEmptyf(t, instance, "instance must be non-empty")

	requestID, _ := p["request_id"].(string)
	assert.NotEmptyf(t, requestID,
		"request_id must be non-empty (RequestIDMiddleware not on this path?)")
}
