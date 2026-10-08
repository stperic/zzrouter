// package server provides HTTP handlers for the zzrouter host server.
// Cluster routing utilities - DRY helpers for cluster -aware request routing

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/routing"
)

// Configuration constants for cluster routing
// These reference centralized timeout constants from pkg/constants/timeouts.go
const (
	// DefaultClusterTimeout is the default timeout for cluster operations
	DefaultClusterTimeout = constants.ClusterDefaultTimeout
	// DefaultQueryTimeout is the default timeout for query operations
	DefaultQueryTimeout = constants.ClusterQueryTimeout
)

// LocalHandlerFunc is a function that handles local requests
type LocalHandlerFunc func(c *gin.Context)

// RoutingConfig configures request routing behavior (works with or without cluster)
type RoutingConfig struct {
	// Endpoint is the API endpoint path (e.g., "/zzrouter/v1/providers")
	Endpoint string

	// ArrayField is the field name in the response containing the array to aggregate
	// e.g., "providers" for {"providers": [...]} or "models" for {"models": [...]}
	ArrayField string

	// LocalHandler is the function to call for local requests
	LocalHandler LocalHandlerFunc

	// AggregationCallback is called with aggregated results before returning (optional)
	// Used for caching aggregated results
	AggregationCallback func(items []map[string]any)

	// Timeout for operations (default: 10s)
	Timeout time.Duration
}

// HandleRoute is a DRY utility for Gin handlers that need routing
// It uses s.cluster.router internally for the actual routing decision
//
// This is a Gin-specific wrapper that:
// - Handles Gin context and response writing
// - Aggregates array responses from multiple hosts
// - Supports caching callbacks
//
// Usage:
//
//	func (s *Server) handleListApps(c *gin.Context) {
//	    s.HandleRoute(c, RoutingConfig{
//	        Endpoint:     "/zzrouter/v1/internal/providers",
//	        ArrayField:   "providers",
//	        LocalHandler: s.listAppsLocal,  // Fallback for workers
//	    })
//	}
func (s *Server) HandleRoute(c *gin.Context, config RoutingConfig) {
	// WORKERS: Always serve local data directly (no aggregation)
	if s.node.IsWorker() {
		config.LocalHandler(c)
		return
	}

	host := QueryNode(c)
	ctx := c.Request.Context()

	// Set default timeout
	if config.Timeout == 0 {
		config.Timeout = DefaultQueryTimeout
	}

	// Use router for the routing decision
	var resp *routing.Response
	var err error

	if host == "" || host == "*" {
		// Broadcast to all nodes
		resp, err = s.cluster.router.Broadcast(ctx, config.Endpoint, c.Request.Method, nil)
	} else {
		// Unicast to specific node
		resp, err = s.cluster.router.Unicast(ctx, host, config.Endpoint, c.Request.Method, nil)
	}

	if err != nil {
		BadGateway(c, "Routing failed: "+err.Error())
		return
	}

	// For broadcast, apply aggregation callback if provided
	if (host == "" || host == "*") && config.AggregationCallback != nil {
		var data map[string]any
		if json.Unmarshal(resp.Body, &data) == nil {
			if items, ok := data[config.ArrayField].([]any); ok {
				itemMaps := make([]map[string]any, 0, len(items))
				for _, item := range items {
					if m, ok := item.(map[string]any); ok {
						itemMaps = append(itemMaps, m)
					}
				}
				config.AggregationCallback(itemMaps)
			}
		}
	}

	c.Data(resp.StatusCode, "application/json", resp.Body)
}

// HandleClusterAction is a DRY utility for cluster-aware action routing (POST/PUT/DELETE)
// Uses s.cluster.router.Unicast internally - this is just a Gin-specific wrapper
//
// Usage:
//
//	if s.HandleClusterAction(c, targetNode, "/zzrouter/v1/internal/deployments", "POST", requestBody) {
//	    return // Request was routed, response already sent
//	}
//	// Continue with local processing (targetNode was local)
func (s *Server) HandleClusterAction(c *gin.Context, targetNode, endpoint, method string, requestBody []byte) bool {
	// If local host requested, let caller handle locally
	if s.node.IsLocalNode(targetNode) {
		return false // Continue with local processing
	}

	// Route via router (handles local vs remote transparently)
	ctx, cancel := context.WithTimeout(c.Request.Context(), DefaultClusterTimeout)
	defer cancel()

	resp, err := s.cluster.router.Unicast(ctx, targetNode, endpoint, method, requestBody)
	if err != nil {
		BadGateway(c, fmt.Sprintf("Failed to route to host '%s': %s", targetNode, err.Error()))
		return true
	}

	c.Data(resp.StatusCode, "application/json", resp.Body)
	return true
}

// ============================================================================
// HOSTNAME HELPERS
// ============================================================================
