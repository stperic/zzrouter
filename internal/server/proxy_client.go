// HTTP proxy forwarding: builds upstream requests, streams responses,
// handles auth injection and response normalization.
package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/dispatch/wire"
	"github.com/stperic/zzrouter/pkg/fallback"
	"github.com/stperic/zzrouter/pkg/httperr"
	obsgenai "github.com/stperic/zzrouter/pkg/observability/genai"
	"github.com/stperic/zzrouter/pkg/observability/llm"
	obsproxy "github.com/stperic/zzrouter/pkg/observability/proxy"
	"github.com/stperic/zzrouter/pkg/utils"
)

// portFromURL returns the explicit port on a URL, defaulting to the
// scheme-default (80/443) when omitted. Returns 0 only for unsupported
// schemes — recorder.SetServer skips port emission on 0.
func portFromURL(u *url.URL) int {
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err == nil {
			return n
		}
	}
	switch u.Scheme {
	case "https":
		return 443
	case "http":
		return 80
	}
	return 0
}

// ProxyClient encapsulates the HTTP forwarding primitives shared by
// all pass-through surfaces (OpenAI compat, Ollama compat, native wire,
// MCP, responses API). It owns the HTTP clients that reach upstreams and
// the response normalizer registry.
type ProxyClient struct {
	streamClient        *http.Client
	clusterClient       func() *http.Client
	normalizers         *ResponseNormalizers
	onRateLimit         func(provider string, resp *http.Response)
	injectUsageMetadata func() bool
	localNode           func() string
}

// recordWireError adapts the wire.RecordPreByteCopyError helper to the
// (errType, err) pair used by this file's call sites. The wire helper
// owns the actual SetError + RecordCompletion(0,0) shape; this is a
// thin caller-side adapter so the message defaults to errType when
// err is nil.
func recordWireError(recorder *llm.InferenceRecorder, errType string, err error) {
	msg := errType
	if err != nil {
		msg = err.Error()
	}
	wire.RecordPreByteCopyError(recorder, errType, msg)
}

// NewProxyClient constructs a ProxyClient. injectUsageMetadata is a
// lazy lookup against the live config so that runtime toggles to
// coordinator.routing.inject_usage_metadata take effect without a
// restart. localNode returns the coord's node name for the zzrouter
// metadata block on cloud-direct responses. clusterClient returns the
// mTLS client a Cluster upstream is reached with, nil when there is none.
func NewProxyClient(
	streamClient *http.Client,
	clusterClient func() *http.Client,
	normalizers *ResponseNormalizers,
	onRateLimit func(provider string, resp *http.Response),
	injectUsageMetadata func() bool,
	localNode func() string,
) *ProxyClient {
	if injectUsageMetadata == nil {
		injectUsageMetadata = func() bool { return false }
	}
	if localNode == nil {
		localNode = func() string { return "" }
	}
	if clusterClient == nil {
		clusterClient = func() *http.Client { return nil }
	}
	return &ProxyClient{
		streamClient:        streamClient,
		clusterClient:       clusterClient,
		normalizers:         normalizers,
		onRateLimit:         onRateLimit,
		injectUsageMetadata: injectUsageMetadata,
		localNode:           localNode,
	}
}

// errNoClusterClient is returned for a Cluster upstream on a node that has
// no mTLS client to reach a worker with.
var errNoClusterClient = fmt.Errorf("%w: no cluster mTLS client", fallback.ErrUnreachable)

// do sends req with the client that reaches up: a worker's cluster port is
// reached over mTLS, everything else over the streaming client.
func (p *ProxyClient) do(req *http.Request, up backend.Upstream) (*http.Response, error) {
	if !up.IsCluster() {
		return p.streamClient.Do(req)
	}
	client := p.clusterClient()
	if client == nil {
		return nil, errNoClusterClient
	}
	return client.Do(req)
}

// ForwardToBackend proxies an inference request to targetURL and relays
// the answer. up decides what of the caller's request travels.
func (p *ProxyClient) ForwardToBackend(w http.ResponseWriter, req *http.Request, providerType, targetURL string, up backend.Upstream, body []byte) {
	model, _ := req.Context().Value(CtxKeyModel).(string)
	recorder := requestRecorder(req.Context(), model, providerType)
	recorder.SetUpstreamRequestData(body)
	var serverHost string
	if u, err := url.Parse(targetURL); err == nil && u.Host != "" {
		serverHost = u.Hostname()
		recorder.SetServer(serverHost, portFromURL(u))
	}

	// In-flight saturation counter: paired Inc/Dec captures the
	// label-tuple at request entry so the +1/-1 nets to zero on the
	// same series. Operation/provider/model labels match the GenAI
	// duration histograms so saturation dashboards slice the same way.
	inflightToken := obsproxy.IncInflight(req.Context(),
		string(obsgenai.OperationName(req.URL.Path)),
		providerType, model, serverHost)
	defer obsproxy.DecInflight(req.Context(), inflightToken)

	// Streaming context: cancelled if client disconnects, no timeout
	streamCtx, cancel := context.WithCancel(req.Context())
	defer cancel()

	proxyReq, err := http.NewRequestWithContext(streamCtx, req.Method, targetURL, bytes.NewReader(body))
	if err != nil {
		recordWireError(recorder, "proxy_request_build_failed", err)
		writeError(w, req, httperr.Error{Status: http.StatusInternalServerError, Type: "server_error", Message: "Failed to create proxy request", Code: "proxy_request_build_failed"})
		return
	}
	proxyReq.Header = upstreamHeaders(req.Header, up)

	resp, err := p.do(proxyReq, up)
	if err != nil {
		recordWireError(recorder, "backend_unreachable", err)
		writeError(w, req, httperr.Error{Status: http.StatusBadGateway, Type: "api_error", Message: "failed to reach backend", Code: "backend_unreachable"})
		return
	}
	defer func() { _ = resp.Body.Close() }()

	// Track rate limit headers from provider responses
	if p.onRateLimit != nil {
		p.onRateLimit(providerType, resp)
	}

	// HTML-leak guard for the OpenAI compat surface. When openai_compat.
	// default_backend points at a provider that doesn't implement a given
	// /v1/* route (e.g. OpenRouter has no /v1/files, /v1/vector_stores,
	// /v1/containers), the upstream's CDN frequently serves their web-app
	// HTML at 200 — SDK consumers parsing the body as JSON would explode
	// on a 94 KB <!DOCTYPE html>. Convert any non-streaming HTML response
	// on a /v1/* path to a structured 502 envelope so the contract matches
	// the rest of the surface. Streaming SSE has its own content-type
	// (text/event-stream) and is not affected.
	if isOpenAICompatPath(req.URL.Path) && isHTMLResponse(resp) {
		recordWireError(recorder, "upstream_route_not_implemented",
			fmt.Errorf("upstream served HTML on /v1/* path"))
		writeUpstreamRouteNotImplemented(w, req, providerType, resp)
		return
	}

	// Backend errors (non-2xx) are always returned as non-streaming JSON
	// bodies — even when the original request asked for streaming — so we
	// intercept here before the streaming vs non-streaming split. The body
	// is rewritten to the surface-appropriate envelope: /v1/* (OpenAI)
	// gets the closed-vocab normalizer; /api/* (Ollama-native) keeps the
	// flat {"error":"..."} shape. Without the surface check, an Ollama
	// 404 on /api/chat used to return double-wrapped JSON.
	if resp.StatusCode >= 400 {
		// Record the classified status after rewriting, before returning.
		mergeUpstreamHeaders(w.Header(), resp.Header)
		stripUpstreamIdentifyingHeaders(w.Header())
		w.Header().Set(constants.HeaderServingProvider, providerType)
		if n := req.Header.Get(constants.HeaderServingNode); n != "" {
			w.Header().Set(constants.HeaderServingNode, n)
		}
		writeRewrittenError(w, req, resp)
		wire.RecordPreByteCopyErrorFromStatus(recorder, resp.StatusCode)
		return
	}

	streaming := isStreamingResponse(resp)

	if !streaming {
		captureNonStreamingMetrics(resp, recorder)
	}

	mergeUpstreamHeaders(w.Header(), resp.Header)
	stripUpstreamIdentifyingHeaders(w.Header())
	w.Header().Set(constants.HeaderServingProvider, providerType)
	if n := req.Header.Get(constants.HeaderServingNode); n != "" {
		w.Header().Set(constants.HeaderServingNode, n)
	}

	// Build routing metadata used by both the streaming + non-streaming
	// inject sites. Deployment is left empty for the direct-cloud path
	// (no model-group); cost+timing fields are filled lazily from the
	// recorder snapshot inside the wire layer. Node is the coord's
	// hostname for cloud-direct paths (the request was served from
	// here); for cluster-routed proxies the upstream sets the header.
	injectUsage := p.injectUsageMetadata()
	node := req.Header.Get(constants.HeaderServingNode)
	if node == "" {
		node = p.localNode()
	}
	meta := wire.RoutingMetadata{
		Provider:             providerType,
		Node:                 node,
		InjectUsage:          injectUsage,
		ClientModel:          clientModelFromContext(req.Context()),
		SuppressBodyMetadata: wire.SuppressBodyFromVerbose(req.URL.Query().Get("verbose")),
		SuppressUsageFrame:   suppressUsageFrameFromContext(req.Context()),
	}

	if streaming {
		streamWithUsageInject(w, resp, p.normalizers, providerType, meta, recorder)
		return
	}
	writeNormalizedBody(w, resp, p.normalizers, providerType, meta, recorder)
}

// ForwardDetached sends a request to a backend but does NOT write to the client.
// Returns the raw *http.Response so the caller can inspect status before committing.
// The caller is responsible for closing resp.Body. up decides what of the
// caller's headers travel.
func (p *ProxyClient) ForwardDetached(ctx context.Context, method, targetURL string, headers http.Header, up backend.Upstream, body []byte) (*http.Response, error) {
	proxyReq, err := http.NewRequestWithContext(ctx, method, targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	proxyReq.Header = upstreamHeaders(headers, up)

	return p.do(proxyReq, up)
}

// htmlSniffPrefixCap caps the number of body bytes we read to confirm the
// "looks like HTML" smell when Content-Type is empty/ambiguous. Real HTML
// pages always carry a recognisable token (<!doctype, <html, <head) within
// the first few hundred bytes; capping avoids buffering megabytes for the
// sake of a sniff.
const htmlSniffPrefixCap = 512

// isOpenAICompatPath returns true if the request path is on the OpenAI
// compat surface (/v1/*) — the only place HTML responses are categorically
// wrong. /api/* (Ollama-native) and other surfaces flow through the same
// proxy but have their own contract layers.
func isOpenAICompatPath(p string) bool {
	return strings.HasPrefix(p, "/v1/") || p == "/v1"
}

// isHTMLResponse reports whether the upstream response body is HTML —
// the load-bearing signal for "upstream doesn't implement this route and
// is serving its web app instead". Trusts an explicit text/html
// Content-Type; if Content-Type is empty/ambiguous, peeks at the body
// prefix. The returned response is safe for downstream readers — the
// peek is performed via http.DetectContentType on a buffered prefix that
// is then re-attached as a fresh body via io.MultiReader.
func isHTMLResponse(resp *http.Response) bool {
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.HasPrefix(ct, "text/html") || strings.HasPrefix(ct, "application/xhtml") {
		return true
	}
	// Streaming + already-known JSON shapes: short-circuit, do not buffer.
	if strings.HasPrefix(ct, "text/event-stream") ||
		strings.HasPrefix(ct, "application/json") ||
		strings.HasPrefix(ct, "application/x-ndjson") {
		return false
	}
	// Ambiguous content-type (some upstream CDNs omit it for HTML
	// fallback pages). Peek at the first chunk.
	prefix := make([]byte, htmlSniffPrefixCap)
	n, _ := io.ReadFull(resp.Body, prefix)
	prefix = prefix[:n]
	resp.Body = io.NopCloser(io.MultiReader(bytes.NewReader(prefix), resp.Body))
	if n == 0 {
		return false
	}
	sniffed := strings.ToLower(http.DetectContentType(prefix))
	return strings.HasPrefix(sniffed, "text/html") ||
		strings.HasPrefix(sniffed, "application/xhtml")
}

// writeUpstreamRouteNotImplemented converts a streaming HTML upstream
// response into a structured OpenAI-shape error envelope so SDK consumers
// don't crash trying to parse <!DOCTYPE html> as JSON. Drains and discards
// the upstream body (we never want to forward HTML on /v1/*). Records the
// upstream status + body size for log triage.
func writeUpstreamRouteNotImplemented(w http.ResponseWriter, req *http.Request, providerType string, resp *http.Response) {
	path := req.URL.Path
	bodyLen, _ := io.Copy(io.Discard, resp.Body)
	utils.LogDebugf("[Proxy] upstream %s returned HTML on %s (status=%d, bytes=%d): converting to upstream_route_not_implemented",
		providerType, path, resp.StatusCode, bodyLen)
	w.Header().Set(constants.HeaderServingProvider, providerType)
	w.Header().Set("X-Zzrouter-Upstream-Status", fmt.Sprintf("%d", resp.StatusCode))
	writeError(w, req, httperr.Error{Status: http.StatusBadGateway, Type: "api_error", Message: fmt.Sprintf("upstream backend %q does not implement %s (returned HTML, status=%d)",
		providerType, path, resp.StatusCode), Code: "upstream_route_not_implemented"})
}

// stripUpstreamIdentifyingHeaders removes headers that leak the identity
// of the cloud provider's edge layer or upstream-of-upstream infrastructure
// (CDN brand, cache vendor, internal trace IDs). These provide no value to
// zzRouter clients and would let downstream agents fingerprint the routing
// path. zzRouter's own X-Zzrouter-Node / X-Zzrouter-Provider headers are
// re-set by the caller after this strip.
func stripUpstreamIdentifyingHeaders(h http.Header) {
	for _, k := range []string{
		"Server",
		"Via",
		"X-Powered-By",
		"CF-Ray",
		"CF-Cache-Status",
		"CF-Apo-Via",
		"X-Amzn-Trace-Id",
		"X-Amz-Cf-Id",
		"X-Amz-Cf-Pop",
	} {
		h.Del(k)
	}
}
