package cache

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/stperic/zzrouter/pkg/utils"
)

// LookupReplicas canonicalizes aliases and variants, keeping one replica per provider/node.
// An explicit node reads only current evidence; unpinned misses wait for catalog population.
func (mc *Cache) LookupReplicas(ctx context.Context, name, provider, node string) ([]*CachedModel, error) {
	if node == "" {
		if err := mc.ensurePopulated(ctx); err != nil {
			return nil, err
		}
	}
	replicas := mc.lookupReplicas(name, provider, node)
	for _, replica := range replicas {
		nodeUnavailable := false
		for _, failure := range mc.UnavailableInventories(replica.Node, replica.Provider) {
			if failure.Node != "" && failure.Provider == "" {
				nodeUnavailable = true
			}
		}
		// Observed provider state is handled by provider admission, not node routing.
		if !nodeUnavailable {
			return replicas, nil
		}
	}
	if len(replicas) > 0 {
		return nil, fmt.Errorf("model %q is known on node %q, whose inventory is unavailable", name, replicas[0].Node)
	}
	return replicas, nil
}

func (mc *Cache) lookupReplicas(name, provider, node string) []*CachedModel {
	mc.mu.RLock()
	defer mc.mu.RUnlock()
	if !mc.valid {
		return nil
	}
	eligible := func(m *CachedModel) bool {
		return (provider == "" || strings.EqualFold(provider, m.Provider)) && (node == "" || strings.EqualFold(node, m.Node))
	}
	// Prefer names, then SourceID aliases, then suffixes.
	canonical := ""
	for pass := range 3 {
		for _, m := range mc.models {
			if !eligible(m) {
				continue
			}
			if (pass == 0 && strings.EqualFold(name, m.Name)) || (pass == 1 && m.SourceID != "" && strings.EqualFold(name, m.SourceID)) || (pass == 2 && strings.EqualFold(name, utils.FormatModelName(m.Name))) {
				canonical = m.Name
				break
			}
		}
		if canonical != "" {
			break
		}
	}
	var result []*CachedModel
	seen := map[providerNode]bool{}
	for _, m := range mc.models {
		if canonical == "" || !eligible(m) || !strings.EqualFold(m.Name, canonical) {
			continue
		}
		key := providerNode{strings.ToLower(m.Node), strings.ToLower(m.Provider)}
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
