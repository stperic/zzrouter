package mesh

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"

	"golang.org/x/sync/errgroup"

	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/ui"
)

// Cluster is the main coordinator for all cluster operations
type Cluster struct {
	config        *Config
	state         *StateManager // Unified state management
	dispatcher    *Dispatcher
	connector     *Connector
	healthMonitor *HealthMonitor
	client        ClusterClient // High-level client interface
	localHandler  LocalHandler
	localNodeURL  string

	// startupUp names the endpoints whose construction-time probe
	// produced a transition into StatusUp. Drained by
	// ReplayStartupReconnects.
	startupMu sync.Mutex
	startupUp []string
}

// NewCluster creates a new cluster coordinator. The caller supplies a
// pre-built Connector so the mTLS dispatch client + cluster port can
// be baked in at construction (coordinators) or defaulted (workers /
// degraded-mode callers passing nil).
func NewCluster(cfg *Config, localNodeURL string, localHandler LocalHandler, reconnectCallback ReconnectionCallback, connector *Connector) (*Cluster, error) {
	if connector == nil {
		connector = NewConnector(ConnectorConfig{})
	}
	registry := NewEndpointRegistry()
	circuitBreakers := NewCircuitBreakerManager()

	// Create unified state manager
	state := NewStateManager(registry, circuitBreakers)

	dispatcher := NewDispatcher(&DispatcherConfig{
		Connector:       connector,
		CircuitBreakers: circuitBreakers,
		LocalHandler:    localHandler,
		LocalNodeURL:    localNodeURL,
	})

	// Create cluster instance
	m := &Cluster{
		config:       cfg,
		state:        state,
		dispatcher:   dispatcher,
		connector:    connector,
		localHandler: localHandler,
		localNodeURL: localNodeURL,
	}

	// Create ClusterClient
	m.client = NewClusterClient(m, localHandler, localNodeURL)

	// Create health monitor with reconnection callback
	healthMonitor := NewHealthMonitor(&HealthMonitorConfig{
		Connector:            connector,
		Registry:             registry,
		CircuitBreakers:      circuitBreakers,
		CheckInterval:        constants.HealthCheckInterval,
		ReconnectionCallback: reconnectCallback,
		NameObserver:         cfg.NameObserver,
	})
	m.healthMonitor = healthMonitor

	// Register local endpoint
	if err := m.registerLocalEndpoint(); err != nil {
		return nil, fmt.Errorf("failed to register local endpoint: %w", err)
	}

	// Register remote endpoints with validation. Registration is
	// sequential (state mutation) but the per-endpoint reachability
	// probe runs in parallel via errgroup so startup is bounded by the
	// slowest probe rather than by their sum.
	if cfg.Enabled && len(cfg.Endpoints) > 0 {
		normalized := make([]string, 0, len(cfg.Endpoints))
		for _, endpointURL := range cfg.Endpoints {
			if !strings.HasPrefix(endpointURL, "http://") && !strings.HasPrefix(endpointURL, "https://") {
				endpointURL = "http://" + endpointURL
			}
			clusterURL, _ := DeriveClusterURL(endpointURL, cfg.BindPort)
			endpoint := &Endpoint{
				URL:        endpointURL,
				ClusterURL: clusterURL,
				Name:       endpointURL,
				Strategy:   StrategyUnicast,
				// Seed at Unknown so the Liveness machine (which also
				// starts at Unknown) agrees with ep.Status from frame
				// zero. The first probe will transition Unknown→UP
				// on success, or Unknown→DEGRADED on the first miss.
				Status: StatusUnknown,
			}
			if err := m.RegisterEndpoint(endpoint); err != nil {
				return nil, fmt.Errorf("failed to register endpoint %s: %w", endpointURL, err)
			}
			normalized = append(normalized, endpointURL)
		}

		// seedMu serializes each probe's observe/record/log so the
		// startup banner stays readable and the transitions are
		// recorded in the order they were seen.
		var seedMu sync.Mutex
		g, gctx := errgroup.WithContext(context.Background())
		for _, endpointURL := range normalized {
			g.Go(func() error {
				probeCtx, cancel := context.WithTimeout(gctx, constants.ClusterQueryTimeout)
				defer cancel()
				conn, connErr := connector.attemptConnection(probeCtx, endpointURL)
				seedMu.Lock()
				defer seedMu.Unlock()
				if connErr == nil && conn != nil {
					t := m.state.GetEndpoints().Observe(endpointURL, ProbeResult{
						OK: true, Quality: conn.Quality, Snapshot: conn,
					})
					if cfg.NameObserver != nil && conn.NodeName != "" {
						cfg.NameObserver(endpointURL, conn.NodeName)
					}
					// This is the transition reconnectCallback exists
					// for, arriving at the one moment it cannot be
					// delivered: NewCluster has not returned, so the
					// callback's own handle on this cluster is still
					// nil. Hold it for ReplayStartupReconnects rather
					// than drop it.
					if t.To == StatusUp {
						m.startupMu.Lock()
						m.startupUp = append(m.startupUp, endpointURL)
						m.startupMu.Unlock()
					}
					log.Printf("%s Cluster node %s(%s) connected (zzRouter version: %s)",
						ui.GetSuccessEmoji(), conn.NodeName, endpointURL, conn.Version.String())
				} else {
					m.state.GetEndpoints().Observe(endpointURL, ProbeResult{OK: false, Err: connErr})
					log.Printf("%s Cluster node (%s) not ready: %v",
						ui.GetWarningEmoji(), endpointURL, connErr)
				}
				// Probe errors are informational — the health monitor
				// retries. Never fail the whole group.
				return nil
			})
		}
		_ = g.Wait()
	}

	// Start health monitor
	m.healthMonitor.Start()

	return m, nil
}

// ReplayStartupReconnects delivers the reconnection callback for every
// endpoint that reached StatusUp while NewCluster was probing, then
// forgets them.
//
// A peer that is already up when this node starts produces its
// Unknown->UP transition inside the constructor, which is the one
// moment the callback cannot run: the caller has not been handed the
// cluster yet, so a callback that reaches back through it sees nil.
// Delivering it here instead of dropping it is what keeps boot from
// being the one transition every ReconnectionCallback consumer misses.
//
// Call once the cluster handle is wired. Idempotent: a second call has
// nothing left to replay.
func (m *Cluster) ReplayStartupReconnects() {
	if m == nil {
		return
	}
	m.startupMu.Lock()
	pending := m.startupUp
	m.startupUp = nil
	m.startupMu.Unlock()

	if m.healthMonitor == nil || m.healthMonitor.reconnectionCallback == nil {
		return
	}
	for _, endpointURL := range pending {
		m.healthMonitor.reconnectionCallback(endpointURL)
	}
}

// GetConnector returns the cluster's connector for reuse
// This allows other components to use the same connector instead of creating new ones
func (m *Cluster) GetConnector() *Connector {
	return m.connector
}

// Stop stops the cluster and cleans up resources
func (m *Cluster) Stop() {
	if m.healthMonitor != nil {
		m.healthMonitor.Stop()
	}
}

// GetClient returns the ClusterClient
func (m *Cluster) GetClient() ClusterClient {
	return m.client
}

// GetState returns the StateManager
func (m *Cluster) GetState() *StateManager {
	return m.state
}

// GetHealthMonitor returns the health monitor
func (m *Cluster) GetHealthMonitor() *HealthMonitor {
	return m.healthMonitor
}

// registerLocalEndpoint registers the local host as an endpoint
func (m *Cluster) registerLocalEndpoint() error {
	// Use configured hostname from config, fallback to "local" if not set
	hostName := m.config.NodeName
	if hostName == "" {
		hostName = "local"
	}

	return m.state.GetEndpoints().RegisterEndpoint(&Endpoint{
		URL:      m.localNodeURL,
		Name:     hostName, // Use configured hostname
		Strategy: StrategyUnicast,
		IsLocal:  true,
		Status:   StatusUp,
	})
}

// HandleRequest is the main entry point for all cluster requests
func (m *Cluster) HandleRequest(ctx context.Context, req *Request) (*Response, error) {
	// 1. Determine routing strategy
	strategy := m.determineStrategy(req)

	// 2. Get target endpoints from registry
	endpoints := m.state.GetEndpoints().GetEndpointsForRequest(req)

	// 3. Dispatch request using appropriate strategy
	return m.dispatcher.Dispatch(ctx, strategy, endpoints, req)
}

// HandleHTTPRequest handles HTTP requests directly
func (m *Cluster) HandleHTTPRequest(w http.ResponseWriter, r *http.Request) error {
	// Create cluster request from HTTP request
	req := NewRequestFromHTTP(r)

	// Extract strategy from query parameter
	if strategyParam := r.URL.Query().Get("cluster_strategy"); strategyParam != "" {
		if strategy, err := ParseStrategy(strategyParam); err == nil {
			req.Strategy = strategy
		}
	}

	// Extract strategy from header (lower priority than query param)
	if req.Strategy == 0 { // Not set by query param
		if strategyHeader := r.Header.Get("X-Cluster-Strategy"); strategyHeader != "" {
			if strategy, err := ParseStrategy(strategyHeader); err == nil {
				req.Strategy = strategy
			}
		}
	}

	// Handle the request
	resp, err := m.HandleRequest(r.Context(), req)
	if err != nil {
		return err
	}

	return resp.WriteHTTP(w)
}

// determineStrategy selects the appropriate routing strategy
func (m *Cluster) determineStrategy(req *Request) DispatchStrategy {
	// 1. Use explicitly set strategy (highest priority)
	if req.Strategy != 0 {
		return req.Strategy
	}

	// 2. Check configured defaults
	for _, sd := range m.config.StrategyDefaults {
		if strings.HasPrefix(req.Path, sd.PathPrefix) {
			if strategy, err := ParseStrategy(sd.Default); err == nil {
				return strategy
			}
		}
	}

	// 3. Determine from target host pattern
	if req.TargetNode == "*" {
		return StrategyBroadcast
	}

	// 4. Check if streaming is requested (for chat completions)
	if strings.Contains(req.Path, "/chat/completions") {
		if stream, ok := req.Metadata["stream"].(bool); ok && stream {
			return StrategyStreaming
		}
	}

	// 5. Default to unicast
	return StrategyUnicast
}

// ParseStrategy converts string to DispatchStrategy
func ParseStrategy(s string) (DispatchStrategy, error) {
	switch strings.ToLower(s) {
	case "unicast":
		return StrategyUnicast, nil
	case "broadcast":
		return StrategyBroadcast, nil
	case "streaming":
		return StrategyStreaming, nil
	case "roundrobin", "round-robin":
		return StrategyRoundRobin, nil
	case "failover":
		return StrategyFailover, nil
	default:
		return 0, fmt.Errorf("unknown strategy: %s", s)
	}
}

// RegisterEndpoint registers an endpoint with the cluster
func (m *Cluster) RegisterEndpoint(endpoint *Endpoint) error {
	return m.state.GetEndpoints().RegisterEndpoint(endpoint)
}

// UnregisterEndpoint removes an endpoint from the cluster
func (m *Cluster) UnregisterEndpoint(url string) error {
	return m.state.GetEndpoints().UnregisterEndpoint(url)
}

// GetAllEndpoints returns all registered endpoints
func (m *Cluster) GetAllEndpoints() []*Endpoint {
	return m.state.GetEndpoints().GetAllEndpoints()
}

// ObserveLocal publishes the coord's own snapshot into the self-slot.
func (m *Cluster) ObserveLocal(snap EndpointSnapshot) {
	m.state.GetEndpoints().ObserveLocal(snap)
}

// Self returns the coord's self-slot, or nil if not yet populated.
func (m *Cluster) Self() *Endpoint {
	return m.state.GetEndpoints().Self()
}

// RefreshAllEndpoints synchronously re-probes all worker endpoints to
// refresh system snapshots. Busts the version-discovery cache first so
// an operator-triggered refresh sees live peer state — the 10-min TTL
// is only appropriate for the steady-state health monitor, not for
// "recheck now, something changed" signals from the TUI or API.
func (m *Cluster) RefreshAllEndpoints() {
	m.connector.BustVersionCache()
	if m.healthMonitor != nil {
		m.healthMonitor.Refresh()
	}
}

// RefreshEndpoint re-probes a single endpoint by URL to get a fresh
// system snapshot. Drops that URL's version-discovery cache entry first
// (see RefreshAllEndpoints for rationale).
func (m *Cluster) RefreshEndpoint(url string) {
	m.connector.BustVersionCache(url)
	if m.healthMonitor != nil {
		m.healthMonitor.Refresh(url)
	}
}

// MarkEndpointDown transitions the endpoint to StatusDown immediately,
// bypassing the liveness hysteresis threshold. Used by the goodbye
// handler when a worker declares it is going away — the peer's own
// signal is authoritative. No probe is issued. Unknown URLs are a
// no-op (idempotent goodbye).
func (m *Cluster) MarkEndpointDown(url string, reason error) {
	if m.state == nil {
		return
	}
	reg := m.state.GetEndpoints()
	if reg == nil {
		return
	}
	reg.MarkDown(url, reason)
}

// Shutdown gracefully shuts down the cluster
func (m *Cluster) Shutdown(ctx context.Context) error {
	if m.healthMonitor != nil {
		m.healthMonitor.Stop()
	}
	return nil
}
