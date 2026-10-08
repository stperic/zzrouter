package server

// This file is the single place where the three dialect responders
// are instantiated and wired into the server. It exists so the rest
// of the server code never needs to import multiple responder
// implementations — it just holds *httperr.PathDispatcher.
//
// Wiring overview:
//
//	                ┌─ /v1/*              → openaiproto.Responder
//	                ├─ /v1/messages*      → anthropicproto.Responder
//	PathDispatcher ─┼─ /api/*             → ollamaResponder
//	                ├─ /zzrouter/v1/*     → problemResponder (same
//	                │                       instance as the fallback,
//	                │                       so the drift check cannot
//	                │                       ever show two Problem
//	                │                       Details dialects)
//	                └─ fallback: problemResponder
//
// Per-group AttachResponder middleware pushes the right instance onto
// each request's gin.Context. The dispatcher is used by the three
// non-group handlers (gin NoRoute, gin NoMethod, GroupAwareRecovery)
// that run outside any group.

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/stperic/zzrouter/pkg/httperr"
	anthropicproto "github.com/stperic/zzrouter/pkg/protocol/anthropic"
	openaiproto "github.com/stperic/zzrouter/pkg/protocol/openai"
)

// responderSet bundles the concrete dialect implementations the
// server uses. All fields are non-nil after newResponderSet returns.
// Instances are safe for concurrent reuse across all requests.
type responderSet struct {
	openai     *openaiproto.Responder
	anthropic  *anthropicproto.Responder
	problem    *problemResponder
	ollama     *ollamaResponder
	dispatcher *httperr.PathDispatcher
}

// newResponderSet constructs the responders and a PathDispatcher
// wired with the correct path-prefix rules. The dispatcher's
// fallback is the Problem Details responder — any path that does
// not match a more specific dialect falls through to RFC 7807,
// matching the pre-refactor behavior for unknown paths.
//
// The OpenAI responder is configured with an internal logger that
// forwards the raw detail to slog at error level, keyed by request id.
// This is the ONLY place those details are written — clients see
// only the sanitized, generic messages the OpenAI spec allows.
func newResponderSet() *responderSet {
	openai := newOpenAIResponder()
	anthropic := anthropicproto.New(func(reqID, detail string) {
		slog.Error("anthropic internal error",
			"request_id", reqID,
			"detail", detail,
		)
	})
	problem := newProblemResponder()
	ollama := newOllamaResponder()

	d := httperr.NewPathDispatcher(problem)
	d.Register("/v1/", openai)
	d.Register("/v1/messages", anthropic)
	d.Register("/api/", ollama)
	d.Register("/zzrouter/v1/", problem)

	return &responderSet{
		openai:     openai,
		anthropic:  anthropic,
		problem:    problem,
		ollama:     ollama,
		dispatcher: d,
	}
}

// intSeconds renders a positive int as its decimal string form.
// Shared between the three dialect responders for Retry-After
// formatting. Kept in this file because it is the only spot where a
// small numeric helper is used by more than one responder_*.go file.
func intSeconds(n int) string { return strconv.Itoa(n) }

func newOpenAIResponder() *openaiproto.Responder {
	return openaiproto.New(
		openaiproto.WithInternalLogger(func(reqID, detail string) {
			slog.Error("openai internal error",
				"request_id", reqID,
				"detail", detail,
			)
		}),
	)
}

// defaultCompatResponder answers for a request that never passed through
// an engine -- code called directly, as tests do. Every routed request
// carries a responder (httperr.AttachByPath).
var defaultCompatResponder httperr.Responder = newOpenAIResponder()

// dialectOf returns the responder for code that holds only the request.
func dialectOf(req *http.Request) httperr.Responder {
	return httperr.FromRequestOr(req, defaultCompatResponder)
}

// writeError writes one zzRouter-originated error in req's dialect.
func writeError(w http.ResponseWriter, req *http.Request, e httperr.Error) {
	dialectOf(req).WriteError(w, req, e)
}

// writeJSONBody writes body as the whole response. Shared by the
// responders' writer-level WriteError, which have no gin context.
func writeJSONBody(w http.ResponseWriter, status int, contentType string, body any) {
	data, _ := json.Marshal(body)
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	_, _ = w.Write(data)
}
