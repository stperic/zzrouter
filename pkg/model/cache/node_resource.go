package cache

import (
	"sync"
	"time"

	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/utils"
)

// NodeResourceCache accepts catalog and health observations in local request order.
type NodeResourceCache struct {
	mu           sync.RWMutex
	resources    map[string]resourceObservation
	localNode    string
	localTracker *mesh.ResourceTracker
}

type resourceObservation struct {
	metrics   *mesh.ResourceMetrics
	startedAt time.Time
}

// NewNodeResourceCache creates an empty node resource cache.
func NewNodeResourceCache() *NodeResourceCache {
	return &NodeResourceCache{
		resources: make(map[string]resourceObservation),
	}
}

// Get returns cached resource metrics for a node. Returns nil if unknown.
func (c *NodeResourceCache) Get(nodeName string) *mesh.ResourceMetrics {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if nodeName == c.localNode && c.localTracker != nil {
		return c.localTracker.GetMetrics()
	}
	observation := c.resources[nodeName]
	if utils.Now().Sub(observation.startedAt) > 2*constants.HealthCheckInterval {
		return nil
	}
	return observation.metrics
}

// Put accepts evidence only when no later-started local read has already won.
func (c *NodeResourceCache) Put(node string, metrics *mesh.ResourceMetrics, startedAt time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !startedAt.Before(c.resources[node].startedAt) {
		c.resources[node] = resourceObservation{metrics: metrics, startedAt: startedAt}
	}
}

// Update merges catalog observations without erasing fresh health evidence.
func (c *NodeResourceCache) Update(
	localTracker *mesh.ResourceTracker,
	localNodeName string,
	workerResources map[string]*mesh.ResourceMetrics,
	startedAt time.Time,
) {
	c.mu.Lock()
	c.localNode, c.localTracker = localNodeName, localTracker
	c.mu.Unlock()
	for node, metrics := range workerResources {
		c.Put(node, metrics, startedAt)
	}
}
