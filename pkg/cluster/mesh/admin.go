package mesh

import (
	"context"
	"errors"
	"sync"

	"golang.org/x/sync/semaphore"
)

// ErrNoConnector reports a probe attempted on a Cluster with no
// connector. Workers have no coordinator, and both validation routes
// mount unconditionally, so this is reachable from a worker's loopback
// admin port. Returning it beats the nil dereference that reading
// m.connector would otherwise produce.
var ErrNoConnector = errors.New("cluster: no connector configured on this node")

// NodeHealth is a per-URL validation result returned by
// ValidateAllConnections. Conn is populated when Err is nil.
type NodeHealth struct {
	URL  string
	Conn *Connection
	Err  error
}

// ValidateNode probes the given node URL and returns its
// health+version snapshot. Pure read — does not touch the registry.
//
// Uses m.connector, not a fresh one: only the stored connector carries
// the mTLS dispatch client and cluster port, and worker admin ports are
// loopback-only (see server_factory bind narrowing), so a connector
// without a cluster port can reach no worker at all.
//
// Errors propagate verbatim so callers can apply
// IsVersionCompatibilityError dispatches at the HTTP boundary.
func (m *Cluster) ValidateNode(ctx context.Context, url string) (*Connection, error) {
	if m == nil || m.connector == nil {
		return nil, ErrNoConnector
	}
	return m.connector.ConnectToClusterNode(ctx, url)
}

// RegisterAdminEndpoint registers endpointURL as a unicast routing
// target. Safe to call idempotently; the registry short-circuits
// duplicate URLs. Seeded at StatusUnknown so ep.Status agrees with
// the Liveness machine until the first probe resolves it.
//
// ClusterURL is derived via DeriveClusterURL(endpointURL, BindPort)
// when BindPort is configured. Mirrors the startup-from-config path
// (cluster.go:88) so pairing-time admission and config-seeded
// admission produce structurally identical Endpoint values; without
// this, dispatch (strategies.go:93) falls back to the admin URL,
// which is loopback-only since af3e7214.
func (m *Cluster) RegisterAdminEndpoint(endpointURL string) error {
	ep := &Endpoint{
		URL:      endpointURL,
		Name:     endpointURL,
		Strategy: StrategyUnicast,
		IsLocal:  false,
		Status:   StatusUnknown,
	}
	if m.config != nil && m.config.BindPort > 0 {
		if clusterURL, err := DeriveClusterURL(endpointURL, m.config.BindPort); err == nil {
			ep.ClusterURL = clusterURL
		}
	}
	return m.RegisterEndpoint(ep)
}

// ValidateAllConnections pings each URL via a single Connector and
// returns one NodeHealth per input. Order is preserved. Callers
// partition into healthy / unhealthy sets by checking NodeHealth.Err.
//
// Fan-out is bounded by the same concurrency cap used for broadcast
// dispatch. On ctx cancellation mid-acquire, trailing slots carry the
// acquire error rather than zero-value NodeHealth (which would have
// been counted as "healthy" with an empty URL by consumers that
// partition on Err != nil).
func (m *Cluster) ValidateAllConnections(ctx context.Context, urls []string) []NodeHealth {
	if len(urls) == 0 {
		return nil
	}
	// m.connector, not a fresh one — see ValidateNode.
	if m == nil || m.connector == nil {
		out := make([]NodeHealth, len(urls))
		for i, u := range urls {
			out[i] = NodeHealth{URL: u, Err: ErrNoConnector}
		}
		return out
	}
	connector := m.connector
	out := make([]NodeHealth, len(urls))

	sem := semaphore.NewWeighted(maxBroadcastConcurrency)
	var wg sync.WaitGroup
	for i, u := range urls {
		if err := sem.Acquire(ctx, 1); err != nil {
			out[i] = NodeHealth{URL: u, Err: err}
			for j := i + 1; j < len(urls); j++ {
				out[j] = NodeHealth{URL: urls[j], Err: err}
			}
			break
		}
		wg.Add(1)
		go func(i int, u string) {
			defer wg.Done()
			defer sem.Release(1)
			conn, err := connector.ConnectToClusterNode(ctx, u)
			// Disjoint index write — safe under wg.Wait happens-before.
			out[i] = NodeHealth{URL: u, Conn: conn, Err: err}
		}(i, u)
	}
	wg.Wait()
	return out
}
