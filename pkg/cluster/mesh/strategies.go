package mesh

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"sync"
	"sync/atomic"

	"golang.org/x/sync/semaphore"

	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/utils"
)

// maxBroadcastConcurrency caps the in-flight fan-out per broadcast. A
// misbehaving cluster registry or an operator accidentally adding
// thousands of endpoints would otherwise burn through FDs / goroutines
// with one unicast per endpoint; capping at 32 keeps the coordinator's
// dispatch budget bounded while still exploiting the common fleet size
// (low tens of workers) fully in parallel.
const maxBroadcastConcurrency = 32

// broadcastSourceNode marks a response as the aggregate of a fan-out
// rather than the answer of any single node.
const broadcastSourceNode = "cluster-broadcast"

// UnicastHandler sends request to a single endpoint
type UnicastHandler struct {
	dispatcher *Dispatcher
}

// NewUnicastHandler creates a new unicast handler
func NewUnicastHandler(d *Dispatcher) *UnicastHandler {
	return &UnicastHandler{dispatcher: d}
}

// Dispatch sends request to single endpoint
func (h *UnicastHandler) Dispatch(ctx context.Context, endpoints []*Endpoint, req *Request) (*Response, error) {
	if len(endpoints) == 0 {
		return nil, fmt.Errorf("no endpoints available")
	}

	endpoint := endpoints[0]

	// Check circuit breaker
	breaker := h.dispatcher.circuitBreakers.GetBreaker(endpoint.URL)

	result, err := breaker.ExecuteAny(func() (any, error) {
		// CRITICAL OPTIMIZATION: In-process dispatch for local endpoint
		if endpoint.IsLocal || endpoint.URL == h.dispatcher.localNodeURL {
			return h.dispatchLocal(ctx, req)
		}
		return h.dispatchRemote(ctx, endpoint, req)
	})

	if err != nil {
		return nil, err
	}

	resp, ok := result.(*Response)
	if !ok {
		return nil, fmt.Errorf("internal: unicast dispatcher returned unexpected type %T", result)
	}
	return resp, nil
}

// dispatchLocal handles local in-process dispatch
func (h *UnicastHandler) dispatchLocal(ctx context.Context, req *Request) (*Response, error) {
	// Directly call the application's internal handler
	// Avoids network overhead, serialization, and deserialization
	if h.dispatcher.localHandler == nil {
		return nil, fmt.Errorf("local handler not configured")
	}

	return h.dispatcher.localHandler.ServeClusterRequest(ctx, req)
}

// dispatchRemote handles remote HTTP dispatch
func (h *UnicastHandler) dispatchRemote(ctx context.Context, endpoint *Endpoint, req *Request) (*Response, error) {
	// Safety net: if no deadline set on context, apply a default.
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, constants.ClusterActionTimeout)
		defer cancel()
	}

	// For remote endpoints, make the HTTP call. Prefer the cluster-port
	// URL for mTLS dispatch when populated (PR 3b); fall back to the
	// public URL when absent (older endpoints, tests, or derivation
	// failure). The X-Cluster-API-Key header below is harmless on mTLS
	// traffic — the worker's mTLSOUCheck ignores it; removed in PR 3c.
	base := endpoint.URL
	if endpoint.ClusterURL != "" {
		base = endpoint.ClusterURL
	}
	targetURL := base + req.Path
	if req.Query != "" {
		targetURL += "?" + req.Query
	}

	httpReq, err := http.NewRequestWithContext(ctx, req.Method, targetURL, bytes.NewReader(req.Body))
	if err != nil {
		return nil, err
	}

	// Copy headers
	maps.Copy(httpReq.Header, req.Headers)

	resp, err := h.dispatcher.connector.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := utils.ReadCapped(resp.Body, constants.MaxClusterBodySize)
	if err != nil {
		return nil, fmt.Errorf("read %s response: %w", endpoint.URL, err)
	}

	return &Response{
		StatusCode: resp.StatusCode,
		Headers:    resp.Header,
		Body:       body,
		SourceNode: endpoint.URL,
	}, nil
}

// BroadcastHandler sends request to all endpoints and aggregates results
type BroadcastHandler struct {
	dispatcher *Dispatcher
}

// NewBroadcastHandler creates a new broadcast handler
func NewBroadcastHandler(d *Dispatcher) *BroadcastHandler {
	return &BroadcastHandler{dispatcher: d}
}

// Dispatch sends request to all endpoints
func (h *BroadcastHandler) Dispatch(ctx context.Context, endpoints []*Endpoint, req *Request) (*Response, error) {
	if len(endpoints) == 0 {
		return nil, fmt.Errorf("no endpoints available")
	}

	// Filter out DOWN endpoints AND local endpoint
	// Broadcast should ONLY go to remote workers, not local master
	// Local data is handled separately by the caller
	activeEndpoints := make([]*Endpoint, 0, len(endpoints))
	for _, ep := range endpoints {
		// Skip local endpoint - caller handles local data separately
		if ep.IsLocal {
			continue
		}
		// Skip DOWN endpoints to avoid timeout delays
		if ep.Status != StatusDown {
			activeEndpoints = append(activeEndpoints, ep)
		}
	}

	// Graceful degradation: with no reachable workers the broadcast still
	// succeeds and simply carries no node responses, so the caller falls
	// back to local data instead of failing the whole request.
	if len(activeEndpoints) == 0 {
		return h.collect(nil)
	}

	// Parallel fan-out to active endpoints, capped by a semaphore so
	// an oversized endpoint list can't exhaust goroutines / FDs in one
	// broadcast. Acquire failures (ctx cancelled mid-acquire) are
	// reported as errors rather than silently skipped so upstream
	// aggregation sees the partial result.
	var wg sync.WaitGroup
	var mu sync.Mutex
	nodes := make([]*NodeResponse, 0, len(activeEndpoints))
	sem := semaphore.NewWeighted(maxBroadcastConcurrency)

	record := func(nr *NodeResponse) {
		mu.Lock()
		defer mu.Unlock()
		nodes = append(nodes, nr)
	}

	for i, endpoint := range activeEndpoints {
		if err := sem.Acquire(ctx, 1); err != nil {
			// Record the acquire failure for the current endpoint plus
			// every endpoint past it — otherwise the result would
			// silently under-report partial coverage.
			record(newNodeFailure(endpoint, fmt.Errorf("acquire slot: %w", err)))
			for j := i + 1; j < len(activeEndpoints); j++ {
				record(newNodeFailure(activeEndpoints[j], fmt.Errorf("skipped: %w", err)))
			}
			break
		}
		wg.Add(1)
		go func(ep *Endpoint) {
			defer wg.Done()
			defer sem.Release(1)
			defer utils.RecoverAndLog("mesh.BroadcastHandler.dispatch")

			unicast := NewUnicastHandler(h.dispatcher)
			resp, err := unicast.Dispatch(ctx, []*Endpoint{ep}, req)
			if err != nil {
				record(newNodeFailure(ep, err))
				return
			}
			record(&NodeResponse{Node: ep.URL, NodeName: ep.NodeName, Response: resp})
		}(endpoint)
	}

	wg.Wait()

	return h.collect(nodes)
}

// newNodeFailure records a node that could not be reached, so a partial
// broadcast is distinguishable from a node that legitimately returned
// nothing. Callers skip entries carrying an Error.
func newNodeFailure(ep *Endpoint, err error) *NodeResponse {
	return &NodeResponse{Node: ep.URL, NodeName: ep.NodeName, Error: err}
}

// collect packages one entry per node into the broadcast result.
//
// Bodies are carried VERBATIM. Only the caller knows the payload shape,
// so no layer between the worker and the caller may merge, unwrap or
// re-key them: a caller that aggregates a `{"data": [...]}` envelope
// cannot do so once an intermediate layer has stripped the envelope.
// Nodes is the authoritative in-process view; Body is its wire form.
func (h *BroadcastHandler) collect(nodes []*NodeResponse) (*Response, error) {
	// Never nil: Nodes being non-nil is what tells the client this was a
	// broadcast at all. A nil slice would send "no reachable workers" down
	// the client's single-response fallback, which would then present this
	// envelope list as though one node had returned it as its body.
	if nodes == nil {
		nodes = []*NodeResponse{}
	}

	envelopes, failed := toNodeEnvelopes(nodes)
	body, err := json.Marshal(envelopes)
	if err != nil {
		return nil, fmt.Errorf("marshal broadcast envelopes: %w", err)
	}

	return &Response{
		StatusCode: http.StatusOK,
		Headers:    make(http.Header),
		Body:       body,
		SourceNode: broadcastSourceNode,
		Nodes:      nodes,
		Metadata: map[string]any{
			"nodes_total":  len(nodes),
			"nodes_failed": failed,
		},
	}, nil
}

// nodeEnvelope is the wire form of one node's contribution to a
// broadcast: its identity plus its response exactly as it sent it.
type nodeEnvelope struct {
	Node   string          `json:"node"`
	Name   string          `json:"name,omitempty"`
	Status int             `json:"status,omitempty"`
	Body   json.RawMessage `json:"body,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// jsonOrQuoted returns a body unchanged when it is JSON, and as a JSON
// string when it is not.
//
// Dropping a non-JSON body would lose it in exactly the case an operator
// needs it: a reverse proxy answering with an HTML error page instead of
// the node. Nodes keeps the raw bytes either way; this keeps the wire
// form agreeing with it.
func jsonOrQuoted(body []byte) json.RawMessage {
	if len(body) == 0 {
		return nil
	}
	if json.Valid(body) {
		return body
	}
	quoted, err := json.Marshal(string(body))
	if err != nil {
		return nil
	}
	return quoted
}

// toNodeEnvelopes renders the wire form and reports how many nodes failed.
func toNodeEnvelopes(nodes []*NodeResponse) ([]nodeEnvelope, int) {
	out := make([]nodeEnvelope, 0, len(nodes))
	failed := 0
	for _, n := range nodes {
		env := nodeEnvelope{Node: n.Node, Name: n.NodeName}
		if n.Error != nil {
			env.Error = n.Error.Error()
			failed++
		}
		if n.Response != nil {
			env.Status = n.Response.StatusCode
			env.Body = jsonOrQuoted(n.Response.Body)
		}
		out = append(out, env)
	}
	return out, failed
}

// RoundRobinHandler load balances across endpoints
type RoundRobinHandler struct {
	dispatcher *Dispatcher
	counter    uint64
}

// NewRoundRobinHandler creates a new round-robin handler
func NewRoundRobinHandler(d *Dispatcher) *RoundRobinHandler {
	return &RoundRobinHandler{dispatcher: d}
}

// Dispatch load balances request across endpoints
func (h *RoundRobinHandler) Dispatch(ctx context.Context, endpoints []*Endpoint, req *Request) (*Response, error) {
	if len(endpoints) == 0 {
		return nil, fmt.Errorf("no endpoints available")
	}

	// Filter to only UP endpoints (skip DOWN to avoid timeout delays)
	healthyEndpoints := make([]*Endpoint, 0)
	for _, ep := range endpoints {
		if ep.Status == StatusUp {
			healthyEndpoints = append(healthyEndpoints, ep)
		}
	}

	// Fail fast if no healthy endpoints available
	// Health monitor will handle reconnection attempts
	if len(healthyEndpoints) == 0 {
		return nil, fmt.Errorf("no healthy endpoints available (all marked as DOWN)")
	}

	// Round-robin selection from healthy endpoints
	idx := atomic.AddUint64(&h.counter, 1) % uint64(len(healthyEndpoints))
	endpoint := healthyEndpoints[idx]

	// Use unicast handler
	unicast := NewUnicastHandler(h.dispatcher)
	return unicast.Dispatch(ctx, []*Endpoint{endpoint}, req)
}

// FailoverHandler tries endpoints in order until one succeeds
type FailoverHandler struct {
	dispatcher *Dispatcher
}

// NewFailoverHandler creates a new failover handler
func NewFailoverHandler(d *Dispatcher) *FailoverHandler {
	return &FailoverHandler{dispatcher: d}
}

// Dispatch tries endpoints in order until success
func (h *FailoverHandler) Dispatch(ctx context.Context, endpoints []*Endpoint, req *Request) (*Response, error) {
	if len(endpoints) == 0 {
		return nil, fmt.Errorf("no endpoints available")
	}

	var lastErr error
	unicast := NewUnicastHandler(h.dispatcher)

	// Skip DOWN endpoints
	for _, endpoint := range endpoints {
		if endpoint.Status == StatusDown {
			continue
		}

		resp, err := unicast.Dispatch(ctx, []*Endpoint{endpoint}, req)
		if err == nil {
			return resp, nil
		}
		lastErr = err
	}

	return nil, fmt.Errorf("all endpoints failed: %w", lastErr)
}
