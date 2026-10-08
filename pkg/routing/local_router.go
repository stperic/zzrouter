package routing

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"time"

	"github.com/stperic/zzrouter/pkg/constants"
)

// ============================================================================
// LocalOnlyRouter - Worker Node Strategy
// ============================================================================

// LocalOnlyRouter serves ONLY local data
// Used by pure worker nodes that cannot initiate cluster communication
//
// Design: Workers are simple - they just serve their local models via HTTP calls to localhost
// No routing logic, no mode checks, no complexity
type LocalOnlyRouter struct {
	nodeName     string
	serverPort   string // Node port for internal API calls
	localHandler LocalHandler
	httpClient   *http.Client // Shared HTTP client for connection pooling
}

// NewLocalOnlyRouterWithClient creates a router with a custom HTTP client for connection pooling
func NewLocalOnlyRouterWithClient(nodeName, serverPort string, localHandler LocalHandler, httpClient *http.Client) *LocalOnlyRouter {
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
	return &LocalOnlyRouter{
		nodeName:     nodeName,
		serverPort:   serverPort,
		localHandler: localHandler,
		httpClient:   httpClient,
	}
}

// ============================================================================
// PRIMARY OPERATIONS: Unicast and Broadcast
// ============================================================================

// Unicast on a worker always executes locally
// Workers cannot route to other nodes - the coordinator handles that
func (r *LocalOnlyRouter) Unicast(ctx context.Context, host, path, method string, body []byte) (*Response, error) {
	log.Printf("[LocalRouter] Unicast → LOCAL: %s %s (workers always execute locally)", method, path)
	return r.localHTTPCall(ctx, &Request{Method: method, Path: path, Body: body})
}

// Broadcast on a worker just returns local data
// Workers don't aggregate - they just serve their own data
func (r *LocalOnlyRouter) Broadcast(ctx context.Context, path, method string, body []byte) (*Response, error) {
	log.Printf("[LocalRouter] Broadcast → LOCAL: %s %s (workers return local data only)", method, path)
	return r.localHTTPCall(ctx, &Request{Method: method, Path: path, Body: body})
}

// Route is a convenience method - on workers, always executes locally
func (r *LocalOnlyRouter) Route(ctx context.Context, req *Request) (*Response, error) {
	return r.Unicast(ctx, req.Node, req.Path, req.Method, req.Body)
}

// ============================================================================
// Helper Functions
// ============================================================================

// MultiRoute executes multiple requests locally
// For LocalOnlyRouter, all requests must be for the local host
// Returns error if any request targets a remote host
func (r *LocalOnlyRouter) MultiRoute(
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

	log.Printf("[LocalRouter] MultiRoute: processing %d requests", len(hostBodies))

	// Sort hosts for deterministic order
	hosts := make([]string, 0, len(hostBodies))
	for host := range hostBodies {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)

	// Execute each request
	responses := make([]map[string]any, 0, len(hosts))

	for _, host := range hosts {
		body := hostBodies[host]

		// Validate this is a local request
		// Accept empty string, @master, or matching node name
		isLocal := host == "" || host == "@master" || host == r.nodeName

		respData := map[string]any{
			"node":        host,
			"status_code": 503,
		}

		if !isLocal {
			log.Printf("[LocalRouter] Cannot route to remote host '%s' (no cluster support)", host)
			respData["error"] = fmt.Sprintf("cannot route to remote host '%s': cluster not configured", host)
			responses = append(responses, respData)
			continue
		}

		// Execute locally. Route may return (nil, err) on catastrophic
		// failure (context cancelled before dispatch, handler panic); guard
		// the nil check before reading resp.StatusCode so a transport
		// failure doesn't panic the aggregator.
		resp, err := r.Route(ctx, &Request{
			Method: method,
			Path:   path,
			Node:   "@local",
			Body:   body,
		})

		if err != nil {
			respData["error"] = err.Error()
			if resp != nil {
				respData["status_code"] = resp.StatusCode
			}
		} else if resp != nil {
			respData["status_code"] = resp.StatusCode
			if len(resp.Body) > 0 {
				var bodyData any
				if jerr := json.Unmarshal(resp.Body, &bodyData); jerr == nil {
					respData["body"] = bodyData
				} else {
					respData["body"] = string(resp.Body)
				}
			}
		}

		responses = append(responses, respData)
	}

	// Marshal all responses
	body, err := json.Marshal(responses)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal multi-route responses: %w", err)
	}

	return &Response{
		StatusCode: 200,
		Body:       body,
	}, nil
}

// localHTTPCall delegates to the shared makeLocalHTTPCall helper
func (r *LocalOnlyRouter) localHTTPCall(ctx context.Context, req *Request) (*Response, error) {
	return makeLocalHTTPCall(ctx, req, &localHTTPCallParams{
		localHandler:   r.localHandler,
		serverPort:     r.serverPort,
		httpClient:     r.httpClient,
		logPrefix:      "[LocalRouter]",
		propagateTrace: false,
	})
}
