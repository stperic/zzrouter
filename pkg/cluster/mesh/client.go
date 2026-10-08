package mesh

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// ClusterClient provides high-level cluster operations with automatic routing
type ClusterClient interface {
	// Query automatically routes based on host parameter
	// - Empty/wildcard host: broadcast to all
	// - "localhost"/@master: local only
	// - Specific host: unicast to that host
	Query(ctx context.Context, endpoint string, params *QueryParams) (*Response, error)

	// Broadcast sends request to all hosts and aggregates responses
	Broadcast(ctx context.Context, endpoint string, params *QueryParams) (*BroadcastResponse, error)

	// Unicast sends request to specific host
	Unicast(ctx context.Context, host string, endpoint string, params *QueryParams) (*Response, error)

	// Stream opens streaming connection to specific host
	Stream(ctx context.Context, host string, endpoint string, params *QueryParams) (io.ReadCloser, error)
}

// QueryParams unified parameter structure
type QueryParams struct {
	Method  string      // HTTP method (GET, POST, DELETE, etc.)
	Query   url.Values  // Query parameters
	Body    []byte      // Request body
	Headers http.Header // HTTP headers
	Timeout time.Duration
}

// BroadcastResponse aggregates responses from multiple hosts
type BroadcastResponse struct {
	Responses []*NodeResponse
	Errors    []error
}

// NodeResponse represents response from a single host
type NodeResponse struct {
	Node     string
	NodeName string
	Response *Response
	Error    error
}

// DefaultClusterClient implements ClusterClient
type DefaultClusterClient struct {
	cluster      *Cluster
	localHandler LocalHandler
	localNodeURL string
}

// NewClusterClient creates a new ClusterClient
func NewClusterClient(cluster *Cluster, localHandler LocalHandler, localNodeURL string) ClusterClient {
	return &DefaultClusterClient{
		cluster:      cluster,
		localHandler: localHandler,
		localNodeURL: localNodeURL,
	}
}

// Query automatically routes based on the `node` query parameter.
func (c *DefaultClusterClient) Query(ctx context.Context, endpoint string, params *QueryParams) (*Response, error) {
	if params == nil {
		params = &QueryParams{}
	}

	// Extract target node from query
	host := params.Query.Get("node")

	// Determine routing strategy
	if host == "" || host == "*" {
		// Broadcast to all hosts
		broadcastResp, err := c.Broadcast(ctx, endpoint, params)
		if err != nil {
			return nil, err
		}
		// Return first successful response from broadcast
		if len(broadcastResp.Responses) == 0 {
			return nil, fmt.Errorf("no responses from broadcast")
		}
		for _, hostResp := range broadcastResp.Responses {
			if hostResp.Error == nil && hostResp.Response != nil {
				return hostResp.Response, nil
			}
		}
		return nil, fmt.Errorf("all broadcast responses failed")
	}

	if host == "localhost" || host == "@master" {
		// Local query only
		return c.queryLocal(ctx, endpoint, params)
	}

	// Unicast to specific host
	return c.Unicast(ctx, host, endpoint, params)
}

// Broadcast sends request to all hosts
func (c *DefaultClusterClient) Broadcast(ctx context.Context, endpoint string, params *QueryParams) (*BroadcastResponse, error) {
	if params == nil {
		params = &QueryParams{}
	}

	// Workers will automatically return their own local data
	// Build cluster request
	var queryString string
	if params.Query != nil {
		queryString = params.Query.Encode()
	}

	clusterReq := &Request{
		Method:   params.Method,
		Path:     endpoint,
		Query:    queryString,
		Body:     params.Body,
		Headers:  params.Headers,
		Strategy: StrategyBroadcast,
	}

	// Execute via cluster
	resp, err := c.cluster.HandleRequest(ctx, clusterReq)
	if err != nil {
		return nil, fmt.Errorf("broadcast failed: %w", err)
	}

	// Parse broadcast response
	return c.parseBroadcastResponse(resp)
}

// Unicast sends request to specific host
func (c *DefaultClusterClient) Unicast(ctx context.Context, host string, endpoint string, params *QueryParams) (*Response, error) {
	if params == nil {
		params = &QueryParams{}
	}

	// Build cluster request
	clusterReq := &Request{
		Method:     params.Method,
		Path:       endpoint,
		Query:      params.Query.Encode(),
		Body:       params.Body,
		Headers:    params.Headers,
		Strategy:   StrategyUnicast,
		TargetNode: host,
	}

	// Execute via cluster
	resp, err := c.cluster.HandleRequest(ctx, clusterReq)
	if err != nil {
		return nil, fmt.Errorf("unicast to %s failed: %w", host, err)
	}

	return &Response{
		StatusCode: resp.StatusCode,
		Body:       resp.Body,
		Headers:    resp.Headers,
		SourceNode: resp.SourceNode, // Preserve source host for routing
	}, nil
}

// Stream opens streaming connection
func (c *DefaultClusterClient) Stream(ctx context.Context, host string, endpoint string, params *QueryParams) (io.ReadCloser, error) {
	// TODO: Implement streaming support
	return nil, fmt.Errorf("streaming not yet implemented")
}

// queryLocal executes query on local host
func (c *DefaultClusterClient) queryLocal(ctx context.Context, endpoint string, params *QueryParams) (*Response, error) {
	// Build request for local handler
	clusterReq := &Request{
		Method:  params.Method,
		Path:    endpoint,
		Query:   params.Query.Encode(),
		Body:    params.Body,
		Headers: params.Headers,
	}

	// Execute locally
	resp, err := c.localHandler.ServeClusterRequest(ctx, clusterReq)
	if err != nil {
		return nil, fmt.Errorf("local query failed: %w", err)
	}

	return &Response{
		StatusCode: resp.StatusCode,
		Body:       resp.Body,
		Headers:    resp.Headers,
	}, nil
}

// parseBroadcastResponse lifts the per-node results out of a broadcast.
//
// BroadcastHandler already carries them typed on Response.Nodes, so this
// is a hand-off rather than a parse: the node bodies are never decoded
// here, because only the caller knows their shape.
func (c *DefaultClusterClient) parseBroadcastResponse(resp *Response) (*BroadcastResponse, error) {
	if resp == nil {
		return &BroadcastResponse{}, nil
	}

	// A non-broadcast strategy answered (single node, or a local-only
	// cluster). Present it as a one-node result so callers keep a single
	// code path.
	if resp.Nodes == nil {
		return &BroadcastResponse{
			Responses: []*NodeResponse{{Node: resp.SourceNode, Response: resp}},
		}, nil
	}

	out := &BroadcastResponse{Responses: resp.Nodes}
	for _, n := range resp.Nodes {
		if n.Error != nil {
			out.Errors = append(out.Errors, n.Error)
		}
	}
	return out, nil
}

// Helper methods for Response

// IsSuccess returns true if status code is 2xx
func (r *Response) IsSuccess() bool {
	return r.StatusCode >= 200 && r.StatusCode < 300
}

// String returns response body as string
func (r *Response) String() string {
	return string(r.Body)
}
