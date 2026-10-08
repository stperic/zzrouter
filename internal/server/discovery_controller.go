package server

import (
	"context"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/discovery/gpu"
)

// ensureDiscoveryController returns a wired DiscoveryController, lazily
// constructing the underlying DiscoveryService on first call. Both the
// public route registration (coord-only) and the internal route
// registration (every mode) call this so the wiring lives in exactly
// one place — without it the two construction sites silently fork on
// any future field addition.
func (s *Server) ensureDiscoveryController() *DiscoveryController {
	if s.controllers.Discovery != nil {
		return s.controllers.Discovery
	}
	svc := s.services.Discovery
	if svc == nil {
		svc = NewDiscoveryService(
			s.config,
			s.cluster.discovery,
			s.cluster.router,
			func() gpu.Inventory { return gpu.List(true) },
		).WithClusterPeers(s.listJobsPeers)
		s.services.Discovery = svc
	}
	ctrl := NewDiscoveryController(svc)
	s.controllers.Discovery = ctrl
	return ctrl
}

// DiscoveryController handles HTTP requests for resource discovery.
//
// Each endpoint follows the same routing pattern via DiscoveryService.Route:
//   - ?node= empty or "localhost" → local discovery
//   - ?node=*                     → fan-out to all cluster nodes
//   - ?node=specific              → proxy to that node
type DiscoveryController struct {
	service *DiscoveryService
}

// NewDiscoveryController creates a new discovery controller.
func NewDiscoveryController(service *DiscoveryService) *DiscoveryController {
	return &DiscoveryController{service: service}
}

// RegisterPublicRoutes registers public discovery routes.
//
// Provider/software-tool detection routes were retired here as part of
// the providers/-tree-as-source-of-truth consolidation: /discover used
// to expose a hardcoded list of provider/converter binaries that
// drifted from the canonical providers/<kind>/<name>/config.yaml tree
// (mlx, openrouter, huggingface were never added to the discovery
// list, so they reported as missing on every host). Agents that
// previously hit /discover/providers* or /discover/software now use
// /runs/capabilities, which reads the providers/ tree directly.
// Hardware + network discovery remain — those signals don't have a
// parallel source.
func (ctrl *DiscoveryController) RegisterPublicRoutes(router *gin.RouterGroup) {
	router.GET("/discover", ctrl.DiscoverAll)
	router.GET("/discover/hardware", ctrl.DiscoverHardware)
	router.GET("/discover/hardware/gpus", ctrl.DiscoverGPUs)
	router.GET("/discover/network/hosts", ctrl.DiscoverNetworkNodes)
}

// RegisterInternalRoutes mounts the read-only discover surface on the
// cluster mTLS internal engine. These handlers ALWAYS run locally —
// no Route() wrapper, no ?node= dispatch — because the only caller is
// the coord's public-side fan-out which has already chosen this peer.
// Auth is by transport (mTLS + OU=coordinator on pkg/cluster/node),
// no app-layer middleware needed.
func (ctrl *DiscoveryController) RegisterInternalRoutes(router *gin.RouterGroup) {
	router.GET("/discover", ctrl.discoverAllLocal)
	router.GET("/discover/hardware", ctrl.discoverHardwareLocal)
	router.GET("/discover/hardware/gpus", ctrl.discoverGPUsLocal)
	router.GET("/discover/network/hosts", ctrl.discoverNetworkNodesLocal)
}

func (ctrl *DiscoveryController) discoverAllLocal(c *gin.Context) {
	ctrl.handleLocal(c, "Discovery", constants.DiscoveryTimeout, ctrl.service.DiscoverAll)
}

func (ctrl *DiscoveryController) discoverHardwareLocal(c *gin.Context) {
	ctrl.handleLocal(c, "Hardware discovery", constants.DiscoveryTimeout, ctrl.service.DiscoverHardware)
}

func (ctrl *DiscoveryController) discoverGPUsLocal(c *gin.Context) {
	ctrl.handleLocal(c, "GPU discovery", constants.HTTPShortTimeout, ctrl.service.DiscoverGPUs)
}

func (ctrl *DiscoveryController) discoverNetworkNodesLocal(c *gin.Context) {
	ctrl.handleLocal(c, "Network host discovery", constants.DiscoveryTimeout, ctrl.service.DiscoverNetworkNodes)
}

// handleLocal is the internal-engine counterpart to handle: same response
// envelope, no cluster routing.
func (ctrl *DiscoveryController) handleLocal(c *gin.Context, label string, timeout time.Duration, fn discoveryFunc) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
	defer cancel()

	data, err := fn(ctx)
	if err != nil {
		InternalNodeError(c, err.Error())
		return
	}
	respondSuccess(c, label+" completed", data)
}

// DiscoverAll handles GET /discover — hardware + network. Provider tools
// were dropped — see /runs/capabilities for provider listings.
//
// This is a live mDNS scan, and mDNS has no completion signal: the scan
// runs until its deadline whether or not anything is left to find, so
// the call always costs the full DiscoveryTimeout unless background
// discovery is already running and can answer from cache. It uses the
// same budget as the three narrower discovery endpoints; it previously
// used the generic HTTP timeout, which made the broadest endpoint on
// the node three times slower than its own siblings.
//
// The endpoint path passed to DiscoveryService.Route is the worker's
// internal mTLS path, not the public path served on coord. Workers do
// not register the /discover surface on the admin port (paired workers
// narrow that port to 127.0.0.1); cross-host fan-out lands on the
// internal handler at /zzrouter/v1/internal/discover/* via mTLS.
func (ctrl *DiscoveryController) DiscoverAll(c *gin.Context) {
	ctrl.handle(c, "/zzrouter/v1/internal/discover", "Discovery", constants.DiscoveryTimeout, ctrl.service.DiscoverAll)
}

// DiscoverHardware handles GET /discover/hardware — full hardware scan.
func (ctrl *DiscoveryController) DiscoverHardware(c *gin.Context) {
	ctrl.handle(c, "/zzrouter/v1/internal/discover/hardware", "Hardware discovery", constants.DiscoveryTimeout, ctrl.service.DiscoverHardware)
}

// DiscoverGPUs handles GET /discover/hardware/gpus — GPU-only scan.
func (ctrl *DiscoveryController) DiscoverGPUs(c *gin.Context) {
	ctrl.handle(c, "/zzrouter/v1/internal/discover/hardware/gpus", "GPU discovery", constants.HTTPShortTimeout, ctrl.service.DiscoverGPUs)
}

// DiscoverNetworkNodes handles GET /discover/network/hosts — mDNS node scan.
func (ctrl *DiscoveryController) DiscoverNetworkNodes(c *gin.Context) {
	ctrl.handle(c, "/zzrouter/v1/internal/discover/network/hosts", "Network host discovery", constants.DiscoveryTimeout, ctrl.service.DiscoverNetworkNodes)
}

// handle is the shared dispatch for all discovery endpoints. It parses the
// ?node= query param, creates a context with the appropriate timeout, routes
// through the service, and writes the HTTP response.
func (ctrl *DiscoveryController) handle(c *gin.Context, endpoint, label string, timeout time.Duration, fn discoveryFunc) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
	defer cancel()

	node := QueryNode(c)
	data, err := ctrl.service.Route(ctx, node, endpoint, fn)
	if err != nil {
		// proxyToNode returns errors for remote failures — map to 502.
		if node != "" && node != "*" && !ctrl.service.isLocal(node) {
			BadGateway(c, err.Error())
			return
		}
		InternalNodeError(c, err.Error())
		return
	}

	respondSuccess(c, label+" completed", data)
}
