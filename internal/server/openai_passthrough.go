// OpenAI /v1/* pass-through routing to a configured default backend.
//
// Stateful OpenAI endpoints (Files, Batches, Fine-tuning, Assistants, Threads,
// Vector Stores, Uploads, Responses retrieval) do not carry a `model` field
// in the request body. There is no way to route them by the existing model
// resolver, and their resources are not synced across cluster nodes anyway —
// a file uploaded to backend A is not visible on backend B.
//
// zzRouter forwards all such requests to a single operator-configured backend
// (openai_compat.default_backend in node.yaml). This is a pure reverse proxy:
// method, path, headers, query string, and body are preserved. zzRouter does
// not store, inspect, or translate any of the resource state.
package server

import (
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/stperic/zzrouter/pkg/config/backend"
)

// DefaultMaxUploadBytes is the default request body cap for multipart uploads
// and stateful endpoints when openai_compat.max_upload_bytes is not set.
// 25 MiB matches OpenAI's own cap for audio inputs.
const DefaultMaxUploadBytes int64 = 25 * 1024 * 1024

// openAIMaxUploadBytes resolves the configured upload cap, falling back to
// DefaultMaxUploadBytes when unset or non-positive.
func (s *Server) openAIMaxUploadBytes() int64 {
	if n := s.config.OpenAICompat.MaxUploadBytes; n > 0 {
		return n
	}
	return DefaultMaxUploadBytes
}

// routeToDefaultOpenAIBackend forwards the current request to the provider
// configured as openai_compat.default_backend in node.yaml, preserving method,
// path, headers, query string, and body. Used for stateful /v1/* endpoints
// that have no model field to route by.
//
// Failure modes:
//   - 503 no_default_backend_configured — openai_compat.default_backend is empty.
//   - 503 default_backend_unavailable   — configured provider is not enabled or
//     has no running endpoint URL.
//   - 413 request_too_large             — body exceeds openai_compat.max_upload_bytes.
//
// On success, proxyToBackend streams the backend response verbatim (including
// its status code, headers, and body) back to the client. Errors from the
// backend are NOT translated to OpenAI error envelopes — the backend's own
// error response propagates unchanged. That translation is Phase 10 scope.
func (s *Server) routeToDefaultOpenAIBackend(c *gin.Context) {
	// Buffer the body opaquely. proxyToBackend expects the payload as a
	// byte slice so it can construct a rewindable bytes.Reader; this also
	// lets us enforce the configured upload cap before contacting the
	// backend. GET/DELETE/HEAD requests typically have an empty body —
	// ReadAll on an empty Body returns nil without error.
	body, bodyOK := s.readPassthroughBody(c)
	if !bodyOK {
		return
	}
	s.forwardBufferedToDefaultBackend(c, body)
}

// forwardBufferedToDefaultBackend forwards an already-buffered body to the
// default backend. Used by callers (e.g. multipart handlers) that have already
// read the request body to extract routing fields and want to avoid a
// duplicate read.
func (s *Server) forwardBufferedToDefaultBackend(c *gin.Context, body []byte) {
	backendKey := s.config.OpenAICompat.DefaultBackend
	if backendKey == "" {
		// Contains "no default backend" so the responder's detail
		// pattern match picks ErrorCodeNoDefaultBackend rather than
		// the generic feature_disabled code.
		s.responders.openai.Unavailable(c,
			"no default backend configured: set openai_compat.default_backend in node.yaml to a provider key that implements this endpoint")
		return
	}

	resolved, ok := s.backend.Resolve(backendKey)
	if !ok {
		s.responders.openai.Unavailable(c,
			fmt.Sprintf("configured default backend %q is not available; check that the provider is enabled and exposes a running endpoint", backendKey))
		return
	}

	target := backend.PassthroughTarget(resolved.Endpoint, c.Request.URL.Path, c.Request.URL.RawQuery)
	s.proxy.ForwardToBackend(c.Writer, c.Request, backendKey, target, resolved.Upstream, body)
}

// readPassthroughBody buffers the request body up to the configured upload
// cap and writes an OpenAI-shape error response on failure. Returns ok=false
// when the caller should stop processing.
func (s *Server) readPassthroughBody(c *gin.Context) (body []byte, ok bool) {
	if c.Request.Body == nil {
		return nil, true
	}
	maxBytes := s.openAIMaxUploadBytes()
	limited := http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes)
	buf, err := io.ReadAll(limited)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			s.responders.openai.RequestTooLarge(c,
				fmt.Sprintf("request body exceeds maximum upload size of %d bytes", maxBytes))
			return nil, false
		}
		s.responders.openai.Internal(c, "failed to read request body: "+err.Error())
		return nil, false
	}
	return buf, true
}
