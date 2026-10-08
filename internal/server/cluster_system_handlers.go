package server

import (
	"context"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/constants"
)

// ClusterSystemHandlers serves cluster-wide /zzrouter/v1/health by
// reading from the unified nodes registry (no broadcasting).
type ClusterSystemHandlers struct {
	nodes *NodesService
}

// HandleHealthFromNodes provides cluster-wide health by reading from the nodes registry.
// This replaces the old HandleHealthClusterV2 that broadcasted to every worker.
func (h *ClusterSystemHandlers) HandleHealthFromNodes(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), constants.ClusterActionTimeout)
	defer cancel()

	resp, err := h.nodes.ListNodes(ctx, &ListNodesRequest{})
	if err != nil {
		InternalNodeError(c, err.Error())
		return
	}

	// Build backwards-compatible health response
	clusterHealth := "healthy"
	nodes := make([]any, 0, len(resp.Data))
	for _, node := range resp.Data {
		// Map node info to old health response format
		healthNode := map[string]any{
			"status":         node["health_status"],
			"hostname":       node["name"],
			"uptime_seconds": node["uptime_seconds"],
			"version":        node["version"],
			"address":        node["address"],
			"role":           node["cluster_role"],
		}

		// Add RAM info if available
		if mem, ok := node["memory"].(map[string]any); ok {
			if total, ok := mem["total_gb"].(float64); ok {
				healthNode["ram_total_gb"] = total
			}
			if avail, ok := mem["available_gb"].(float64); ok {
				if total, ok := mem["total_gb"].(float64); ok {
					healthNode["ram_used_gb"] = total - avail
				}
			}
		}

		if node["health_status"] == "down" {
			clusterHealth = "degraded"
		}
		nodes = append(nodes, healthNode)
	}

	respondSuccess(c, "Cluster health retrieved", gin.H{
		"cluster_health": clusterHealth,
		"nodes":          nodes,
		"total_nodes":    len(nodes),
	})
}
