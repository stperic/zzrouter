package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	clusternode "github.com/stperic/zzrouter/pkg/cluster/node"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/discovery/gpu"
	"github.com/stperic/zzrouter/pkg/host/inventory"
	"github.com/stperic/zzrouter/pkg/modelregistry"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/routing"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stperic/zzrouter/pkg/version"
)

// ============================================================================
// Nodes Module - Unified Node Information Layer
// ============================================================================
//
// This file consolidates all node-related code:
// - NodesService: Business logic, reads from coordinator's EndpointRegistry
// - Public handlers: HTTP layer for /zzrouter/nodes
// - Executor: Internal API for /zzrouter/internal/nodes

// ============================================================================
// ClusterState - interface for listing cluster nodes
// ============================================================================

// ClusterState provides cluster node and app information.
// Used by NodesService and AppsService to read from the coordinator's cached state.
type ClusterState interface {
	// GetLocalNodeInfo returns this node's full info (coordinator or standalone)
	GetLocalNodeInfo() map[string]any
	// GetLocalAppsInfo returns this node's app details
	GetLocalAppsInfo() []prov_apps.LocalProviderInfo
	// GetClusterEndpoints returns all worker endpoints from the registry (nil if standalone/worker)
	GetClusterEndpoints() []*mesh.Endpoint
	// IsCoordinator returns true if this node is the cluster coordinator
	IsCoordinator() bool
	// GetStartTime returns the server start time (for uptime)
	GetStartTime() time.Time
	// RefreshClusterEndpoints synchronously re-probes all workers to refresh system snapshots
	RefreshClusterEndpoints()
	// RefreshClusterEndpoint re-probes a single worker by URL to get fresh data
	RefreshClusterEndpoint(url string)
}

// ============================================================================
// Service Layer
// ============================================================================

// NodesService handles host-related business logic
type NodesService struct {
	provider ClusterState
	router   routing.Router // still used for ListCompatibleNodes (needs actual worker participation)

	// peerName resolves a peer's cached node name from its host:port.
	// The registry only learns a peer's name by probing it, so between
	// a coordinator restart and the first successful probe there is no
	// name to report; node.yaml remembers one across that gap. May be
	// nil, in which case the pre-probe window simply stays as it was.
	peerName func(address string) string
}

// NewNodesService creates a new nodes service. peerName may be nil.
func NewNodesService(provider ClusterState, router routing.Router, peerName func(address string) string) *NodesService {
	return &NodesService{provider: provider, router: router, peerName: peerName}
}

// ListNodesRequest represents a request to list hosts
type ListNodesRequest struct {
	Node    string // Filter by specific host name (optional)
	Refresh bool   // Force re-probe all workers before listing
}

// ListNodesResponse represents the response for listing hosts
// Uses standard envelope: data, total, has_more
type ListNodesResponse struct {
	Data    []map[string]any `json:"data"`
	Total   int              `json:"total"`
	HasMore bool             `json:"has_more,omitempty"`
}

// ListNodes retrieves all nodes by reading from the coordinator's EndpointRegistry.
// No broadcasting — the coordinator already tracks all workers via HealthMonitor.
// Pass Refresh=true to synchronously re-probe all workers before reading.
func (s *NodesService) ListNodes(_ context.Context, req *ListNodesRequest) (*ListNodesResponse, error) {
	slog.Info("[NodesService] ListNodes", "node", req.Node, "refresh", req.Refresh)

	// If refresh requested, re-probe all workers to get fresh system snapshots
	if req.Refresh {
		s.provider.RefreshClusterEndpoints()
	}

	var nodes []map[string]any

	// 1. Add this node (coordinator/standalone) first
	// Copy the cached map to avoid mutating the cache
	cached := s.provider.GetLocalNodeInfo()
	localInfo := make(map[string]any, len(cached)+1)
	maps.Copy(localInfo, cached)
	localInfo["uptime_seconds"] = int(time.Since(s.provider.GetStartTime()).Seconds())

	// Filter by name if requested
	localName, _ := localInfo["name"].(string)
	if req.Node != "" && !strings.EqualFold(localName, req.Node) {
		// skip local node, it doesn't match
	} else {
		nodes = append(nodes, localInfo)
	}
	// Everything after this point is a worker, and only workers get sorted.
	localCount := len(nodes)

	// 2. Add worker nodes from EndpointRegistry
	endpoints := s.provider.GetClusterEndpoints()
	for _, ep := range endpoints {
		if ep.IsLocal {
			continue // already added above
		}
		nodeInfo := s.endpointToNodeInfo(ep)

		// Filter by name if requested
		epName, _ := nodeInfo["name"].(string)
		if req.Node != "" && !strings.EqualFold(epName, req.Node) {
			continue
		}
		nodes = append(nodes, nodeInfo)
	}

	// Workers come out of the endpoint registry, which is a map, so without
	// this the list reshuffles between calls. That matters wherever a caller
	// acts on a row by position: a refresh landing between choosing a node
	// and acting on it sends the action somewhere else. The local node keeps
	// its place at the head; only the workers are ordered.
	sortNodesByName(nodes[localCount:])

	slog.Info("[NodesService] ListNodes result", "count", len(nodes))
	return &ListNodesResponse{Data: nodes, Total: len(nodes), HasMore: false}, nil
}

// sortNodesByName orders node payloads by their "name" field. A payload
// missing one sorts first rather than at a random index.
func sortNodesByName(nodes []map[string]any) {
	// Stable, so payloads sharing a name (or both missing one) keep a fixed
	// order instead of falling back to map order.
	sort.SliceStable(nodes, func(i, j int) bool {
		a, _ := nodes[i]["name"].(string)
		b, _ := nodes[j]["name"].(string)
		return a < b
	})
}

// snapshotGPUInfo renders a peer's GPU block from its health snapshot,
// or nil when the peer reported no GPU. Split out of endpointToNodeInfo
// so the per-card shaping is readable on its own.
func snapshotGPUInfo(snap mesh.EndpointSnapshot) map[string]any {
	if snap.GPUCount == 0 {
		return nil
	}

	gpuInfo := map[string]any{
		"count": snap.GPUCount,
		"type":  snap.GPUType,
	}
	// Apple Silicon shares one pool with system RAM. Say so, rather than
	// hiding the GPU: a caller that cannot see the accelerator will not
	// schedule anything on it, and one that cannot see the sharing will
	// double-count the node's memory.
	if snap.GPUType == "apple" {
		gpuInfo["unified_memory"] = true
	}

	// Per-card detail wins when the worker reported it (post-v7 binaries
	// fill snap.GPUs from /internal/health). Pre-v7 workers only populate
	// the flat scalars, so we fall back to the single-summary shape.
	if len(snap.GPUs) > 0 {
		cards := make([]map[string]any, 0, len(snap.GPUs))
		for _, g := range snap.GPUs {
			cards = append(cards, gpuCardFields(g))
		}
		gpuInfo["gpus"] = cards
		return gpuInfo
	}

	if snap.VRAMTotalGB > 0 {
		summary := map[string]any{
			"index":           0,
			"name":            snap.GPUName,
			"memory_total_gb": math.Round(snap.VRAMTotalGB*10) / 10,
		}
		if snap.VRAMAvailableGB > 0 {
			summary["memory_available_gb"] = math.Round(snap.VRAMAvailableGB*10) / 10
		}
		gpuInfo["gpus"] = []map[string]any{summary}
	}
	return gpuInfo
}

// gpuCardFields renders one card. Optional identifiers are omitted rather
// than zeroed: absent means "not measured", which is not the same claim as
// zero — a card reporting 0 GB free reads as full.
func gpuCardFields(g mesh.GPUDetail) map[string]any {
	card := map[string]any{
		"index":           g.Index,
		"name":            g.Name,
		"memory_total_gb": math.Round(g.MemoryTotalGB*10) / 10,
	}
	if g.MemoryAvailGB != nil {
		card["memory_available_gb"] = math.Round(*g.MemoryAvailGB*10) / 10
	}
	if g.Vendor != "" {
		card["vendor"] = g.Vendor
	}
	if g.PCIAddress != "" {
		card["pci_address"] = g.PCIAddress
	}
	if g.UUID != "" {
		card["uuid"] = g.UUID
	}
	if g.DriverIndex != nil {
		card["driver_index"] = *g.DriverIndex
	}
	return card
}

// endpointToNodeInfo converts a mesh.Endpoint (from the registry) to the node info map
func (s *NodesService) endpointToNodeInfo(ep *mesh.Endpoint) map[string]any {
	snap := ep.Snapshot

	// Map EndpointStatus to health_status string
	healthStatus := "unknown"
	switch ep.Status {
	case mesh.StatusUp:
		healthStatus = "healthy"
	case mesh.StatusDown:
		healthStatus = "down"
	case mesh.StatusDegraded:
		healthStatus = "degraded"
	}

	// Extract host + port from the endpoint URL. These are the
	// ground-truth values written at registration time; snapshot-
	// derived overrides (from a successful probe) take precedence
	// below but we always have a sensible fallback for peers that
	// haven't been probed yet.
	ipAddress := ""
	urlPort := 0
	if parsed, err := url.Parse(ep.URL); err == nil {
		ipAddress = parsed.Hostname()
		if p := parsed.Port(); p != "" {
			urlPort, _ = strconv.Atoi(p)
		}
	}

	// NodeName is set only by a successful probe. Falling straight
	// through to ep.Name means the URL (RegisterAdminEndpoint seeds Name
	// with it), and a URL is not a name: every `node` selector in the API
	// takes a node name, so GET /nodes would be advertising an
	// identifier its siblings reject. The cached name closes that window.
	name := ep.NodeName
	if name == "" && s.peerName != nil && ipAddress != "" && urlPort > 0 {
		name = s.peerName(net.JoinHostPort(ipAddress, strconv.Itoa(urlPort)))
	}
	if name == "" {
		name = ep.Name
	}
	if name == "" {
		name = ep.URL
	}

	clusterRole := string(config.ClusterModeWorker)
	if snap.ClusterRole != "" {
		clusterRole = snap.ClusterRole
	}

	// Port: prefer the probed bind address, fall back to the endpoint
	// URL's port so unprobed peers still report the address they were
	// registered with — avoids a `:0` leaking into ListNodes (and into
	// DELETE /cluster/endpoints/:addr from the TUI).
	port := urlPort
	if snap.Address != "" {
		if _, p, err := net.SplitHostPort(snap.Address); err == nil {
			if parsed, perr := strconv.Atoi(p); perr == nil && parsed > 0 {
				port = parsed
			}
		}
	}
	nodeInfo := map[string]any{
		"name":           name,
		"ip_address":     ipAddress,
		"port":           port,
		"address":        fmt.Sprintf("%s:%d", ipAddress, port),
		"cluster_role":   clusterRole,
		"health_status":  healthStatus,
		"os":             snap.OS,
		"version":        snap.Version,
		"uptime_seconds": snap.UptimeSeconds,
	}

	// health_status alone is an observation with no date on it: a peer
	// that stopped answering between probes still reads "healthy", so a
	// caller picking a node cannot tell one that answered a second ago
	// from one last reached twenty minutes ago.
	//
	// This is last_seen_at, not last_checked_at, and the distinction is
	// load-bearing: CollectedAt only advances on a SUCCESSFUL probe
	// (Registry.Observe applies the snapshot only when probe.OK), so on a
	// failing peer it is the last time the node was reachable while the
	// failing check ran seconds ago. Naming it after the check would have
	// told a caller the coordinator had stopped probing. The reason for
	// the most recent failure is published as last_error below.
	if !snap.CollectedAt.IsZero() {
		nodeInfo["last_seen_at"] = snap.CollectedAt.UTC().Format(time.RFC3339)
	}

	if snap.RAMTotalGB > 0 {
		usedPercent := (snap.RAMTotalGB - snap.RAMAvailableGB) / snap.RAMTotalGB * 100
		nodeInfo["memory"] = map[string]any{
			"total_gb":     math.Round(snap.RAMTotalGB*10) / 10,
			"available_gb": math.Round(snap.RAMAvailableGB*10) / 10,
			"used_percent": math.Round(usedPercent*10) / 10,
		}
	}

	if snap.DiskTotalGB > 0 {
		usedPercent := (snap.DiskTotalGB - snap.DiskAvailableGB) / snap.DiskTotalGB * 100
		nodeInfo["disk"] = map[string]any{
			"total_gb":     math.Round(snap.DiskTotalGB),
			"available_gb": math.Round(snap.DiskAvailableGB),
			"used_percent": math.Round(usedPercent*10) / 10,
		}
	}

	if gpuInfo := snapshotGPUInfo(snap); gpuInfo != nil {
		nodeInfo["gpu"] = gpuInfo
	}

	if len(snap.Apps) > 0 {
		apps := make([]map[string]any, 0, len(snap.Apps))
		for _, a := range snap.Apps {
			apps = append(apps, nodeProviderInfo(a))
		}
		nodeInfo["providers"] = apps
	}

	// Sanitized like every other error that reaches a client: this is a
	// raw transport/TLS error from the probe, and route_error.go states
	// the policy that such errors are scrubbed before they go on the wire.
	if snap.LastError != "" {
		nodeInfo["last_error"] = utils.SanitizeErrorMessage(snap.LastError)
	}

	return nodeInfo
}

// GetNode retrieves a single node with fresh data.
// For remote workers, it re-probes the endpoint before reading from the registry.
func (s *NodesService) GetNode(_ context.Context, nodeName string) (map[string]any, error) {
	// Check if it's the local node
	localInfo := s.provider.GetLocalNodeInfo()
	localInfo["uptime_seconds"] = int(time.Since(s.provider.GetStartTime()).Seconds())
	localName, _ := localInfo["name"].(string)
	if strings.EqualFold(localName, nodeName) {
		return localInfo, nil
	}

	// Find the worker endpoint and refresh it
	endpoints := s.provider.GetClusterEndpoints()
	for _, ep := range endpoints {
		if ep.IsLocal {
			continue
		}
		name := ep.NodeName
		if name == "" {
			name = ep.Name
		}
		if strings.EqualFold(name, nodeName) {
			// Re-probe this specific worker for fresh data
			s.provider.RefreshClusterEndpoint(ep.URL)
			// Re-read from registry after refresh
			updated := s.provider.GetClusterEndpoints()
			for _, uep := range updated {
				uName := uep.NodeName
				if uName == "" {
					uName = uep.Name
				}
				if strings.EqualFold(uName, nodeName) {
					return s.endpointToNodeInfo(uep), nil
				}
			}
		}
	}

	return nil, fmt.Errorf("node %q not found", nodeName)
}

// ListCompatibleNodesRequest represents a request to list compatible hosts for a model
type ListCompatibleNodesRequest struct {
	Model    string
	Registry string
	Provider string
}

// ListCompatibleNodesResponse represents the response for listing compatible hosts
// Uses standard envelope: data, total, has_more
type ListCompatibleNodesResponse struct {
	Data    []map[string]any `json:"data"`
	Total   int              `json:"total"`
	HasMore bool             `json:"has_more,omitempty"`
}

// ListCompatibleNodes retrieves hosts compatible with a given model
func (s *NodesService) ListCompatibleNodes(ctx context.Context, req *ListCompatibleNodesRequest) (*ListCompatibleNodesResponse, error) {
	slog.Info("[NodesService] ListCompatibleNodes", "model", req.Model)

	ctx, cancel := ensureTimeout(ctx, constants.ClusterActionTimeout)
	defer cancel()

	query := url.Values{"model": {req.Model}}
	if req.Registry != "" {
		query.Set("registry", req.Registry)
	}
	if req.Provider != "" {
		query.Set("provider", req.Provider)
	}

	resp, err := s.router.Route(ctx, &routing.Request{
		Path:   "/zzrouter/v1/internal/nodes/compatible?" + query.Encode(),
		Method: "GET",
		Node:   "*",
	})
	if err != nil {
		return nil, newRoutedTransportError(err, broadcastNodeTarget)
	}
	if resp.StatusCode >= 400 {
		// Aggregator collapses to the inner body when all peers fail; surface as Problem Details rather than 500ing.
		return nil, parseRoutedError(resp.StatusCode, resp.Body)
	}

	var nodesData struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(resp.Body, &nodesData); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	// Peers answer a broadcast concurrently, so without this the list is
	// in completion order and reshuffles between identical calls. This
	// endpoint feeds node pickers, and a caller acting on a row by
	// position would be sent to a different node than the one it chose.
	sortNodesByName(nodesData.Data)

	return &ListCompatibleNodesResponse{Data: nodesData.Data, Total: len(nodesData.Data), HasMore: false}, nil
}

// ============================================================================
// Public Handlers (HTTP Layer)
// ============================================================================

// ============================================================================
// ClusterState interface implementation on Server
// ============================================================================

// GetLocalNodeInfo returns this node's full info (implements ClusterState).
// Inventory lookups have their own 30s TTL cache (inventory.Default), so
// this is cheap per call and needs no Server-level cache.
func (s *Server) GetLocalNodeInfo() map[string]any {
	nodeName := s.node.Nodename()

	versionStr := "unknown"
	if version.Current != nil {
		versionStr = version.Current.String()
		if versionStr == "" || versionStr == "0.0.0" {
			versionStr = "dev"
		}
	}

	clusterRole := string(s.node.Mode())
	if clusterRole == string(config.ClusterModeDisabled) {
		clusterRole = "coordinator"
	}

	ipAddr, port, addr := s.nodeEndpointFields()
	hostInfo := map[string]any{
		"name":          nodeName,
		"ip_address":    ipAddr,
		"port":          port,
		"address":       addr,
		"cluster_role":  clusterRole,
		"health_status": "healthy",
		"os":            getOSName(),
		"version":       versionStr,
		// This node is answering the request, so it is being reached
		// right now. Same meaning as the peer field: last successful
		// observation.
		"last_seen_at": utils.NowUTC().Format(time.RFC3339),
	}

	apps := []map[string]any{}
	for _, app := range s.GetLocalAppsInfo() {
		apps = append(apps, nodeProviderInfo(app))
	}
	hostInfo["providers"] = apps

	ctx := context.Background()
	cache := inventory.Default()
	modelsDir := filepath.Join(config.NewConfigManager("zzrouter").GetNodeConfigDir(), "models")
	if diskInfo := inventory.GatherDisk(ctx, modelsDir, cache); diskInfo != nil {
		hostInfo["disk"] = diskInfo
	}
	if memInfo := inventory.GatherMemory(ctx, cache); memInfo != nil {
		hostInfo["memory"] = memInfo
	}
	if gpuInfo := inventory.GatherGPU(ctx, gpu.List(false), cache); gpuInfo != nil {
		hostInfo["gpu"] = gpuInfo
	}
	return hostInfo
}

func nodeProviderInfo(info prov_apps.LocalProviderInfo) map[string]any {
	result := map[string]any{"key": info.Key, "type": info.Type, "name": info.Name,
		"formats": info.Formats, "cloud": info.Kind == string(config.KindCloud), "kind": info.Kind}
	if info.Service != nil {
		result["running"] = info.Service.Running
		result["service"] = info.Service.Clone()
	}
	return result
}

// GetLocalAppsInfo returns app details for this node (implements ClusterState).
// On coord with a populated self-slot, reads from there; otherwise computes
// fresh from the provider manager. ProviderAppManager.LocalInfo is cheap.
func (s *Server) GetLocalAppsInfo() []prov_apps.LocalProviderInfo {
	if s.cluster.coordinator != nil {
		if self := s.cluster.coordinator.Self(); self != nil {
			return self.Snapshot.Apps
		}
	}
	return s.buildLocalAppsInfo()
}

// publishSelfSnapshot computes a fresh self-slot snapshot and publishes
// it to the registry. No-op when the coord subsystem isn't wired (worker
// mode, pre-claim startup). Safe to call repeatedly.
func (s *Server) publishSelfSnapshot() {
	if s.cluster.coordinator == nil {
		return
	}
	s.cluster.coordinator.ObserveLocal(s.buildSelfSnapshot())
}

// buildSelfSnapshot composes the coord's own EndpointSnapshot from the
// provider manager's local view. Resource fields stay zero — consumers
// that need them go through GetLocalNodeInfo's inventory path.
func (s *Server) buildSelfSnapshot() mesh.EndpointSnapshot {
	versionStr := ""
	if version.Current != nil {
		versionStr = version.Current.String()
	}
	return mesh.EndpointSnapshot{
		HealthReport: mesh.HealthReport{
			ClusterRole: string(s.node.Mode()),
			Apps:        s.buildLocalAppsInfo(),
		},
		Version:     versionStr,
		CollectedAt: utils.Now(),
	}
}

// GetClusterEndpoints returns all worker endpoints from the registry (implements ClusterState)
func (s *Server) GetClusterEndpoints() []*mesh.Endpoint {
	if s.cluster.coordinator == nil {
		return nil
	}
	if !s.node.IsCoordinator() {
		return nil
	}
	return s.cluster.coordinator.GetAllEndpoints()
}

// resolveNodePublicURL returns a node's public URL by name, or empty
// string when the node isn't in the endpoint registry. Used by
// DeploymentsService to build /sync/deploy SourceURL payloads.
// Coordinator-only — on workers GetClusterEndpoints returns nil, so
// the empty-string return collapses peer-sync cleanly to the normal
// registry-pull fallback.
func (s *Server) resolveNodePublicURL(name string) string {
	if name == "" {
		return ""
	}
	for _, ep := range s.GetClusterEndpoints() {
		if ep == nil {
			continue
		}
		// NodeName is the runtime-discovered identifier the deploy
		// service uses; Name/Alias are the user-visible handles.
		// Either can match.
		if ep.NodeName == name || ep.Name == name || ep.Alias == name {
			return ep.URL
		}
	}
	return ""
}

// IsCoordinator reports whether the node is currently acting as
// coordinator. Reads through role.Manager so ClusterState
// consumers observe runtime transitions; use s.node.IsCoordinator()
// only for config-source-of-truth identity checks.
func (s *Server) IsCoordinator() bool {
	return s.role.Current().IsCoordinator()
}

// GetStartTime returns the server start time (implements ClusterState)
func (s *Server) GetStartTime() time.Time {
	return s.startTime
}

// RefreshClusterEndpoints synchronously re-probes all workers and republishes
// the coord's self-slot (implements ClusterState).
func (s *Server) RefreshClusterEndpoints() {
	s.publishSelfSnapshot()
	if s.cluster.coordinator != nil {
		s.cluster.coordinator.RefreshAllEndpoints()
	}
}

// RefreshClusterEndpoint re-probes a single worker (implements ClusterState)
func (s *Server) RefreshClusterEndpoint(url string) {
	if s.cluster.coordinator != nil {
		s.cluster.coordinator.RefreshEndpoint(url)
	}
}

// MarkClusterEndpointDown forces a worker endpoint to StatusDown
// without probing. Used by the goodbye handler when a worker declares
// graceful shutdown; unknown URLs are an idempotent no-op.
func (s *Server) MarkClusterEndpointDown(url string) {
	if s.cluster.coordinator == nil {
		return
	}
	s.cluster.coordinator.MarkEndpointDown(url, errors.New("peer reported graceful shutdown"))
}

// workerPublicURL builds the URL this worker publishes for coordinator
// dispatch: scheme://host:node-port. Resolution is delegated to
// clusternode.ResolveSelfHost (bind → first non-loopback advertise IP
// → nodeName). Returns "" on non-worker nodes.
func (s *Server) workerPublicURL() string {
	if !s.role.Current().IsWorker() {
		return ""
	}
	scheme := "http"
	if s.config.Node.IsTLSEnabled() {
		scheme = "https"
	}
	ips := clusternode.ParseAdvertiseIPs(s.config.Cluster.AdvertiseIPs)
	host := clusternode.ResolveSelfHost(s.config.Node.Bind, s.config.Node.Name, ips)
	return fmt.Sprintf("%s://%s:%d", scheme, host, s.config.Node.Port)
}

// RefreshClusterEndpointsAsync re-probes all endpoints in a drain-tracked
// goroutine. Coalesced: concurrent calls collapse to at most 1 in-flight +
// 1 queued, so install→enable double-notify doesn't fan out twice. No-op
// while draining.
func (s *Server) RefreshClusterEndpointsAsync() {
	if s.draining.Load() {
		return
	}
	s.refreshMu.Lock()
	if s.refreshRunning {
		s.refreshQueued = true
		s.refreshMu.Unlock()
		return
	}
	s.refreshRunning = true
	s.refreshMu.Unlock()
	s.refreshWG.Add(1)
	go s.runRefreshLoop()
}

// runRefreshLoop is the coalescer body. Runs RefreshClusterEndpoints,
// then checks whether another request arrived during the run and repeats
// if so. Does not re-check draining — new Async calls short-circuit at
// the entrypoint once draining is set, so the loop drains its ≤1 queued
// iteration then exits.
func (s *Server) runRefreshLoop() {
	defer s.refreshWG.Done()
	for {
		s.RefreshClusterEndpoints()
		s.refreshMu.Lock()
		if !s.refreshQueued {
			s.refreshRunning = false
			s.refreshMu.Unlock()
			return
		}
		s.refreshQueued = false
		s.refreshMu.Unlock()
	}
}

// ============================================================================
// Executor (Internal API)
// ============================================================================

// NodesExecutor handles direct local host operations
type NodesExecutor struct {
	getLocalNodeInfo  func() map[string]any
	isLocalCompatible func(model, repo, provider string) bool
	eligibleProviders func(model, repo, format string, force bool) []string
}

// NewNodesExecutor creates a new hosts executor
func NewNodesExecutor(getLocalNodeInfo func() map[string]any, isLocalCompatible func(model, repo, provider string) bool) *NodesExecutor {
	return &NodesExecutor{getLocalNodeInfo: getLocalNodeInfo, isLocalCompatible: isLocalCompatible}
}

// HandleInternalListNodes handles GET /zzrouter/internal/nodes (internal API)
func (e *NodesExecutor) HandleInternalListNodes(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"data": []map[string]any{e.getLocalNodeInfo()}})
}

// HandleInternalListCompatibleNodes handles GET /zzrouter/internal/nodes/compatible (internal API)
func (e *NodesExecutor) HandleInternalListCompatibleNodes(c *gin.Context) {
	model := QueryModel(c)
	if model == "" {
		BadRequest(c, "model parameter is required")
		return
	}

	nodes := []map[string]any{}
	var eligible []string
	force := c.Query("force") == "true"
	if e.eligibleProviders != nil {
		eligible = e.eligibleProviders(model, c.Query("registry"), c.Query("format"), force)
	}
	compatible := e.isLocalCompatible(model, c.Query("registry"), c.Query("provider"))
	if e.eligibleProviders != nil && (c.Query("format") != "" || force) {
		compatible = len(eligible) > 0
	}
	if compatible {
		hostInfo := e.getLocalNodeInfo()
		hostInfo["compatible"] = true
		if e.eligibleProviders != nil {
			hostInfo["eligible_providers"] = eligible
		}
		nodes = append(nodes, hostInfo)
	}
	c.JSON(http.StatusOK, gin.H{"data": nodes})
}

// eligibleDeployProviders names installed or configured local providers that
// can serve the requested format, even when the weights are already cached.
func (s *Server) eligibleDeployProviders(model, repo, format string, force bool) []string {
	if format == "" {
		format = modelregistry.DetectFormatFromName(model)
	}
	names := []string{}
	if s.providers.appMgr == nil {
		return names
	}
	for _, info := range s.buildCompatibleAppsInfo() {
		if metadata.RepoAppCompatible(repo, info.Type) && (force || modelregistry.FormatSupported(info.Formats, format)) {
			names = append(names, info.Key)
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// buildLocalAppsInfo builds the app details list from the
// provider registry. Called by refreshLocalCaches to populate
// the cache entry served by /hosts and by the /health handler's
// apps_detail field.
//
// Wire-format note: each entry carries both "mode" (legacy, preserved
// for back-compat with older client builds) and "kind" (canonical new
// discriminator from config.Provider.Kind()). During the kind-split
// arc the two always match; post-Commit-6 only "kind" remains.
// buildLocalAppsInfo returns this node's routable providers. Filters
// ProviderAppManager.LocalInfo down to live/cloud-available — dormant /
// installing / not-installed stay internal to install coordinator and
// recovery APIs.
func (s *Server) buildLocalAppsInfo() []prov_apps.LocalProviderInfo {
	infos := s.providers.appMgr.LocalInfo(s.node.Name())
	out := make([]prov_apps.LocalProviderInfo, 0, len(infos))
	for _, info := range infos {
		if !info.State.IsRoutable() {
			continue
		}
		if info.Managed && info.Endpoint != "" {
			status, err := s.providers.appMgr.ProviderServiceStatus(context.Background(), info.Key)
			if err == nil {
				info.Service = &status
			} else {
				slog.Warn("Provider supervision observation failed", "provider", info.Key, "error", err)
			}
		}
		out = append(out, info)
	}
	return out
}

// buildCompatibleAppsInfo returns the providers that count when asking
// whether this node can serve a model. That is a weaker question than
// routability, and the difference matters in both directions:
//
//   - Routable is too strict. A provider's state collapses to
//     not-installed whenever its version is unknown, and for a provider
//     zzRouter did not install the only version source is a live probe of
//     the running service. An Ollama that was not yet listening when
//     zzRouter started therefore reads as absent, and the version cache
//     only re-syncs on a config reload — so every pull to that node would
//     400 while Ollama sat there serving inference.
//   - Every provider is too loose. On-demand providers ship enabled in
//     the template whether or not their binary was ever installed, which
//     is what let a Mac claim it could serve safetensors on the strength
//     of a vllm config file.
//
// So: an external provider is one the operator deliberately pointed us
// at, and we take its presence on trust. An on-demand provider has to be
// installed on disk. Dormant counts — a provider that is installed but
// stopped can still serve once started.
func (s *Server) buildCompatibleAppsInfo() []prov_apps.LocalProviderInfo {
	infos := s.providers.appMgr.LocalInfo(s.node.Name())
	out := make([]prov_apps.LocalProviderInfo, 0, len(infos))
	for _, info := range infos {
		if countsForCompatibility(info) {
			out = append(out, info)
		}
	}
	return out
}

// countsForCompatibility is the per-provider rule described on
// buildCompatibleAppsInfo.
func countsForCompatibility(info prov_apps.LocalProviderInfo) bool {
	switch info.Kind {
	case string(config.KindCloud):
		// A cloud provider serves a remote API; it cannot hold a model
		// file, and this predicate also gates local downloads. It is
		// excluded here rather than relying on cloud configs declaring no
		// formats — that convention makes the gate come out right today
		// but is enforced nowhere, so one cloud config gaining a formats
		// entry would silently make every node a download candidate.
		return false
	case string(config.KindExternal):
		return true
	default:
		return info.State.IsInstalled()
	}
}
