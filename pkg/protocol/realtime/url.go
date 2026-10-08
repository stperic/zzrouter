// Package realtime holds helpers for the OpenAI Realtime WebSocket
// proxy that are not coupled to the HTTP server struct. The route
// upgrade handler still lives in internal/server because it needs
// access to backend resolution and the responder set, but the URL
// rewriting and header scrubbing logic is pure and lives here.
package realtime

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// BuildUpstreamURL converts the provider runtime endpoint into the
// ws:// or wss:// URL the WebSocket dialer expects, and appends the
// original request path plus query string so the upstream sees the
// exact route the client asked for.
//
// Contract:
//
// The endpoint path, if any, is preserved as a prefix and the request
// path is appended verbatim. For the common case where the provider
// is configured with just the host — https://api.openai.com — a
// request to /v1/realtime yields wss://api.openai.com/v1/realtime.
// When the provider endpoint already ends in a path segment — e.g.
// https://api.openai.com/v1 — the two /v1 prefixes compose and the
// upstream sees wss://api.openai.com/v1/v1/realtime, which is almost
// certainly not what the operator intended. Treat the endpoint as a
// host-only base and let the route path do the mounting. An
// alternative convention (truncate the overlap) was considered and
// rejected because it would silently rewrite operator-authored
// paths, making misconfiguration harder to diagnose.
func BuildUpstreamURL(endpoint string, req *http.Request) (string, error) {
	u, err := url.Parse(strings.TrimRight(endpoint, "/"))
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	case "ws", "wss":
		// already a WebSocket URL
	default:
		return "", fmt.Errorf("realtime: endpoint %q must use http/https/ws/wss scheme", endpoint)
	}
	u.Path = strings.TrimRight(u.Path, "/") + req.URL.Path
	u.RawQuery = req.URL.RawQuery
	return u.String(), nil
}

// StripHandshakeHeaders removes the Sec-WebSocket-* headers from h in
// place. The dialer negotiates the upstream upgrade itself and refuses a
// header set that already carries them.
func StripHandshakeHeaders(h http.Header) {
	h.Del("Sec-WebSocket-Key")
	h.Del("Sec-WebSocket-Version")
	h.Del("Sec-WebSocket-Extensions")
	h.Del("Sec-WebSocket-Protocol")
}
