package cache

import (
	"log/slog"
	"maps"
	"sync"

	"github.com/stperic/zzrouter/pkg/cluster/mesh"
)

// NodeResourceCache is an EventCache: no TTL, no independent
// invalidation entry point. It is refreshed atomically with the
// cluster-join model cache — worker resource metrics piggyback on
// the model-list broadcast, so the same Cache.Invalidate →
// RefreshCacheSync path that rebuilds the model index also replaces
// NodeResourceCache's contents.
//
// There is therefore no NodeResourceCache.Invalidate method by
// design: callers invalidate Cache, and node resources follow.
type NodeResourceCache struct {
	mu        sync.RWMutex
	resources map[string]*mesh.ResourceMetrics // nodeName → metrics
}

// NewNodeResourceCache creates an empty node resource cache.
func NewNodeResourceCache() *NodeResourceCache {
	return &NodeResourceCache{
		resources: make(map[string]*mesh.ResourceMetrics),
	}
}

// Get returns cached resource metrics for a node. Returns nil if unknown.
func (c *NodeResourceCache) Get(nodeName string) *mesh.ResourceMetrics {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.resources[nodeName]
}

// Update replaces all cached metrics with local + worker data.
// Called from Cache.RefreshCacheSync with piggybacked worker metrics.
func (c *NodeResourceCache) Update(
	localTracker *mesh.ResourceTracker,
	localNodeName string,
	workerResources map[string]*mesh.ResourceMetrics,
) {
	resources := make(map[string]*mesh.ResourceMetrics)

	// Local node metrics
	if localTracker != nil && localNodeName != "" {
		if local := localTracker.GetMetrics(); local != nil {
			resources[localNodeName] = local
		}
	}

	// Worker metrics (piggybacked from model list broadcast)
	maps.Copy(resources, workerResources)

	c.mu.Lock()
	c.resources = resources
	c.mu.Unlock()

	slog.Debug("[NodeResourceCache] Updated", "nodes", len(resources))
}
