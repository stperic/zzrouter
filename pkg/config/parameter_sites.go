package config

import (
	"cmp"
	"slices"
	"strings"
)

// ParameterSite is one place in a provider's tree where parameters are
// set, named by the merge-patch path to its parameters map.
type ParameterSite struct {
	// Path is the dotted merge-patch path, such as
	// "nodes.gpu1.models.qwen.endpoints.embeddings.parameters".
	Path string
	// Endpoint is the overlay's endpoint, "" for a tier's base parameters.
	Endpoint string
	// Parameters is the site's map; callers must not modify it.
	Parameters map[string]string
}

// ParameterSites returns every place parameters are set in the tree, in
// path order: each tier (defaults, model_defaults, models, nodes,
// node×model) and each of their endpoint overlays. A consumer asking "where is this key set?"
// walks this rather than the tree's shape.
func (s *ServiceConfig) ParameterSites() []ParameterSite {
	var sites []ParameterSite
	add := func(base []string, params map[string]string, endpoints map[string]EndpointOverlay) {
		if len(params) > 0 {
			sites = append(sites, ParameterSite{Path: dotted(append(base, "parameters")...), Parameters: params})
		}
		for ep, overlay := range endpoints {
			if len(overlay.Parameters) > 0 {
				sites = append(sites, ParameterSite{
					Path:       dotted(append(base, "endpoints", ep, "parameters")...),
					Endpoint:   ep,
					Parameters: overlay.Parameters,
				})
			}
		}
	}
	if s.Defaults != nil {
		add([]string{"defaults"}, s.Defaults.Parameters, s.Defaults.Endpoints)
	}
	for m, spec := range s.ModelDefaults {
		add([]string{"model_defaults", m}, spec.Parameters, spec.Endpoints)
	}
	for m, spec := range s.Models {
		add([]string{"models", m}, spec.Parameters, spec.Endpoints)
	}
	for n, spec := range s.Nodes {
		add([]string{"nodes", n}, spec.Parameters, spec.Endpoints)
		for m, cell := range spec.Models {
			add([]string{"nodes", n, "models", m}, cell.Parameters, cell.Endpoints)
		}
	}
	slices.SortFunc(sites, func(a, b ParameterSite) int { return cmp.Compare(a.Path, b.Path) })
	return sites
}

func dotted(parts ...string) string { return strings.Join(parts, ".") }
