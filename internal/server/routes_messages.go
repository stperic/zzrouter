// Anthropic Messages surface: POST /v1/messages and its token counter,
// routed by the body's model to providers whose wire_endpoints declare
// "messages". Request and reply bytes are the provider's; zzRouter
// admits, meters and routes, and speaks the Anthropic dialect only when
// it has to say something itself.
package server

import (
	"encoding/json"

	"github.com/gin-gonic/gin"

	"github.com/stperic/zzrouter/pkg/httperr"
)

// messagesEndpoint is the wire_endpoints value of a provider that serves
// the Messages API natively; messagesCompatEndpoint is its translation.
const messagesEndpoint = "messages"

func (s *Server) registerMessagesRoutes() {
	messages := s.engine.Group("/v1/messages",
		httperr.AttachResponder(s.responders.anthropic),
		RequestIDMiddleware(),
		s.auth.OptionalAuthMiddleware(),
	)
	messages.POST("", s.handleMessages)
	messages.POST("/count_tokens", s.handleCountTokens)
}

// messagesRequest is the part of a Messages body zzRouter reads; the
// rest is the provider's.
type messagesRequest struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

// handleMessages serves POST /v1/messages. The recorder it stashes gets
// the caller's key and team in enforceAndAttribute, so tokens and spend
// settle like any chat completion.
func (s *Server) handleMessages(c *gin.Context) {
	req, body, ok := s.readMessagesRequest(c)
	if !ok {
		return
	}
	stashInferenceContext(c, req.Model, body)
	recorder := newChatRecorder(c, req.Model, body, req.Stream)
	s.routeMessages(c, req.Model, body, messagesAsChat, func(model string) (func(), bool) {
		return s.enforceAndAttribute(c, model, recorder)
	})
}

// handleCountTokens serves POST /v1/messages/count_tokens. Counting spends
// no tokens, so the caller is authorized but not metered: no RPM tick, no
// budget hold, and no inference-log row on this node or the one it is
// proxied to, which runs this handler too.
func (s *Server) handleCountTokens(c *gin.Context) {
	req, body, ok := s.readMessagesRequest(c)
	if !ok {
		return
	}
	stashInferenceContext(c, req.Model, body)
	markNotInference(c)
	s.routeMessages(c, req.Model, body, countTokensAsChat, func(model string) (func(), bool) {
		return func() {}, s.access.Authorize(c, model)
	})
}

// routeMessages admits the request, resolves the model to the targets
// that serve the Messages API, and dispatches it: as is to an engine that
// speaks it, translated as tr says to one that speaks only Chat
// Completions. A native body is never prepared the way Chat's is: an
// Anthropic body has no stream_options, and the engine reports usage
// unasked.
func (s *Server) routeMessages(c *gin.Context, model string, body []byte, tr messagesTranslation, admit func(model string) (release func(), ok bool)) {
	model, body, nodeHint := s.stripNodeHint(c, model, body)
	release, ok := admit(model)
	defer release()
	if !ok {
		return
	}
	resolved, ok := s.resolveTarget(c, model, nodeHint)
	if !ok {
		return
	}
	if !s.admitImages(c, model, body, resolved) {
		return
	}
	mode, ok := s.narrowToWireEndpoint(c, model, resolved, messagesEndpoint, messagesCompatEndpoint)
	switch {
	case !ok:
	case mode == wireModeCompat:
		s.dispatchTranslated(c, resolved, model, body, tr)
	default:
		s.dispatchTo(c, resolved, model, body, "chat")
	}
}

// readMessagesRequest reads the body and refuses, in the Anthropic
// dialect, one without a model.
func (s *Server) readMessagesRequest(c *gin.Context) (messagesRequest, []byte, bool) {
	var req messagesRequest
	body, ok := readRequestBody(c)
	if !ok {
		return req, nil, false
	}
	r := httperr.FromContext(c)
	if err := json.Unmarshal(body, &req); err != nil {
		r.BadRequest(c, "request body is not valid JSON", "")
		return req, nil, false
	}
	if req.Model == "" {
		r.BadRequest(c, "model: field required", "model")
		return req, nil, false
	}
	return req, body, true
}
