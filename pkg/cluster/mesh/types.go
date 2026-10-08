// Package mesh provides coordination, state management, and health
// monitoring for zzRouter multi-node deployments.
//
// # Terminology Glossary
//
//   - Node: A zzRouter server instance (the binary is zzrouter-node). Each node
//     is identified by a name (hostname) and reachable at an endpoint URL.
//   - Endpoint: A node's network address/URL (e.g., "http://192.0.2.10:9090").
//     Within this package, the [Endpoint] struct represents a registered
//     cluster member with its URL, capabilities, and health status.
//   - Provider/App: An LLM backend (Ollama, vLLM, etc.) registered on a node.
//   - Connection: A validated, version-checked link to a remote node, created
//     by [Connector]. Holds the node's endpoint URL, friendly name, and a
//     snapshot of its system resources.
package mesh

import (
	"bytes"
	"context"
	"io"
	"maps"
	"net/http"
	"time"

	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/prov_apps"
)

// Request represents a cluster request
type Request struct {
	Method  string
	Path    string
	Query   string
	Headers http.Header
	Body    []byte

	// Cluster-specific fields
	TargetNode string           // Target node: "" (local), "localhost", a specific node name/IP, or "*" (all nodes)
	Strategy   DispatchStrategy // Routing strategy
	Metadata   map[string]any   // Additional metadata
}

// Response represents a cluster response
type Response struct {
	StatusCode int
	Headers    http.Header
	Body       []byte

	// Cluster-specific fields
	SourceNode string         // Which node responded (hostname, IP, or endpoint URL)
	Metadata   map[string]any // Additional metadata

	// Nodes carries the per-node results of a broadcast, each holding
	// that node's response body verbatim. Only BroadcastHandler sets it,
	// and it is what every in-process caller reads.
	//
	// Body holds the same information in wire form, for HandleHTTPRequest
	// — the package's HTTP entry point, which nothing in this repo calls
	// today. Do not read Body on a broadcast path; read Nodes.
	Nodes []*NodeResponse
}

// Endpoint represents a cluster endpoint.
type Endpoint struct {
	URL string
	// ClusterURL is the mTLS cluster-port URL for this peer — used by
	// coordinator→worker /internal/* dispatch. Derived from URL at
	// registration via DeriveClusterURL. Empty for endpoints that
	// predate the mTLS path or for the local endpoint; dispatch falls
	// back to URL when empty. Populated and consumed in mTLS PR 3b.
	ClusterURL string
	Name       string
	Alias      string // User-friendly alias from config (optional, persistent)
	NodeName   string // Nodename reported by worker (discovered at runtime)
	Strategy   DispatchStrategy
	IsLocal    bool
	Status     EndpointStatus   // Health status (updated by HealthMonitor)
	Snapshot   EndpointSnapshot // System-resource snapshot from the most recent successful probe
}

// HealthReport is the typed payload parsed from a worker's /health
// response — the 16 system-resource fields that every probe returns.
// Shared by Connection (probe output) and EndpointSnapshot (registry
// state) via anonymous embedding so field access stays terse at
// consumer sites (conn.OS, snap.RAMTotalGB) while the type system
// enforces the probe-produces vs registry-tracks boundary.
type HealthReport struct {
	ClusterRole string // "coordinator" | "worker" | "standalone"
	OS          string

	UptimeSeconds int
	Address       string // Worker's self-reported bind address (e.g. "0.0.0.0:9090")
	RunningModels int
	AppCount      int

	RAMTotalGB      float64
	RAMAvailableGB  float64
	DiskTotalGB     float64
	DiskAvailableGB float64
	VRAMTotalGB     float64
	VRAMAvailableGB float64

	GPUCount int
	GPUType  string
	GPUName  string

	// GPUs is the per-card detail decoded from the worker's
	// /zzrouter/v1/internal/health response. Each entry carries the
	// fields a caller needs to pin a workload to a specific physical
	// GPU on the worker: pci_address (cross-reboot stable hardware
	// slot), uuid (vendor durable identifier — CUDA_VISIBLE_DEVICES /
	// HIP_VISIBLE_DEVICES accept it directly), driver_index (what
	// nvidia-smi -i N / rocm-smi index reports — what every operator
	// sees in vendor tools). Empty on workers running pre-v7
	// binaries that don't emit these fields.
	GPUs []GPUDetail

	// Apps is the worker's apps_detail array decoded from the /health
	// response. Typed end-to-end: wire → HealthReport → EndpointSnapshot.
	Apps []prov_apps.LocalProviderInfo

	// LastConfigMutationAt is the worker's most recent FinalizeOn/Offboarding
	// timestamp. Compared against EndpointSnapshot.CollectedAt on the coord
	// to detect silent drift (notify was dropped, worker stayed UP).
	LastConfigMutationAt time.Time
}

// GPUDetail is one card's identity in a HealthReport. Populated from
// /internal/health gpus[] array, surfaced through to /zzrouter/v1/nodes
// so coord-side consumers (TUI detail view, dispatch / affinity logic)
// can pick the right card without re-probing the worker. Field semantics
// mirror pkg/discovery/gpu.Card — see those doc comments for full detail.
type GPUDetail struct {
	Index         int     `json:"index"`
	Name          string  `json:"name,omitempty"`
	Vendor        string  `json:"vendor,omitempty"`
	PCIAddress    string  `json:"pci_address,omitempty"`
	UUID          string  `json:"uuid,omitempty"`
	DriverIndex   *int    `json:"driver_index,omitempty"`
	MemoryTotalGB float64 `json:"memory_total_gb,omitempty"`
	// MemoryAvailGB is nil when the worker could not measure this card
	// (no vendor SMI tool, Apple silicon, a failed probe). Nil and 0.0
	// are opposite claims, so this stays a pointer all the way to the
	// display rather than collapsing to a zero.
	MemoryAvailGB *float64 `json:"memory_available_gb,omitempty"`
}

// EndpointSnapshot holds the registry's authoritative view of a worker.
// Embeds HealthReport (populated on every successful probe) and adds
// registry-owned state that the probe does NOT produce: the stringified
// Version (for JSON serialization), the most recent probe error, and a
// CollectedAt stamp. Written inside registry.Observe; read by consumers
// via Endpoint.Snapshot. All fields zero-valued before the first probe.
type EndpointSnapshot struct {
	HealthReport

	// Version is conn.Version.String() — stringified for display +
	// JSON serialization. Connection keeps the typed *version.Version
	// for compatibility checks.
	Version string

	// LastError is the most recent probe error message. Cleared on a
	// successful probe; populated on failure.
	LastError string

	// CollectedAt stamps when the snapshot was written (on a successful
	// probe). Zero before the first success.
	CollectedAt time.Time
}

// EndpointStatus defines the health status of an endpoint
type EndpointStatus string

const (
	StatusUp       EndpointStatus = "UP"       // Healthy via the mTLS cluster-port probe; full dispatch viability
	StatusDown     EndpointStatus = "DOWN"     // Unreachable (threshold consecutive probe failures)
	StatusDegraded EndpointStatus = "DEGRADED" // Either (a) one miss within hysteresis grace, or (b) only public /health answers (mTLS broken)
	StatusUnknown  EndpointStatus = "UNKNOWN"  // Freshly registered, no probe yet
)

// DispatchStrategy defines routing strategy
type DispatchStrategy int

const (
	StrategyUnicast    DispatchStrategy = iota // Single target (default)
	StrategyBroadcast                          // All nodes
	StrategyStreaming                          // Streaming response
	StrategyRoundRobin                         // Load balance
	StrategyFailover                           // Try nodes in order until success
)

// LocalHandler interface for in-process local dispatch
type LocalHandler interface {
	ServeClusterRequest(ctx context.Context, req *Request) (*Response, error)
}

// Config holds cluster networking configuration
// Duplicated from pkg/config to avoid circular dependency
type Config struct {
	Enabled          bool
	BindAddr         string
	BindPort         int
	AdvertisePort    int
	Members          []string
	Endpoints        []string
	StrategyDefaults []StrategyDefault
	NodeName         string // Configured hostname from server config

	// NameObserver receives each peer's own node name as probes resolve
	// it, so the caller can cache it across restarts. Optional.
	NameObserver NameObserver
}

// StrategyDefault defines default strategy for path patterns
type StrategyDefault struct {
	PathPrefix string
	Default    string
}

// NewRequestFromHTTP creates a cluster request from HTTP request
func NewRequestFromHTTP(r *http.Request) *Request {
	body, _ := io.ReadAll(io.LimitReader(r.Body, constants.MaxClusterRequestBodySize))
	_ = r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(body))

	return &Request{
		Method:   r.Method,
		Path:     r.URL.Path,
		Query:    r.URL.RawQuery,
		Headers:  r.Header.Clone(),
		Body:     body,
		Metadata: make(map[string]any),
	}
}

// WriteHTTP writes response to HTTP response writer
func (r *Response) WriteHTTP(w http.ResponseWriter) error {
	// Copy headers
	maps.Copy(w.Header(), r.Headers)

	w.WriteHeader(r.StatusCode)
	_, err := w.Write(r.Body)
	return err
}

// String returns strategy name
func (s DispatchStrategy) String() string {
	switch s {
	case StrategyUnicast:
		return "unicast"
	case StrategyBroadcast:
		return "broadcast"
	case StrategyStreaming:
		return "streaming"
	case StrategyRoundRobin:
		return "round-robin"
	case StrategyFailover:
		return "failover"
	default:
		return "unknown"
	}
}

// String returns endpoint status name
func (s EndpointStatus) String() string {
	return string(s)
}

// ============================================================================
// Resource Metrics - Real-time resource availability tracking
// ============================================================================

// ResourceMetrics represents real-time resource availability on a node
type ResourceMetrics struct {
	// GPU memory metrics (in MB for precision)
	GPUMemoryTotalMB int64 `json:"gpu_memory_total_mb"`
	GPUMemoryUsedMB  int64 `json:"gpu_memory_used_mb"`
	GPUMemoryFreeMB  int64 `json:"gpu_memory_free_mb"`

	// GPU utilization (0-100 percentage)
	GPUUtilization float64 `json:"gpu_utilization"`

	// GPU type and count
	GPUType  string `json:"gpu_type"`  // "nvidia", "amd", "apple", "none"
	GPUCount int    `json:"gpu_count"` // Number of GPUs

	// RAM metrics (in MB for consistency)
	RAMTotalMB     int64 `json:"ram_total_mb"`
	RAMAvailableMB int64 `json:"ram_available_mb"`

	// Active model information
	ActiveModels int `json:"active_models"` // Number of models currently loaded

	// Metadata
	NodeName    string `json:"node_name"`    // Node identifier
	CollectedAt int64  `json:"collected_at"` // Unix timestamp of collection
}

// HasGPU returns true if the node has GPU resources
func (r *ResourceMetrics) HasGPU() bool {
	return r.GPUCount > 0 && r.GPUType != "none" && r.GPUType != ""
}

// GPUMemoryUsedPercent returns the percentage of GPU memory used
func (r *ResourceMetrics) GPUMemoryUsedPercent() float64 {
	if r.GPUMemoryTotalMB == 0 {
		return 0
	}
	return float64(r.GPUMemoryUsedMB) / float64(r.GPUMemoryTotalMB) * 100
}

// RAMUsedPercent returns the percentage of RAM used
func (r *ResourceMetrics) RAMUsedPercent() float64 {
	if r.RAMTotalMB == 0 {
		return 0
	}
	used := r.RAMTotalMB - r.RAMAvailableMB
	return float64(used) / float64(r.RAMTotalMB) * 100
}

// CanFitModel checks if there's enough GPU memory (or RAM for CPU inference) to fit a model
func (r *ResourceMetrics) CanFitModel(requiredMemoryMB int64, useGPU bool) bool {
	if useGPU && r.HasGPU() {
		return r.GPUMemoryFreeMB >= requiredMemoryMB
	}
	// CPU inference uses RAM
	return r.RAMAvailableMB >= requiredMemoryMB
}
