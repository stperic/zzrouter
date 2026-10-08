// Backend error writers delegate status and body policy to pkg/httperr so
// direct proxies, replica fallback and cold streaming classify errors alike.
package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/utils"
)

// writeRewrittenError reads, rewrites, and serializes an upstream
// 4xx/5xx response to a ResponseWriter. Used by ProxyClient.ForwardToBackend
// and Server.proxyToRemoteNode (both write directly to the writer).
//
// Caller is expected to have already populated w.Header() with any
// provider-identifying or upstream allowlisted headers BEFORE calling.
// Content-Type and Content-Length are set here (and stale upstream
// Content-Length / Content-Encoding stripped) since the body is being
// replaced with the rewritten envelope.
//
// After return, the response is fully written — caller should `return`
// from the handler immediately.
func writeRewrittenError(w http.ResponseWriter, req *http.Request, resp *http.Response) {
	applyRewrittenError(req, resp)
	h := w.Header()
	h.Del("Content-Length")
	h.Del("Content-Encoding")
	h.Set("Content-Type", resp.Header.Get("Content-Type"))
	h.Set("Content-Length", resp.Header.Get("Content-Length"))
	if resp.StatusCode == http.StatusBadRequest {
		h.Del("Retry-After")
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// applyRewrittenError mutates a *http.Response in place with the
// surface-appropriate rewritten error envelope. Used by the
// httputil.ReverseProxy.ModifyResponse hook in proxyToInstance — that
// API mutates the response struct rather than writing to a separate
// writer.
//
// Closes the original resp.Body and replaces it with a bytes.Reader
// over the rewritten body. Stale upstream Content-Encoding is
// stripped; Content-Length + ContentLength are sized to the new body.
// The response consumer closes the replacement body.
func applyRewrittenError(req *http.Request, resp *http.Response) {
	httperr.NormalizeUpstreamResponse(resp, dialectOf(req))
}

// ollamaSurfaceErrorBody renders an upstream backend error in the
// flat Ollama wire shape ({"error":"<message>"}) used by /api/*.
// When the upstream already emitted that shape (the common case for
// Ollama daemon 4xx), it's forwarded byte-for-byte. Object-form
// envelopes ({"error":{"message":"..."}} — Ollama daemon 5xx,
// llama.cpp server, LM Studio) get unwrapped into the flat shape so
// agents never see a stringified inner JSON. Anything else (empty
// body, malformed JSON, non-string non-object error value) is
// sanitized into a fresh flat envelope.
func ollamaSurfaceErrorBody(status int, raw []byte) []byte {
	trimmed := bytes.TrimSpace(raw)

	if len(trimmed) > 0 {
		var probe map[string]json.RawMessage
		if json.Unmarshal(trimmed, &probe) == nil {
			if errRaw, ok := probe["error"]; ok && len(errRaw) > 0 {
				switch errRaw[0] {
				case '"':
					return raw
				case '{':
					var inner struct {
						Message string `json:"message"`
						Error   string `json:"error"`
					}
					if json.Unmarshal(errRaw, &inner) == nil {
						msg := inner.Message
						if msg == "" {
							msg = inner.Error
						}
						if msg != "" {
							body, _ := json.Marshal(map[string]string{"error": msg})
							return body
						}
					}
				}
			}
		}
	}

	msg := utils.SanitizeErrorMessage(string(trimmed))
	if msg == "" {
		msg = fmt.Sprintf("backend returned status %d", status)
	}
	body, _ := json.Marshal(map[string]string{"error": msg})
	return body
}
