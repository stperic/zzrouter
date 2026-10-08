// Package wire shapes the bytes that traverse zzRouter's proxy copy path:
// streaming detection, header propagation, SSE framing, JSON body
// transforms, stream copy with metrics, and commit of detached backend
// responses to the client. Errors zzRouter writes in its own voice are
// the request dialect's (httperr.Responder), not this package's.
//
// The package is gin-free. It depends on pkg/dispatch/normalizer (for the
// Registry type used by commit helpers) and pkg/observability/llm (for
// the InferenceRecorder metric sink).
package wire

import (
	"fmt"
	"maps"
	"net/http"
	"strings"
)

// StreamBufferSize matches io.Copy's internal buffer size (32 KiB). This
// ensures optimal performance for streaming without excessive memory use.
const StreamBufferSize = 32 * 1024

// IsStreaming returns true if the HTTP response indicates a streaming
// format. Checks for NDJSON (Ollama), SSE (OpenAI), or chunked transfer
// encoding.
func IsStreaming(resp *http.Response) bool {
	contentType := resp.Header.Get("Content-Type")
	return contentType == "application/x-ndjson" ||
		strings.Contains(contentType, "text/event-stream") ||
		resp.Header.Get("Transfer-Encoding") == "chunked"
}

// connectionHeaders is the RFC 9110 §7.6.1 set that describes one
// connection, not the message, so an intermediary never forwards it.
var connectionHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"TE",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

// StripConnectionHeaders removes the connection-specific headers from h
// in place, including any the Connection header itself lists. Use it on
// a request zzRouter forwards; the body travels as it arrived, so its
// framing headers stay.
func StripConnectionHeaders(h http.Header) {
	for _, name := range strings.Split(h.Get("Connection"), ",") {
		if name = strings.TrimSpace(name); name != "" {
			h.Del(name)
		}
	}
	for _, name := range connectionHeaders {
		h.Del(name)
	}
}

// StripHopByHop removes the connection-specific headers and the body
// framing (Content-Length, Content-Encoding) from h in place. Use it on
// upstream response headers copied into a writer that re-encodes the
// body: gin re-chunks only when no Transfer-Encoding is set, and the
// upstream's framing no longer describes the bytes the client gets.
func StripHopByHop(h http.Header) {
	StripConnectionHeaders(h)
	h.Del("Content-Length")
	h.Del("Content-Encoding")
}

// MergeUpstreamHeaders clones src, strips hop-by-hop + body framing from
// the clone, and merges the survivors into dst. The canonical primitive
// for "copy upstream response headers to my writer" — relays must never
// forward Content-Encoding/Content-Length verbatim when the body is
// being re-encoded (normalizer dispatch, error rewrite, SSE injection),
// and Connection-listed names are per-hop by RFC 7230. Cloning first
// preserves any pre-existing dst entries that happen to share a
// hop-by-hop name (rare but possible if earlier middleware set one).
func MergeUpstreamHeaders(dst, src http.Header) {
	cloned := src.Clone()
	StripHopByHop(cloned)
	maps.Copy(dst, cloned)
}

// SendSSEComment sends an SSE comment line. Comments are ignored by
// every conforming parser (the OpenAI SDKs included), so this keeps a
// connection and its intermediaries awake during a long silence without
// putting anything on the wire a client could mistake for a chat chunk.
func SendSSEComment(w http.ResponseWriter, text string) {
	_, _ = fmt.Fprintf(w, ": %s\n\n", strings.ReplaceAll(text, "\n", " "))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// SendSSEDone sends the stream termination marker.
func SendSSEDone(w http.ResponseWriter) {
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
