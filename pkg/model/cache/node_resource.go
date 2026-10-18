package cache

import (
	"log/slog"
	"maps"
	"sync"

	"github.com/stperic/zzrouter/pkg/cluster/mesh"
)

// NodeResourceCache follows accepted catalog publications without independent invalidation.
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

// Put replaces one worker's metrics without discarding other nodes.
func (c *NodeResourceCache) Put(node string, metrics *mesh.ResourceMetrics) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if metrics == nil {
		delete(c.resources, node)
	} else {
		c.resources[node] = metrics
	}
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
