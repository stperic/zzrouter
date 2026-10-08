// Package openai implements the OpenAI REST API error envelope,
// closed-vocabulary error codes, and an httperr.Responder that emits
// responses indistinguishable from https://api.openai.com/v1/.
//
// The package is the single source of truth for OpenAI wire
// compatibility in zzRouter. Everything that makes a /v1/* response
// look like OpenAI — the JSON envelope shape, the allowed error.type
// values, the allowed error.code values, the sanitization policy for
// detail strings, the request-id propagation rule — lives here.
//
// # Scope
//
// The package owns:
//
//   - The Envelope JSON type (envelope.go).
//   - Typed ErrorType and ErrorCode closed-vocabulary constants
//     (vocab.go), derived from the OpenAI API reference at
//     https://platform.openai.com/docs/guides/error-codes.
//   - A Responder implementing pkg/httperr.Responder, pinned to a
//     specific SpecVersion (version.go) and usable as the /v1/*
//     dialect (responder.go).
//   - The sanitization wrapper that adds OpenAI-specific scrubbing on
//     top of pkg/utils.SanitizeErrorMessage (sanitize.go).
//
// The package does NOT own:
//
//   - Routing of /v1/* requests to backends.
//   - The success-path payload shapes (chat completions, embeddings,
//     models, ...). Those live beside their handlers.
//   - Backend error normalization — once a backend has already
//     produced an OpenAI-ish body, internal/server/backend_error_normalize.go
//     rewrites it in place; this package never inspects backend bodies.
//
// # Versioning
//
// OpenAI does not version its REST API via URL paths. The envelope
// shape, the error.type closed vocabulary, and the error.code set are
// however subject to additive change as OpenAI ships new endpoints
// and retry semantics. The SpecVersion constant in version.go
// identifies the snapshot of the OpenAI API reference that the
// package currently targets. See version.go for the upgrade protocol.
//
// # Usage
//
//	r := openai.New()
//	group := engine.Group("/v1",
//	    httperr.AttachResponder(r),
//	    server.OptionalAuthMiddleware(),
//	)
//
// Once attached to a route group, downstream handlers and middleware
// emit errors via httperr.FromContext(c).{BadRequest,Unauthorized,...}
// and get an OpenAI-shaped response for free.
//
// # Security posture
//
// The Responder sanitizes every caller-provided detail string before
// emitting it. Internal 500s and 502s emit fixed generic messages
// regardless of what the caller passed — the caller-supplied detail
// is retained on the API only so it lands in server-side logs for
// correlation. See responder.go for the per-method rules.
//
// An OPAQUE mode (WithOpaqueErrors) collapses every error response to
// its HTTP status code with no body. It is intended for production
// deployments where even sanitized error messages are considered
// leakage — matching LocalAI's OPAQUE_ERRORS behavior.
package openai
