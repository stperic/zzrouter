package routing

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/utils"
	"golang.org/x/sync/errgroup"
)

// maxAggregatedItems caps the total item count returned by
// aggregateResponses. A legitimate fleet at current scale (tens of
// workers, low-thousands of items each) stays well under this; exceeding
// it indicates either a dramatic fleet-size change or a misbehaving
// worker returning pathological payloads. Aggregation fails closed with
// a structured 502 on overflow rather than truncating silently.
const maxAggregatedItems = 100_000

// ============================================================================
// ClusterAwareRouter - Coordinator/Standalone Node Strategy
// ============================================================================

// ClusterAwareRouter can route to local OR cluster
// Used by coordinator and standalone nodes
//
// Design: Coordinators make routing decisions based on the target node parameter
// All local execution is done via HTTP calls to localhost internal API
// Cluster execution delegates to V1's cluster client for all the complex cluster logic
type ClusterAwareRouter struct {
	nodeName      string
	clusterClient mesh.ClusterClient
	serverPort    string // Node port for internal API calls
	localHandler  LocalHandler
	httpClient    *http.Client // Shared HTTP client for connection pooling

	// Cached local IPs to avoid syscalls on every unicast request.
	// Populated at construction time and refreshed periodically.
	localIPs   map[string]struct{}
	localIPsMu sync.RWMutex
}

// NewClusterAwareRouterWithClient creates a router with a custom HTTP client for connection pooling
func NewClusterAwareRouterWithClient(nodeName string, clusterClient mesh.ClusterClient, serverPort string, localHandler LocalHandler, httpClient *http.Client) *ClusterAwareRouter {
	if httpClient == nil {
		httpClient = &http.Client{
			Timeout: constants.HTTPDefaultTimeout,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 20,
				IdleConnTimeout:     90 * time.Second,
			},
		}
	}
	r := &ClusterAwareRouter{
		nodeName:      nodeName,
		clusterClient: clusterClient,
		serverPort:    serverPort,
		localHandler:  localHandler,
		httpClient:    httpClient,
	}
	r.localIPs = r.snapshotLocalIPs()
	return r
}

// ============================================================================
// PRIMARY OPERATIONS: Unicast and Broadcast
// ============================================================================

// Unicast sends request to ONE node (local or remote - transparent to caller)
//
// The routing layer decides internally:
//   - If host matches this node → execute locally via HTTP to localhost
//   - If host is remote → execute via cluster client
//
// The caller just says "send to host X" and doesn't care where X is.
func (r *ClusterAwareRouter) Unicast(ctx context.Context, host, path, method string, body []byte) (*Response, error) {
	hostID := constants.ParseNodeIdentifier(host)

	// Determine if this request targets the local node.
	// Local targeting conventions:
	//   - "" (empty):    no explicit target, defaults to local
	//   - "@master":     resolved to localhost by ParseNodeIdentifier
	//   - "localhost":   explicit local targeting
	//   - r.nodeName:    this node's own hostname (e.g., "gpu-server")
	//   - local IP:      any IP address bound to a local network interface
	isLocal := hostID.Nodename == "" ||
		hostID.IsMaster() ||
		hostID.Nodename == r.nodeName ||
		hostID.Nodename == "localhost" ||
		r.isLocalIP(hostID.Nodename)

	if isLocal {
		utils.LogDebugf("[Router] Unicast → LOCAL: %s %s", method, path)
		return r.localHTTPCall(ctx, &Request{Method: method, Path: path, Body: body})
	}

	utils.LogDebugf("[Router] Unicast → REMOTE %s: %s %s", hostID.Nodename, method, path)
	return r.unicast(ctx, hostID.Nodename, path, &mesh.QueryParams{
		Method: method,
		Body:   body,
	})
}

// Broadcast sends request to ALL nodes and aggregates results
// Always includes: local node + all cluster workers
func (r *ClusterAwareRouter) Broadcast(ctx context.Context, path, method string, body []byte) (*Response, error) {
	utils.LogDebugf("[Router] Broadcast: %s %s", method, path)
	return r.broadcast(ctx, path, &mesh.QueryParams{
		Method: method,
		Body:   body,
	})
}

// Route is a convenience method that chooses Unicast or Broadcast based on target node
// - Empty or "*" -> Broadcast
// - Specific node -> Unicast
func (r *ClusterAwareRouter) Route(ctx context.Context, req *Request) (*Response, error) {
	hostID := constants.ParseNodeIdentifier(req.Node)

	if hostID.Nodename == "" || hostID.IsWildcard() {
		return r.Broadcast(ctx, req.Path, req.Method, req.Body)
	}
	return r.Unicast(ctx, hostID.Nodename, req.Path, req.Method, req.Body)
}

// snapshotLocalIPs reads all local interface addresses and returns them as a set.
func (r *ClusterAwareRouter) snapshotLocalIPs() map[string]struct{} {
	ips := map[string]struct{}{
		"127.0.0.1": {},
		"::1":       {},
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ips
	}
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok {
			ips[ipnet.IP.String()] = struct{}{}
		}
	}
	return ips
}

// isLocalIP reports whether the given host (host, host:port, [v6]:port
// or bare v6) refers to this machine, using the cached local-IP set.
// Also returns true when the host parses as a loopback literal so
// bare "::1" / "127.0.0.1" bypass the cache altogether.
func (r *ClusterAwareRouter) isLocalIP(host string) bool {
	ip := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		ip = h
	}
	// Strip any remaining IPv6 brackets (SplitHostPort returns the
	// address unbracketed when a port was present; a bare "[::1]"
	// without a port falls through).
	ip = strings.TrimPrefix(ip, "[")
	ip = strings.TrimSuffix(ip, "]")

	if parsed := net.ParseIP(ip); parsed != nil && parsed.IsLoopback() {
		return true
	}

	r.localIPsMu.RLock()
	_, ok := r.localIPs[ip]
	r.localIPsMu.RUnlock()
	return ok
}

// localHTTPCall delegates to the shared makeLocalHTTPCall helper
func (r *ClusterAwareRouter) localHTTPCall(ctx context.Context, req *Request) (*Response, error) {
	return makeLocalHTTPCall(ctx, req, &localHTTPCallParams{
		localHandler:   r.localHandler,
		serverPort:     r.serverPort,
		httpClient:     r.httpClient,
		logPrefix:      "[ClusterRouter]",
		propagateTrace: true,
	})
}

// broadcast executes on local + all cluster workers, then aggregates
// Delegates to V1's cluster client which handles circuit breakers, health monitoring, etc.
func (r *ClusterAwareRouter) broadcast(ctx context.Context, path string, params *mesh.QueryParams) (*Response, error) {
	var localResp *Response
	var clusterResp *mesh.BroadcastResponse

	// 1. Execute locally (coordinator can have local models/hosts/providers too)
	// Always include local execution in broadcast
	utils.LogDebugf("[ClusterRouter] Including local execution in broadcast")
	localResp, localErr := r.localHTTPCall(ctx, &Request{
		Method:  params.Method,
		Path:    path,
		Body:    params.Body,
		Headers: params.Headers,
		Timeout: params.Timeout,
	})
	if localErr != nil {
		utils.LogDebugf("[ClusterRouter] Local execution error: %v (continuing with cluster only)", localErr)
	}

	// 2. Broadcast to cluster workers
	// V1's cluster client handles: circuit breakers, health checks, filtering offline workers
	if r.clusterClient != nil {
		utils.LogDebugf("[ClusterRouter] Broadcasting to cluster workers")
		broadcastResp, err := r.clusterClient.Broadcast(ctx, path, params)
		if err != nil {
			utils.LogDebugf("[ClusterRouter] Cluster broadcast error: %v (continuing with local only)", err)
		} else {
			clusterResp = broadcastResp
		}
	}

	// 3. Aggregate: local + cluster responses
	return r.aggregateResponses(localResp, clusterResp)
}

// unicast sends request to a specific remote node
// Delegates to V1's cluster client which handles circuit breakers, retries, etc.
func (r *ClusterAwareRouter) unicast(ctx context.Context, host, path string, params *mesh.QueryParams) (*Response, error) {
	if r.clusterClient == nil {
		return &Response{
			StatusCode: 503,
			Error:      fmt.Errorf("cluster client not available"),
		}, fmt.Errorf("no cluster client configured")
	}

	// Delegate to V1's cluster client
	clusterResp, err := r.clusterClient.Unicast(ctx, host, path, params)
	if err != nil {
		return &Response{
			StatusCode: 503,
			Error:      fmt.Errorf("unicast to %s failed: %w", host, err),
		}, err
	}

	// Convert cluster response to routing response
	// Extract hostname from SourceNode URL (e.g., "http://192.0.2.10:9090" → "192.0.2.10")
	sourceNode := clusterResp.SourceNode
	if u, err := url.Parse(sourceNode); err == nil && u.Host != "" {
		sourceNode = u.Hostname() // Just the IP/hostname without port
	}

	return &Response{
		StatusCode: clusterResp.StatusCode,
		Headers:    clusterResp.Headers,
		Body:       clusterResp.Body,
		Node:       sourceNode,
	}, nil
}

// MultiRoute routes to multiple nodes with different request bodies per node.
// Requests are dispatched concurrently for low latency. Results are returned
// in deterministic (sorted-host) order regardless of completion order.
func (r *ClusterAwareRouter) MultiRoute(
	ctx context.Context,
	path string,
	method string,
	hostBodies map[string][]byte,
) (*Response, error) {
	if len(hostBodies) == 0 {
		return &Response{
			StatusCode: 200,
			Body:       []byte(`[]`),
		}, nil
	}

	utils.LogDebugf("[ClusterRouter] MultiRoute: routing to %d nodes concurrently", len(hostBodies))

	// Sort hosts for deterministic output order
	hosts := make([]string, 0, len(hostBodies))
	for host := range hostBodies {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)

	// Fan out concurrently, collect results into index-aligned slice
	type result struct {
		host string
		resp *Response
		err  error
	}
	results := make([]result, len(hosts))

	g, gctx := errgroup.WithContext(ctx)
	for i, host := range hosts {
		results[i].host = host
		g.Go(func() error {
			utils.LogDebugf("[ClusterRouter] Routing to node '%s'", results[i].host)
			resp, err := r.Route(gctx, &Request{
				Method: method,
				Path:   path,
				Node:   results[i].host,
				Body:   hostBodies[results[i].host],
			})
			results[i].resp = resp
			results[i].err = err
			return nil // never cancel siblings; collect all results
		})
	}
	_ = g.Wait()

	// Assemble responses in deterministic order
	responses := make([]map[string]any, len(results))
	for i, r := range results {
		respData := map[string]any{
			"node":        r.host,
			"status_code": r.resp.StatusCode,
		}
		if r.err != nil {
			utils.LogDebugf("[ClusterRouter] Routing to node '%s' failed: %v", r.host, r.err)
			respData["error"] = r.err.Error()
		} else if len(r.resp.Body) > 0 {
			var bodyData any
			if err := json.Unmarshal(r.resp.Body, &bodyData); err == nil {
				respData["body"] = bodyData
			} else {
				respData["body"] = string(r.resp.Body)
			}
		}
		responses[i] = respData
	}

	body, err := json.Marshal(responses)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal multi-route responses: %w", err)
	}

	return &Response{
		StatusCode: 200,
		Body:       body,
	}, nil
}

// aggregateResponses merges local and cluster responses.
// Auto-detects the array field name ("models", "runs", "providers", etc.)
//
// This is the routing-layer aggregation, which:
// - Combines LOCAL server response with CLUSTER worker responses
// - Auto-detects API field names for proper JSON merging
// - Handles both array responses and single-object responses
//
// NOTE: The cluster layer (pkg/cluster/strategies.go) has its own simpler
// aggregation for worker-to-worker responses. These are separate by design
// to maintain routing layer isolation.
//
//nolint:gocyclo,cyclop // merge of heterogeneous response shapes; splitting forks the detect/copy flow
func (r *ClusterAwareRouter) aggregateResponses(localResp *Response, clusterResp *mesh.BroadcastResponse) (*Response, error) {
	// Auto-detect field name from first available response
	fieldName := r.detectArrayFieldName(localResp, clusterResp)

	// If no array field detected, this is a single-object response (not a list)
	// Return the first successful response without aggregation
	if fieldName == "" {
		// Helper to check if a response is successful (2xx status code)
		isSuccess := func(statusCode int) bool {
			return statusCode >= 200 && statusCode < 300
		}

		// Try local first - but only if it's a successful response
		if localResp != nil && localResp.Error == nil && len(localResp.Body) > 0 && isSuccess(localResp.StatusCode) {
			utils.LogDebugf("[ClusterRouter] Returning single-object response from local (status=%d)", localResp.StatusCode)
			return localResp, nil
		}
		// Try cluster responses - find first successful one
		if clusterResp != nil {
			for _, hostResp := range clusterResp.Responses {
				if hostResp.Response != nil && hostResp.Error == nil && len(hostResp.Response.Body) > 0 && isSuccess(hostResp.Response.StatusCode) {
					utils.LogDebugf("[ClusterRouter] Returning single-object response from %s (status=%d)", hostResp.Node, hostResp.Response.StatusCode)
					return &Response{
						StatusCode: hostResp.Response.StatusCode,
						Headers:    hostResp.Response.Headers,
						Body:       hostResp.Response.Body,
						Node:       hostResp.Node,
					}, nil
				}
			}
		}
		// No successful (2xx) responses found - return the best error we have
		// Prefer local error if available, as it's usually more specific
		if localResp != nil && localResp.Error == nil && len(localResp.Body) > 0 {
			utils.LogDebugf("[ClusterRouter] No successful responses, returning local error (status=%d)", localResp.StatusCode)
			return localResp, nil
		}
		// Return generic error
		return &Response{
			StatusCode: 404,
			Body:       []byte(`{"error": "no successful responses"}`),
			Headers:    http.Header{"Content-Type": []string{"application/json"}},
		}, nil
	}

	// Pre-allocate slice with estimated capacity. Cap aggregated size to
	// maxAggregatedItems — a fleet of 50 workers × 1000 items each is
	// 50k, so 100k leaves headroom while preventing an unbounded worker
	// response from OOMing the coordinator. Overflow collapses to a
	// structured 502 rather than truncating silently.
	estimatedSize := 0
	if localResp != nil && len(localResp.Body) > 0 {
		estimatedSize += 10
	}
	if clusterResp != nil {
		estimatedSize += len(clusterResp.Responses) * 10
	}
	allItems := make([]any, 0, estimatedSize)
	overflowed := false

	appendBounded := func(items []any, source string) {
		if overflowed {
			return
		}
		if len(allItems)+len(items) > maxAggregatedItems {
			overflowed = true
			utils.LogDebugf("[ClusterRouter] Aggregation cap (%d) exceeded; rejecting %s contribution from %s",
				maxAggregatedItems, fieldName, source)
			return
		}
		allItems = append(allItems, items...)
	}

	// 1. Add local items with defensive type assertions
	if localResp != nil && localResp.Error == nil && len(localResp.Body) > 0 {
		var localData map[string]any
		if err := json.Unmarshal(localResp.Body, &localData); err == nil {
			if items, ok := localData[fieldName].([]any); ok {
				appendBounded(items, "local")
				utils.LogDebugf("[ClusterRouter] Added %d local %s", len(items), fieldName)
			} else if localData[fieldName] != nil {
				utils.LogDebugf("[ClusterRouter] Warning: local %s field is not []interface{}, got %T", fieldName, localData[fieldName])
			}
		} else {
			utils.LogDebugf("[ClusterRouter] Warning: failed to unmarshal local response: %v", err)
		}
	}

	// 2. Add cluster items with defensive type assertions
	if clusterResp != nil {
		for _, hostResp := range clusterResp.Responses {
			if hostResp.Response != nil && hostResp.Error == nil && len(hostResp.Response.Body) > 0 {
				var data map[string]any
				if err := json.Unmarshal(hostResp.Response.Body, &data); err == nil {
					if items, ok := data[fieldName].([]any); ok {
						appendBounded(items, hostResp.Node)
						utils.LogDebugf("[ClusterRouter] Added %d %s from %s", len(items), fieldName, hostResp.Node)
					} else if data[fieldName] != nil {
						utils.LogDebugf("[ClusterRouter] Warning: %s from %s is not []interface{}, got %T", fieldName, hostResp.Node, data[fieldName])
					}
				} else {
					utils.LogDebugf("[ClusterRouter] Warning: failed to unmarshal response from %s: %v", hostResp.Node, err)
				}
			}
		}
	}

	if overflowed {
		body, _ := json.Marshal(map[string]any{
			"error": fmt.Sprintf("cluster aggregation exceeded %d items (field %q); one or more workers returned oversize payloads", maxAggregatedItems, fieldName),
			"field": fieldName,
			"cap":   maxAggregatedItems,
		})
		return &Response{
			StatusCode: http.StatusBadGateway,
			Body:       body,
			Headers:    http.Header{"Content-Type": []string{"application/json"}},
			Node:       "aggregated",
		}, nil
	}

	// 3. Return aggregated response. Envelope includes `count` alongside
	// the items so callers can distinguish "empty cluster" from "payload
	// truncated at some layer" at a glance.
	if len(allItems) == 0 {
		utils.LogDebugf("[ClusterRouter] No %s found (local or cluster)", fieldName)
		body, _ := json.Marshal(map[string]any{
			fieldName: []any{},
			"count":   0,
		})
		return &Response{
			StatusCode: http.StatusOK,
			Body:       body,
			Headers:    http.Header{"Content-Type": []string{"application/json"}},
		}, nil
	}

	finalMap := map[string]any{fieldName: allItems, "count": len(allItems)}
	body, err := json.Marshal(finalMap)
	if err != nil {
		return &Response{
			StatusCode: 500,
			Error:      fmt.Errorf("failed to marshal aggregated response: %w", err),
		}, err
	}

	utils.LogDebugf("[ClusterRouter] Aggregated total: %d %s", len(allItems), fieldName)
	return &Response{
		StatusCode: 200,
		Body:       body,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Node:       "aggregated",
	}, nil
}

// aggregatableFields are the response fields a broadcast may merge, in
// precedence order. "data" is the standard envelope from respondSuccess;
// the named fields predate it and are still emitted by several internal
// handlers.
var aggregatableFields = []string{"models", "instances", "downloads", "providers", "hosts", "data"}

// detectArrayField returns the name of the mergeable array field in a
// response body, or "" when the body is a single object rather than a
// list. A field only counts when its value is actually an array: the
// standard envelope puts single objects in "data" too, and merging those
// would corrupt them.
func detectArrayField(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var data map[string]any
	if err := json.Unmarshal(body, &data); err != nil {
		return ""
	}
	for _, field := range aggregatableFields {
		if _, ok := data[field].([]any); ok {
			return field
		}
	}
	return ""
}

// detectArrayFieldName picks the field to aggregate on, preferring the
// local response and falling back to the workers. Both sides consult the
// same field list: when they diverged, a worker-only field name silently
// dropped every worker's contribution.
func (r *ClusterAwareRouter) detectArrayFieldName(localResp *Response, clusterResp *mesh.BroadcastResponse) string {
	if localResp != nil {
		if field := detectArrayField(localResp.Body); field != "" {
			return field
		}
	}
	if clusterResp != nil {
		for _, hostResp := range clusterResp.Responses {
			if hostResp.Response == nil {
				continue
			}
			if field := detectArrayField(hostResp.Response.Body); field != "" {
				return field
			}
		}
	}
	return ""
}
