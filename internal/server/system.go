package server

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"runtime"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	"github.com/stperic/zzrouter/pkg/cluster/role"
	"github.com/stperic/zzrouter/pkg/connectivity"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/modelregistry"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/routing"
)

// ============================================================================
// System Module - Unified System Information Layer
// ============================================================================
//
// - SystemService: Business logic / routing layer
// - SystemExecutor / ResourcesExecutor: Internal API for cluster operations
// - V2 handler wrappers: Health/version/compat endpoints

// ============================================================================
// Service Layer
// ============================================================================

// SystemService handles system-related business logic
type SystemService struct {
	router routing.Router
}

// NewSystemService creates a new system service
func NewSystemService(router routing.Router) *SystemService {
	return &SystemService{router: router}
}

// GetSystemInfoRequest represents a request to get system information
type GetSystemInfoRequest struct {
	Node string // Target host (empty = cluster-wide aggregation)
}

// GetSystemInfoResponse represents the response for system information
type GetSystemInfoResponse struct {
	System map[string]any `json:"system"`
}

// GetSystemInfo retrieves system information via routing layer
func (s *SystemService) GetSystemInfo(ctx context.Context, req *GetSystemInfoRequest) (*GetSystemInfoResponse, error) {
	slog.Info("[SystemService] GetSystemInfo", "node", req.Node)

	ctx, cancel := ensureTimeout(ctx, constants.ClusterActionTimeout)
	defer cancel()

	// Create routing request
	path := "/zzrouter/v1/internal/system"
	if req.Node != "" {
		path += "?node=" + url.QueryEscape(req.Node)
	}

	routingReq := &routing.Request{
		Path:   path,
		Method: "GET",
		Node:   req.Node,
	}

	// Route the request
	resp, err := s.router.Route(ctx, routingReq)
	if err != nil {
		return nil, newRoutedTransportError(err, req.Node)
	}
	if resp.StatusCode >= 400 {
		// Preserve upstream status + Problem Details so RespondToError forwards 4xx verbatim instead of unmarshaling Problem fields into systemData.
		return nil, parseRoutedError(resp.StatusCode, resp.Body)
	}

	// Parse response
	var systemData map[string]any
	if err := json.Unmarshal(resp.Body, &systemData); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	return &GetSystemInfoResponse{System: systemData}, nil
}

// ============================================================================
// Executor (Internal API)
// ============================================================================

// SystemExecutor handles direct local system operations
type SystemExecutor struct {
	node                 *NodeIdentity
	role                 *role.Manager
	appMgr               *prov_apps.ProviderAppManager
	registry             *modelregistry.Registry
	getSystemInfo        func() map[string]any
	getPoolStats         func() *connectivity.ConnectionPoolStats
	invalidateCache      func()
	refreshEndpoints     func()           // Coord-side broadcast re-probe; nil on workers
	refreshEndpointByURL func(url string) // Coord-side narrowed re-probe; nil on workers
	markEndpointDown     func(url string) // Coord-side force DOWN; nil on workers
}

// NewSystemExecutor creates a new system executor
func NewSystemExecutor(node *NodeIdentity, rm *role.Manager, appMgr *prov_apps.ProviderAppManager, registry *modelregistry.Registry, getSystemInfo func() map[string]any, getPoolStats func() *connectivity.ConnectionPoolStats, invalidateCache func(), refreshEndpoints func(), refreshEndpointByURL func(string), markEndpointDown func(url string)) *SystemExecutor {
	return &SystemExecutor{node: node, role: rm, appMgr: appMgr, registry: registry, getSystemInfo: getSystemInfo, getPoolStats: getPoolStats, invalidateCache: invalidateCache, refreshEndpoints: refreshEndpoints, refreshEndpointByURL: refreshEndpointByURL, markEndpointDown: markEndpointDown}
}

// HandleInternalGetSystemInfo handles GET /zzrouter/internal/system (internal API)
func (e *SystemExecutor) HandleInternalGetSystemInfo(c *gin.Context) {
	slog.Info("[SystemExecutor] GetSystemInfo")
	c.JSON(http.StatusOK, e.getLocalSystemInfo())
}

// HandleInternalGoodbye handles POST /zzrouter/v1/internal/goodbye.
// Called by a worker during graceful shutdown to mark its endpoint
// DOWN immediately on the coordinator, bypassing the N-consecutive-
// miss liveness hysteresis that would otherwise delay DOWN detection
// until the next health-poll tick.
//
// Request body: {"node_url": "http://192.0.2.10:9090"}. The URL must
// exactly match a registered endpoint on the coordinator — coord-side
// `cluster.endpoints` list is the source of truth. Unknown URL 200s
// (idempotent: a second goodbye, or one from a node that isn't
// managed here, is a no-op).
func (e *SystemExecutor) HandleInternalGoodbye(c *gin.Context) {
	if e.node.IsWorker() {
		BadRequest(c, "workers don't manage endpoint liveness")
		return
	}
	var req struct {
		NodeURL string `json:"node_url"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		BadRequest(c, "invalid request body: "+err.Error())
		return
	}
	if req.NodeURL == "" {
		BadRequest(c, "node_url is required")
		return
	}
	slog.Info("[InternalAPI] Worker goodbye", "node_url", req.NodeURL)
	if e.markEndpointDown != nil {
		e.markEndpointDown(req.NodeURL)
	}
	c.JSON(http.StatusOK, gin.H{"status": "success"})
}

// HandleInternalRefreshCache handles POST /zzrouter/v1/internal/models/refresh.
// Called by worker nodes to notify coordinator when downloads/deletions complete.
//
// Narrowing: the body may carry {"node_url": "..."}; when non-empty AND
// the URL's host matches a SAN on the peer's client cert, the coord
// re-probes just that endpoint instead of broadcasting. Empty body,
// missing peer cert, or SAN mismatch falls back to broadcast — keeps
// the notify useful even if a worker hands up a malformed URL, while
// preventing a malicious authenticated worker from forcing re-probes
// of peers it doesn't own.
func (e *SystemExecutor) HandleInternalRefreshCache(c *gin.Context) {
	if e.node.IsWorker() {
		BadRequest(c, "workers don't have cache to refresh")
		return
	}

	var req struct {
		NodeURL string `json:"node_url"`
	}
	// Empty body is legal — older workers send {} for broadcast refresh.
	_ = c.ShouldBindJSON(&req)

	narrowOK := false
	if req.NodeURL != "" && e.refreshEndpointByURL != nil {
		narrowOK = nodeURLMatchesPeerCert(req.NodeURL, peerCertFromRequest(c.Request))
	}

	slog.Info("[InternalAPI] Cache refresh requested by worker",
		"node_url", req.NodeURL,
		"narrowed", narrowOK)
	e.invalidateCache()

	if narrowOK {
		e.refreshEndpointByURL(req.NodeURL)
	} else if e.refreshEndpoints != nil {
		e.refreshEndpoints()
	}

	c.JSON(http.StatusOK, gin.H{
		"status":  "success",
		"message": "Cache invalidated successfully",
	})
}

// peerCertFromRequest returns the client's leaf certificate if the
// request was made over mTLS with a verified chain, otherwise nil. The
// cluster listener's wrapInternal already enforces the mTLS + OU check,
// so this is a read-only safety lookup.
func peerCertFromRequest(r *http.Request) *x509.Certificate {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 {
		return nil
	}
	return r.TLS.PeerCertificates[0]
}

// nodeURLMatchesPeerCert reports whether the host in nodeURL matches a
// DNS name or IP SAN on cert. The worker-issued cert's SANs are the
// advertise IPs and hostname supplied in the CSR at pairing — the
// authoritative binding of "which peer is this" to "which URL do they
// own."
func nodeURLMatchesPeerCert(nodeURL string, cert *x509.Certificate) bool {
	if cert == nil {
		return false
	}
	u, err := url.Parse(nodeURL)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "" {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		for _, certIP := range cert.IPAddresses {
			if certIP.Equal(ip) {
				return true
			}
		}
		return false
	}
	for _, dn := range cert.DNSNames {
		if dn == host {
			return true
		}
	}
	return false
}

// getLocalSystemInfo returns local system information
func (e *SystemExecutor) getLocalSystemInfo() map[string]any {
	systemInfo := map[string]any{
		"hostname":      e.node.Name(),
		"role":          string(e.node.Mode()),
		"os":            runtime.GOOS,
		"arch":          runtime.GOARCH,
		"go_version":    runtime.Version(),
		"num_cpu":       runtime.NumCPU(),
		"num_goroutine": runtime.NumGoroutine(),
	}

	// Add cluster info. Source the runtime role — covers all four
	// states (disabled/coordinator/unclaimed/worker) and reflects
	// live post-pairing promotions, not the static config snapshot.
	systemInfo["cluster_mode"] = e.role.Current().String()

	// Add apps info
	if e.appMgr != nil {
		apps := e.appMgr.Protocols().GetAll()
		systemInfo["apps_total"] = len(apps)
		systemInfo["apps_enabled"] = len(apps)
		systemInfo["instances_running"] = len(e.appMgr.Instances().List())
	}

	// Add models count if available
	if e.registry != nil {
		models, _ := e.registry.ListModels()
		systemInfo["models_total"] = len(models)
	}

	// Merge hardware info (memory, disk, CPU, GPU).
	for k, v := range e.getSystemInfo() {
		systemInfo[k] = v
	}

	// Add connection pool metrics for observability
	if poolStats := e.getPoolStats(); poolStats != nil {
		systemInfo["connection_pool"] = map[string]any{
			"http_active_requests":     poolStats.HTTPActiveRequests,
			"http_total_requests":      poolStats.HTTPTotalRequests,
			"http_max_idle_conns":      poolStats.HTTPMaxIdleConns,
			"http_max_idle_per_host":   poolStats.HTTPMaxIdlePerNode,
			"stream_active_requests":   poolStats.StreamActiveRequests,
			"stream_total_requests":    poolStats.StreamTotalRequests,
			"stream_max_idle_conns":    poolStats.StreamMaxIdleConns,
			"stream_max_idle_per_host": poolStats.StreamMaxIdlePerNode,
		}
	}

	return systemInfo
}

// ============================================================================
// Internal Handlers (delegating wrappers for V2 API compatibility)
// ============================================================================

// ============================================================================
// Resources Executor (Internal API for resource metrics)
// ============================================================================

// ResourcesExecutor handles resource metrics collection for cluster routing
type ResourcesExecutor struct {
	nodeName  string
	resources *mesh.ResourceTracker
	appMgr    *prov_apps.ProviderAppManager
}

// NewResourcesExecutor creates a new resources executor
func NewResourcesExecutor(nodeName string, resources *mesh.ResourceTracker, appMgr *prov_apps.ProviderAppManager) *ResourcesExecutor {
	return &ResourcesExecutor{nodeName: nodeName, resources: resources, appMgr: appMgr}
}

// HandleInternalGetResources handles GET /zzrouter/internal/resources
// Returns current resource metrics for this node
func (e *ResourcesExecutor) HandleInternalGetResources(c *gin.Context) {
	slog.Info("[ResourcesExecutor] GetResources")

	// Collect current metrics
	metrics := e.collectLocalResources()

	c.JSON(http.StatusOK, metrics)
}

// collectLocalResources gathers resource metrics for this node
func (e *ResourcesExecutor) collectLocalResources() *mesh.ResourceMetrics {
	// Use the resource tracker if available
	if e.resources != nil {
		metrics := e.resources.GetMetrics()
		if metrics != nil {
			if e.appMgr != nil {
				metrics.ActiveModels = len(e.appMgr.Instances().List())
			}
			return metrics
		}
	}

	// Fallback: collect metrics directly
	metrics := mesh.CollectResourceMetrics(e.nodeName)
	if metrics != nil {
		if e.appMgr != nil {
			metrics.ActiveModels = len(e.appMgr.Instances().List())
		}
	}
	return metrics
}
