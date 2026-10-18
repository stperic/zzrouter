// package server provides HTTP handlers for the zzrouter host server.
// Model instance runtime - on-demand container launching and management

package server

import (
	"context"
	"fmt"

	"github.com/stperic/zzrouter/pkg/utils"
)

// localCatalogContains reads the node snapshot using the launch catalog's name match.
func (s *Server) localCatalogContains(ctx context.Context, weights string) (bool, error) {
	if s.model.Registry == nil {
		return false, fmt.Errorf("local model registry unavailable")
	}
	models, _, err := s.model.Registry.ModelSnapshot(ctx)
	if err != nil {
		return false, err
	}
	for _, model := range models {
		if model.Name == weights || utils.FormatModelName(model.Name) == weights {
			return true, nil
		}
	}
	return false, nil
}
