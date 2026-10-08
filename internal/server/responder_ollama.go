package server

// ollamaResponder adapts the Ollama flat-shape error helpers to the
// pkg/httperr.Responder interface.
//
// Ollama uses a deliberately minimal error envelope: a top-level JSON
// object with a single `error` string field
// (`{"error": "model not found"}`). The Ollama CLI and Open WebUI
// both parse only that field — anything else is ignored. This
// responder therefore does not expose a code or type vocabulary.

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/stperic/zzrouter/pkg/httperr"
)

// ollamaResponder implements httperr.Responder for the /api/*
// compatibility surface.
type ollamaResponder struct{}

func newOllamaResponder() *ollamaResponder { return &ollamaResponder{} }

var _ httperr.Responder = (*ollamaResponder)(nil)

func (*ollamaResponder) Name() string { return "ollama" }

func (*ollamaResponder) BadRequest(c *gin.Context, detail, _ string) {
	OllamaError(c, http.StatusBadRequest, detail)
}

func (*ollamaResponder) Unauthorized(c *gin.Context, detail string) {
	httperr.SetAuthenticateChallenge(c)
	OllamaError(c, http.StatusUnauthorized, detail)
}

func (*ollamaResponder) Forbidden(c *gin.Context, detail string) {
	OllamaError(c, http.StatusForbidden, detail)
}

func (*ollamaResponder) NotFound(c *gin.Context, detail string) {
	OllamaError(c, http.StatusNotFound, detail)
}

func (*ollamaResponder) MethodNotAllowed(c *gin.Context, detail string) {
	OllamaError(c, http.StatusMethodNotAllowed, detail)
}

func (*ollamaResponder) RequestTooLarge(c *gin.Context, detail string) {
	OllamaError(c, http.StatusRequestEntityTooLarge, detail)
}

func (*ollamaResponder) TooManyRequests(c *gin.Context, detail string, retryAfterSeconds int) {
	if retryAfterSeconds > 0 {
		c.Header("Retry-After", intSeconds(retryAfterSeconds))
	}
	OllamaError(c, http.StatusTooManyRequests, detail)
}

func (*ollamaResponder) Internal(c *gin.Context, _ string) {
	// Ollama surface follows the same "never echo internal detail"
	// rule as OpenAI — emit a fixed generic message regardless of
	// what the caller passed. The detail is assumed to have already
	// been logged by the caller (typically GroupAwareRecovery's
	// logger) before this method ran.
	OllamaError(c, http.StatusInternalServerError, "internal server error")
}

func (*ollamaResponder) BadGateway(c *gin.Context, _ string) {
	OllamaError(c, http.StatusBadGateway, "upstream error")
}

func (*ollamaResponder) Unavailable(c *gin.Context, detail string) {
	OllamaError(c, http.StatusServiceUnavailable, detail)
}

func (*ollamaResponder) GatewayTimeout(c *gin.Context, _ string) {
	OllamaError(c, http.StatusGatewayTimeout, "upstream timeout")
}

// WriteError renders the flat shape; Type and Code have no field in it.
func (*ollamaResponder) WriteError(w http.ResponseWriter, _ *http.Request, e httperr.Error) {
	writeJSONBody(w, e.Status, "application/json", flatError(e))
}

func flatError(e httperr.Error) map[string]any {
	body := map[string]any{}
	for k, v := range e.Extra {
		body[k] = v
	}
	body["error"] = e.Message
	return body
}

func (*ollamaResponder) NormalizeUpstreamError(status int, raw []byte) []byte {
	return ollamaSurfaceErrorBody(status, raw)
}
