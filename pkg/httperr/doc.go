// Package httperr defines a dialect-neutral abstraction for emitting HTTP
// error responses.
//
// # Problem
//
// zzRouter exposes three HTTP surfaces that each expect a different error
// envelope shape:
//
//   - OpenAI compatibility routes (/v1/*) — the OpenAI envelope
//     `{"error":{"message","type","code","param"}}` consumed by the
//     official OpenAI SDKs for error classification.
//   - Ollama compatibility routes (/api/*) — Ollama's flat shape
//     `{"error":"..."}`.
//   - Management routes (/zzrouter/v1/*) — RFC 9457 Problem Details
//     `application/problem+json` for operator tooling.
//
// Handlers that live inside a single route group know their dialect and
// can call the right helper directly. Cross-cutting handlers cannot:
//
//   - gin.Recovery() runs before any group and catches panics from every
//     group indiscriminately.
//   - engine.NoRoute() / engine.NoMethod() serve 404/405 for paths that
//     never matched a group, so there is no per-group middleware in play.
//   - Group-attached middleware (auth, rate limit) runs before the route
//     handler and must emit an error in the group's dialect even though
//     the middleware itself is shared.
//
// Without an abstraction, each of these paths hard-codes one dialect and
// breaks the contract for the other surfaces. Today a wrong-bearer probe
// against /v1/chat/completions returns RFC 7807, which the OpenAI SDK
// cannot parse into AuthenticationError.
//
// # Design
//
// The package exposes a Responder interface whose methods are the
// semantic error cases (BadRequest, Unauthorized, NotFound, Internal,
// ...). Concrete implementations live beside the code that owns the
// dialect: the OpenAI responder is pkg/protocol/openai.Responder, the
// Problem Details and Ollama responders live in internal/server.
//
// Responders are wired into the server in two complementary ways:
//
//  1. Per-group: AttachResponder(r) is installed as middleware on each
//     route group. Handlers and group middleware read the active
//     responder via FromContext and emit errors in the correct dialect
//     without having to know what group they are in.
//
//  2. Cross-cutting: a PathDispatcher holds a set of path-prefix →
//     Responder rules. NoRoute, NoMethod, and GroupAwareRecovery use
//     the dispatcher to pick a dialect based on the request path when
//     no group middleware has run.
//
// Both mechanisms share the same Responder instances so every dialect
// is implemented exactly once.
//
// # Stability
//
// The Responder interface is intentionally narrow — it lists the error
// cases that every dialect must support. New methods SHOULD be added
// sparingly and only when a new category of error has no reasonable
// mapping to an existing method. Adding a method is a breaking change
// for external implementations; prefer extending an existing method's
// semantics (e.g. adding a new code via a typed dialect constant)
// where possible.
package httperr
