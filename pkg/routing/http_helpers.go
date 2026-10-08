package routing

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/observability"
	"github.com/stperic/zzrouter/pkg/utils"
)

// localHTTPCallParams holds the parameters needed for a local HTTP call.
// This allows both LocalOnlyRouter and ClusterAwareRouter to share the same
// implementation while passing their specific fields.
type localHTTPCallParams struct {
	localHandler   LocalHandler
	serverPort     string
	httpClient     *http.Client
	logPrefix      string // e.g., "[LocalRouter]" or "[ClusterRouter]"
	propagateTrace bool   // Whether to inject OpenTelemetry trace context
}

// makeLocalHTTPCall makes an HTTP call to the local internal API with cluster auth.
// Shared implementation used by both LocalOnlyRouter and ClusterAwareRouter.
func makeLocalHTTPCall(ctx context.Context, req *Request, params *localHTTPCallParams) (*Response, error) {
	// If local handler is provided, use it directly (in-process)
	if params.localHandler != nil {
		utils.LogDebugf("%s Calling local handler directly (in-process)", params.logPrefix)
		return params.localHandler(ctx, req)
	}

	url := fmt.Sprintf("http://localhost:%s%s", params.serverPort, req.Path)
	utils.LogDebugf("%s Calling local internal API: %s", params.logPrefix, url)

	// Create HTTP request with body if provided
	var bodyReader io.Reader
	if len(req.Body) > 0 {
		bodyReader = bytes.NewReader(req.Body)
	}

	httpReq, err := http.NewRequestWithContext(ctx, req.Method, url, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("failed to create HTTP request: %w", err)
	}

	// Set Content-Type for JSON requests
	if len(req.Body) > 0 {
		httpReq.Header.Set("Content-Type", "application/json")
	}

	// Propagate trace context if enabled (used by ClusterAwareRouter)
	if params.propagateTrace {
		observability.InjectTraceContext(ctx, httpReq.Header)
	}

	// Use shared HTTP client for connection pooling
	httpResp, err := params.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer func() { _ = httpResp.Body.Close() }()

	// Read response body capped to guard against OOM from malicious /
	// runaway workers. utils.ReadCapped returns ErrResponseTooLarge on
	// oversize rather than silently truncating — dispatch propagates
	// the error so callers don't treat a cut-off body as complete.
	body, err := utils.ReadCapped(httpResp.Body, constants.MaxClusterBodySize)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}

	return &Response{
		StatusCode: httpResp.StatusCode,
		Headers:    httpResp.Header,
		Body:       body,
		Node:       "localhost", // Local calls always return "localhost"
	}, nil
}
