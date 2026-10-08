package server

// problemResponder adapts the RFC 9457 Problem Details helpers in
// problem.go to the pkg/httperr.Responder interface. It is the
// responder used by the management surface (/zzrouter/v1/*) and as
// the global fallback for any path that does not match a more
// specific dialect prefix.
//
// The goal here is NOT to reimplement Problem Details — every method
// delegates to the existing helpers so there is exactly one
// implementation of the RFC 7807 envelope. This file is pure glue
// that maps the neutral Responder method set onto the legacy helper
// signatures.

import (
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/openaicompat"
	"github.com/stperic/zzrouter/pkg/utils"
)

// problemResponder implements httperr.Responder by delegating to the
// existing problem.go helpers. It holds no state.
type problemResponder struct{}

// newProblemResponder returns a singleton-style Responder. Problem
// Details has no configuration knobs so all instances are
// interchangeable.
func newProblemResponder() *problemResponder { return &problemResponder{} }

// Compile-time interface check.
var _ httperr.Responder = (*problemResponder)(nil)

func (*problemResponder) Name() string { return "problem" }

func (*problemResponder) BadRequest(c *gin.Context, detail, _ string) {
	BadRequest(c, detail)
}

func (*problemResponder) Unauthorized(c *gin.Context, detail string) {
	Unauthorized(c, detail)
}

func (*problemResponder) Forbidden(c *gin.Context, detail string) {
	Forbidden(c, detail)
}

func (*problemResponder) NotFound(c *gin.Context, detail string) {
	NotFound(c, detail)
}

func (*problemResponder) MethodNotAllowed(c *gin.Context, detail string) {
	// The legacy helpers do not expose a dedicated 405; we inline a
	// Problem Details call with the right status and title so the
	// wire shape stays consistent.
	RespondWithProblem(c, 405, "Method Not Allowed", detail)
}

func (*problemResponder) RequestTooLarge(c *gin.Context, detail string) {
	RespondWithProblem(c, 413, "Request Entity Too Large", detail)
}

func (*problemResponder) TooManyRequests(c *gin.Context, detail string, retryAfterSeconds int) {
	if retryAfterSeconds > 0 {
		c.Header("Retry-After", intSeconds(retryAfterSeconds))
	}
	RespondWithProblem(c, 429, "Too Many Requests", detail)
}

func (*problemResponder) Internal(c *gin.Context, detail string) {
	InternalNodeError(c, detail)
}

func (*problemResponder) BadGateway(c *gin.Context, detail string) {
	BadGateway(c, detail)
}

func (*problemResponder) Unavailable(c *gin.Context, detail string) {
	ServiceUnavailable(c, detail)
}

func (*problemResponder) GatewayTimeout(c *gin.Context, detail string) {
	RespondWithProblem(c, 504, "Gateway Timeout", detail)
}

// WriteError renders e as problem+json, with e.Extra as extension
// members.
func (*problemResponder) WriteError(w http.ResponseWriter, req *http.Request, e httperr.Error) {
	title := e.Title
	if title == "" {
		title = http.StatusText(e.Status)
	}
	instance := ""
	if req != nil {
		instance = req.URL.Path
	}
	problem := utils.NewProblemDetails(e.Status, title, e.Message, instance)
	problem.RequestID = w.Header().Get("X-Request-ID")
	if e.Code != "" {
		problem.Code = e.Code
	}
	body := map[string]any{}
	for k, v := range e.Extra {
		body[k] = v
	}
	// The document's own members win over any Extra of the same name.
	raw, _ := json.Marshal(problem)
	_ = json.Unmarshal(raw, &body)
	writeJSONBody(w, e.Status, "application/problem+json", body)
}

// NormalizeUpstreamError keeps the backend's message, in a problem
// document typed by the status.
func (p *problemResponder) NormalizeUpstreamError(status int, raw []byte) []byte {
	var oai struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(openaicompat.NormalizeError(status, raw), &oai)
	out, _ := json.Marshal(utils.NewProblemDetails(status, http.StatusText(status), oai.Error.Message, ""))
	return out
}
