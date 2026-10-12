package cache

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/utils"
)

// ReplicaSelection selects an exact physical model and optional source identity.
type ReplicaSelection struct {
	// Populate opts a pinned inference lookup into the existing catalog population.
	Populate bool
	Exact    bool
	SourceID string
}

// LookupReplicas canonicalizes aliases and variants, retaining each physical source per provider/node.
// Pins read current evidence unless inference opts into population; unpinned reads populate.
func (mc *Cache) LookupReplicas(ctx context.Context, name, provider, node, registry string, options ...ReplicaSelection) ([]*CachedModel, error) {
	selection := ReplicaSelection{}
	if len(options) > 0 {
		selection = options[0]
	}
	if node == "" || selection.Populate {
		if err := mc.ensurePopulated(ctx); err != nil {
			return nil, err
		}
	}
	replicas := mc.lookupReplicas(name, provider, node, registry, selection)
	if selection.Exact && len(replicas) == 0 && len(mc.UnavailableInventories(node, provider)) > 0 {
		return nil, httperr.Error{Status: 503, Code: string(httperr.CodeClusterUnavailable), Message: "selected model inventory is unavailable; rescan when the provider is reachable"}
	}
	for _, replica := range replicas {
		nodeUnavailable := false
		for _, failure := range mc.UnavailableInventories(replica.Node, replica.Provider) {
			if failure.Node != "" && (failure.Provider == "" || selection.Exact) {
				nodeUnavailable = true
			}
		}
		// Observed provider state is handled by provider admission, not node routing.
		if !nodeUnavailable {
			return replicas, nil
		}
	}
	if len(replicas) > 0 {
		if selection.Exact {
			return nil, httperr.Error{Status: 503, Code: string(httperr.CodeClusterUnavailable), Message: "selected model inventory is unavailable"}
		}
		return nil, fmt.Errorf("model %q is known on node %q, whose inventory is unavailable", name, replicas[0].Node)
	}
	return replicas, nil
}

func (mc *Cache) lookupReplicas(name, provider, node, registry string, options ...ReplicaSelection) []*CachedModel {
	mc.mu.RLock()
	defer mc.mu.RUnlock()
	if !mc.valid {
		return nil
	}
	// Management names are literal; inference also accepts SourceID aliases and suffixes.
	selection := ReplicaSelection{}
	if len(options) > 0 {
		selection = options[0]
	}
	management := selection.Exact
	passes := 3
	if management {
		passes = 1
	}
	canonical := ""
	alias := false
	for pass := range passes {
		for _, m := range mc.models {
			if !replicaEligible(m, provider, node, registry, selection) {
				continue
			}
			if (pass == 0 && strings.EqualFold(name, m.Name)) || (pass == 1 && m.SourceID != "" && strings.EqualFold(name, m.SourceID)) || (pass == 2 && strings.EqualFold(name, utils.FormatModelName(m.Name))) {
				canonical = m.Name
				alias = pass == 1
				break
			}
		}
		if canonical != "" {
			break
		}
	}
	var result []*CachedModel
	type identity struct{ node, provider, source, id string }
	seen := map[identity]bool{}
	for _, m := range mc.models {
		if canonical == "" || !replicaEligible(m, provider, node, registry, selection) || !strings.EqualFold(m.Name, canonical) || (alias && !strings.EqualFold(m.SourceID, name)) {
			continue
		}
		key := identity{strings.ToLower(m.Node), strings.ToLower(m.Provider), strings.ToLower(m.SourceRepo), strings.ToLower(m.SourceID)}
		if seen[key] {
			continue
		}
		seen[key] = true
		copy := *m
		if m.VariantOf != "" && mc.appsConfig != nil {
			if cfg, ok := mc.appsConfig().LookupApp(m.Provider); ok && cfg.SameLaunch(m.Node, m.Name, m.VariantOf) {
				copy.Model = m.VariantOf
			}
		}
		result = append(result, &copy)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Provider != result[j].Provider {
			return result[i].Provider < result[j].Provider
		}
		return result[i].Node < result[j].Node
	})
	return result
}

// replicaEligible applies the exact source/provider/node constraints before canonicalization.
func replicaEligible(m *CachedModel, provider, node, registry string, selection ReplicaSelection) bool {
	return (selection.Exact || !m.Incomplete) && (provider == "" || strings.EqualFold(provider, m.Provider)) && (node == "" || strings.EqualFold(node, m.Node)) && (registry == "" || strings.EqualFold(registry, m.SourceRepo)) && (selection.SourceID == "" || strings.EqualFold(selection.SourceID, m.SourceID))
}
