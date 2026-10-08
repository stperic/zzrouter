package anthropic

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/stperic/zzrouter/pkg/httperr"
)

// InternalLogger receives the detail behind a 5xx the client only sees
// generically.
type InternalLogger func(reqID, detail string)

// Responder speaks the Anthropic error dialect.
type Responder struct {
	logger InternalLogger
}

// New returns a Responder; logger may be nil.
func New(logger InternalLogger) *Responder { return &Responder{logger: logger} }

var _ httperr.Responder = (*Responder)(nil)

func (*Responder) Name() string { return "anthropic" }

func (r *Responder) write(c *gin.Context, status int, message string) {
	if reqID := c.GetString("request_id"); reqID != "" {
		c.Header("X-Request-Id", reqID)
	}
	c.AbortWithStatusJSON(status, envelopeWithRequestID(status, message, c.GetString("request_id")))
}

func (r *Responder) BadRequest(c *gin.Context, detail, _ string) {
	r.write(c, http.StatusBadRequest, detail)
}

func (r *Responder) Unauthorized(c *gin.Context, detail string) {
	httperr.SetAuthenticateChallenge(c)
	r.write(c, http.StatusUnauthorized, detail)
}

func (r *Responder) Forbidden(c *gin.Context, detail string) {
	r.write(c, http.StatusForbidden, detail)
}

func (r *Responder) NotFound(c *gin.Context, detail string) {
	r.write(c, http.StatusNotFound, detail)
}

func (r *Responder) MethodNotAllowed(c *gin.Context, detail string) {
	r.write(c, http.StatusMethodNotAllowed, detail)
}

func (r *Responder) RequestTooLarge(c *gin.Context, detail string) {
	r.write(c, http.StatusRequestEntityTooLarge, detail)
}

func (r *Responder) TooManyRequests(c *gin.Context, detail string, retryAfterSeconds int) {
	if retryAfterSeconds > 0 {
		c.Header("Retry-After", strconv.Itoa(retryAfterSeconds))
	}
	r.write(c, http.StatusTooManyRequests, detail)
}

func (r *Responder) Internal(c *gin.Context, detail string) {
	r.logInternal(c, detail)
	r.write(c, http.StatusInternalServerError, "internal server error")
}

func (r *Responder) BadGateway(c *gin.Context, detail string) {
	r.logInternal(c, detail)
	r.write(c, http.StatusBadGateway, "upstream error")
}

func (r *Responder) Unavailable(c *gin.Context, detail string) {
	r.write(c, http.StatusServiceUnavailable, detail)
}

func (r *Responder) GatewayTimeout(c *gin.Context, detail string) {
	r.logInternal(c, detail)
	r.write(c, http.StatusGatewayTimeout, "upstream timeout")
}

func (r *Responder) logInternal(c *gin.Context, detail string) {
	if r.logger != nil && detail != "" {
		r.logger(c.GetString("request_id"), detail)
	}
}

// WriteError renders the Anthropic envelope beside any e.Extra members.
// e.Type is zzRouter's OpenAI vocabulary; the Anthropic type follows the
// status instead.
//
// It does not stream failures in band (httperr.InBandStreamer): the
// Anthropic SDKs retry a 429 or 529 on their own, which a failure tucked
// inside a 200 stream would defeat.
func (*Responder) WriteError(w http.ResponseWriter, _ *http.Request, e httperr.Error) {
	body := map[string]any{}
	for k, v := range e.Extra {
		body[k] = v
	}
	for k, v := range envelopeWithRequestID(e.Status, e.Message, w.Header().Get("X-Request-ID")) {
		body[k] = v
	}
	data, _ := json.Marshal(body)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.Status)
	_, _ = w.Write(data)
}

func (*Responder) NormalizeUpstreamError(status int, raw []byte) []byte {
	return NormalizeError(status, raw)
}

// envelopeWithRequestID adds the top-level request_id the Anthropic API
// carries, when there is one to give.
func envelopeWithRequestID(status int, message, reqID string) map[string]any {
	env := Envelope(status, message)
	if reqID != "" {
		env["request_id"] = reqID
	}
	return env
}
