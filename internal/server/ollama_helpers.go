// package server provides HTTP handlers for the zzrouter host server.
// Ollama helpers - centralized Ollama endpoint resolution and utilities

package server

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/stperic/zzrouter/pkg/config/backend"
)

// errModelRegistryUnavailable means the registry itself failed to
// initialize at startup — an infrastructure failure, not an Ollama-enabled
// question. Distinguished from ollama.ErrNotConfigured so callers (and
// logs) can tell them apart.
var errModelRegistryUnavailable = fmt.Errorf("model registry unavailable")

// ollamaDaemon returns the configured Ollama, its endpoint and what it is
// sent, from the model registry's Ollama connector — the single source of
// truth for "where is Ollama and how is it reached". Server-level callers
// never read provider config directly; keeping the lookup confined to the
// connector avoids reintroducing duplicate `if name == "ollama"` branches
// outside the ollama-specific type.
//
// Returns errModelRegistryUnavailable if the registry failed to
// initialize, or ollama.ErrNotConfigured if the registry is healthy
// but Ollama is disabled.
func (s *Server) ollamaDaemon() (*backend.Resolved, error) {
	if s.model.Registry == nil {
		return nil, errModelRegistryUnavailable
	}
	return s.model.Registry.OllamaConnector().Daemon()
}

// writeOllamaResponse mirrors an upstream Ollama HTTP response back to the
// client: copy headers (with hop-by-hop + body framing stripped), set the
// status, and stream the body. The body-normalizer dispatch in
// proxyToOllamaProvider intentionally does NOT use this helper — only sites
// that pass through the upstream body verbatim (pullLocalProxy,
// pullClusterRoute, CreateBlob) share this path. Caller must defer the body
// close.
func writeOllamaResponse(c *gin.Context, resp *http.Response) {
	mergeUpstreamHeaders(c.Writer.Header(), resp.Header)
	c.Status(resp.StatusCode)
	if err := streamResponseWithFlush(c.Writer, resp.Body); err != nil {
		slog.Error("Stream error", "error", err)
	}
}

// proxyToOllamaProvider forwards a request to an Ollama daemon and streams
// the response back. Non-streaming responses go through the body-normalizer
// registry (like MLX's reasoning→content fix); no Ollama normalizer is
// registered today, so the hot path stays allocation-free.
func (s *Server) proxyToOllamaProvider(c *gin.Context, daemon *backend.Resolved, path string, body []byte) {
	targetURL := strings.TrimRight(daemon.Endpoint, "/") + path

	// Preserve the original HTTP method (e.g., DELETE for /api/delete)
	proxyReq, err := http.NewRequestWithContext(c.Request.Context(), c.Request.Method, targetURL, bytes.NewReader(body))
	if err != nil {
		OllamaError(c, http.StatusInternalServerError, "Failed to create proxy request")
		return
	}

	// Copy headers
	proxyReq.Header = upstreamHeaders(c.Request.Header, daemon.Upstream)

	// Use streaming client (no timeout) for streaming operations
	resp, err := s.httpStreamingClient.Do(proxyReq)
	if err != nil {
		OllamaError(c, http.StatusBadGateway, "upstream Ollama request failed")
		return
	}
	defer func() { _ = resp.Body.Close() }()

	mergeUpstreamHeaders(c.Writer.Header(), resp.Header)

	// A per-chunk stream normalizer for the NDJSON path would need
	// streamResponseWithFlush to thread a transform; no Ollama stream fix
	// needs it today.
	if !isStreamingResponse(resp) {
		if bodyNorm := s.inference.normalizers.Body("ollama"); bodyNorm != nil {
			bodyBytes, readErr := io.ReadAll(resp.Body)
			if readErr != nil {
				c.Status(resp.StatusCode)
				return
			}
			rewritten := bodyNorm(bodyBytes)
			if rewritten == nil {
				rewritten = bodyBytes
			}
			c.Writer.Header().Set("Content-Length", strconv.Itoa(len(rewritten)))
			c.Status(resp.StatusCode)
			_, _ = c.Writer.Write(rewritten)
			return
		}
	}

	c.Status(resp.StatusCode)
	if err := streamResponseWithFlush(c.Writer, resp.Body); err != nil {
		slog.Error("Stream error", "error", err)
	}
}
