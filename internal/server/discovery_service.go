package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"sync"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/discovery"
	"github.com/stperic/zzrouter/pkg/discovery/gpu"
	"github.com/stperic/zzrouter/pkg/discovery/hardware"
	"github.com/stperic/zzrouter/pkg/discovery/network"
	"github.com/stperic/zzrouter/pkg/host/inventory"
	"github.com/stperic/zzrouter/pkg/routing"
)

// DiscoveryService performs local and cluster-wide resource discovery.
//
// All public methods return (data, error) — the controller handles HTTP
// response serialization. Each method implements the same routing pattern
// via Route: local, fan-out, or single-node proxy based on the node param.
type DiscoveryService struct {
	config       *pkgConfig.NodeConfig
	mdns         *network.NodeDiscovery // nullable — only needed for network host discovery
	router       routing.Router         // cluster mTLS dispatcher (nil on standalone nodes)
	clusterPeers func() []string        // names of every non-local cluster peer (typically Server.listJobsPeers). When nil, fanOutToCluster returns only the local node's result — there is no usable substitute, since config.Cluster.Endpoints holds host:port strings the mesh registry can't resolve back to a paired-peer NodeName.
	gpuInventory func() gpu.Inventory   // startup-cached inventory; injected by Server so /discover/hardware/gpus serves from cache instead of re-probing nvidia-smi on every request
}

// NewDiscoveryService creates a new discovery service.
//
// gpuInventory is a getter for the server's startup-cached
// gpu.Inventory. Required (not nullable) — DiscoveryService never
// falls back to a live probe because re-running nvidia-smi on
// every /discover/hardware/gpus request would defeat the startup
// cache. Pass Server.gpuInventory at wiring time.
//
// router is the cluster mTLS dispatcher used by collectRemote for
// cross-host fan-out. Nullable — standalone nodes have no peers and
// the route param "*"/specific-node never reaches the remote branch.
func NewDiscoveryService(
	config *pkgConfig.NodeConfig,
	mdns *network.NodeDiscovery,
	router routing.Router,
	gpuInventory func() gpu.Inventory,
) *DiscoveryService {
	return &DiscoveryService{
		config:       config,
		mdns:         mdns,
		router:       router,
		gpuInventory: gpuInventory,
	}
}

// WithClusterPeers wires the runtime peer-name source — typically
// Server.listJobsPeers, which reads the live mesh registry. Without it
// fanOutToCluster falls back to config.Cluster.Endpoints (host:port
// strings) which are not always populated on auto-paired clusters.
func (svc *DiscoveryService) WithClusterPeers(fn func() []string) *DiscoveryService {
	svc.clusterPeers = fn
	return svc
}

// Start warms the GPU inventory in the background so it's available
// by the time the first /discover or /health request arrives.
func (svc *DiscoveryService) Start() {
	if svc == nil {
		return
	}
	gpu.StartAsync()
}

// Stop is a no-op — the GPU probe is a one-shot sync.Once goroutine that
// exits naturally. Present for Start/Stop symmetry.
func (svc *DiscoveryService) Stop() {}

// ============================================================================
// Routed operations — called from DiscoveryController
// ============================================================================

// Route dispatches a discovery call based on the node parameter.
//   - ""/"localhost" → local
//   - "*"           → fan-out to all cluster nodes
//   - "specific"    → proxy to that node
func (svc *DiscoveryService) Route(ctx context.Context, node, endpoint string, fn discoveryFunc) (map[string]any, error) {
	switch {
	case node == "*":
		return svc.fanOutToCluster(ctx, endpoint, fn)
	case svc.isLocal(node):
		return fn(ctx)
	default:
		return svc.proxyToNode(ctx, node, endpoint)
	}
}

// discoveryFunc is a local discovery operation that returns data.
type discoveryFunc func(ctx context.Context) (map[string]any, error)

// DiscoverAll performs unified discovery (hardware + network).
//
// Provider/converter tool detection used to live here too, sourced
// from a hardcoded list at pkg/discovery/tools/definitions/. That
// list drifted from the canonical providers/<kind>/<name>/config.yaml
// tree,
// so it was retired alongside /discover/providers* and
// /discover/software. Use /runs/capabilities for provider listings —
// that endpoint reads the providers/ tree directly.
func (svc *DiscoveryService) DiscoverAll(ctx context.Context) (map[string]any, error) {
	service := discovery.NewService()
	result, err := service.DiscoverAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("discovery failed: %w", err)
	}

	response := map[string]any{
		"network_hosts": result.NetworkNodes,
		"hardware":      result.Hardware,
	}
	if len(result.Errors) > 0 {
		response["warnings"] = errorsToStrings(result.Errors)
	}
	return response, nil
}

// DiscoverHardware performs a full hardware scan (GPU, CPU, RAM, disk).
func (svc *DiscoveryService) DiscoverHardware(ctx context.Context) (map[string]any, error) {
	hwDiscovery := hardware.NewDiscovery()
	hwInfo, errs := hwDiscovery.DiscoverAll(ctx)

	response := map[string]any{
		"gpu_type":         hwInfo.GPUType,
		"gpu_count":        hwInfo.GPUCount,
		"gpus":             hwInfo.GPUs,
		"cpu_info":         hwInfo.CPUInfo,
		"total_ram":        hwInfo.TotalRAM,
		"available_ram":    hwInfo.AvailableRAM,
		"total_ram_gb":     float64(hwInfo.TotalRAM) / 1024 / 1024 / 1024,
		"available_ram_gb": float64(hwInfo.AvailableRAM) / 1024 / 1024 / 1024,
		"disk_info":        hwInfo.DiskInfo,
	}
	if len(errs) > 0 {
		response["warnings"] = errorsToStrings(errs)
	}
	return response, nil
}

// DiscoverGPUs performs GPU-only discovery.
//
// Serves from the server's startup-cached gpu.Inventory rather
// than re-probing nvidia-smi / rocm-smi / ghw per request. Static
// inventory doesn't change at runtime; a driver install would
// require a cache refresh (not yet implemented — planned with
// Phase 2 automated driver install).
//
// Startup probe diagnostics (e.g. "NVML driver/library version
// mismatch" captured by ProbeContext on the first scan) are
// forwarded in the "warnings" field so an operator debugging
// "why is my card missing?" sees the reason inline instead of
// having to grep server logs.
func (svc *DiscoveryService) DiscoverGPUs(_ context.Context) (map[string]any, error) {
	inv := svc.gpuInventory()
	hwInfo := hardware.InventoryToHardwareInfo(inv)

	response := map[string]any{
		"gpu_type":  hwInfo.GPUType,
		"gpu_count": hwInfo.GPUCount,
		"gpus":      hwInfo.GPUs,
	}
	if warnings := inventory.StartupGPUWarnings(inv); len(warnings) > 0 {
		response["warnings"] = warnings
	}
	return response, nil
}

// DiscoverNetworkNodes performs mDNS-based node discovery.
func (svc *DiscoveryService) DiscoverNetworkNodes(ctx context.Context) (map[string]any, error) {
	hostDiscovery := svc.mdns
	if hostDiscovery == nil {
		hostDiscovery = network.NewNodeDiscovery("", false, 0)
	}

	hosts, err := hostDiscovery.DiscoverNodesWithContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("network host discovery failed: %w", err)
	}
	return map[string]any{"hosts": hosts, "count": len(hosts)}, nil
}

// ============================================================================
// Cluster routing
// ============================================================================

func (svc *DiscoveryService) isLocal(host string) bool {
	return host == "" || host == constants.Localhost || host == svc.config.Node.Name
}

// fanOutToCluster aggregates discovery results from all cluster nodes.
//
// Peer iteration uses the runtime registry (clusterPeers) — auto-paired
// clusters keep peers in the mesh registry, not in node.yaml. When
// clusterPeers is nil (standalone tests, pre-pairing coord), only the
// local node is collected: config.Cluster.Endpoints can't substitute
// because its host:port strings won't round-trip through the mesh
// registry's name-indexed Unicast lookup.
func (svc *DiscoveryService) fanOutToCluster(ctx context.Context, endpoint string, localFn discoveryFunc) (map[string]any, error) {
	var peers []string
	if svc.clusterPeers != nil {
		peers = svc.clusterPeers()
	}
	allNodes := append([]string{svc.config.Node.Name}, peers...)
	results := make([]ClusterDiscoveryResult, len(allNodes))
	var wg sync.WaitGroup

	for i, host := range allNodes {
		wg.Add(1)
		go func(idx int, hostname string) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					slog.Error("PANIC recovered in discovery worker", "node", hostname, "panic", r, "stack", string(debug.Stack()))
					results[idx] = ClusterDiscoveryResult{Node: hostname, Error: fmt.Sprintf("panic: %v", r)}
				}
			}()

			if hostname == svc.config.Node.Name {
				results[idx] = svc.collectLocal(ctx, localFn)
			} else {
				results[idx] = svc.collectRemote(ctx, hostname, endpoint)
			}
		}(i, host)
	}

	wg.Wait()

	return map[string]any{
		"cluster_wide": true,
		"hosts":        results,
		"summary": map[string]any{
			"total_hosts":      len(results),
			"successful_hosts": countSuccessful(results),
			"failed_hosts":     countFailed(results),
		},
	}, nil
}

// proxyToNode routes a discovery request to a single remote node.
func (svc *DiscoveryService) proxyToNode(ctx context.Context, host, endpoint string) (map[string]any, error) {
	result := svc.collectRemote(ctx, host, endpoint)
	if !result.Success {
		return nil, fmt.Errorf("discovery failed on host %s: %s", host, result.Error)
	}
	return result.Data, nil
}

// collectLocal calls a local discovery function and wraps the result.
func (svc *DiscoveryService) collectLocal(ctx context.Context, fn discoveryFunc) ClusterDiscoveryResult {
	data, err := fn(ctx)
	if err != nil {
		return ClusterDiscoveryResult{
			Node:    svc.config.Node.Name,
			Address: fmt.Sprintf("%s:%d", svc.config.Node.Bind, svc.config.Node.Port),
			Error:   err.Error(),
		}
	}
	return ClusterDiscoveryResult{
		Node:    svc.config.Node.Name,
		Address: fmt.Sprintf("%s:%d", svc.config.Node.Bind, svc.config.Node.Port),
		Success: true,
		Data:    data,
	}
}

// collectRemote queries a remote cluster node for discovery data via
// the cluster mTLS dispatcher. The endpoint passed in is the worker's
// /zzrouter/v1/internal/* path — the worker's internal engine mounts
// these read-only discover handlers and the transport-level mTLS+OU
// gate on pkg/cluster/node is the auth boundary.
func (svc *DiscoveryService) collectRemote(ctx context.Context, hostname, endpoint string) ClusterDiscoveryResult {
	result := ClusterDiscoveryResult{
		Node:    hostname,
		Address: hostname,
	}

	if svc.router == nil {
		result.Error = "cluster router not configured"
		return result
	}

	resp, err := svc.router.Unicast(ctx, hostname, endpoint, http.MethodGet, nil)
	if err != nil {
		result.Error = fmt.Sprintf("failed to dispatch: %v", err)
		return result
	}

	if resp.StatusCode != http.StatusOK {
		result.Error = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(resp.Body))
		return result
	}

	var apiResp struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(resp.Body, &apiResp); err != nil {
		result.Error = fmt.Sprintf("failed to parse response: %v", err)
		return result
	}

	result.Success = true
	result.Data = apiResp.Data
	return result
}

// ============================================================================
// Types + helpers
// ============================================================================

// ClusterDiscoveryResult contains discovery results from a specific node.
type ClusterDiscoveryResult struct {
	Node    string         `json:"node"`
	Address string         `json:"address"`
	Success bool           `json:"success"`
	Data    map[string]any `json:"data,omitempty"`
	Error   string         `json:"error,omitempty"`
}

func countSuccessful(results []ClusterDiscoveryResult) int {
	n := 0
	for _, r := range results {
		if r.Success {
			n++
		}
	}
	return n
}

func countFailed(results []ClusterDiscoveryResult) int {
	n := 0
	for _, r := range results {
		if !r.Success {
			n++
		}
	}
	return n
}

func errorsToStrings(errs []error) []string {
	msgs := make([]string, len(errs))
	for i, e := range errs {
		msgs[i] = e.Error()
	}
	return msgs
}
