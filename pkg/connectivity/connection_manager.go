package connectivity

import (
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/stperic/zzrouter/pkg/constants"
)

// ConnectionPoolStats contains metrics about HTTP connection pool utilization
type ConnectionPoolStats struct {
	// Regular HTTP client stats
	HTTPActiveRequests int64 `json:"http_active_requests"`
	HTTPTotalRequests  int64 `json:"http_total_requests"`
	HTTPMaxIdleConns   int   `json:"http_max_idle_conns"`
	HTTPMaxIdlePerNode int   `json:"http_max_idle_per_host"`

	// Streaming client stats
	StreamActiveRequests int64 `json:"stream_active_requests"`
	StreamTotalRequests  int64 `json:"stream_total_requests"`
	StreamMaxIdleConns   int   `json:"stream_max_idle_conns"`
	StreamMaxIdlePerNode int   `json:"stream_max_idle_per_host"`
}

// metricsRoundTripper wraps an http.RoundTripper to track active requests.
// Active count is decremented when the response body is closed (not when
// headers arrive), so streaming connections are tracked accurately.
type metricsRoundTripper struct {
	transport      http.RoundTripper
	activeRequests *atomic.Int64
	totalRequests  *atomic.Int64
}

// RoundTrip implements http.RoundTripper with metrics tracking
func (m *metricsRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	m.activeRequests.Add(1)
	m.totalRequests.Add(1)

	resp, err := m.transport.RoundTrip(req)
	if err != nil {
		m.activeRequests.Add(-1)
		return nil, err
	}

	// Wrap the body so the active count is decremented on Close,
	// not when headers arrive. This tracks streaming duration accurately.
	resp.Body = &trackingBody{
		ReadCloser: resp.Body,
		active:     m.activeRequests,
	}
	return resp, nil
}

// trackingBody wraps a response body to decrement the active request counter on Close.
type trackingBody struct {
	io.ReadCloser
	active *atomic.Int64
	closed atomic.Bool
}

func (t *trackingBody) Close() error {
	if !t.closed.CompareAndSwap(false, true) {
		return nil // already closed
	}
	t.active.Add(-1)
	return t.ReadCloser.Close()
}

// ConnectionManager provides centralized HTTP connection management
// This is the SINGLE SOURCE OF TRUTH for all HTTP connections in zzRouter
type ConnectionManager struct {
	// Shared HTTP clients with optimized connection pooling
	httpClient      *http.Client // For regular requests (30s timeout)
	streamingClient *http.Client // For streaming operations (no timeout)

	// Metrics tracking
	httpMetrics   metricsRoundTripper
	streamMetrics metricsRoundTripper

	// Pool configuration (for stats reporting)
	httpMaxIdleConns     int
	httpMaxIdlePerNode   int
	streamMaxIdleConns   int
	streamMaxIdlePerNode int
}

// ConnectionManagerOption configures a ConnectionManager.
type ConnectionManagerOption func(*ConnectionManager, *http.Transport, *http.Transport)

// WithTLSConfig sets the TLS configuration for outbound connections.
func WithTLSConfig(cfg *tls.Config) ConnectionManagerOption {
	return func(_ *ConnectionManager, httpTransport, streamTransport *http.Transport) {
		if cfg != nil {
			httpTransport.TLSClientConfig = cfg
			streamTransport.TLSClientConfig = cfg
		}
	}
}

// NewConnectionManager creates a centralized connection manager
func NewConnectionManager(opts ...ConnectionManagerOption) *ConnectionManager {
	// Pool configuration constants
	const (
		httpMaxIdleConns     = 100
		httpMaxIdlePerNode   = 20
		streamMaxIdleConns   = 100
		streamMaxIdlePerNode = 30
	)

	// Create transports
	httpTransport := &http.Transport{
		MaxIdleConns:          httpMaxIdleConns,
		MaxIdleConnsPerHost:   httpMaxIdlePerNode,
		IdleConnTimeout:       constants.HTTPLongTimeout,
		TLSHandshakeTimeout:   3 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableKeepAlives:     false,
		ForceAttemptHTTP2:     false,
		DialContext: (&net.Dialer{
			Timeout:   constants.ClusterHealthCheckTimeout,
			KeepAlive: constants.HealthCheckInterval,
		}).DialContext,
	}

	streamTransport := &http.Transport{
		MaxIdleConns:          streamMaxIdleConns,
		MaxIdleConnsPerHost:   streamMaxIdlePerNode,
		IdleConnTimeout:       180 * time.Second,
		TLSHandshakeTimeout:   3 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableKeepAlives:     false,
		ForceAttemptHTTP2:     false,
		ResponseHeaderTimeout: 0,
		DialContext: (&net.Dialer{
			Timeout:   constants.ClusterHealthCheckTimeout,
			KeepAlive: constants.HealthCheckInterval,
		}).DialContext,
	}

	cm := &ConnectionManager{
		httpMaxIdleConns:     httpMaxIdleConns,
		httpMaxIdlePerNode:   httpMaxIdlePerNode,
		streamMaxIdleConns:   streamMaxIdleConns,
		streamMaxIdlePerNode: streamMaxIdlePerNode,
	}

	// Apply options (e.g., TLS config) before wrapping in metrics
	for _, opt := range opts {
		opt(cm, httpTransport, streamTransport)
	}

	// Initialize metrics counters
	cm.httpMetrics = metricsRoundTripper{
		transport:      httpTransport,
		activeRequests: &atomic.Int64{},
		totalRequests:  &atomic.Int64{},
	}
	cm.streamMetrics = metricsRoundTripper{
		transport:      streamTransport,
		activeRequests: &atomic.Int64{},
		totalRequests:  &atomic.Int64{},
	}

	// Create clients with metrics-tracking transports
	cm.httpClient = &http.Client{
		Timeout:   constants.HTTPDefaultTimeout,
		Transport: &cm.httpMetrics,
	}
	cm.streamingClient = &http.Client{
		Timeout:   0,
		Transport: &cm.streamMetrics,
	}

	return cm
}

// GetHTTPClient returns the shared HTTP client for regular requests
func (cm *ConnectionManager) GetHTTPClient() *http.Client {
	return cm.httpClient
}

// GetStreamingClient returns the shared HTTP client for streaming operations
func (cm *ConnectionManager) GetStreamingClient() *http.Client {
	return cm.streamingClient
}

// GetStats returns connection pool utilization statistics
func (cm *ConnectionManager) GetStats() ConnectionPoolStats {
	return ConnectionPoolStats{
		HTTPActiveRequests:   cm.httpMetrics.activeRequests.Load(),
		HTTPTotalRequests:    cm.httpMetrics.totalRequests.Load(),
		HTTPMaxIdleConns:     cm.httpMaxIdleConns,
		HTTPMaxIdlePerNode:   cm.httpMaxIdlePerNode,
		StreamActiveRequests: cm.streamMetrics.activeRequests.Load(),
		StreamTotalRequests:  cm.streamMetrics.totalRequests.Load(),
		StreamMaxIdleConns:   cm.streamMaxIdleConns,
		StreamMaxIdlePerNode: cm.streamMaxIdlePerNode,
	}
}
