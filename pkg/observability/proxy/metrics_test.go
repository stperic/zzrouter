package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/stperic/zzrouter/pkg/observability/genai"
)

func TestErrorTypeFromStatus(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{http.StatusBadRequest, "invalid_request_error"},
		{http.StatusUnauthorized, "authentication_error"},
		{http.StatusForbidden, "permission_error"},
		{http.StatusNotFound, "invalid_request_error"},
		{http.StatusTooManyRequests, "rate_limit_error"},
		{http.StatusInternalServerError, "server_error"},
		{http.StatusBadGateway, "api_error"},
		{http.StatusServiceUnavailable, "server_error"},
		{http.StatusGatewayTimeout, "api_error"},
		{200, ""}, // no failure tag
	}
	for _, c := range cases {
		if got := errorTypeFromStatus(c.status); got != c.want {
			t.Errorf("errorTypeFromStatus(%d) = %q, want %q", c.status, got, c.want)
		}
	}
}

// TestRecordFailedRequest_NilSafety pins that the recorder no-ops on
// nil receivers and nil contexts — both legitimate during early-startup
// initialisation paths or when the global meter provider hasn't bound.
func TestRecordFailedRequest_NilSafety(t *testing.T) {
	var m *Metrics
	// Should not panic — nil receiver path.
	m.RecordFailedRequest(nil, 500)
	m.RecordFailedRequest(&gin.Context{}, 400)
}

// TestStringFromCtx_MissingAndWrongType asserts the helper returns "" on
// missing keys and on entries with the wrong type.
func TestStringFromCtx_MissingAndWrongType(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	if got := stringFromCtx(c, "missing"); got != "" {
		t.Errorf("missing key = %q, want empty", got)
	}
	c.Set("wrongtype", 42)
	if got := stringFromCtx(c, "wrongtype"); got != "" {
		t.Errorf("non-string entry = %q, want empty", got)
	}
	c.Set("ok", "value")
	if got := stringFromCtx(c, "ok"); got != "value" {
		t.Errorf("string entry = %q, want value", got)
	}
}

// TestMiddleware_NoPanic exercises the gate paths without crashing.
// End-to-end metric assertion lives in
// internal/server/observability_integration_test.go where a real
// /metrics endpoint scrape is available.
func TestMiddleware_NoPanic(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(Middleware())
	r.GET("/ok", func(c *gin.Context) { c.Status(http.StatusOK) })
	r.GET("/bad", func(c *gin.Context) { c.Status(http.StatusBadRequest) })
	r.GET("/err", func(c *gin.Context) { c.Status(http.StatusInternalServerError) })

	for _, path := range []string{"/ok", "/bad", "/err"} {
		req := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
	}
}

// TestModelAllowlist_ProxyForwarder pins that the package-level
// SetModelAllowlist forwarder reaches the canonical genai gate. Same
// observable behavior as calling genai.SetModelAllowlist directly;
// kept on the proxy surface so existing callers don't have to switch
// imports. Detailed gate behavior (passthrough / known / unknown /
// concurrent-swap) is exercised in the genai package's own tests.
func TestModelAllowlist_ProxyForwarder(t *testing.T) {
	defer genai.SwapModelAllowlist(func(name string) bool {
		return name == "ok"
	})()

	if got := genai.GateModelLabel("ok"); got != "ok" {
		t.Errorf("known model = %q, want passthrough", got)
	}
	if got := genai.GateModelLabel("nope"); got != UnknownModelLabel {
		t.Errorf("unknown model = %q, want %q sentinel", got, UnknownModelLabel)
	}
}

// TestSetErrorTypeOnRequest_RoundTrip pins the bare-http stash mechanism:
// a writeError-style handler that doesn't have a *gin.Context
// can override the error.type via SetErrorTypeOnRequest, and the middleware
// reads it from req.Context() at metric record time. Regression for the
// 503+api_error site at internal/server/model_streaming.go.
func TestSetErrorTypeOnRequest_RoundTrip(t *testing.T) {
	req := httptest.NewRequest("GET", "/v1/chat/completions", nil)
	req = SetErrorTypeOnRequest(req, "api_error")
	got, ok := req.Context().Value(reqCtxErrorTypeKey).(string)
	if !ok || got != "api_error" {
		t.Fatalf("stash round-trip = %q (ok=%v), want api_error", got, ok)
	}

	// Empty-string overrides do not stash (avoid clobbering downstream
	// fallbacks with sentinels).
	req2 := httptest.NewRequest("GET", "/v1/chat/completions", nil)
	req2 = SetErrorTypeOnRequest(req2, "")
	if got := req2.Context().Value(reqCtxErrorTypeKey); got != nil {
		t.Errorf("empty errType should not stash; got %v", got)
	}

	// Nil request returns nil — production callers pass through without
	// panicking even on bizarre inputs.
	if SetErrorTypeOnRequest(nil, "api_error") != nil {
		t.Errorf("nil req should pass through")
	}
}
