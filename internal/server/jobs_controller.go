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
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/utils"
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
	// peerProbeTimeout bounds fan-out so callers can retry an incomplete lookup.
	peerProbeTimeout = 2 * time.Second
	// jobLookupRetryAfter spaces retries when a possible owner did not answer.
	jobLookupRetryAfter = 5 * time.Second
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
	// coord knows. Used by job lookup fan-out when no ?node=
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
	pagination, ok := ParsePagination(ctx)
	if !ok {
		return
	}
	if c.handleRemote(ctx, func(ctx *gin.Context, node string) { c.proxyList(ctx, node, pagination) }) {
		return
	}
	respondJobList(ctx, c.registry.List(jobs.Kind(ctx.Query("kind"))), c.nodeName, pagination)
}

// respondJobList writes the collection envelope shared by every list
// endpoint on the management surface. Both the local answer and the
// ?node=<peer> answer go through here, so the public shape of GET /jobs
// no longer depends on which node happens to own the job.
func respondJobList(ctx *gin.Context, events []jobs.Event, node string, pagination PaginationParams) {
	slices.SortFunc(events, func(a, b jobs.Event) int {
		if order := b.At.Compare(a.At); order != 0 {
			return order
		}
		return strings.Compare(a.JobID, b.JobID)
	})
	page, total, hasMore := ApplyPagination(events, pagination)
	respondListWithMetadata(ctx, page, total, hasMore, map[string]any{"node": node})
}

// GetJob returns the latest event snapshot for a single job. When
// ?node=<remote>, forwards the JSON reply from the owning worker. When
// no node hint is provided and the job isn't local, fans out across
// known peers (first 200 wins) so agents don't have to track owners.
func (c *JobsController) GetJob(ctx *gin.Context) {
	if c.handleRemote(ctx, c.proxyOwnedJob) {
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
	if owner, ok := c.locatePeerJob(ctx, id); ok {
		// Same re-wrap as the ?node= path: a caller who let the
		// coordinator find the owner must not get a different shape
		// than one who named it.
		c.proxyOwnedJob(ctx, owner)
	}
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
	if owner, ok := c.locatePeerJob(ctx, id); ok {
		c.proxyStream(ctx, owner)
	}
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
		// A coordinator asked to cancel a worker's job finds nothing locally;
		// a bare 404 here read as "already gone" while the work continued.
		if owner, ok := c.locatePeerJob(ctx, id); ok {
			c.proxyCancel(ctx, owner)
		}
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
		respondJobNotFound(ctx, c.nodeName)
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
		respondJobNotFound(ctx, c.nodeName)
		return
	}
	ctx.JSON(http.StatusAccepted, gin.H{"accepted": true})
}

// --- helpers ---

var jobOutcomeTools = map[jobs.Kind]string{
	jobs.KindInstall:  "zz_providers",
	jobs.KindService:  "zz_providers",
	jobs.KindDownload: "zz_models",
	jobs.KindRun:      "zz_runs",
	jobs.KindUpdate:   "zz_update_status",
}

func respondJobNotFound(ctx *gin.Context, node string) {
	hint := "The outcome is unavailable here; inspect the operation's current state."
	tool := jobOutcomeTools[jobs.KindFromID(ctx.Param("id"))]
	if tool != "" {
		hint = "Inspect the outcome with " + tool + "."
	}
	RespondWithProblemOpts(ctx, http.StatusNotFound, "Not Found",
		fmt.Sprintf("job not found within the requested lookup scope; jobs live in memory on the node that runs them and may be absent after retention cleanup or a node restart; retention lasts %g hours after completion or the latest hand-out, whichever is later", constants.IdempotencyReplayWindow.Hours()),
		ProblemOpts{
			Code: string(httperr.CodeJobNotFound), Node: node,
			Errors: []utils.ParamError{{
				Key: "job_id", Code: string(httperr.CodeJobNotFound),
				Message: "Job outcome unavailable", Hint: hint,
			}},
		})
	ctx.Abort()
}

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

// locatePeerJob names the peer holding a job missing locally, or answers the miss itself and returns ok=false.
func (c *JobsController) locatePeerJob(ctx *gin.Context, id string) (owner string, ok bool) {
	owner, complete := c.findJobOwner(ctx, id)
	if owner != "" {
		return owner, true
	}
	if complete {
		respondJobNotFound(ctx, "")
	} else {
		httperr.SetRetryAfter(ctx.Writer.Header(), jobLookupRetryAfter)
		RespondWithProblemOpts(ctx, http.StatusServiceUnavailable, "Service Unavailable",
			"a peer that may own the job did not answer; retry after the indicated delay",
			ProblemOpts{Code: string(httperr.CodeNodeUnreachable)})
	}
	ctx.Abort()
	return "", false
}

// findJobOwner reports complete absence only when every known peer answers 404.
func (c *JobsController) findJobOwner(ctx *gin.Context, id string) (owner string, complete bool) {
	if c.proxy == nil || c.proxy.ListPeers == nil || c.proxy.ResolveEndpoint == nil {
		return "", true
	}
	peers := c.proxy.ListPeers()
	if len(peers) == 0 {
		return "", true
	}
	probeCtx, cancel := context.WithTimeout(ctx.Request.Context(), peerProbeTimeout)
	defer cancel()

	type result struct {
		node     string
		complete bool
	}
	hits := make(chan result, len(peers))
	for _, peer := range peers {
		go func(node string) {
			var r result
			defer func() { hits <- r }()
			ep := c.proxy.ResolveEndpoint(node)
			if ep == nil {
				return
			}
			clusterURL, err := c.proxy.endpointURL(ep)
			if err != nil {
				return
			}
			resp, err := c.proxy.open(probeCtx, http.MethodGet,
				internalJobsURL(clusterURL, "/"+url.PathEscape(id), nil), "application/json")
			if err != nil {
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				r.node = node
			}
			r.complete = resp.StatusCode == http.StatusNotFound
		}(peer)
	}
	complete = true
	for range peers {
		select {
		case r := <-hits:
			if r.node != "" {
				return r.node, false
			}
			complete = complete && r.complete
		case <-probeCtx.Done():
			return "", false
		}
	}
	return "", complete
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

// readUpstreamJSON owns bounded body reading and refusal handling before any response headers.
func readUpstreamJSON(ctx *gin.Context, node string, resp *http.Response, successStatus int) ([]byte, bool) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, upstreamBodyLimit+1))
	if errors.Is(err, context.Canceled) || errors.Is(ctx.Request.Context().Err(), context.Canceled) {
		return nil, false
	}
	if resp.StatusCode >= http.StatusBadRequest {
		if err != nil || len(body) > upstreamBodyLimit {
			body = nil // Preserve the refusal status without forwarding a partial document.
		}
		RespondToError(ctx, parseRoutedError(resp.StatusCode, body, resp.Header))
		return nil, false
	}
	if err != nil || len(body) > upstreamBodyLimit || resp.StatusCode != successStatus {
		RespondToError(ctx, newRouteProblemError(http.StatusBadGateway, "Bad Gateway",
			"upstream returned an incomplete or unexpected response", httperr.CodeUpstreamShape, node))
		return nil, false
	}
	return body, true
}

// upstreamURL resolves node → cluster-port base URL and builds the
// upstream /internal/jobs<suffix> path with optional query forwarding.
// The canonical owner accompanies the URL so aliases never become response identity.
func (c *JobsController) upstreamURL(ctx *gin.Context, node, pathSuffix string, forwardQuery bool) (string, string, bool) {
	ep := c.proxy.ResolveEndpoint(node)
	if ep == nil {
		RespondWithProblemOpts(ctx, http.StatusNotFound, "Not Found",
			"unknown node; pass ?node=<nodename|alias|endpoint-url>",
			ProblemOpts{Code: "node_unknown", Node: node})
		ctx.Abort()
		return "", "", false
	}
	base, err := c.proxy.endpointURL(ep)
	if err != nil {
		RespondWithProblemOpts(ctx, http.StatusBadGateway, "Bad Gateway", "derive cluster URL: "+err.Error(),
			ProblemOpts{Code: string(httperr.CodeNodeUnreachable), Node: node})
		ctx.Abort()
		return "", "", false
	}
	var src url.Values
	if forwardQuery {
		src = ctx.Request.URL.Query()
	}
	return internalJobsURL(base, pathSuffix, src), ep.NodeName, true
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
func (c *JobsController) proxyList(ctx *gin.Context, node string, pagination PaginationParams) {
	c.proxyRewrapped(ctx, node, http.MethodGet, "", true, http.StatusOK,
		func(owner string, body []byte) error {
			var page struct {
				Node string       `json:"node"`
				Jobs []jobs.Event `json:"jobs"`
			}
			if err := json.Unmarshal(body, &page); err != nil {
				return err
			}
			if page.Jobs == nil || owner == "" || (page.Node != "" && page.Node != owner) {
				return errors.New("missing or mismatched peer job list")
			}
			for _, ev := range page.Jobs {
				if ev.Node != owner || ev.Validate() != nil {
					return errors.New("missing or mismatched peer job identity")
				}
			}
			if page.Node == "" {
				page.Node = owner
			}
			respondJobList(ctx, page.Jobs, page.Node, pagination)
			return nil
		})
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
// Refusals use the canonical routed-error responder.
func (c *JobsController) proxyRewrapped(ctx *gin.Context, node, method, pathSuffix string,
	forwardQuery bool, successStatus int, rewrap func(owner string, body []byte) error) {
	upstream, owner, ok := c.upstreamURL(ctx, node, pathSuffix, forwardQuery)
	if !ok {
		return
	}
	resp, err := c.proxy.open(ctx.Request.Context(), method, upstream, "application/json")
	if err != nil {
		c.writeProxyError(ctx, node, err)
		return
	}
	defer resp.Body.Close()

	body, ok := readUpstreamJSON(ctx, node, resp, successStatus)
	if !ok {
		return
	}
	if rewrap(owner, body) == nil {
		return
	}
	RespondToError(ctx, newRouteProblemError(http.StatusBadGateway, "Bad Gateway",
		"upstream returned a response this endpoint could not parse, which usually means peer version skew",
		httperr.CodeUpstreamShape, node))
}

// proxyOwnedJob fetches one job from its owner and answers in the
// public envelope. Both GetJob routes reach it — the explicit ?node=
// hint and the owner fan-out — so neither can drift into its own shape.
func (c *JobsController) proxyOwnedJob(ctx *gin.Context, node string) {
	id := ctx.Param("id")
	c.proxyRewrapped(ctx, node, http.MethodGet, "/"+url.PathEscape(id), true, http.StatusOK,
		func(owner string, body []byte) error {
			var ev jobs.Event
			if err := json.Unmarshal(body, &ev); err != nil {
				return fmt.Errorf("decode peer job: %w", err)
			}
			if owner == "" || ev.JobID != id || ev.Node != owner || ev.Validate() != nil {
				return errors.New("missing or mismatched peer job identity")
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
	c.proxyRewrapped(ctx, node, http.MethodPost, "/"+url.PathEscape(id)+"/cancel", false, http.StatusAccepted,
		func(_ string, body []byte) error {
			var reply struct {
				Accepted bool `json:"accepted"`
			}
			if err := json.Unmarshal(body, &reply); err != nil {
				return err
			}
			if !reply.Accepted {
				return errors.New("peer did not accept cancellation")
			}
			// The peer answers {"accepted":true} with no job_id. Local
			// cancel carries it, and a caller cancelling several jobs
			// needs it to tell the replies apart, so restore it here.
			ctx.JSON(http.StatusAccepted, gin.H{"accepted": true, "job_id": id})
			return nil
		})
}

// writeProxyError maps an upstream dispatch failure to an HTTP
// response. Safe to call only before any bytes have been written.
func (c *JobsController) writeProxyError(ctx *gin.Context, node string, err error) {
	if errors.Is(err, context.Canceled) {
		return // client gave up; nothing to write
	}
	RespondToError(ctx, newRoutedTransportError(err, node))
	ctx.Abort()
}

// proxyStream forwards GET /jobs/:id/stream to the owning worker's
// internal SSE endpoint. Frames are copied byte-for-byte; blank-line
// boundaries trigger a flush so events are not coalesced by transport
// buffering. The client's request context propagates to the upstream
// request so a client disconnect cancels the upstream read.
func (c *JobsController) proxyStream(ctx *gin.Context, node string) {
	upstream, _, ok := c.upstreamURL(ctx, node, "/"+url.PathEscape(ctx.Param("id"))+"/stream", true)
	if !ok {
		return
	}

	resp, err := c.proxy.open(ctx.Request.Context(), http.MethodGet, upstream, "text/event-stream")
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
		readUpstreamJSON(ctx, node, resp, http.StatusOK)
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
		respondJobNotFound(ctx, c.nodeName)
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
