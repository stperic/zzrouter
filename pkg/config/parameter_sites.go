package config

import (
	"cmp"
	"fmt"
	"maps"
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
// path order: each tier (defaults, models, nodes,
// node×model) and each of their endpoint overlays. A consumer asking "where is this key set?"
// walks this rather than the tree's shape.
func (s *ServiceConfig) ParameterSites() []ParameterSite {
	var sites []ParameterSite
	s.parameterTiers(func(base []string, params map[string]string, endpoints map[string]EndpointOverlay) {
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
	})
	slices.SortFunc(sites, func(a, b ParameterSite) int { return cmp.Compare(a.Path, b.Path) })
	return sites
}

// parameterTiers visits every tier, including empty and environment-only overlays.
func (s *ServiceConfig) parameterTiers(visit func([]string, map[string]string, map[string]EndpointOverlay)) {
	if s.Defaults != nil {
		visit([]string{"defaults"}, s.Defaults.Parameters, s.Defaults.Endpoints)
	}
	for m, spec := range s.Models {
		visit([]string{"models", m}, spec.Parameters, spec.Endpoints)
	}
	for n, spec := range s.Nodes {
		visit([]string{"nodes", n}, spec.Parameters, spec.Endpoints)
		for m, cell := range spec.Models {
			visit([]string{"nodes", n, "models", m}, cell.Parameters, cell.Endpoints)
		}
	}
}

// checkEndpointNames holds every overlay to the closed set of run endpoints.
func (s ServiceConfig) checkEndpointNames() error {
	var problems TreeProblems
	s.parameterTiers(func(base []string, _ map[string]string, endpoints map[string]EndpointOverlay) {
		for _, endpoint := range slices.Sorted(maps.Keys(endpoints)) {
			if _, ok := runWireEndpoints[endpoint]; !ok {
				problems = append(problems, TreeProblem{Path: dotted(append(base, "endpoints", endpoint)...),
					Want: strings.Join(slices.Sorted(maps.Keys(runWireEndpoints)), ", "), Message: fmt.Sprintf("unknown run endpoint %q", endpoint)})
			}
		}
	})
	slices.SortFunc(problems, func(a, b TreeProblem) int { return cmp.Compare(a.Path, b.Path) })
	return problems.orNil()
}

func dotted(parts ...string) string { return strings.Join(parts, ".") }
