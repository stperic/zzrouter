// Cluster management handlers — thin gin adapters over mesh.Cluster
// admin methods. Validation, registry, and connector work lives in
// pkg/cluster/mesh; this file only translates HTTP ↔ domain calls.

package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	clusternode "github.com/stperic/zzrouter/pkg/cluster/node"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/utils"
)

// ClusterHandlers groups all cluster management HTTP handlers.
type ClusterHandlers struct {
	node            *NodeIdentity
	config          *pkgConfig.NodeConfig
	nodeConfigStore *pkgConfig.NodeConfigStore
	coordinator     *mesh.Cluster
	listener        *clusternode.Node
	httpClient      *http.Client
	clusterScheme   func() string
	invalidateCache func()

	// advertiseURL is the operator-configured coordinator URL workers
	// persist on pairing-accept. Snapshotted at construction from
	// cfg.Cluster.AdvertiseURL so handlers don't re-read config on
	// every request. Empty = fall back to deriving from listener bind.
	advertiseURL string
}

// admitClusterMember wires a newly-paired worker's host:port address
// into the coordinator's cluster membership: persist to node.yaml,
// register the admin endpoint in the routing registry, and invalidate
// the node cache so the next ListNodes reflects the change. The sole
// caller is handleClusterPairingAccept — cluster membership is a
// pairing-time concern, not an admin-POST concern.
//
// Returns pkgConfig.ErrEndpointExists when the address is already in
// the config (treated as a no-op by the pair path). Mesh-registration
// failures are logged and swallowed — config persistence is the source
// of truth and the registry rebuilds from it on restart.
func (h *ClusterHandlers) admitClusterMember(hostAddr string) error {
	if err := h.nodeConfigStore.AddClusterEndpoint(hostAddr); err != nil {
		return err
	}
	if h.coordinator != nil {
		endpointURL := h.clusterScheme() + "://" + hostAddr
		if err := h.coordinator.RegisterAdminEndpoint(endpointURL); err != nil {
			slog.Warn("cluster admission: mesh register failed", "host_addr", hostAddr, "err", err)
		}
	}
	h.invalidateCache()
	return nil
}

// handleRemoveClusterEndpoint drops a host from the cluster routing
// table. mTLS revocation flows through the separate deny-list
// endpoint — this only touches coordinator state.
func (h *ClusterHandlers) handleRemoveClusterEndpoint(c *gin.Context) {
	params := ValidateParams(c, []ParamRule{RequiredPathParam("address")})
	if params == nil {
		return
	}

	address := params.GetString("address")
	slog.Info("Removing host from cluster", "address", address)

	found, err := h.nodeConfigStore.RemoveClusterEndpoint(address)
	if err != nil {
		InternalNodeError(c, "Failed to save config: "+err.Error())
		return
	}
	if !found {
		NotFound(c, "Cluster endpoint not found")
		return
	}

	if h.coordinator != nil {
		endpointURL := h.clusterScheme() + "://" + address
		if err := h.coordinator.UnregisterEndpoint(endpointURL); err != nil {
			slog.Warn("Config saved but failed to unregister endpoint from cluster registry", "address", address, "error", err)
		}
	}

	h.invalidateCache()
	slog.Info("Successfully removed host from cluster", "address", address)
	respondSuccess(c, fmt.Sprintf("Node %s removed from cluster successfully", address), nil)
}

// handleConnectToClusterNode probes a single host URL and reports
// its version + health snapshot. Used by the CLI's
// `cluster connect` to test reachability without mutating state.
func (h *ClusterHandlers) handleConnectToClusterNode(c *gin.Context) {
	var request struct {
		NodeURL string `json:"host_url" binding:"required"`
		Timeout int    `json:"timeout,omitempty"`
	}
	if !BindJSONStrict(c, &request) {
		return
	}

	timeout := constants.HTTPDefaultTimeout
	if request.Timeout > 0 {
		timeout = time.Duration(request.Timeout) * time.Second
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
	defer cancel()

	if h.coordinator == nil {
		ServiceUnavailable(c, "Cluster coordinator is not initialized on this node")
		return
	}

	slog.Info("Attempting robust connection to cluster node", "node", request.NodeURL)
	// Through the coordinator, not a fresh connector: only the
	// coordinator's connector carries the cluster port, and worker admin
	// ports are loopback-only.
	conn, err := h.coordinator.ValidateNode(ctx, request.NodeURL)
	if err != nil {
		slog.Error("Failed to connect to cluster node", "node", request.NodeURL, "error", err)
		ServiceUnavailable(c, fmt.Sprintf("Failed to connect to cluster node: %v", err))
		return
	}

	slog.Info("Successfully connected to cluster node", "node", conn.NodeURL, "version", conn.Version.String())
	respondSuccess(c, "Connected to cluster node", gin.H{
		"status":       "connected",
		"node_url":     conn.NodeURL,
		"version":      conn.Version.String(),
		"is_healthy":   true,
		"connected_at": conn.LastChecked.Format(time.RFC3339),
		"message":      fmt.Sprintf("Successfully connected to cluster node %s (version %s)", conn.NodeURL, conn.Version.String()),
	})
}

// handleValidateClusterConnections pings every configured host and
// reports the healthy / unhealthy partition.
func (h *ClusterHandlers) handleValidateClusterConnections(c *gin.Context) {
	clusterNodes := append([]string{}, h.node.Endpoints()...)
	if len(clusterNodes) == 0 {
		respondSuccess(c, "No cluster hosts configured", gin.H{
			"total_hosts":     0,
			"healthy_hosts":   0,
			"unhealthy_hosts": 0,
			"validation_time": utils.NowUTC().Format(time.RFC3339),
		})
		return
	}

	if h.coordinator == nil {
		ServiceUnavailable(c, "Cluster coordinator is not initialized on this node")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), constants.HTTPLongTimeout)
	defer cancel()

	slog.Info("Validating cluster host connections", "count", len(clusterNodes))
	results := h.coordinator.ValidateAllConnections(ctx, clusterNodes)

	var healthy, unhealthy []map[string]any
	for _, r := range results {
		row := map[string]any{"host_url": r.URL}
		if r.Err != nil {
			row["status"] = "unhealthy"
			row["error"] = utils.SanitizeErrorMessage(r.Err.Error())
			row["is_healthy"] = false
			unhealthy = append(unhealthy, row)
			slog.Info("Cluster host is unhealthy", "host_url", r.URL, "error", r.Err)
			continue
		}
		row["status"] = "healthy"
		row["version"] = r.Conn.Version.String()
		row["is_healthy"] = true
		row["last_checked"] = r.Conn.LastChecked.Format(time.RFC3339)
		healthy = append(healthy, row)
		slog.Info("Cluster host is healthy", "host_url", r.URL, "version", r.Conn.Version.String())
	}

	message := fmt.Sprintf("Validated %d cluster hosts: %d healthy, %d unhealthy",
		len(clusterNodes), len(healthy), len(unhealthy))
	respondSuccess(c, message, gin.H{
		"total_hosts":     len(clusterNodes),
		"healthy_hosts":   len(healthy),
		"unhealthy_hosts": len(unhealthy),
		"validation_time": utils.Now().Format(time.RFC3339),
		"healthy":         healthy,
		"unhealthy":       unhealthy,
	})

	if len(unhealthy) > 0 {
		c.Header("X-Status-Code", fmt.Sprintf("%d", http.StatusPartialContent))
	}
}
