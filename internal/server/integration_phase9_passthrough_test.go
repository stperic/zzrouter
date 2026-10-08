// Integration tests for Phase 9 — OpenAI /v1/* pass-through coverage.
//
// Three things are exercised here that the unit tests don't:
//  1. Route registration: every Phase 9 route must be reachable on the
//     configured /v1 group and must not 404.
//  2. Default-backend behaviour: 503 when no backend is configured,
//     successful forward when one is, 503 when the configured backend
//     is not registered in the apps config.
//  3. End-to-end forwarding: an httptest.Server stands in for a real
//     provider and asserts that method, path, body, and Content-Type
//     survive the round trip unchanged.
package server

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordedRequest captures one intercepted proxy hop from the test backend.
type recordedRequest struct {
	Method      string
	Path        string
	Query       string
	ContentType string
	Body        []byte
	Headers     http.Header
}

// newBackendRecorder spins up an httptest.Server that records every incoming
// request and returns the response chosen by the caller. The returned close
// function must be called on cleanup.
func newBackendRecorder(t *testing.T, respStatus int, respBody []byte, respContentType string) (*httptest.Server, *[]recordedRequest, *sync.Mutex) {
	t.Helper()
	var mu sync.Mutex
	var recorded []recordedRequest

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		recorded = append(recorded, recordedRequest{
			Method:      r.Method,
			Path:        r.URL.Path,
			Query:       r.URL.RawQuery,
			ContentType: r.Header.Get("Content-Type"),
			Body:        body,
			Headers:     r.Header.Clone(),
		})
		mu.Unlock()
		if respContentType != "" {
			w.Header().Set("Content-Type", respContentType)
		}
		w.WriteHeader(respStatus)
		_, _ = w.Write(respBody)
	}))
	t.Cleanup(func() { closeHTTPTestServer(ts) })
	return ts, &recorded, &mu
}

// configureBackend attaches a test provider named `key` to the server's
// appsConfig pointing at the given URL, and wires it as the openai_compat
// default backend. The provider runs in service mode so HasEndpoint() is true.
func configureBackend(t *testing.T, s *Server, key, url string) {
	t.Helper()
	enabled := true
	svc := pkgConfig.ServiceConfig{
		Enabled:  &enabled,
		Name:     key,
		Protocol: pkgConfig.ProtocolOpenAI, // required by ExternalProvider.Validate post-PopulateProviders
		Mode:     "service",
		Runtime: &pkgConfig.AppRuntimeConfig{
			Endpoint: url,
		},
		Capabilities: &pkgConfig.AppCapabilities{
			WireEndpoints: []string{"chat_completions", "responses"},
		},
	}
	s.appsConfig = &pkgConfig.AppsConfig{
		Version: "1.0",
		Name:    "test",
	}
	if err := s.appsConfig.AddApp(key, svc); err != nil {
		t.Fatalf("AddApp(%q): %v", key, err)
	}
	s.config.OpenAICompat.DefaultBackend = key
}

// ---------- /v1/realtime upgrade without a configured backend ----------

// TestRealtime_NoBackendConfigured_Returns503 verifies that when
// openai_compat.realtime_backend is empty, the WebSocket upgrade handler
// refuses with a structured OpenAI-envelope 503 instead of attempting an
// upgrade against nothing. This is the previous "501 stub" contract,
// replaced with a real 503 that explains how to configure the backend.
func TestRealtime_NoBackendConfigured_Returns503(t *testing.T) {
	server := createTestNodeWithDefaults(t)

	for _, method := range []string{"GET", "POST"} {
		resp := makeRequest(t, server, TestRequest{Method: method, Path: "/v1/realtime"})
		require.Equalf(t, http.StatusServiceUnavailable, resp.Code,
			"expected 503 for %s /v1/realtime with no backend, got %d: %s", method, resp.Code, resp.Body)

		var env struct {
			Error struct {
				Type    string `json:"type"`
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(resp.Body, &env))
		assert.Equal(t, "server_error", env.Error.Type)
		// Responder collapses "feature is gated off / not configured"
		// into feature_disabled. Specific cause lives in the message.
		assert.Equal(t, "feature_disabled", env.Error.Code)
		assert.NotEmpty(t, env.Error.Message)
		assert.Contains(t, env.Error.Message, "realtime")
	}
}

// ---------- Stateful endpoints: no default backend configured ----------

func TestPhase9_Stateful_NoDefaultBackend_Returns503(t *testing.T) {
	server := createTestNodeWithDefaults(t)
	// Explicitly DO NOT configure a default backend.

	cases := []struct {
		method, path string
	}{
		{"GET", "/v1/files"},
		{"POST", "/v1/files"},
		{"GET", "/v1/files/file-abc"},
		{"GET", "/v1/batches"},
		{"POST", "/v1/batches"},
		{"POST", "/v1/threads"},
		{"POST", "/v1/threads/runs"},
		{"GET", "/v1/threads/thread-xyz"},
		{"POST", "/v1/vector_stores"},
		{"POST", "/v1/uploads"},
		{"POST", "/v1/fine_tuning/jobs"},
		{"POST", "/v1/assistants"},
	}

	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			resp := makeRequest(t, server, TestRequest{Method: tc.method, Path: tc.path})
			require.Equal(t, http.StatusServiceUnavailable, resp.Code, "body=%s", resp.Body)

			var env struct {
				Error struct {
					Type string `json:"type"`
					Code string `json:"code"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(resp.Body, &env))
			assert.Equal(t, "server_error", env.Error.Type)
			assert.Equal(t, "no_default_backend_configured", env.Error.Code)
		})
	}
}

// TestPhase9_Responses_Retrieve_NoAffinity_Returns404 asserts that retrieving
// an unknown response id without a default backend configured returns a
// proper 404 (response_not_found) rather than the generic 503
// no_default_backend_configured. POST /v1/responses forces store=false
// inside zzrouter, so retrievable state is never created locally — an
// affinity miss with no default backend means the id genuinely does not
// exist. Distinct from the other stateful endpoints in this file because
// those are config errors (operator forgot to wire a backend), not
// data-existence errors.
func TestPhase9_Responses_Retrieve_NoAffinity_Returns404(t *testing.T) {
	server := createTestNodeWithDefaults(t)

	resp := makeRequest(t, server, TestRequest{Method: "GET", Path: "/v1/responses/resp-not-real"})
	require.Equal(t, http.StatusNotFound, resp.Code, "body=%s", resp.Body)

	var env struct {
		Error struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(resp.Body, &env))
	assert.Equal(t, "invalid_request_error", env.Error.Type)
	assert.Equal(t, "response_not_found", env.Error.Code)
}

// ---------- Stateful endpoints: default backend configured but unreachable ----------

func TestPhase9_Stateful_UnknownBackendKey_Returns503(t *testing.T) {
	server := createTestNodeWithDefaults(t)
	server.config.OpenAICompat.DefaultBackend = "nonexistent-provider"
	// appsConfig stays nil or unset — the key resolves to nothing.

	resp := makeRequest(t, server, TestRequest{Method: "GET", Path: "/v1/files"})
	require.Equal(t, http.StatusServiceUnavailable, resp.Code)

	var env struct {
		Error struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(resp.Body, &env))
	assert.Equal(t, "backend_unavailable", env.Error.Code)
}

// ---------- Stateful endpoints: full forward with a live test backend ----------

func TestPhase9_Stateful_ForwardsToBackend_PreservingMethodPathBody(t *testing.T) {
	server := createTestNodeWithDefaults(t)

	respPayload := []byte(`{"id":"file-abc","object":"file","purpose":"assistants"}`)
	ts, recorded, mu := newBackendRecorder(t, http.StatusOK, respPayload, "application/json")
	configureBackend(t, server, "test-backend", ts.URL)

	// Issue the request with a raw opaque body so we can verify byte-for-byte
	// preservation. makeRequest would JSON-marshal a typed body, which would
	// defeat the test.
	reqBody := []byte(`opaque-pass-through-payload`)
	httpReq := httptest.NewRequest("POST", "/v1/files", bytes.NewReader(reqBody))
	httpReq.Header.Set("Content-Type", "application/octet-stream")
	w := httptest.NewRecorder()
	server.engine.ServeHTTP(w, httpReq)

	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	assert.JSONEq(t, string(respPayload), w.Body.String())

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, *recorded, 1, "expected exactly one backend hit")
	got := (*recorded)[0]
	assert.Equal(t, "POST", got.Method)
	assert.Equal(t, "/v1/files", got.Path)
	assert.Equal(t, "application/octet-stream", got.ContentType)
	assert.Equal(t, reqBody, got.Body)
}

// ---------- Stateful endpoints: GET forwarded with query string and path param ----------

func TestPhase9_Stateful_GetForwardsPathAndMethod(t *testing.T) {
	server := createTestNodeWithDefaults(t)
	ts, recorded, mu := newBackendRecorder(t, http.StatusOK, []byte(`{"object":"list","data":[]}`), "application/json")
	configureBackend(t, server, "test-backend", ts.URL)

	httpReq := httptest.NewRequest("GET", "/v1/vector_stores/vs-123/files?limit=5", nil)
	w := httptest.NewRecorder()
	server.engine.ServeHTTP(w, httpReq)

	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, *recorded, 1)
	assert.Equal(t, "GET", (*recorded)[0].Method)
	assert.Equal(t, "/v1/vector_stores/vs-123/files", (*recorded)[0].Path)
	assert.Equal(t, "limit=5", (*recorded)[0].Query)
}

// ---------- Multipart without model field falls through to default backend ----------

func TestPhase9_Multipart_ImageVariationsFallsToDefault(t *testing.T) {
	server := createTestNodeWithDefaults(t)
	ts, recorded, mu := newBackendRecorder(t, http.StatusOK, []byte(`{"created":1,"data":[]}`), "application/json")
	configureBackend(t, server, "test-backend", ts.URL)

	// Build a multipart body WITHOUT a model field (legal for variations).
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	fw, err := writer.CreateFormFile("image", "in.png")
	require.NoError(t, err)
	_, _ = fw.Write([]byte("fake png bytes"))
	require.NoError(t, writer.WriteField("n", "1"))
	require.NoError(t, writer.Close())

	httpReq := httptest.NewRequest("POST", "/v1/images/variations", body)
	httpReq.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	server.engine.ServeHTTP(w, httpReq)

	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, *recorded, 1, "expected exactly one backend hit")
	got := (*recorded)[0]
	assert.Equal(t, "POST", got.Method)
	assert.Equal(t, "/v1/images/variations", got.Path)
	// Content-Type with boundary must be preserved verbatim, otherwise the
	// backend cannot parse the multipart body.
	assert.True(t, strings.HasPrefix(got.ContentType, "multipart/form-data; boundary="),
		"expected multipart content-type with boundary, got %q", got.ContentType)
	// The body must contain the original file bytes and form field.
	assert.Contains(t, string(got.Body), "fake png bytes")
	assert.Contains(t, string(got.Body), `name="n"`)
}
