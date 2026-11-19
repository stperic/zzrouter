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
)

// BuildUpstreamURL converts an already-composed target to its WebSocket scheme.
func BuildUpstreamURL(target string) (string, error) {
	u, err := url.Parse(target)
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
		return "", fmt.Errorf("realtime: target %q must use http/https/ws/wss scheme", target)
	}
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
