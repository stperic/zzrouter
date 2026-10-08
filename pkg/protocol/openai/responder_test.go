package openai

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// newTestContext builds a gin.Context + recorder pair suitable for
// exercising a Responder method without routing through an engine.
// The returned context has a synthetic request id so X-Request-Id
// propagation can be asserted.
func newTestContext(method, path string) (*gin.Context, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(method, path, nil)
	c.Request = req
	c.Set("request_id", "req-test-123")
	return c, rec
}

// decodeEnvelope parses an OpenAI error envelope from a recorder body
// and fails the test on malformed JSON.
func decodeEnvelope(t *testing.T, rec *httptest.ResponseRecorder) Envelope {
	t.Helper()
	var env Envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("response body is not a valid envelope: %v\nbody=%s", err, rec.Body.String())
	}
	return env
}

// TestResponder_Unauthorized pins the 401 shape and status.
func TestResponder_Unauthorized(t *testing.T) {
	r := New()
	c, rec := newTestContext("POST", "/v1/chat/completions")

	r.Unauthorized(c, "bearer rejected")

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if got := rec.Header().Get("X-Request-Id"); got != "req-test-123" {
		t.Errorf("X-Request-Id = %q, want req-test-123", got)
	}
	env := decodeEnvelope(t, rec)
	if env.Error.Type != string(ErrorTypeAuthentication) {
		t.Errorf("type = %q, want %q", env.Error.Type, ErrorTypeAuthentication)
	}
	if env.Error.Code != string(ErrorCodeInvalidAPIKey) {
		t.Errorf("code = %q, want %q", env.Error.Code, ErrorCodeInvalidAPIKey)
	}
}

// TestResponder_NotFound_ModelVsURL exercises the disambiguation
// heuristic between ErrorCodeUnknownURL and ErrorCodeModelNotFound.
func TestResponder_NotFound_ModelVsURL(t *testing.T) {
	cases := []struct {
		name   string
		detail string
		want   ErrorCode
	}{
		{"unknown endpoint", "endpoint not found: GET /v1/banana", ErrorCodeUnknownURL},
		{"model missing", "model gpt-banana not found", ErrorCodeModelNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := New()
			c, rec := newTestContext("POST", "/v1/chat/completions")
			r.NotFound(c, tc.detail)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", rec.Code)
			}
			env := decodeEnvelope(t, rec)
			if env.Error.Code != string(tc.want) {
				t.Errorf("code = %q, want %q", env.Error.Code, tc.want)
			}
		})
	}
}

// TestResponder_Internal_HidesDetail is the most important security
// assertion in the package: the raw panic or error string MUST NOT
// appear in the response body. It MUST be delivered only to the
// InternalLogger.
func TestResponder_Internal_HidesDetail(t *testing.T) {
	var loggedReqID, loggedDetail string
	r := New(WithInternalLogger(func(reqID, detail string) {
		loggedReqID = reqID
		loggedDetail = detail
	}))
	c, rec := newTestContext("POST", "/v1/chat/completions")

	r.Internal(c, "panic: types.InstanceStatus nil pointer dereference at /home/ci/zzrouter/foo.go:42")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "types.InstanceStatus") {
		t.Errorf("detail leaked into client body: %s", body)
	}
	if strings.Contains(body, "/home/") {
		t.Errorf("path leaked into client body: %s", body)
	}
	env := decodeEnvelope(t, rec)
	if env.Error.Message != "internal server error" {
		t.Errorf("message = %q, want fixed generic string", env.Error.Message)
	}
	if env.Error.Type != string(ErrorTypeServer) {
		t.Errorf("type = %q, want %q", env.Error.Type, ErrorTypeServer)
	}
	if loggedReqID != "req-test-123" {
		t.Errorf("logger reqID = %q, want req-test-123", loggedReqID)
	}
	if !strings.Contains(loggedDetail, "types.InstanceStatus") {
		t.Errorf("logger did not receive raw detail: %q", loggedDetail)
	}
}

// TestResponder_TooManyRequests_SetsRetryAfter pins the header
// semantics: delta-seconds when positive, omitted when zero.
func TestResponder_TooManyRequests_SetsRetryAfter(t *testing.T) {
	r := New()
	c, rec := newTestContext("POST", "/v1/chat/completions")
	r.TooManyRequests(c, "too fast", 20)
	if got := rec.Header().Get("Retry-After"); got != "20" {
		t.Errorf("Retry-After = %q, want 20", got)
	}

	c2, rec2 := newTestContext("POST", "/v1/chat/completions")
	r.TooManyRequests(c2, "too fast", 0)
	if got := rec2.Header().Get("Retry-After"); got != "" {
		t.Errorf("Retry-After = %q, want empty", got)
	}
}

// TestResponder_Unavailable_CodeDisambiguation verifies the 503 sub-code
// heuristic picks up the zzRouter-specific configurational states.
func TestResponder_Unavailable_CodeDisambiguation(t *testing.T) {
	cases := []struct {
		detail string
		want   ErrorCode
	}{
		{"feature is not enabled", ErrorCodeFeatureDisabled},
		{"server draining for shutdown", ErrorCodeDraining},
		{"no default backend configured for openai_compat", ErrorCodeNoDefaultBackend},
	}
	for _, tc := range cases {
		r := New()
		c, rec := newTestContext("POST", "/v1/responses")
		r.Unavailable(c, tc.detail)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503", rec.Code)
		}
		env := decodeEnvelope(t, rec)
		if env.Error.Code != string(tc.want) {
			t.Errorf("detail=%q: code = %q, want %q", tc.detail, env.Error.Code, tc.want)
		}
	}
}

// TestResponder_Opaque_DropsBody pins the opaque-mode contract:
// status code intact, headers intact, body empty.
func TestResponder_Opaque_DropsBody(t *testing.T) {
	r := New(WithOpaqueErrors(true))
	c, rec := newTestContext("POST", "/v1/chat/completions")
	r.BadRequest(c, "malformed", "messages")

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("opaque body should be empty, got %q", rec.Body.String())
	}
	if got := rec.Header().Get("X-Request-Id"); got != "req-test-123" {
		t.Errorf("X-Request-Id dropped in opaque mode: %q", got)
	}
}

// TestResponder_PanicsOnInvalidVocab is the contract enforcement
// point: the Responder refuses to emit unknown vocabulary values. A
// developer who mistypes a constant gets a loud test failure rather
// than a silent wire-compat regression.
func TestResponder_PanicsOnInvalidVocab(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on unknown ErrorType")
		}
	}()
	r := New()
	c, _ := newTestContext("POST", "/v1/chat/completions")
	r.writeEnvelope(c, http.StatusBadRequest, ErrorType("nope"), ErrorCodeInvalidRequest, "x", "")
}
