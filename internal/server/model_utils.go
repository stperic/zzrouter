// Model Utilities — request/response DTOs and server-side helpers that
// don't belong to the cache package.

package server

import "fmt"

// ============================================================================
// REQUEST/RESPONSE TYPES
// ============================================================================

// ModelToDelete represents a model deletion request
type ModelToDelete struct {
	Name     string `json:"name"`
	Registry string `json:"registry"`
	Node     string `json:"node"`
}

// DeleteModelsRequest represents the request body for model deletion
type DeleteModelsRequest struct {
	Node    string          `json:"node,omitempty"`    // Target cluster node (for routing)
	Pattern string          `json:"pattern,omitempty"` // Pattern matching (e.g., "llama*")
	Paths   []string        `json:"paths,omitempty"`   // Specific file paths
	Models  []ModelToDelete `json:"models,omitempty"`  // Specific models with node/repo
}

// Validate validates the delete request
func (req *DeleteModelsRequest) Validate() error {
	if req.Pattern == "" && len(req.Paths) == 0 && len(req.Models) == 0 {
		return fmt.Errorf("either 'pattern', 'paths', or 'models' must be provided")
	}
	return nil
}

// DeleteModelsResponse represents the response from deleting models
type DeleteModelsResponse struct {
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
func (s *Server) RescanLocal() (map[string]any, error) {
	if s.model.Registry == nil {
		return nil, fmt.Errorf("model registry not initialized")
	}

	// Invalidate the cluster-join ModelCache; this cascades to the
	// Registry scan cache so the next ListModels call re-scans all
	// sources. Using cache.Invalidate rather than registry.Rescan
	// keeps the single-invalidation-entry-point contract.
	if s.model.Cache != nil {
		s.model.Cache.Invalidate()
	} else if err := s.model.Registry.Rescan(); err != nil {
		return nil, err
	}

	models, _ := s.model.Registry.ListModels()

	return map[string]any{
		"message":      "Rescan completed",
		"models_found": len(models),
	}, nil
}
