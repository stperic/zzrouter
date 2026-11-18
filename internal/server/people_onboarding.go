package server

import (
	"github.com/stperic/zzrouter/pkg/access/people"
	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	"github.com/stperic/zzrouter/pkg/model/cache"
	"github.com/stperic/zzrouter/pkg/prov_apps"
)

type onboardingProgress struct {
	people.Progress
	Node          string `json:"node,omitempty"`
	ProviderReady bool   `json:"provider_ready"`
	ModelReady    bool   `json:"model_ready"`
}

func (s *Server) getOnboardingProgress(node string) onboardingProgress {
	progress := onboardingProgress{Progress: s.people.Progress(), Node: node}
	if s.services.Apps == nil {
		return progress
	}
	var models []*cache.CachedModel
	if s.model != nil && s.model.Cache != nil {
		// An invalid snapshot is unknown. Opening setup must not trigger a
		// synchronous inventory refresh or delay the rest of the overview.
		models = s.model.Cache.GetAllModels()
	}
	progress.ProviderReady, progress.ModelReady = observedOnboardingReadiness(s.services.Apps.provider, models, node)
	return progress
}

func observedOnboardingReadiness(state ClusterState, models []*cache.CachedModel, selectedNode string) (bool, bool) {
	if state == nil {
		return false, false
	}
	type target struct{ node, provider string }
	available := make(map[target]struct{})
	add := func(providers []prov_apps.LocalProviderInfo, node string) {
		for _, provider := range providers {
			name := node
			if name == "" {
				name = provider.Node
			}
			if selectedNode != "" && name != selectedNode {
				continue
			}
			if name != "" && provider.Key != "" && provider.State.IsRoutable() {
				available[target{name, provider.Key}] = struct{}{}
			}
		}
	}
	add(state.GetLocalAppsInfo(), "")
	for _, endpoint := range state.GetClusterEndpoints() {
		// Retained inventory from a missed or unauthenticated probe is useful
		// for diagnostics, but cannot establish observed setup availability.
		if endpoint != nil && !endpoint.IsLocal && endpoint.Status == mesh.StatusUp && !endpoint.Snapshot.CollectedAt.IsZero() {
			add(endpoint.Snapshot.Apps, endpoint.NodeName)
		}
	}
	for _, model := range models {
		if model != nil && model.Name != "" && !model.Incomplete {
			if _, ok := available[target{model.Node, model.Provider}]; ok {
				return true, true
			}
		}
	}
	return len(available) > 0, false
}
