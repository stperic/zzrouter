package spend

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/stperic/zzrouter/pkg/observability/proxy"
)

// TestTotalRequestsMiddleware_NoPanic exercises the middleware on
// success / failure / non-LLM routes without crashing. End-to-end
// metric emission lives in
// internal/server/observability_integration_test.go where a real
// /metrics endpoint scrape is available.
func TestTotalRequestsMiddleware_NoPanic(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(TotalRequestsMiddleware())
	r.POST("/v1/chat/completions", func(c *gin.Context) { c.Status(http.StatusOK) })
	r.POST("/v1/chat/completions/bad", func(c *gin.Context) { c.Status(http.StatusBadRequest) })
	r.GET("/health", func(c *gin.Context) { c.Status(http.StatusOK) })
	r.OPTIONS("/v1/chat/completions", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	r.GET("/metrics", func(c *gin.Context) { c.Status(http.StatusOK) })

	for _, method := range []struct{ method, path string }{
		{"POST", "/v1/chat/completions"},
		{"POST", "/v1/chat/completions/bad"},
		{"GET", "/health"},
		{"OPTIONS", "/v1/chat/completions"},
		{"GET", "/metrics"},
	} {
		req := httptest.NewRequest(method.method, method.path, nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
	}
}

// TestCallerLabelsFromContext_ReadsProxyKeys pins the read-back from
// the same gin keys setAccessContext populates. A drift here would
// silently produce empty-label total-requests series even when the
// caller is authenticated. Mirrors the contract pinned in
// pkg/observability/proxy's similar setup.
func TestCallerLabelsFromContext_ReadsProxyKeys(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set(proxy.CtxKeyAPIKeyAlias, "alice-prod")
	c.Set(proxy.CtxKeyHashedAPIKey, "abcd...1234")
	c.Set(proxy.CtxKeyTeamID, "team-engineering")
	c.Set(proxy.CtxKeyTeamAlias, "Engineering")
	c.Set(proxy.CtxKeyRequestModel, "fast-chat")

	got := callerLabelsFromContext(c)

	cases := []struct {
		name string
		got  string
		want string
	}{
		{"APIKeyAlias", got.APIKeyAlias, "alice-prod"},
		{"HashedAPIKey", got.HashedAPIKey, "abcd...1234"},
		{"Team", got.Team, "team-engineering"},
		{"TeamAlias", got.TeamAlias, "Engineering"},
		{"RequestedModel", got.RequestedModel, "fast-chat"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

// TestCallerLabelsFromContext_NilCtx confirms nil-safety on the empty
// path — early-init or malformed gin contexts must not panic.
func TestCallerLabelsFromContext_NilCtx(t *testing.T) {
	got := callerLabelsFromContext(nil)
	if got != (CallerLabels{}) {
		t.Errorf("nil ctx should return zero CallerLabels; got %+v", got)
	}
}
