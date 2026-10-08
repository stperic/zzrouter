package httperr

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
)

type testContextKey struct{}

func init() {
	gin.SetMode(gin.TestMode)
}

// stubResponder records the most recent method invocation per
// instance. It implements httperr.Responder with inert bodies — the
// dispatcher tests only care about which Responder was selected, not
// about the wire output.
type stubResponder struct {
	mu   sync.Mutex
	name string
	last string
}

func (s *stubResponder) Name() string { return s.name }

func (s *stubResponder) record(m string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.last = m
}

func (s *stubResponder) BadRequest(c *gin.Context, _, _ string) {
	s.record("BadRequest")
	c.Status(400)
}
func (s *stubResponder) Unauthorized(c *gin.Context, _ string) {
	s.record("Unauthorized")
	c.Status(401)
}
func (s *stubResponder) Forbidden(c *gin.Context, _ string) { s.record("Forbidden"); c.Status(403) }
func (s *stubResponder) NotFound(c *gin.Context, _ string)  { s.record("NotFound"); c.Status(404) }
func (s *stubResponder) MethodNotAllowed(c *gin.Context, _ string) {
	s.record("MethodNotAllowed")
	c.Status(405)
}
func (s *stubResponder) RequestTooLarge(c *gin.Context, _ string) {
	s.record("RequestTooLarge")
	c.Status(413)
}
func (s *stubResponder) TooManyRequests(c *gin.Context, _ string, _ int) {
	s.record("TooManyRequests")
	c.Status(429)
}
func (s *stubResponder) Internal(c *gin.Context, _ string)    { s.record("Internal"); c.Status(500) }
func (s *stubResponder) BadGateway(c *gin.Context, _ string)  { s.record("BadGateway"); c.Status(502) }
func (s *stubResponder) Unavailable(c *gin.Context, _ string) { s.record("Unavailable"); c.Status(503) }
func (s *stubResponder) GatewayTimeout(c *gin.Context, _ string) {
	s.record("GatewayTimeout")
	c.Status(504)
}
func (s *stubResponder) WriteError(w http.ResponseWriter, _ *http.Request, e Error) {
	s.record("WriteError")
	w.WriteHeader(e.Status)
}
func (s *stubResponder) NormalizeUpstreamError(_ int, raw []byte) []byte { return raw }

func newCtx(path string) (*gin.Context, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest("GET", path, nil)
	return c, rec
}

// TestPathDispatcher_LongestPrefixWins asserts the core ordering
// invariant: when multiple rules match, the one with the longest
// prefix is selected.
func TestPathDispatcher_LongestPrefixWins(t *testing.T) {
	fallback := &stubResponder{name: "fallback"}
	v1 := &stubResponder{name: "openai"}
	mgmt := &stubResponder{name: "problem"}

	d := NewPathDispatcher(fallback)
	d.Register("/v1/", v1)
	d.Register("/zzrouter/v1/", mgmt)

	cases := map[string]string{
		"/v1/chat/completions":        "openai",
		"/zzrouter/v1/providers":      "problem",
		"/health":                     "fallback",
		"/api/tags":                   "fallback",
		"/zzrouter/v1/internal/nodes": "problem",
	}
	for path, want := range cases {
		if got := d.For(path).Name(); got != want {
			t.Errorf("For(%q) = %s, want %s", path, got, want)
		}
	}
}

// TestPathDispatcher_ReregisterReplaces guarantees Register is
// idempotent: registering the same prefix twice replaces the earlier
// rule rather than stacking.
func TestPathDispatcher_ReregisterReplaces(t *testing.T) {
	fallback := &stubResponder{name: "fallback"}
	a := &stubResponder{name: "a"}
	b := &stubResponder{name: "b"}
	d := NewPathDispatcher(fallback)
	d.Register("/v1/", a)
	d.Register("/v1/", b)
	if got := d.For("/v1/chat").Name(); got != "b" {
		t.Errorf("expected replacement, got %s", got)
	}
}

// TestPathDispatcher_NilFallbackPanics codifies the "no silent
// fallback to empty" invariant from the doc comment.
func TestPathDispatcher_NilFallbackPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for nil fallback")
		}
	}()
	_ = NewPathDispatcher(nil)
}

// TestAttachResponder_ExposesViaFromContext wires the middleware and
// asserts FromContext returns the attached responder within the
// handler chain.
func TestAttachResponder_ExposesViaFromContext(t *testing.T) {
	r := &stubResponder{name: "openai"}
	engine := gin.New()
	engine.Use(AttachResponder(r))
	engine.GET("/v1/x", func(c *gin.Context) {
		got := FromContext(c)
		if got == nil || got.Name() != "openai" {
			t.Errorf("FromContext returned %+v", got)
		}
		c.Status(http.StatusOK)
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/x", nil)
	engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

// TestFromContextOr_FallsBack covers the convenience helper used by
// shared middleware that may run before any group has attached a
// responder.
func TestFromContextOr_FallsBack(t *testing.T) {
	fallback := &stubResponder{name: "fallback"}
	c, _ := newCtx("/")
	if got := FromContextOr(c, fallback).Name(); got != "fallback" {
		t.Errorf("FromContextOr returned %q, want fallback", got)
	}

	inCtx := &stubResponder{name: "attached"}
	c.Request = withResponder(c.Request, inCtx)
	if got := FromContextOr(c, fallback).Name(); got != "attached" {
		t.Errorf("FromContextOr returned %q, want attached", got)
	}
}

// TestGroupAwareRecovery_PanicPath simulates a panic in a handler and
// verifies (a) the recovery emits a 500 via the selected Responder,
// (b) the logger receives the panic value and a non-empty stack,
// (c) the chain is aborted.
func TestGroupAwareRecovery_PanicPath(t *testing.T) {
	r := &stubResponder{name: "openai"}
	d := NewPathDispatcher(r)
	var gotReqID, gotValue string
	var gotStack []byte
	logger := func(reqID string, panicValue any, stack []byte) {
		gotReqID = reqID
		if s, ok := panicValue.(string); ok {
			gotValue = s
		}
		gotStack = stack
	}
	engine := gin.New()
	engine.Use(GroupAwareRecovery(d, logger))
	engine.Use(func(c *gin.Context) { c.Set("request_id", "req-abc"); c.Next() })
	engine.GET("/v1/boom", func(c *gin.Context) { panic("kaboom") })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/boom", nil)
	engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if r.last != "Internal" {
		t.Errorf("Responder.Internal not called (last=%q)", r.last)
	}
	if gotReqID != "req-abc" {
		t.Errorf("logger reqID = %q", gotReqID)
	}
	if gotValue != "kaboom" {
		t.Errorf("logger value = %q, want kaboom", gotValue)
	}
	if len(gotStack) == 0 {
		t.Error("logger stack is empty")
	}
}

// Code below gin (proxies, the wire layer) holds only the request; the
// attached responder must reach it, including through derived requests.
func TestAttachResponder_ReachesRequestHolders(t *testing.T) {
	attached := &stubResponder{name: "attached"}
	c, _ := newCtx("/")
	AttachResponder(attached)(c)

	derived := c.Request.WithContext(context.WithValue(c.Request.Context(), testContextKey{}, 1))
	if got := FromRequest(derived); got != attached {
		t.Errorf("FromRequest(derived) = %v, want the attached responder", got)
	}
	if FromRequest(nil) != nil {
		t.Error("FromRequest(nil) must be nil")
	}
}
