package server

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stperic/zzrouter/pkg/version"
)

// HealthHandler handles health check endpoints with version information
type HealthHandler struct {
	startTime    time.Time
	capabilities []string
	isDraining   func() bool // Returns true when server is draining (graceful shutdown in progress)
	identity     func() NodeIdentityReport
}

// NewHealthHandler creates a new health handler.
// isDraining reports graceful shutdown; identity reports which config
// this process loaded, and may be nil.
func NewHealthHandler(capabilities []string, isDraining func() bool, identity func() NodeIdentityReport) *HealthHandler {
	return &HealthHandler{
		startTime:    utils.Now(),
		capabilities: capabilities,
		isDraining:   isDraining,
		identity:     identity,
	}
}

// HealthResponse is the single payload shape for every public /health*
// endpoint. status + HTTP code vary across routes (healthy / ready /
// draining); the fields here do not. Consolidated so peer version
// discovery (which probes /health) sees the same protocol window as a
// kubelet readiness probe (which hits /health/ready) — the old split
// trapped cluster_protocol on /ready only and broke mesh validation.
type HealthResponse struct {
	Status             string         `json:"status"`
	Version            string         `json:"version"`
	APIVersion         string         `json:"api_version"`
	ServiceType        string         `json:"service_type"`
	Uptime             string         `json:"uptime"`
	Capabilities       []string       `json:"capabilities"`
	ClusterProtocol    int            `json:"cluster_protocol"`
	MinClusterProtocol int            `json:"min_cluster_protocol"`
	ClusterCompatible  string         `json:"cluster_compatible"`
	ClientCompatible   string         `json:"client_compatible"`
	Timestamp          time.Time      `json:"timestamp"`
	Details            map[string]any `json:"details,omitempty"`
}

// buildHealthResponse assembles the common fields. Callers set Status.
func (hh *HealthHandler) buildHealthResponse(status string) *HealthResponse {
	return &HealthResponse{
		Status:             status,
		Version:            version.Current.String(),
		APIVersion:         "v1",
		ServiceType:        version.ServiceTypeHost,
		Uptime:             time.Since(hh.startTime).String(),
		Capabilities:       hh.capabilities,
		ClusterProtocol:    version.ClusterProtocolVersion,
		MinClusterProtocol: version.MinClusterProtocolVersion,
		ClusterCompatible:  version.ClusterCompatibility.MinSupportedVersion.String(),
		ClientCompatible:   version.ClientCompatibility.MinSupportedVersion.String(),
		Timestamp:          utils.Now(),
	}
}

// GetHealth handles GET /health — always 200 when the process is alive.
func (hh *HealthHandler) GetHealth(c *gin.Context) {
	response := hh.buildHealthResponse("healthy")
	if c.Query("detailed") == "true" {
		response.Details = map[string]any{
			"build_info": version.GetCurrentVersionInfo(version.ServiceTypeHost, hh.capabilities).BuildInfo,
		}
		// Which config this process loaded, so a CLI can tell whether
		// the server answering here read the same file it did. This
		// route is the only one every node serves: a worker mounts no
		// /zzrouter/v1/* at all, and a worker is exactly where the
		// service-versus-operator config split shows up.
		if hh.identity != nil {
			response.Details["node_identity"] = hh.identity().forRemoteAddr(c.Request.RemoteAddr)
		}
	}
	c.JSON(http.StatusOK, response)
}

// GetHealthLive handles GET /health/live — minimal liveness probe. Kept
// tiny (status + version) so load balancers with cheap probe budgets
// don't pay the full-payload cost of /health.
func (hh *HealthHandler) GetHealthLive(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status":  "alive",
		"version": version.Current.String(),
	})
}

// GetHealthReady handles GET /health/ready — same shape as /health, but
// flips status to "draining" + returns 503 during graceful shutdown so
// orchestrators stop routing traffic here.
func (hh *HealthHandler) GetHealthReady(c *gin.Context) {
	status := "ready"
	statusCode := http.StatusOK
	if hh.isDraining != nil && hh.isDraining() {
		status = "draining"
		statusCode = http.StatusServiceUnavailable
	}
	c.JSON(statusCode, hh.buildHealthResponse(status))
}

// GetHealthServices handles GET /health/services — reports the state of the
// cross-component integrations this node depends on. The response lists each
// known integration with a simple {status, detail} entry; the full
// per-integration probing is wired from the cluster/providers/observability
// subsystems as they gain structured health reporting.
func (hh *HealthHandler) GetHealthServices(c *gin.Context) {
	services := make(map[string]gin.H, len(hh.capabilities))
	for _, capability := range hh.capabilities {
		services[capability] = gin.H{
			"status": "registered",
			"detail": "capability advertised at startup; no live probe wired yet",
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"status":    "ok",
		"services":  services,
		"timestamp": utils.Now(),
	})
}

// GetHealthSharedStatus handles GET /health/shared-status — multi-pod
// coordination state. Single-node zzrouter returns a stable "standalone"
// payload. When the cluster registry is wired to this endpoint it will
// enumerate peer nodes and their last-seen state; until then the route
// exists so multi-pod probes get a structured 200 instead of a NoRoute 404.
func (hh *HealthHandler) GetHealthSharedStatus(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status":    "standalone",
		"node":      version.ServiceTypeHost,
		"version":   version.Current.String(),
		"peers":     []gin.H{},
		"timestamp": utils.Now(),
	})
}
