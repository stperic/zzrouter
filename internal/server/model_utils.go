// Model Utilities — request/response DTOs and server-side helpers that
// don't belong to the cache package.

package server

import (
	"context"
	"fmt"
	"strings"
)

// ============================================================================
// REQUEST/RESPONSE TYPES
// ============================================================================

// ModelToDelete represents a model deletion request
type ModelToDelete struct {
	Provider string `json:"provider,omitempty"`
	SourceID string `json:"source_id,omitempty"`
	Name     string `json:"name"`
	Registry string `json:"registry"`
	Node     string `json:"node"`
}

// DeleteModelsRequest represents the request body for model deletion
type DeleteModelsRequest struct {
	DefaultProvider string          `json:"-"`
	Model           string          `json:"model,omitempty"`
	Provider        string          `json:"provider,omitempty"`
	Registry        string          `json:"registry,omitempty"`
	SourceID        string          `json:"source_id,omitempty"`
	Node            string          `json:"node,omitempty"`    // Target cluster node (for routing)
	Pattern         string          `json:"pattern,omitempty"` // Pattern matching (e.g., "llama*")
	Paths           []string        `json:"paths,omitempty"`   // Specific file paths
	Models          []ModelToDelete `json:"models,omitempty"`  // Specific models with node/repo
}

// Validate validates the delete request
func (req *DeleteModelsRequest) Validate() error {
	criteria := 0
	for _, present := range []bool{req.Model != "", req.Pattern != "", len(req.Paths) > 0, len(req.Models) > 0} {
		if present {
			criteria++
		}
	}
	if criteria != 1 {
		return fmt.Errorf("supply exactly one of model, pattern, paths or models")
	}
	if req.Model != "" && strings.ContainsAny(req.Model, "*?[]") {
		return fmt.Errorf("model deletion requires an exact name without glob syntax")
	}
	if req.Model == "" && (req.Provider != "" || req.Registry != "" || req.SourceID != "") {
		return fmt.Errorf("provider, registry and source_id selectors require model; use per-model selectors for batches")
	}
	return nil
}

// DeleteModelsResponse represents the response from deleting models
type DeleteModelsResponse struct {
	Model   string   `json:"model,omitempty"`
	Node    string   `json:"node,omitempty"`
	Deleted int      `json:"deleted"`
	Errors  []string `json:"errors,omitempty"`
}

// ============================================================================
// UTILITY HANDLERS
// ============================================================================

// GetLocalStats returns statistics from the local host.
func (s *Server) GetLocalStats() map[string]any {
	if s.model.Registry == nil {
		return map[string]any{
			"total_models": 0,
			"total_size":   0,
			"by_format":    map[string]any{},
			"by_source":    map[string]any{},
		}
	}

	stats := s.model.Registry.GetStats()

	return map[string]any{
		"total_models": stats.TotalModels,
		"total_size":   stats.TotalSize,
		"by_format":    stats.ByFormat,
		"by_source":    stats.BySource,
	}
}

// RescanLocal performs a rescan on the local host.
func (s *Server) RescanLocal(parent context.Context) (map[string]any, error) {
	if s.model.Registry == nil {
		return nil, fmt.Errorf("model registry not initialized")
	}

	ctx, cancel := context.WithTimeout(parent, DefaultCacheRefreshTimeout)
	defer cancel()
	// Cache invalidation also drops the registry scan evidence.
	if s.model.Cache != nil {
		s.model.Cache.Invalidate()
		if err := s.model.Cache.RefreshCacheSync(ctx); err != nil {
			return nil, err
		}
	} else if err := s.model.Registry.Rescan(); err != nil {
		return nil, err
	}
	if s.node.IsWorker() && s.cluster.signaler != nil {
		if err := s.cluster.signaler.NotifyCacheRefresh(ctx); err != nil {
			return nil, err
		}
	}

	models, _, err := s.model.Registry.ModelSnapshot(ctx)
	if err != nil {
		return nil, err
	}

	return map[string]any{
		"message":      "Rescan completed",
		"models_found": len(models),
	}, nil
}
