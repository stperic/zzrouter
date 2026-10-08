package mesh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/retry"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stperic/zzrouter/pkg/version"
)

// Connector retry policy tuning — the defaults baked into NewConnector.
// Extracted so a single grep lands on the whole policy instead of
// literals scattered across the constructor body.
const (
	// connectorBackoffInitial is the first retry wait for a single
	// cluster-node connect attempt.
	connectorBackoffInitial = 500 * time.Millisecond
	// connectorBackoffMultiplier doubles the wait each attempt.
	connectorBackoffMultiplier = 2.0
	// connectorBackoffJitter is ±15% of the computed delay to prevent
	// coordinated fleet retries after a coord reboot or shared-fate flap.
	connectorBackoffJitter = 0.15
	// connectorMaxRetries bounds attemptConnection retries per node.
	connectorMaxRetries = 5

	// HTTP transport tuning. No client-level Timeout — each request uses
	// its own context deadline so short ops (health checks) and long
	// ops (provider installs) can coexist on the same client.
	connectorMaxIdleConns        = 100
	connectorMaxIdleConnsPerHost = 10
	connectorIdleConnTimeout     = 90 * time.Second

	// connectorDialTimeout bounds the TCP dial for a cluster-side
	// connection. Without this, a half-open SYN rides the kernel
	// tcp_syn_retries budget (~75s on Linux) — way past any useful
	// deadline. 5s absorbs normal LAN/WAN retransmits.
	connectorDialTimeout = 5 * time.Second
	// connectorDialKeepAlive matches the Go-stdlib default — explicit
	// so a future Transport tweak doesn't silently disable it.
	connectorDialKeepAlive = 30 * time.Second
	// connectorTLSHandshakeTimeout covers a cold mTLS 1.3 handshake +
	// one retransmit on a stalled LAN. Typical warm handshake is
	// <150ms; 5s is generous without being a hang.
	connectorTLSHandshakeTimeout = 5 * time.Second
	// connectorResponseHeaderTimeout is the real defense against a
	// half-open TCP — bounds time-to-first-byte well under the 30s
	// ClusterActionTimeout so retry+classify fires first.
	connectorResponseHeaderTimeout = 10 * time.Second
	// connectorExpectContinueTimeout is the Go-stdlib default. Zero
	// effect today (we don't emit Expect: 100-continue) but cheap +
	// correct if we ever do.
	connectorExpectContinueTimeout = 1 * time.Second
)

// Connector handles robust connections to cluster nodes with retry
// logic. Dispatch + health probes ride the mTLS cluster-port listener
// via an http.Client supplied at construction. The struct is
// immutable after NewConnector returns — no field is written after
// the constructor, so readers never need synchronization.
type Connector struct {
	backoff          retry.Backoff
	maxRetries       int
	classify         retry.Classifier
	versionDiscovery *version.VersionDiscovery
	httpClient       *http.Client
	// clusterPort is the mTLS cluster-listener port workers bind on.
	// performHealthCheck uses it to derive the cluster URL from a
	// peer's public URL. Zero means the mTLS path is disabled and
	// probes fall back to the public /health port.
	clusterPort int
}

// ConnectorConfig configures NewConnector. All fields are optional:
//   - HTTPClient zero → a default http.Client with reasonable transport
//     pooling is created. Callers that need mTLS must inject their
//     identity-cert-bearing client here.
//   - ClusterPort zero → health probes use only the public /health port.
//     Coordinators must set this so dispatch viability is reflected in
//     endpoint status.
type ConnectorConfig struct {
	HTTPClient  *http.Client
	ClusterPort int
}

// Connection represents a validated connection to a remote cluster node.
// Created by [Connector] after version negotiation and health checking.
//
// Anonymously embeds HealthReport — the 16 system-resource fields
// parsed from /health are accessible as conn.OS / conn.RAMTotalGB /
// etc. via Go field promotion, and the same HealthReport value is
// assigned into ep.Snapshot in applyConnectionSnapshot without a
// field-by-field copy.
type Connection struct {
	// NodeURL is the base endpoint URL of the remote node (e.g., "http://192.0.2.10:9090").
	// This is the address used for all subsequent HTTP calls to the node.
	NodeURL string

	// NodeName is the friendly hostname reported by the remote node (e.g., "gpu-server").
	// Discovered at connection time from the node's health endpoint, not configured locally.
	NodeName string
	Version  *version.Version
	// Quality records whether the health probe succeeded on the mTLS
	// cluster port (Full) or only the public /health fallback (Degraded).
	// Consumers use this to decide whether the peer can serve dispatch.
	Quality     HealthQuality
	LastChecked time.Time

	// HealthReport — populated from /zzrouter/internal/health response.
	// Trust the fields only when Quality == QualityFull; a Degraded
	// probe may carry zero-valued resources.
	HealthReport
}

// NewConnector builds a Connector with retry/backoff/classifier wired
// in. HTTPClient and ClusterPort are accepted at construction — pass
// the mTLS dispatch client + cluster listener port for coordinators,
// zero values for workers or callers that only need a default client.
// The returned Connector is immutable; no setter exists.
func NewConnector(cfg ConnectorConfig) *Connector {
	netClassify := retry.ClassifyNetwork()
	client := cfg.HTTPClient
	if client == nil {
		dialer := &net.Dialer{
			Timeout:   connectorDialTimeout,
			KeepAlive: connectorDialKeepAlive,
		}
		client = &http.Client{
			Transport: &http.Transport{
				DialContext:           dialer.DialContext,
				MaxIdleConns:          connectorMaxIdleConns,
				MaxIdleConnsPerHost:   connectorMaxIdleConnsPerHost,
				IdleConnTimeout:       connectorIdleConnTimeout,
				TLSHandshakeTimeout:   connectorTLSHandshakeTimeout,
				ResponseHeaderTimeout: connectorResponseHeaderTimeout,
				ExpectContinueTimeout: connectorExpectContinueTimeout,
			},
		}
	}
	return &Connector{
		backoff: retry.Backoff{
			Initial:    connectorBackoffInitial,
			Max:        constants.ClusterActionTimeout,
			Multiplier: connectorBackoffMultiplier,
			Jitter:     connectorBackoffJitter,
		},
		maxRetries: connectorMaxRetries,
		// Stop on version incompatibility + the standard network-level
		// permanents (TLS cert, DNS NXDOMAIN, ctx cancel/deadline).
		classify: func(err error) retry.Decision {
			var verr *version.VersionCompatibilityError
			if errors.As(err, &verr) {
				return retry.Stop
			}
			return netClassify(err)
		},
		versionDiscovery: version.NewVersionDiscovery(constants.VersionDiscoveryTimeout),
		httpClient:       client,
		clusterPort:      cfg.ClusterPort,
	}
}

// ConnectToClusterNode establishes a robust connection to a cluster node
func (c *Connector) ConnectToClusterNode(ctx context.Context, hostURL string) (*Connection, error) {
	hostURL = version.NormalizeHostURL(hostURL)
	var lastErr error

	for attempt := 0; attempt < c.maxRetries; attempt++ {
		if attempt > 0 {
			if err := retry.Sleep(ctx, c.backoff.Delay(attempt-1)); err != nil {
				return nil, fmt.Errorf("connection cancelled: %w", err)
			}
		}

		conn, err := c.attemptConnection(ctx, hostURL)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		if c.classify(err) == retry.Stop {
			break
		}
	}

	return nil, fmt.Errorf("failed to connect to cluster node %s after %d attempts: %w",
		hostURL, c.maxRetries, lastErr)
}

// attemptConnection performs a single connection attempt
func (c *Connector) attemptConnection(ctx context.Context, hostURL string) (*Connection, error) {
	// Step 1: Check version compatibility (use shorter timeout for faster startup)
	versionInfo, err := c.discoverVersion(ctx, hostURL)
	if err != nil {
		return nil, fmt.Errorf("version discovery failed: %w", err)
	}

	// Step 2: Validate compatibility.
	//
	// Protocol precheck first — fatal if the peer's cluster_protocol
	// falls outside our accepted window (see version.CheckClusterProtocol).
	// This is what evicts v(N-1) peers after a coordinator bumps
	// MinClusterProtocolVersion during a rolling upgrade. The richer
	// CheckCompatibility (capabilities, breaking changes) runs after.
	localInfo := version.GetCurrentVersionInfo(version.ServiceTypeHost, []string{version.CapabilityCluster})
	if err := version.LocalProtocolWindow().Check(versionInfo.ClusterProtocol); err != nil {
		return nil, &version.VersionCompatibilityError{
			LocalVersion:   localInfo.Version,
			RemoteVersion:  versionInfo.Version,
			NodeURL:        hostURL,
			Errors:         []string{err.Error()},
			ConnectionType: version.CapabilityCluster,
		}
	}
	matrix := version.GetCompatibilityMatrix(version.CapabilityCluster)
	result := version.CheckCompatibility(localInfo, versionInfo, matrix)

	if !result.IsCompatible {
		return nil, &version.VersionCompatibilityError{
			LocalVersion:   localInfo.Version,
			RemoteVersion:  versionInfo.Version,
			NodeURL:        hostURL,
			Errors:         result.Errors,
			ConnectionType: version.CapabilityCluster,
		}
	}

	// Step 3: Perform health check, which populates the system snapshot
	// directly into the returned Connection.
	conn, err := c.performHealthCheck(ctx, hostURL)
	if err != nil {
		return nil, fmt.Errorf("health check failed: %w", err)
	}
	conn.NodeURL = hostURL
	conn.Version = versionInfo.Version
	conn.LastChecked = utils.Now()
	return conn, nil
}

// discoverVersion probes the peer for its version + cluster_protocol.
// Tries the mTLS cluster URL first via c.httpClient when ClusterPort
// is configured, then falls back to plain HTTP on the admin URL via
// the shared version.VersionDiscovery cache.
//
// The cluster-port-first ordering matters since af3e7214 narrowed
// worker admin ports to 127.0.0.1 — the admin URL is unreachable
// from coord, so version discovery against that URL times out and
// the connect chain never reaches performHealthCheck. Probing the
// cluster URL via the mTLS-bound httpClient sidesteps both gates.
func (c *Connector) discoverVersion(ctx context.Context, hostURL string) (*version.VersionInfo, error) {
	var clusterErr error
	if c.clusterPort > 0 {
		clusterURL, derr := DeriveClusterURL(hostURL, c.clusterPort)
		if derr != nil {
			clusterErr = fmt.Errorf("derive cluster URL: %w", derr)
		} else {
			vi, err := c.probeVersionViaCluster(ctx, clusterURL)
			if err == nil {
				return vi, nil
			}
			clusterErr = fmt.Errorf("cluster %s: %w", clusterURL, err)
		}
	}
	vi, adminErr := c.versionDiscovery.DiscoverVersionWithTimeout(hostURL, 2*time.Second)
	if adminErr == nil {
		return vi, nil
	}
	if clusterErr != nil {
		return nil, fmt.Errorf("admin: %w; %s", adminErr, clusterErr.Error())
	}
	return nil, adminErr
}

// probeVersionViaCluster GETs /zzrouter/v1/internal/version on the
// cluster mTLS URL using the connector's mTLS-bound HTTP client and
// decodes the response directly into a version.VersionInfo.
//
// We target /zzrouter/v1/internal/version (not /health) because it
// emits the structured VersionInfo shape directly — including the
// cluster_protocol + min_cluster_protocol fields the mesh
// compatibility gate requires. The cluster engine's minimal /health
// only returns {status, mode}; the wrapped admin handler's
// /zzrouter/v1/internal/health returns a SuccessResponse-wrapped
// rich snapshot but omits cluster_protocol. /version is the only
// endpoint with all four fields the connector needs in one request.
//
// The endpoint is mTLS+OU-gated via wrapInternal at the listener;
// c.httpClient already carries the coordinator's client cert with
// OU=coordinator.
//
// 5s timeout (vs the plain-HTTP path's 2s) accommodates first-hop
// TLS handshake latency on tailnet / cross-region links; the slower
// cluster probe is still preferable to the admin path which will
// just connection-refuse on a worker bound to 127.0.0.1.
func (c *Connector) probeVersionViaCluster(ctx context.Context, clusterURL string) (*version.VersionInfo, error) {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	url := clusterURL + "/zzrouter/v1/internal/version"
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}
	var vi version.VersionInfo
	if err := json.NewDecoder(resp.Body).Decode(&vi); err != nil {
		return nil, fmt.Errorf("decode %s: %w", url, err)
	}
	if vi.Version == nil {
		return nil, fmt.Errorf("no version field in %s response", url)
	}
	return &vi, nil
}

// performHealthCheck checks if the cluster node is healthy and responsive.
// Probe order:
//
//  1. mTLS cluster port at /zzrouter/v1/internal/health — same path as
//     dispatch; endpoint status reflects dispatch viability. Requires
//     ClusterPort + HTTPClient to have been supplied in ConnectorConfig.
//  2. Public port at /health — unauthenticated minimal probe. Proves
//     the worker process is alive (rich snapshot fields stay zero-
//     valued); retained so a worker whose mTLS pairing is broken still
//     appears "reachable" for operator diagnosis.
//
// Returns a *Connection with system-snapshot fields populated (NodeName,
// OS, RAM, GPU, …, Quality). NodeURL, Version, and LastChecked are left
// zero — attemptConnection fills those from the version-discovery step.
func (c *Connector) performHealthCheck(ctx context.Context, hostURL string) (*Connection, error) { //nolint:gocyclo,cyclop // Endpoint fallback and optional health fields are decoded in one probe flow.
	type probeURL struct {
		url     string
		quality HealthQuality
	}
	probes := make([]probeURL, 0, 2)
	if c.clusterPort > 0 {
		if clusterURL, err := DeriveClusterURL(hostURL, c.clusterPort); err == nil {
			probes = append(probes, probeURL{clusterURL + "/zzrouter/v1/internal/health", QualityFull})
		}
	}
	probes = append(probes, probeURL{hostURL + "/health", QualityDegraded})

	var resp *http.Response
	var quality HealthQuality
	var lastErr error
	for _, p := range probes {
		req, err := http.NewRequestWithContext(ctx, "GET", p.url, nil)
		if err != nil {
			lastErr = err
			continue
		}
		r, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if r.StatusCode == http.StatusOK {
			resp = r
			quality = p.quality
			break
		}
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
		lastErr = fmt.Errorf("health check returned %d from %s", r.StatusCode, p.url)
	}

	if resp == nil {
		return nil, fmt.Errorf("health check failed on all endpoints: %w", lastErr)
	}
	defer func() { _ = resp.Body.Close() }()

	conn := &Connection{Quality: quality}

	if resp.StatusCode == http.StatusOK {
		// Parse full health response into a generic map
		var data map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&data); err == nil {
			// The health response may be wrapped in {"data": {...}} envelope
			if inner, ok := data["data"].(map[string]any); ok {
				data = inner
			}
			conn.NodeName = getStr(data, "hostname")
			conn.OS = detectOSFromHealth(data)
			conn.UptimeSeconds = getInt(data, "uptime_seconds")
			conn.Address = getStr(data, "address")
			conn.RunningModels = getInt(data, "running_models")
			conn.AppCount = getInt(data, "providers")
			conn.ClusterRole = getStr(data, "cluster_role")
			if conn.ClusterRole == "" {
				if getBool(data, "master") {
					conn.ClusterRole = string(config.ClusterModeCoordinator)
				} else {
					conn.ClusterRole = string(config.ClusterModeWorker)
				}
			}
			// Parse memory (bytes → GB)
			conn.RAMTotalGB = getNestedValueGB(data, "memory_total")
			conn.RAMAvailableGB = getNestedValueGB(data, "memory_available")
			// Parse disk (bytes → GB)
			conn.DiskTotalGB = getNestedValueGB(data, "disk_total")
			conn.DiskAvailableGB = getNestedValueGB(data, "disk_free")
			// Parse GPU info
			if gpus, ok := data["gpus"].([]any); ok {
				conn.GPUCount = len(gpus)
				conn.GPUs = make([]GPUDetail, 0, len(gpus))
				for _, g := range gpus {
					gpu, ok := g.(map[string]any)
					if !ok {
						continue
					}
					if conn.GPUType == "" {
						conn.GPUType = detectGPUType(getStr(gpu, "name"))
						conn.GPUName = getStr(gpu, "name")
					}
					memTotal := getGPUMemoryGB(gpu, "memory_total_gb", "vram_total")
					memAvail, measured := getGPUMemoryGBIfPresent(gpu, "memory_available_gb", "vram_free")
					conn.VRAMTotalGB += memTotal
					conn.VRAMAvailableGB += memAvail
					detail := GPUDetail{
						Index:         getInt(gpu, "index"),
						Name:          getStr(gpu, "name"),
						Vendor:        getStr(gpu, "vendor"),
						PCIAddress:    getStr(gpu, "pci_address"),
						UUID:          getStr(gpu, "uuid"),
						MemoryTotalGB: memTotal,
					}
					if measured {
						avail := memAvail
						detail.MemoryAvailGB = &avail
					}
					if v, present := gpu["driver_index"]; present {
						switch n := v.(type) {
						case float64:
							idx := int(n)
							detail.DriverIndex = &idx
						case int:
							idx := n
							detail.DriverIndex = &idx
						}
					}
					conn.GPUs = append(conn.GPUs, detail)
				}
			}
			// Apple Silicon: unified memory — use system RAM as VRAM
			if conn.GPUType == "apple" && conn.VRAMTotalGB == 0 && conn.RAMTotalGB > 0 {
				conn.VRAMTotalGB = conn.RAMTotalGB
				conn.VRAMAvailableGB = conn.RAMAvailableGB
			}
			if conn.GPUCount == 0 {
				conn.GPUCount = getInt(data, "gpu_count")
			}
			// Parse apps detail into typed LocalProviderInfo via
			// re-marshal + unmarshal — the health body arrived as a
			// generic map, but we own the inner schema.
			if raw, ok := data["apps_detail"]; ok && raw != nil {
				if b, err := json.Marshal(raw); err == nil {
					_ = json.Unmarshal(b, &conn.Apps)
				}
			}
			if s := getStr(data, "last_config_mutation_at"); s != "" {
				if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
					conn.LastConfigMutationAt = t
				}
			}
		}
		return conn, nil
	}

	// Consider 503 Service Unavailable as temporarily unhealthy
	if resp.StatusCode == http.StatusServiceUnavailable {
		return conn, fmt.Errorf("service temporarily unavailable")
	}
	return conn, fmt.Errorf("unexpected health check status: %d", resp.StatusCode)
}

// getStr safely extracts a string from a map
func getStr(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// getInt safely extracts an int from a map (handles float64 from JSON)
func getInt(m map[string]any, key string) int {
	if v, ok := m[key]; ok {
		switch n := v.(type) {
		case float64:
			return int(n)
		case int:
			return n
		}
	}
	return 0
}

// getGPUMemoryGB extracts GPU memory in GB, trying direct float field then nested {value: bytes}.
func getGPUMemoryGB(gpu map[string]any, directKey, nestedKey string) float64 {
	if v := getFloat(gpu, directKey); v > 0 {
		return v
	}
	return getNestedValueGB(gpu, nestedKey)
}

// getFloat safely extracts a float64 from a map
func getFloat(m map[string]any, key string) float64 {
	if v, ok := m[key]; ok {
		if f, ok := v.(float64); ok {
			return f
		}
	}
	return 0
}

// getBool safely extracts a bool from a map
func getBool(m map[string]any, key string) bool {
	if v, ok := m[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return false
}

// getGPUMemoryGBIfPresent reports a memory figure and whether the worker
// sent one at all. A worker that could not measure a card omits both keys;
// treating that absence as 0 GB free would claim a full card.
func getGPUMemoryGBIfPresent(gpu map[string]any, directKey, nestedKey string) (float64, bool) {
	if _, ok := gpu[directKey]; ok {
		return getFloat(gpu, directKey), true
	}
	if _, ok := gpu[nestedKey]; ok {
		return getNestedValueGB(gpu, nestedKey), true
	}
	return 0, false
}

// getNestedValueGB extracts a nested {"value": <bytes>} field and converts to GB
func getNestedValueGB(m map[string]any, key string) float64 {
	if v, ok := m[key]; ok {
		if nested, ok := v.(map[string]any); ok {
			if val, ok := nested["value"]; ok {
				switch n := val.(type) {
				case float64:
					return n / (1024 * 1024 * 1024) // bytes to GB
				case uint64:
					return float64(n) / (1024 * 1024 * 1024)
				}
			}
		}
	}
	return 0
}

// detectOSFromHealth infers OS from the health response hostname or runtime info
func detectOSFromHealth(data map[string]any) string {
	// Check if there's an explicit OS field
	if os := getStr(data, "os"); os != "" {
		return os
	}
	return ""
}

// detectGPUType classifies GPU type from name
func detectGPUType(name string) string {
	lower := strings.ToLower(name)
	switch {
	case strings.Contains(lower, "nvidia") || strings.Contains(lower, "geforce") || strings.Contains(lower, "rtx") || strings.Contains(lower, "tesla"):
		return "nvidia"
	case strings.Contains(lower, "amd") || strings.Contains(lower, "radeon"):
		return "amd"
	case strings.Contains(lower, "apple") || strings.Contains(lower, "m1") || strings.Contains(lower, "m2") || strings.Contains(lower, "m3") || strings.Contains(lower, "m4"):
		return "apple"
	default:
		return "other"
	}
}

// ConnectToHealthyClusterNode tries multiple node endpoints and returns the first healthy one
func (c *Connector) ConnectToHealthyClusterNode(ctx context.Context, hostURLs []string) (*Connection, error) {
	if len(hostURLs) == 0 {
		return nil, fmt.Errorf("no cluster nodes provided")
	}

	log.Printf("Attempting to connect to %d cluster nodes", len(hostURLs))

	var lastErr error
	var incompatibleNodes []string

	for i, hostURL := range hostURLs {
		log.Printf("Trying cluster node %d/%d: %s", i+1, len(hostURLs), hostURL)

		conn, err := c.ConnectToClusterNode(ctx, hostURL)
		if err == nil {
			log.Printf("Successfully connected to cluster node: %s", hostURL)
			return conn, nil
		}

		// Track version compatibility errors separately
		if IsVersionCompatibilityError(err) {
			incompatibleNodes = append(incompatibleNodes, hostURL)
		}

		lastErr = err
		log.Printf("Failed to connect to cluster node %s: %v", hostURL, err)
	}

	// Provide detailed error message
	errorMsg := fmt.Sprintf("failed to connect to any cluster node: %v", lastErr)
	if len(incompatibleNodes) > 0 {
		errorMsg += fmt.Sprintf("\nIncompatible nodes: %v", incompatibleNodes)
		errorMsg += "\nConsider upgrading incompatible nodes to supported versions"
	}

	return nil, fmt.Errorf("%s", errorMsg)
}

// BustVersionCache invalidates cached version-discovery entries. With
// no arguments, the entire cache is cleared; with one or more URLs,
// only those entries are invalidated. Lets callers bypass the 10-min
// TTL when they know peer state has changed (operator-triggered
// refresh, mid-rolling-upgrade sweep).
//
// Footgun note: `BustVersionCache(mySlice...)` where mySlice is empty
// collapses to the no-arg form and clears the whole cache. Callers
// building URL lists programmatically should guard `len(urls) == 0`
// at their site if "no-op on empty" is the intent.
func (c *Connector) BustVersionCache(urls ...string) {
	if len(urls) == 0 {
		c.versionDiscovery.ClearCache()
		return
	}
	for _, url := range urls {
		c.versionDiscovery.InvalidateURL(url)
	}
}

// IsVersionCompatibilityError reports whether err (or any wrapped error
// in its chain) is a version.VersionCompatibilityError. Retry policy
// for these lives in the Connector's Classifier; this helper exists for
// callers that want to surface compatibility errors distinctly.
func IsVersionCompatibilityError(err error) bool {
	var verr *version.VersionCompatibilityError
	return errors.As(err, &verr)
}
