// package server - jobs controller wires pkg/jobs.Registry onto the
// HTTP surface. Public endpoints live on the coordinator; internal
// endpoints live on every cluster-mode node. The coord proxies
// cross-node requests (?node=<remote>) onto the owning worker's
// internal endpoint over mTLS. Workers construct the controller
// without a proxy — remote queries 501 on that surface, but workers
// don't mount the public group, so the path is unreachable there
// today.

package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/jobs"
)

// sseKeepAliveInterval picks the cadence of `:ping` comment frames
// the stream emits when the producer is quiet. 15s is small enough to
// survive typical ALB/nginx idle timeouts (30–60s) with headroom,
// large enough to stay off the log noise floor.
const sseKeepAliveInterval = 15 * time.Second

// Proxy dial/read tuning. The streaming mTLS client MUST NOT inherit
// DialClient's transport verbatim — DialClient sets no header deadline
// so a half-open worker would wedge the proxy goroutine forever.
const (
	// proxyDialTimeout bounds the TCP+TLS handshake phase.
	proxyDialTimeout = 10 * time.Second
	// proxyKeepAlive enables TCP keepalives to detect half-open peers.
	proxyKeepAlive = 30 * time.Second
	// proxyResponseHeaderTimeout bounds the upstream header reply.
	// Streaming read after headers is bounded by the request context,
	// which cancels when the downstream client disconnects.
	proxyResponseHeaderTimeout = 30 * time.Second
	// peerProbeTimeout caps per-peer GetJob probes during owner
	// fan-out. Short by design — agents poll, so a slow peer that
	// would take seconds is better treated as a miss; the next poll
	// will retry.
	peerProbeTimeout = 2 * time.Second
)

// JobsProxy bundles the coord-side dependencies needed to forward
// remote ?node= queries onto the owning worker's internal endpoint.
// Nil on worker controllers (public group is coord-only); when nil,
// remote queries return 501.
type JobsProxy struct {
	// StreamClient returns a fresh *http.Client per call, presenting
	// the coord's mTLS identity and expecting peer OU=worker. The
	// transport is streaming-tuned (ResponseHeaderTimeout + TCP
	// keepalive); do NOT reuse the general DialClient transport here.
	StreamClient func() (*http.Client, error)
	// ResolveEndpoint looks up an endpoint by nodename/name/alias/URL.
	// Returns nil endpoint on miss so the handler can 404.
	ResolveEndpoint func(node string) *mesh.Endpoint
	// ListPeers returns the names of every non-local cluster peer the
	// coord can reach. Used by GetJob/StreamJob fan-out when no ?node=
	// is provided so agents don't have to remember which worker owns a
	// job. Optional — nil disables fan-out and the handler 404s on
	// local-miss instead.
	ListPeers func() []string
	// ClusterPort is the homogeneous cluster mTLS port used by
	// DeriveClusterURL when Endpoint.ClusterURL is empty.
	ClusterPort int
}

// JobsController wires jobs.Registry onto HTTP handlers. Pure
// delegation; no business logic of its own.
type JobsController struct {
	registry    *jobs.Registry
	nodeName    string
	isLocalFn   func(string) bool
	proxy       *JobsProxy // optional; nil = 501 on remote
	cancelOwner func(string) error
}

// NewJobsController constructs a worker-side (or pre-proxy) controller.
// Remote queries return 501. isLocalFn reports whether the given node
// hint refers to this node (accepts both nodename and endpoint URL).
func NewJobsController(registry *jobs.Registry, nodeName string, isLocalFn func(string) bool) *JobsController {
	return &JobsController{registry: registry, nodeName: nodeName, isLocalFn: isLocalFn}
}

// NewJobsControllerWithProxy constructs the coord-side controller with
// a cross-node proxy. A partially-filled proxy (missing StreamClient
// or ResolveEndpoint) is treated as no proxy — remote queries 501 —
// so a wiring bug can't masquerade as a transient upstream failure.
func NewJobsControllerWithProxy(registry *jobs.Registry, nodeName string, isLocalFn func(string) bool, proxy *JobsProxy) *JobsController {
	if proxy != nil && (proxy.StreamClient == nil || proxy.ResolveEndpoint == nil) {
		proxy = nil
	}
	return &JobsController{registry: registry, nodeName: nodeName, isLocalFn: isLocalFn, proxy: proxy}
}

// RegisterPublicRoutes mounts coord-facing routes under the public API
// group. Caller gates on IsCoordinator.
func (c *JobsController) RegisterPublicRoutes(router *gin.RouterGroup) {
	router.GET("/jobs", c.ListJobs)
	router.GET("/jobs/:id", c.GetJob)
	router.GET("/jobs/:id/stream", c.StreamJob)
	router.DELETE("/jobs/:id", c.CancelJob)
}

// The controller is stateless; safe to re-instantiate on router
// rebuilds during role transitions (Worker → Coord promote and back).

// RegisterInternalRoutes mounts cluster-internal routes. Mounted on
// every node that serves /internal/* (coord + worker).
func (c *JobsController) RegisterInternalRoutes(router *gin.RouterGroup) {
	router.GET("/jobs", c.InternalListJobs)
	router.GET("/jobs/:id", c.InternalGetJob)
	router.GET("/jobs/:id/stream", c.InternalStreamJob)
	router.POST("/jobs/:id/cancel", c.InternalCancelJob)
}

// Public handlers wrap responses in respondSuccess. Internal mirrors
// return the raw payload so the coord proxy can forward upstream
// without double-wrapping.

// ListJobs returns a snapshot of every job in the local registry. With
// ?node=<remote>, forwards the JSON list from the owning worker — a
// unicast list, distinct from cluster-wide fan-out which remains
// deferred (plan doc §Design decisions).
func (c *JobsController) ListJobs(ctx *gin.Context) {
	if c.handleRemote(ctx, c.proxyList) {
		return
	}
	kind := jobs.Kind(ctx.Query("kind"))
	events := c.registry.List(kind)
	// The spec advertises ?limit, and total/has_more only mean anything
	// if something actually pages. Uses the shared helpers so this route
	// pages the way every other list endpoint does.
	pagination, ok := ParsePagination(ctx)
	if !ok {
		return
	}
	page, total, hasMore := ApplyPagination(events, pagination)
	respondJobPage(ctx, page, total, hasMore, c.nodeName)
}

// respondJobList writes the collection envelope shared by every list
// endpoint on the management surface. Both the local answer and the
// ?node=<peer> answer go through here, so the public shape of GET /jobs
// no longer depends on which node happens to own the job.
func respondJobList(ctx *gin.Context, events []jobs.Event, node string) {
	respondJobPage(ctx, events, len(events), false, node)
}

// respondJobPage is respondJobList for a caller that has already paged.
func respondJobPage(ctx *gin.Context, events []jobs.Event, total int, hasMore bool, node string) {
	respondListWithMetadata(ctx, events, total, hasMore, map[string]any{"node": node})
}

// GetJob returns the latest event snapshot for a single job. When
// ?node=<remote>, forwards the JSON reply from the owning worker. When
// no node hint is provided and the job isn't local, fans out across
// known peers (first non-404 wins) so agents don't have to track owners.
func (c *JobsController) GetJob(ctx *gin.Context) {
	if c.handleRemote(ctx, c.proxyJSON) {
		return
	}
	id := ctx.Param("id")
	ev, err := c.registry.Get(id)
	if err == nil {
		respondSuccess(ctx, "Job retrieved", ev)
		return
	}
	if !errors.Is(err, jobs.ErrNotFound) {
		InternalNodeError(ctx, err.Error())
		ctx.Abort()
		return
	}
	if owner := c.findJobOwner(ctx, id); owner != "" {
		// Same re-wrap as the ?node= path: a caller who let the
		// coordinator find the owner must not get a different shape
		// than one who named it.
		c.proxyOwnedJob(ctx, owner, id, false)
		return
	}
	NotFound(ctx, "job not found")
}

// StreamJob opens an SSE stream for the job's events. Honors
// ?from=<seq> + ?epoch=<e>; SSE framing matches the inference-log
// pattern (event: progress | done | stream_closed | error). When
// ?node=<remote>, proxies the remote worker's internal stream. When
// no node hint and the job isn't local, fans out a snapshot probe to
// find the owner before opening the proxied stream.
func (c *JobsController) StreamJob(ctx *gin.Context) {
	if c.handleRemote(ctx, c.proxyStream) {
		return
	}
	id := ctx.Param("id")
	if _, err := c.registry.Get(id); err == nil {
		c.serveStream(ctx)
		return
	} else if !errors.Is(err, jobs.ErrNotFound) {
		InternalNodeError(ctx, err.Error())
		ctx.Abort()
		return
	}
	if owner := c.findJobOwner(ctx, id); owner != "" {
		c.proxyStream(ctx, owner)
		return
	}
	NotFound(ctx, "job not found")
}

// CancelJob fires cancel on the local registry and returns 202. Cancel
// is non-blocking (plan doc §Endpoints) — the stream is authoritative
// for the outcome, not this response. Remote jobs proxy the cancel
// POST onto the owning worker's internal endpoint.
func (c *JobsController) CancelJob(ctx *gin.Context) {
	if c.handleRemote(ctx, c.proxyCancel) {
		return
	}
	id := ctx.Param("id")
	if c.cancelOwner != nil {
		if err := c.cancelOwner(id); err != nil {
			InternalNodeError(ctx, "persist job cancellation: "+err.Error())
			return
		}
	}
	if err := c.registry.Cancel(id); errors.Is(err, jobs.ErrNotFound) {
		// Jobs are owned by the node performing the work, so a coordinator
		// asked to cancel a worker's job finds nothing locally. GetJob and
		// StreamJob already resolve the owner; without the same fan-out here
		// the caller got a 404 that clients read as "already gone" and
		// reported as a successful cancel while the work continued.
		if owner := c.findJobOwner(ctx, id); owner != "" {
			c.proxyCancel(ctx, owner)
			return
		}
		NotFound(ctx, "job not found")
		return
	}
	ctx.JSON(http.StatusAccepted, gin.H{
		"accepted": true,
		"job_id":   id,
	})
}

// --- internal mirrors ---

func (c *JobsController) InternalListJobs(ctx *gin.Context) {
	kind := jobs.Kind(ctx.Query("kind"))
	events := c.registry.List(kind)
	ctx.JSON(http.StatusOK, gin.H{"node": c.nodeName, "jobs": events})
}

func (c *JobsController) InternalGetJob(ctx *gin.Context) {
	id := ctx.Param("id")
	ev, err := c.registry.Get(id)
	if errors.Is(err, jobs.ErrNotFound) {
		ctx.AbortWithStatus(http.StatusNotFound)
		return
	}
	if err != nil {
		InternalNodeError(ctx, err.Error())
		ctx.Abort()
		return
	}
	ctx.JSON(http.StatusOK, ev)
}

// InternalStreamJob deliberately skips handleRemote — /internal/* is
// addressed to this node by construction (mTLS peer selects node),
// so a node= hint would be noise.
func (c *JobsController) InternalStreamJob(ctx *gin.Context) {
	c.serveStream(ctx)
}

func (c *JobsController) InternalCancelJob(ctx *gin.Context) {
	id := ctx.Param("id")
	if err := c.registry.Cancel(id); errors.Is(err, jobs.ErrNotFound) {
		ctx.AbortWithStatus(http.StatusNotFound)
		return
	}
	ctx.JSON(http.StatusAccepted, gin.H{"accepted": true})
}

// --- helpers ---

// handleRemote returns true (after writing a response) if the request
// targeted a non-local node and was dispatched. fn is invoked with the
// resolved node hint when a proxy is available; when no proxy is
// wired, the handler writes 501. Local requests return false so the
// local-serve path continues.
func (c *JobsController) handleRemote(ctx *gin.Context, fn func(*gin.Context, string)) bool {
	node := ctx.Query("node")
	if node == "" || node == c.nodeName {
		return false
	}
	if c.isLocalFn != nil && c.isLocalFn(node) {
		return false
	}
	if c.proxy == nil || c.proxy.StreamClient == nil {
		RespondWithProblemOpts(ctx, http.StatusNotImplemented, "Not Implemented",
			"cross-node job proxying not yet implemented; retry the request directly against the owning node's public URL",
			ProblemOpts{Code: "job_proxy_unavailable", Node: node})
		ctx.Abort()
		return true
	}
	fn(ctx, node)
	return true
}

// findJobOwner probes every known peer's /internal/jobs/:id in parallel
// and returns the node name of the first 200 response. Returns "" when
// no proxy/peers are wired or no peer claims the job. Bounded by
// peerProbeTimeout per peer; peers are racing so the slowest is not on
// the critical path. Used by GetJob/StreamJob fallback when the agent
// didn't pass ?node=.
func (c *JobsController) findJobOwner(ctx *gin.Context, id string) string {
	if c.proxy == nil || c.proxy.ListPeers == nil || c.proxy.ResolveEndpoint == nil {
		return ""
	}
	peers := c.proxy.ListPeers()
	if len(peers) == 0 {
		return ""
	}
	probeCtx, cancel := context.WithTimeout(ctx.Request.Context(), peerProbeTimeout)
	defer cancel()

	type result struct{ node string }
	hits := make(chan result, len(peers))
	for _, peer := range peers {
		go func(node string) {
			ep := c.proxy.ResolveEndpoint(node)
			if ep == nil {
				hits <- result{}
				return
			}
			clusterURL := ep.ClusterURL
			if clusterURL == "" {
				derived, err := mesh.DeriveClusterURL(ep.URL, c.proxy.ClusterPort)
				if err != nil {
					hits <- result{}
					return
				}
				clusterURL = derived
			}
			client, err := c.proxy.StreamClient()
			if err != nil {
				hits <- result{}
				return
			}
			req, err := http.NewRequestWithContext(probeCtx, http.MethodGet,
				clusterURL+"/zzrouter/v1/internal/jobs/"+url.PathEscape(id), nil)
			if err != nil {
				hits <- result{}
				return
			}
			resp, err := client.Do(req)
			if err != nil {
				hits <- result{}
				return
			}
			defer resp.Body.Close()
			_, _ = io.Copy(io.Discard, resp.Body)
			if resp.StatusCode == http.StatusOK {
				hits <- result{node: node}
				return
			}
			hits <- result{}
		}(peer)
	}
	for range peers {
		select {
		case r := <-hits:
			if r.node != "" {
				return r.node
			}
		case <-probeCtx.Done():
			return ""
		}
	}
	return ""
}

// resolveUpstream derives the cluster-port base URL for the named
// node. Returns ("", false) after writing a 404/502 response when the
// node is unknown or its cluster URL cannot be derived. Assumes
// handleRemote already gated on a fully-wired proxy.
func (c *JobsController) resolveUpstream(ctx *gin.Context, node string) (string, bool) {
	ep := c.proxy.ResolveEndpoint(node)
	if ep == nil {
		RespondWithProblemOpts(ctx, http.StatusNotFound, "Not Found",
			"unknown node; pass ?node=<nodename|alias|endpoint-url>",
			ProblemOpts{Code: "node_unknown", Node: node})
		ctx.Abort()
		return "", false
	}
	clusterURL := ep.ClusterURL
	if clusterURL == "" {
		derived, err := mesh.DeriveClusterURL(ep.URL, c.proxy.ClusterPort)
		if err != nil {
			RespondWithProblemOpts(ctx, http.StatusBadGateway, "Bad Gateway",
				"derive cluster URL: "+err.Error(),
				ProblemOpts{Code: string(httperr.CodeNodeUnreachable), Node: node})
			ctx.Abort()
			return "", false
		}
		clusterURL = derived
	}
	return clusterURL, true
}

// internalJobsURL builds the upstream /internal/jobs path with
// forwarded query (node= hint stripped — the remote is the target, a
// node hint there would be noise).
func internalJobsURL(base, suffix string, src url.Values) string {
	u := base + "/zzrouter/v1/internal/jobs" + suffix
	q := url.Values{}
	for k, v := range src {
		if k == "node" {
			continue
		}
		q[k] = v
	}
	if enc := q.Encode(); enc != "" {
		u += "?" + enc
	}
	return u
}

// copyUpstreamJSON forwards a non-streaming JSON reply from the
// upstream worker. Status + Content-Type pass through verbatim; body
// is copied with a 1 MiB ceiling (snapshots are tens of bytes to a
// few KiB in practice). Remainder is drained so the underlying
// connection can be returned to the keep-alive pool.
func copyUpstreamJSON(ctx *gin.Context, resp *http.Response) {
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		ctx.Header("Content-Type", ct)
	}
	ctx.Status(resp.StatusCode)
	_, _ = io.Copy(ctx.Writer, io.LimitReader(resp.Body, upstreamBodyLimit))
	_, _ = io.Copy(io.Discard, resp.Body)
}

// upstreamURL resolves node → cluster-port base URL and builds the
// upstream /internal/jobs<suffix> path with optional query forwarding.
// Returns ("", false) after writing a 404/502 response on failure.
func (c *JobsController) upstreamURL(ctx *gin.Context, node, pathSuffix string, forwardQuery bool) (string, bool) {
	base, ok := c.resolveUpstream(ctx, node)
	if !ok {
		return "", false
	}
	var src url.Values
	if forwardQuery {
		src = ctx.Request.URL.Query()
	}
	return internalJobsURL(base, pathSuffix, src), true
}

// proxyList forwards GET /jobs → GET /internal/jobs on the owning
// worker. Forwards ?kind= if set; ?node= is stripped by
// internalJobsURL.
//
// The worker answers in the cluster-internal {node, jobs} shape, which
// is a different contract and stays as it is. Rather than copy that
// body through, the payload is lifted into the public collection
// envelope: a caller asking for one peer's jobs should not get a
// different shape than a caller asking for the local node's.
func (c *JobsController) proxyList(ctx *gin.Context, node string) {
	upstream, ok := c.upstreamURL(ctx, node, "", true)
	if !ok {
		return
	}
	resp, err := c.doUpstream(ctx, http.MethodGet, upstream, nil)
	if err != nil {
		c.writeProxyError(ctx, node, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		copyUpstreamJSON(ctx, resp)
		return
	}
	var body struct {
		Node string       `json:"node"`
		Jobs []jobs.Event `json:"jobs"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, upstreamBodyLimit)).Decode(&body); err != nil {
		c.writeProxyError(ctx, node, fmt.Errorf("decode peer job list: %w", err))
		return
	}
	if body.Node == "" {
		body.Node = node
	}
	respondJobList(ctx, body.Jobs, body.Node)
}

// upstreamBodyLimit caps the peer body the coordinator will read, both
// when streaming one through and when buffering one to re-wrap, so
// re-wrapping cannot become the more permissive path.
const upstreamBodyLimit = 1 << 20

// proxyRewrapped forwards to the owning worker and re-emits a success
// body through the PUBLIC responder, so a proxied answer has the same
// shape as a local one.
//
// The /internal/* handlers speak the cluster's own wire shape — a bare
// jobs.Event, a bare {"accepted":true} — which is correct for a peer
// but is not the management surface's contract. Copying those bytes
// straight to a public caller made the response shape depend on which
// node happened to own the job, and owner is exactly the detail a
// client should not have to know. respondJobList already established
// this pattern for GET /jobs; these are the two paths it never reached.
//
// Non-2xx passes through untouched: an upstream error is already
// problem-shaped, and re-wrapping it would dress a failure up as a
// success envelope.
func (c *JobsController) proxyRewrapped(ctx *gin.Context, node, method, pathSuffix string,
	forwardQuery bool, rewrap func(status int, body []byte) error) {
	upstream, ok := c.upstreamURL(ctx, node, pathSuffix, forwardQuery)
	if !ok {
		return
	}
	resp, err := c.doUpstream(ctx, method, upstream, nil)
	if err != nil {
		c.writeProxyError(ctx, node, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		copyUpstreamJSON(ctx, resp)
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, upstreamBodyLimit))
	if err != nil {
		c.writeProxyError(ctx, node, fmt.Errorf("read peer response: %w", err))
		return
	}
	if err := rewrap(resp.StatusCode, body); err != nil {
		c.writeProxyError(ctx, node, err)
	}
}

// proxyJSON forwards GET /jobs/:id to the owning worker's internal
// endpoint. No streaming; the response is a snapshot.
func (c *JobsController) proxyJSON(ctx *gin.Context, node string) {
	c.proxyOwnedJob(ctx, node, ctx.Param("id"), true)
}

// proxyOwnedJob fetches one job from its owner and answers in the
// public envelope. Both GetJob routes reach it — the explicit ?node=
// hint and the owner fan-out — so neither can drift into its own shape.
func (c *JobsController) proxyOwnedJob(ctx *gin.Context, node, id string, forwardQuery bool) {
	c.proxyRewrapped(ctx, node, http.MethodGet, "/"+url.PathEscape(id), forwardQuery,
		func(_ int, body []byte) error {
			var ev jobs.Event
			if err := json.Unmarshal(body, &ev); err != nil {
				return fmt.Errorf("decode peer job: %w", err)
			}
			respondSuccess(ctx, "Job retrieved", ev)
			return nil
		})
}

// proxyCancel forwards DELETE /jobs/:id → POST /internal/jobs/:id/cancel
// on the owning worker. Cancel is idempotent + non-blocking: a 202
// upstream maps to 202 downstream.
func (c *JobsController) proxyCancel(ctx *gin.Context, node string) {
	id := ctx.Param("id")
	c.proxyRewrapped(ctx, node, http.MethodPost, "/"+url.PathEscape(id)+"/cancel", false,
		func(status int, _ []byte) error {
			// The peer answers {"accepted":true} with no job_id. Local
			// cancel carries it, and a caller cancelling several jobs
			// needs it to tell the replies apart, so restore it here.
			ctx.JSON(status, gin.H{"accepted": true, "job_id": id})
			return nil
		})
}

// doUpstream constructs and dispatches a proxied request using the
// coord's mTLS client. Client disconnect at ctx.Request.Context()
// cancels the upstream request.
func (c *JobsController) doUpstream(ctx *gin.Context, method, u string, body io.Reader) (*http.Response, error) {
	client, err := c.proxy.StreamClient()
	if err != nil {
		return nil, fmt.Errorf("mtls client: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx.Request.Context(), method, u, body)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	return client.Do(req)
}

// writeProxyError maps an upstream dispatch failure to an HTTP
// response. Safe to call only before any bytes have been written.
func (c *JobsController) writeProxyError(ctx *gin.Context, node string, err error) {
	if errors.Is(err, context.Canceled) {
		return // client gave up; nothing to write
	}
	RespondWithProblemOpts(ctx, http.StatusBadGateway, "Bad Gateway",
		"proxy to owning node failed: "+err.Error(),
		ProblemOpts{Code: "proxy_failed", Node: node})
	ctx.Abort()
}

// proxyStream forwards GET /jobs/:id/stream to the owning worker's
// internal SSE endpoint. Frames are copied byte-for-byte; blank-line
// boundaries trigger a flush so events are not coalesced by transport
// buffering. The client's request context propagates to the upstream
// request so a client disconnect cancels the upstream read.
func (c *JobsController) proxyStream(ctx *gin.Context, node string) {
	upstream, ok := c.upstreamURL(ctx, node, "/"+url.PathEscape(ctx.Param("id"))+"/stream", true)
	if !ok {
		return
	}

	client, err := c.proxy.StreamClient()
	if err != nil {
		c.writeProxyError(ctx, node, err)
		return
	}
	req, err := http.NewRequestWithContext(ctx.Request.Context(), http.MethodGet, upstream, nil)
	if err != nil {
		c.writeProxyError(ctx, node, err)
		return
	}
	req.Header.Set("Accept", "text/event-stream")

	resp, err := client.Do(req)
	if err != nil {
		c.writeProxyError(ctx, node, err)
		return
	}
	defer resp.Body.Close()

	// Sniff status BEFORE flipping to SSE framing — worker emits
	// JSON for 404 / 409 epoch_mismatch / 400 replay_unsupported /
	// 503 registry_stopped. Forwarding those as SSE would corrupt
	// the framing contract.
	if resp.StatusCode != http.StatusOK {
		copyUpstreamJSON(ctx, resp)
		return
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		RespondWithProblemOpts(ctx, http.StatusBadGateway, "Bad Gateway",
			"upstream did not return SSE",
			ProblemOpts{Code: string(httperr.CodeUpstreamShape), Node: node})
		ctx.Abort()
		return
	}

	ctx.Header("Content-Type", "text/event-stream")
	ctx.Header("Cache-Control", "no-cache")
	ctx.Header("Connection", "keep-alive")
	ctx.Header("X-Accel-Buffering", "no")
	ctx.Status(http.StatusOK)
	ctx.Writer.Flush()

	// Byte-for-byte scan. Flush on the end of every event (a line
	// whose trimmed content is empty — "\n" or "\r\n"). Worker's
	// own keep-alive ":ping\n\n" passes through unchanged. The
	// coord does NOT add its own ticker here — that would collide
	// with the upstream's pings and corrupt event boundaries.
	reader := bufio.NewReader(resp.Body)
	lastWasBlank := true
	for {
		line, rerr := reader.ReadBytes('\n')
		if len(line) > 0 {
			if _, werr := ctx.Writer.Write(line); werr != nil {
				return // downstream gone
			}
			lastWasBlank = isBlankSSELine(line)
			if lastWasBlank {
				ctx.Writer.Flush()
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				// Defense-in-depth: upstream producer is expected to
				// terminate every event with "\n\n", but if the last
				// bytes lacked the terminator the event cannot be
				// dispatched by a standards-compliant SSE parser.
				// Inject the terminator before flushing.
				if !lastWasBlank {
					_, _ = ctx.Writer.Write([]byte("\n\n"))
				}
				ctx.Writer.Flush()
				return
			}
			// Headers already sent, so we can't change status.
			// Emit an SSE error event per plan doc §Failure modes.
			_, _ = fmt.Fprint(ctx.Writer, "event: error\ndata: {\"code\":\"node_disconnected\"}\n\n")
			ctx.Writer.Flush()
			return
		}
	}
}

// isBlankSSELine reports whether line is an SSE event terminator
// ("\n" or "\r\n"). Any other content — data:, event:, id:, :comment
// — is part of an event body. A bare "\r" without LF is treated as
// non-terminator: ReadBytes('\n') only returns on LF or EOF, so a
// lone \r in the stream is already malformed upstream and will not
// trigger event dispatch on the client either way.
func isBlankSSELine(line []byte) bool {
	switch len(line) {
	case 1:
		return line[0] == '\n'
	case 2:
		return line[0] == '\r' && line[1] == '\n'
	}
	return false
}

// serveStream subscribes to the local registry and copies events to
// the response writer using SSE framing. Event names mirror the plan
// doc §SSE framing: progress for running, done for terminal, error
// for stream-level failures, stream_closed for non-terminal clean
// shutdown.
func (c *JobsController) serveStream(ctx *gin.Context) {
	id := ctx.Param("id")

	opts := jobs.SubscribeOptions{Epoch: ctx.Query("epoch")}
	if fromStr := ctx.Query("from"); fromStr != "" {
		v, err := strconv.ParseUint(fromStr, 10, 64)
		if err != nil {
			BadRequest(ctx, "from must be a uint64")
			ctx.Abort()
			return
		}
		opts.From = &v
	}

	sub, err := c.registry.Subscribe(id, opts)
	switch {
	case errors.Is(err, jobs.ErrNotFound):
		NotFound(ctx, "job not found")
		return
	case errors.Is(err, jobs.ErrEpochMismatch):
		// 409 (Conflict) reflects "resource state changed since your
		// last view" — client must resnapshot.
		RespondWithProblemOpts(ctx, http.StatusConflict, "Conflict",
			"epoch mismatch; the client's view is stale and must be resnapshotted",
			ProblemOpts{Code: "epoch_mismatch"})
		ctx.Abort()
		return
	case errors.Is(err, jobs.ErrReplayUnsupported):
		RespondWithProblemOpts(ctx, http.StatusBadRequest, "Bad Request",
			"historical replay not supported on this kind",
			ProblemOpts{Code: "replay_unsupported"})
		ctx.Abort()
		return
	case errors.Is(err, jobs.ErrRegistryStopped):
		// Treat as 503: the node is going down; client should retry.
		RespondWithProblemOpts(ctx, http.StatusServiceUnavailable, "Service Unavailable",
			"job registry is stopping; retry shortly",
			ProblemOpts{Code: "registry_stopped"})
		ctx.Abort()
		return
	case err != nil:
		InternalNodeError(ctx, err.Error())
		ctx.Abort()
		return
	}
	defer sub.Unsubscribe()

	ctx.Header("Content-Type", "text/event-stream")
	ctx.Header("Cache-Control", "no-cache")
	ctx.Header("Connection", "keep-alive")
	ctx.Header("X-Accel-Buffering", "no")
	ctx.Status(http.StatusOK)
	ctx.Writer.Flush()

	// stream_closed is emitted on clean disconnect for non-terminal
	// streams. Producer-side terminal events ARE the final `done` and
	// are forwarded verbatim from the registry channel.
	streamEndEmitted := false
	defer func() {
		if !streamEndEmitted {
			ctx.SSEvent("stream_closed", "stream closed")
			ctx.Writer.Flush()
		}
	}()

	keepAlive := time.NewTicker(sseKeepAliveInterval)
	defer keepAlive.Stop()

	// Client SSE readers MUST explicitly handle the `events_dropped`
	// event name — a silently-ignored drop marker converts a drop
	// notification into a silent drop (loss of loss-of-data signal).
	// When jobs clients land in P3+, add an explicit case.

	for {
		select {
		case <-ctx.Request.Context().Done():
			return
		case <-keepAlive.C:
			if _, err := ctx.Writer.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			ctx.Writer.Flush()
		case ev, ok := <-sub.Events():
			if !ok {
				return
			}
			data, err := json.Marshal(ev)
			if err != nil {
				ctx.SSEvent("error", "encode failed: "+err.Error())
				ctx.Writer.Flush()
				continue
			}
			switch {
			case ev.Dropped != nil:
				ctx.SSEvent("events_dropped", string(data))
			case ev.Phase.IsTerminal():
				ctx.SSEvent("done", string(data))
				streamEndEmitted = true
				ctx.Writer.Flush()
				return
			default:
				ctx.SSEvent("progress", string(data))
			}
			ctx.Writer.Flush()
		}
	}
}
