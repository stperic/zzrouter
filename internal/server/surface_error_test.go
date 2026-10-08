package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/stperic/zzrouter/pkg/httperr"
)

// One middleware, three surfaces, three envelopes.
//
// The idempotency store is mounted from three separate call sites: onto
// /v1/* (OpenAI) in routes_openai.go, and onto /zzrouter/v1/runs and
// /zzrouter/v1/model-groups (admin) in their controllers. It used to
// emit a hard-coded shape per branch — problem+json on the 422, the
// OpenAI envelope on the 504 — so each branch was unreadable on some
// surface it actually served. An OpenAI SDK hitting the 422 got a body
// with no error.type to classify on.
//
// These pin that the shape follows the route group's dialect, not the
// branch. routedGinContext attaches the responder the group would.

func routedGinContext(method, path string) (*gin.Context, *httptest.ResponseRecorder) {
	c, rec := testGinContext(method, path)
	c.Request = routedRequest(c.Request)
	return c, rec
}

func decodeSurfaceBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v (raw=%s)", err, rec.Body.String())
	}
	return body
}

func TestRespondSurfaceError_OpenAISurface(t *testing.T) {
	c, rec := routedGinContext(http.MethodPost, "/v1/chat/completions")
	respondSurfaceError(c, httperr.Error{Status: http.StatusUnprocessableEntity, Type: "invalid_request_error", Code: "some_code", Message: "boom"})

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d", rec.Code)
	}
	body := decodeSurfaceBody(t, rec)
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("/v1/* must nest the OpenAI error object, got %v", body)
	}
	// type is what the OpenAI SDKs branch on; without it the body is
	// not classifiable no matter what else it carries.
	if errObj["type"] != "invalid_request_error" {
		t.Errorf("error.type = %v, want invalid_request_error", errObj["type"])
	}
	if errObj["code"] != "some_code" {
		t.Errorf("error.code = %v", errObj["code"])
	}
	if errObj["message"] != "boom" {
		t.Errorf("error.message = %v", errObj["message"])
	}
	if _, leaked := body["type"]; leaked {
		t.Error("problem+json fields must not appear on the OpenAI surface")
	}
}

func TestRespondSurfaceError_AdminSurface(t *testing.T) {
	c, rec := routedGinContext(http.MethodPost, "/zzrouter/v1/runs")
	respondSurfaceError(c, httperr.Error{Status: http.StatusUnprocessableEntity, Type: "invalid_request_error", Code: "some_code", Message: "boom"})

	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
	body := decodeSurfaceBody(t, rec)
	if body["code"] != "some_code" {
		t.Errorf("problem code = %v", body["code"])
	}
	if body["detail"] != "boom" {
		t.Errorf("problem detail = %v", body["detail"])
	}
	if body["status"] != float64(http.StatusUnprocessableEntity) {
		t.Errorf("problem status = %v", body["status"])
	}
	if _, nested := body["error"]; nested {
		t.Error("the admin surface must not emit a nested OpenAI error object")
	}
}

func TestRespondSurfaceError_OllamaSurface(t *testing.T) {
	c, rec := routedGinContext(http.MethodPost, "/api/chat")
	respondSurfaceError(c, httperr.Error{Status: http.StatusUnprocessableEntity, Type: "invalid_request_error", Code: "some_code", Message: "boom"})

	body := decodeSurfaceBody(t, rec)
	// Ollama's shape is a flat string; a nested object breaks its client.
	msg, ok := body["error"].(string)
	if !ok {
		t.Fatalf("/api/* must emit a flat error string, got %v", body["error"])
	}
	if msg != "boom" {
		t.Errorf("error = %q", msg)
	}
}

// The admin surface is the default rather than a listed case, so a path
// on no known surface still gets a structured body instead of falling
// through to something unhandled.
func TestRespondSurfaceError_UnknownSurfaceFallsBackToProblem(t *testing.T) {
	c, rec := routedGinContext(http.MethodPost, "/some/unrouted/path")
	respondSurfaceError(c, httperr.Error{Status: http.StatusInternalServerError, Type: "api_error", Code: "x", Message: "boom"})

	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want problem+json fallback", ct)
	}
}

// The idempotency conflict is the branch that was hard-coded to
// problem+json while being mounted on /v1/*. Drive the real middleware
// on both surfaces and check each gets its own shape.
func TestIdempotencyConflict_ShapeFollowsSurface(t *testing.T) {
	cases := []struct {
		surface string
		path    string
		check   func(t *testing.T, body map[string]any)
	}{
		{
			surface: "openai", path: "/v1/chat/completions",
			check: func(t *testing.T, body map[string]any) {
				errObj, ok := body["error"].(map[string]any)
				if !ok {
					t.Fatalf("want nested OpenAI error, got %v", body)
				}
				if errObj["code"] != "idempotency_key_conflict" {
					t.Errorf("error.code = %v", errObj["code"])
				}
			},
		},
		{
			surface: "admin", path: "/zzrouter/v1/runs",
			check: func(t *testing.T, body map[string]any) {
				if body["code"] != "idempotency_key_conflict" {
					t.Errorf("problem code = %v", body["code"])
				}
				if _, nested := body["error"]; nested {
					t.Error("admin surface must not nest an OpenAI error object")
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.surface, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			store := newIdempotencyStore()
			router := gin.New()
			// As mounted for real: the group's responder, then the store.
			router.Use(httperr.AttachResponder(newResponderSet().dispatcher.For(tc.path)), store.middleware())
			router.POST(tc.path, func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })

			first := httptest.NewRecorder()
			router.ServeHTTP(first, requestWithKey(tc.path, `{"a":1}`))
			if first.Code != http.StatusOK {
				t.Fatalf("first request should succeed, got %d (%s)", first.Code, first.Body.String())
			}

			// Same key, different body — the conflict branch.
			second := httptest.NewRecorder()
			router.ServeHTTP(second, requestWithKey(tc.path, `{"a":2}`))
			if second.Code != http.StatusUnprocessableEntity {
				t.Fatalf("want 422 conflict, got %d (%s)", second.Code, second.Body.String())
			}
			tc.check(t, decodeSurfaceBody(t, second))
		})
	}
}

func TestRespondSurfaceError_AnthropicSurface(t *testing.T) {
	c, rec := routedGinContext(http.MethodPost, "/v1/messages")
	respondSurfaceError(c, httperr.Error{Status: http.StatusUnprocessableEntity, Type: "invalid_request_error", Code: "some_code", Message: "boom"})

	body := decodeSurfaceBody(t, rec)
	if body["type"] != "error" {
		t.Fatalf("/v1/messages must emit the Anthropic envelope, got %v", body)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj["message"] != "boom" || errObj["type"] != "invalid_request_error" {
		t.Errorf("error = %v", errObj)
	}
}

func requestWithKey(path, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "same-key")
	return req
}

// Title identifies the problem TYPE and should survive to the wire; an
// earlier refactor of mine flattened it to the generic status text,
// which a pre-existing test caught. Pin it so it stays specific.
func TestRespondSurfaceError_KeepsSpecificTitle(t *testing.T) {
	c, rec := routedGinContext(http.MethodPost, "/zzrouter/v1/runs")
	respondSurfaceError(c, httperr.Error{
		Status: http.StatusUnprocessableEntity, Title: "Idempotency Key Conflict",
		Type: "invalid_request_error", Code: "idempotency_key_conflict", Message: "boom",
	})
	body := decodeSurfaceBody(t, rec)
	if body["title"] != "Idempotency Key Conflict" {
		t.Errorf("title = %v, want the specific problem type", body["title"])
	}
}

func TestRespondSurfaceError_TitleDefaultsToStatusText(t *testing.T) {
	c, rec := routedGinContext(http.MethodPost, "/zzrouter/v1/runs")
	respondSurfaceError(c, httperr.Error{Status: http.StatusConflict, Message: "boom"})
	body := decodeSurfaceBody(t, rec)
	if body["title"] != "Conflict" {
		t.Errorf("title = %v, want the status text fallback", body["title"])
	}
}
