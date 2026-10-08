// WebSocket proxy for /v1/realtime plus the three HTTP session-token
// helpers (/v1/realtime/sessions, /v1/realtime/transcription_sessions,
// /v1/realtime/client_secrets). All four routes target the provider
// configured as openai_compat.realtime_backend in node.yaml; cloud-mode
// providers get upstream-credential injection via the standard
// resolveBackend path. /v1/realtime/calls (SIP/phone) is out of scope.
package server

import (
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"github.com/stperic/zzrouter/pkg/protocol/realtime"
)

// realtimeUpgrader is the shared upgrader for /v1/realtime.
//
// CheckOrigin returns true because the compat surface is anonymous by
// default and zzrouter is typically deployed behind an ingress that
// enforces origin policy upstream. If you expose /v1/realtime directly
// to untrusted browser clients, install an ingress-level origin
// allowlist or wrap this upgrader to check Origin against a configured
// set — a cross-origin WebSocket reaching a misconfigured realtime
// backend is equivalent to CSRF against the upstream provider's state.
var realtimeUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

// realtimeDialTimeout caps the upstream dial so a stuck backend cannot
// leave clients hanging indefinitely on upgrade.
const realtimeDialTimeout = 10 * time.Second

func (s *Server) registerRealtimeRoutes(openai *gin.RouterGroup) {
	openai.GET("/realtime", s.handleRealtimeUpgrade)
	openai.POST("/realtime", s.handleRealtimeUpgrade) // some clients POST-upgrade

	openai.POST("/realtime/sessions", s.handleRealtimeSession)
	openai.POST("/realtime/transcription_sessions", s.handleRealtimeSession)
	openai.POST("/realtime/client_secrets", s.handleRealtimeSession)
}

func (s *Server) handleRealtimeUpgrade(c *gin.Context) {
	backendKey := s.config.OpenAICompat.RealtimeBackend
	if backendKey == "" {
		s.responders.openai.Unavailable(c,
			"/v1/realtime requires a backend to be configured. Set openai_compat.realtime_backend in node.yaml to a provider key that implements the Realtime WebSocket protocol.")
		return
	}

	resolved, ok := s.backend.Resolve(backendKey)
	if !ok {
		s.responders.openai.Unavailable(c,
			"Configured realtime backend is not available. Check that the provider is enabled and exposes a running endpoint.")
		return
	}

	if !s.admitOpaqueForward(c, "/v1/realtime", resolved.Cloud) {
		return
	}
	release, ok := s.enforceAndAttribute(c, "realtime /v1/realtime", nil)
	if !ok {
		return
	}
	defer release()

	upstreamURL, err := realtime.BuildUpstreamURL(resolved.Endpoint, c.Request)
	if err != nil {
		s.responders.openai.Internal(c, "realtime: build upstream url: "+err.Error())
		return
	}

	upstreamHeader := upstreamHeaders(c.Request.Header, resolved.Upstream)
	realtime.StripHandshakeHeaders(upstreamHeader)

	dialer := &websocket.Dialer{
		HandshakeTimeout: realtimeDialTimeout,
		Subprotocols:     websocket.Subprotocols(c.Request),
	}

	upstream, upstreamResp, err := dialer.DialContext(c.Request.Context(), upstreamURL, upstreamHeader) //nolint:bodyclose // error path: writeRealtimeDialError closes body; success path: upgraded conn has no body
	if err != nil {
		s.writeRealtimeDialError(c, err, upstreamResp)
		return
	}
	defer func() { _ = upstream.Close() }()

	// Echo the subprotocol the upstream accepted so the client sees
	// the same handshake it would have gotten talking to the backend
	// directly. Without this, browsers that advertise a subprotocol
	// reject the upgrade as protocol-mismatch. Gorilla's Upgrade only
	// sets Sec-WebSocket-Protocol on its response when the upgrader's
	// Subprotocols include a client-advertised value, so we also
	// pass the negotiated value via the responseHeader argument.
	var upgradeResponseHeader http.Header
	if proto := upstream.Subprotocol(); proto != "" {
		upgradeResponseHeader = http.Header{"Sec-WebSocket-Protocol": []string{proto}}
	}

	clientConn, err := realtimeUpgrader.Upgrade(c.Writer, c.Request, upgradeResponseHeader)
	if err != nil {
		// Upgrade already wrote an error response on its own. Nothing
		// more to do here.
		return
	}
	defer func() { _ = clientConn.Close() }()

	proxyRealtimeFrames(clientConn, upstream)
}

// proxyRealtimeFrames pipes WebSocket messages in both directions
// until one side returns an error. The handler waits for both pipes
// to drain before returning so gin does not recycle the
// ResponseWriter while either direction is still running.
//
// When the first pipe exits, it forces both connections closed so the
// second pipe's ReadMessage unblocks immediately instead of waiting
// for TCP keepalive. Without that forced close, a misbehaving client
// that drops without sending a close frame can stall the opposite
// pipe indefinitely.
func proxyRealtimeFrames(client, upstream *websocket.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)

	pipe := func(dst, src *websocket.Conn) {
		defer wg.Done()
		defer func() {
			_ = client.Close()
			_ = upstream.Close()
		}()
		for {
			mt, data, err := src.ReadMessage()
			if err != nil {
				if closeErr, ok := err.(*websocket.CloseError); ok {
					_ = dst.WriteMessage(websocket.CloseMessage,
						websocket.FormatCloseMessage(closeErr.Code, closeErr.Text))
				} else {
					_ = dst.WriteMessage(websocket.CloseMessage,
						websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
				}
				return
			}
			if err := dst.WriteMessage(mt, data); err != nil {
				return
			}
		}
	}

	go pipe(upstream, client)
	go pipe(client, upstream)
	wg.Wait()
}

// writeRealtimeDialError routes an upstream dial failure through the
// OpenAI responder. Client-visible details are sanitized by the
// responder; the raw error is logged via the responder's internal
// logger for server-side correlation.
func (s *Server) writeRealtimeDialError(c *gin.Context, _ error, upstreamResp *http.Response) {
	if upstreamResp != nil {
		defer func() { _ = upstreamResp.Body.Close() }()
		// Upstream-authored status — 401/403/404/429 matter to SDKs.
		// Pick the matching responder method so the envelope's type
		// code matches what the client expects.
		switch upstreamResp.StatusCode {
		case http.StatusUnauthorized:
			s.responders.openai.Unauthorized(c, "realtime backend rejected upgrade: "+upstreamResp.Status)
		case http.StatusForbidden:
			s.responders.openai.Forbidden(c, "realtime backend rejected upgrade: "+upstreamResp.Status)
		case http.StatusNotFound:
			s.responders.openai.NotFound(c, "realtime backend: "+upstreamResp.Status)
		case http.StatusTooManyRequests:
			s.responders.openai.TooManyRequests(c, "realtime backend rate-limited", 0)
		default:
			s.responders.openai.BadGateway(c, "realtime backend rejected upgrade: "+upstreamResp.Status)
		}
		return
	}
	s.responders.openai.BadGateway(c, "realtime backend dial failed")
}

// handleRealtimeSession handles the HTTP POST helpers under /v1/realtime
// (sessions / transcription_sessions / client_secrets). They are
// ordinary JSON bodies carrying a model field, so the shared
// model-field routing path handles them end-to-end.
func (s *Server) handleRealtimeSession(c *gin.Context) {
	var req OpenAIModelRoutingRequest
	body, ok := readOpenAIBody(c, &req)
	if !ok {
		return
	}
	if req.Model == "" {
		param := "model"
		OpenAIInvalidRequest(c, "Model field is required for routing", &param)
		return
	}
	stashInferenceContext(c, req.Model, body)
	s.resolveAndDispatchSimple(c, req.Model, body, "chat")
}
