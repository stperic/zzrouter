package cache

import (
	"context"
	"sort"
	"strings"

	"github.com/stperic/zzrouter/pkg/prov_apps"
)

// InventoryUnavailable records failed discovery without inventing model identities.
type InventoryUnavailable struct {
	Node     string                           `json:"node"`
	Provider string                           `json:"provider,omitempty"`
	Reason   string                           `json:"reason"`
	Service  *prov_apps.ProviderServiceStatus `json:"service,omitempty"`
}

type providerNode struct{ node, provider string }
type modelNode struct{ model, node string }

func cloneInventory(in InventoryUnavailable) InventoryUnavailable {
	if in.Service != nil {
		status := in.Service.Clone()
		in.Service = &status
	}
	return in
}

// UnavailableInventories only reads the last published evidence, without populating.
func (mc *Cache) UnavailableInventories(node, provider string) []InventoryUnavailable {
	mc.mu.RLock()
	defer mc.mu.RUnlock()
	var result []InventoryUnavailable
	for key, unavailable := range mc.inventoryUnavailable {
		if node != "" && !strings.EqualFold(key.node, node) {
			continue
		}
		if provider != "" && key.provider != "" && key.provider != provider {
			continue
		}
		result = append(result, cloneInventory(unavailable))
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Node != result[j].Node {
			return result[i].Node < result[j].Node
		}
		return result[i].Provider < result[j].Provider
	})
	return result
}

// LookupByNode identifies the requested replica without substituting another node.
func (mc *Cache) LookupByNode(model, node string) (*CachedModel, bool) {
	mc.mu.RLock()
	defer mc.mu.RUnlock()
	value, found := mc.indexByNode[modelNode{strings.ToLower(model), strings.ToLower(node)}]
	return value, found
}

func (mc *Cache) localUnavailable(ctx context.Context, failures map[string]string) []InventoryUnavailable {
	if mc.inventoryStatus != nil {
		return mc.inventoryStatus(ctx, failures)
	}
	var unavailable []InventoryUnavailable
	for provider, reason := range failures {
		unavailable = append(unavailable, InventoryUnavailable{Node: mc.node.Nodename(), Provider: provider, Reason: reason})
	}
	return unavailable
}
