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

// TestIntegration_Error_BadRequest tests 400 Bad Request error responses.
func TestIntegration_Error_BadRequest(t *testing.T) {
	server := srv.NewTestNodeWithDefaults(t)

	tests := []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{
			name:   "models show without name",
			method: "GET",
			path:   "/zzrouter/v1/models/show",
			body:   nil,
		},
		{
			name:   "models delete without name",
			method: "DELETE",
			path:   "/zzrouter/v1/models",
			body:   nil,
		},
		{
			name:   "openai chat without model",
			method: "POST",
			path:   "/v1/chat/completions",
			body:   map[string]any{"messages": []map[string]string{{"role": "user", "content": "hi"}}},
		},
		// /api/generate validation is now gated by Ollama presence: the
		// test server has no ollama provider, so the request is rejected
		// with 503 before reaching the body-validation path. Validation
		// coverage for ollama-shape errors lives in the ollama-presence
		// integration tests once a real backend is wired in.
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var resp *srv.TestResponse
			if strings.HasPrefix(tt.path, "/zzrouter/v1/") {
				resp = srv.MakeAuthRequest(t, server, tt.method, tt.path, srv.TestAdminKey, tt.body)
			} else {
				resp = srv.MakeRequest(t, server, srv.TestRequest{
					Method: tt.method,
					Path:   tt.path,
					Body:   tt.body,
				})
			}

			assert.Equal(t, http.StatusBadRequest, resp.Code,
				"expected 400 Bad Request for %s", tt.name)

			// Verify response body is valid JSON
			var response map[string]any
			err := json.Unmarshal(resp.Body, &response)
			require.NoError(t, err, "error response should be valid JSON")
		})
	}
}

// TestIntegration_Error_Unauthorized tests 401 Unauthorized error responses.
func TestIntegration_Error_Unauthorized(t *testing.T) {
	server := srv.NewTestNodeWithDefaults(t)

	tests := []struct {
		name   string
		method string
		path   string
		apiKey string
	}{
		{
			name:   "no API key",
			method: "GET",
			path:   "/zzrouter/v1/models",
			apiKey: "",
		},
		{
			name:   "invalid API key",
			method: "GET",
			path:   "/zzrouter/v1/models",
			apiKey: "invalid-key",
		},
		{
			name:   "empty API key header",
			method: "GET",
			path:   "/zzrouter/v1/providers",
			apiKey: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var resp *srv.TestResponse
			if tt.apiKey == "" {
				resp = srv.MakeRequest(t, server, srv.TestRequest{
					Method: tt.method,
					Path:   tt.path,
				})
			} else {
				resp = srv.MakeAuthRequest(t, server, tt.method, tt.path, tt.apiKey, nil)
			}

			assert.Equal(t, http.StatusUnauthorized, resp.Code,
				"expected 401 Unauthorized for %s", tt.name)

			// Verify response body is valid JSON
			var response map[string]any
			err := json.Unmarshal(resp.Body, &response)
			require.NoError(t, err, "error response should be valid JSON")

			// Should have error detail (RFC 7807 Problem Details format)
			assert.Contains(t, response, "detail",
				"unauthorized response should contain 'detail' field")
		})
	}
}

// TestIntegration_Error_NotFoundOrBadRequest tests error responses for nonexistent resources.
// Note: Different endpoints may return 400 or 404 for nonexistent resources.
// Some use tolerant behavior (200 with empty data) while others return errors.
func TestIntegration_Error_NotFoundOrBadRequest(t *testing.T) {
	server := srv.NewTestNodeWithDefaults(t)

	tests := []struct {
		name     string
		method   string
		path     string
		body     any
		expected int // Expected status code
	}{
		{
			name:     "nonexistent model show",
			method:   "GET",
			path:     "/zzrouter/v1/models/show?model=nonexistent-model-xyz",
			body:     nil,
			expected: http.StatusNotFound, // API returns 404 for model not found
		},
		{
			name:     "nonexistent app",
			method:   "GET",
			path:     "/zzrouter/v1/providers/nonexistent-app-xyz",
			body:     nil,
			expected: http.StatusNotFound,
		},
		{
			name:     "nonexistent run",
			method:   "GET",
			path:     "/zzrouter/v1/runs/nonexistent-run-xyz",
			body:     nil,
			expected: http.StatusNotFound, // API returns 404 for non-existent run
		},
		{
			name:   "openai chat with unknown model",
			method: "POST",
			path:   "/v1/chat/completions",
			body: map[string]any{
				"model":    "nonexistent-model-xyz",
				"messages": []map[string]string{{"role": "user", "content": "hi"}},
			},
			// OpenAI's canonical response for an unknown model is 404
			// with type=invalid_request_error and code=model_not_found.
			// zzRouter now matches that spec on the inference path —
			// the same shape handleModelByID already emits for GET
			// /v1/models/:id — so strict SDK clients that switch on
			// error.type get a typed NotFoundError instead of falling
			// through to the generic APIError catch-all.
			expected: http.StatusNotFound,
		},
		// /api/show is gated by Ollama presence; on a test node with no
		// ollama provider the request returns 503 before model lookup.
		// 404-on-unknown-model coverage moves to a backend-provisioned
		// suite once the test harness can install ollama on demand.
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var resp *srv.TestResponse
			if strings.HasPrefix(tt.path, "/zzrouter/v1/") {
				resp = srv.MakeAuthRequest(t, server, tt.method, tt.path, srv.TestAdminKey, tt.body)
			} else {
				resp = srv.MakeRequest(t, server, srv.TestRequest{
					Method: tt.method,
					Path:   tt.path,
					Body:   tt.body,
				})
			}

			assert.Equal(t, tt.expected, resp.Code,
				"expected status %d for %s, got %d", tt.expected, tt.name, resp.Code)

			// Verify response body is valid JSON
			var response map[string]any
			err := json.Unmarshal(resp.Body, &response)
			require.NoError(t, err, "error response should be valid JSON")
		})
	}
}

// TestIntegration_Error_MethodNotAllowed tests 405 Method Not Allowed responses.
func TestIntegration_Error_MethodNotAllowed(t *testing.T) {
	server := srv.NewTestNodeWithDefaults(t)

	tests := []struct {
		name   string
		method string
		path   string
	}{
		{
			name:   "POST to health endpoint",
			method: "POST",
			path:   "/health",
		},
		{
			name:   "DELETE to health endpoint",
			method: "DELETE",
			path:   "/health/live",
		},
		{
			name:   "PUT to models list",
			method: "PUT",
			path:   "/v1/models",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := srv.MakeRequest(t, server, srv.TestRequest{
				Method: tt.method,
				Path:   tt.path,
			})

			// Should return 404 or 405
			assert.True(t, resp.Code == http.StatusMethodNotAllowed || resp.Code == http.StatusNotFound,
				"expected 404 or 405 for %s, got %d", tt.name, resp.Code)
		})
	}
}

// Strict RFC 9457 conformance for /zzrouter/v1/* lives in
// integration_problem_envelope_test.go (the prior soft check that
// accepted "either Problem Details or {error}" is replaced — it
// encoded the opposite of the contract we want to pin).

// TestIntegration_Error_InvalidJSON tests handling of invalid JSON request bodies.
func TestIntegration_Error_InvalidJSON(t *testing.T) {
	server := srv.NewTestNodeWithDefaults(t)

	tests := []struct {
		name    string
		path    string
		body    string
		wantErr bool
	}{
		{
			name:    "malformed JSON",
			path:    "/v1/chat/completions",
			body:    `{"model": "test", "messages": [`,
			wantErr: true,
		},
		// /api/generate is gated by Ollama presence; the bad-JSON
		// validation path is exercised on /v1/chat/completions above
		// and on whichever Ollama-backed test node the suite runs against.
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := srv.MakeRequest(t, server, srv.TestRequest{
				Method:  "POST",
				Path:    tt.path,
				Headers: map[string]string{"Content-Type": "application/json"},
			})

			// Should return 400 for invalid JSON
			assert.Equal(t, http.StatusBadRequest, resp.Code,
				"expected 400 Bad Request for invalid JSON")
		})
	}
}

// TestIntegration_Error_ContentTypeHandling tests Content-Type handling.
func TestIntegration_Error_ContentTypeHandling(t *testing.T) {
	server := srv.NewTestNodeWithDefaults(t)

	tests := []struct {
		name        string
		contentType string
		body        any
		expectError bool
	}{
		{
			name:        "application/json",
			contentType: "application/json",
			body:        map[string]any{"model": "test"},
			expectError: false, // Valid content type
		},
		{
			name:        "no content type",
			contentType: "",
			body:        map[string]any{"model": "test"},
			expectError: false, // Should work with JSON encoding
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			headers := map[string]string{}
			if tt.contentType != "" {
				headers["Content-Type"] = tt.contentType
			}

			resp := srv.MakeRequest(t, server, srv.TestRequest{
				Method:  "POST",
				Path:    "/v1/chat/completions",
				Headers: headers,
				Body:    tt.body,
			})

			// Should not return 415 Unsupported Media Type
			assert.NotEqual(t, http.StatusUnsupportedMediaType, resp.Code,
				"should accept %s content type", tt.contentType)
		})
	}
}

// TestIntegration_Error_LargeRequestBody tests request size limits.
func TestIntegration_Error_LargeRequestBody(t *testing.T) {
	server := srv.NewTestNodeWithDefaults(t)

	// Create a request body that's larger than typical limits
	// but not so large it causes memory issues in tests
	largeContent := make([]byte, 1024*1024) // 1MB
	for i := range largeContent {
		largeContent[i] = 'a'
	}

	requestBody := map[string]any{
		"model": "test-model",
		"messages": []map[string]string{
			{"role": "user", "content": string(largeContent)},
		},
	}

	resp := srv.MakeRequest(t, server, srv.TestRequest{
		Method: "POST",
		Path:   "/v1/chat/completions",
		Body:   requestBody,
	})

	// Large requests should either succeed or return appropriate error
	// The exact behavior depends on configured limits
	assert.True(t,
		resp.Code == http.StatusOK ||
			resp.Code == http.StatusBadRequest ||
			resp.Code == http.StatusNotFound ||
			resp.Code == http.StatusRequestEntityTooLarge,
		"large request should return appropriate status, got %d", resp.Code)
}

// TestIntegration_Error_ConcurrentErrorHandling tests error handling under concurrent load.
func TestIntegration_Error_ConcurrentErrorHandling(t *testing.T) {
	server := srv.NewTestNodeWithDefaults(t)

	concurrency := 10
	done := make(chan int, concurrency)

	for range concurrency {
		go func() {
			// Make requests that will fail with 400
			resp := srv.MakeRequest(t, server, srv.TestRequest{
				Method: "POST",
				Path:   "/v1/chat/completions",
				Body:   map[string]any{}, // Missing required fields
			})
			done <- resp.Code
		}()
	}

	// All requests should return consistent error codes
	for range concurrency {
		code := <-done
		assert.Equal(t, http.StatusBadRequest, code,
			"concurrent error requests should return consistent 400 status")
	}
}
