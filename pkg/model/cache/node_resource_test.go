package cache

import (
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stretchr/testify/require"
)

func TestNodeResourceObservationOrdering(t *testing.T) {
	c := NewNodeResourceCache()
	start := utils.Now()
	newer := &mesh.ResourceMetrics{NodeName: "node", GPUMemoryFreeMB: 2048, CollectedAt: 1}
	older := &mesh.ResourceMetrics{NodeName: "node", GPUMemoryFreeMB: 1024, CollectedAt: 9999999999}
	c.Put("node", newer, start)
	c.Put("node", older, start.Add(-time.Second))
	require.Same(t, newer, c.Get("node"), "peer clocks do not order coordinator observations")
	c.Put("node", nil, start.Add(-time.Second))
	c.Update(nil, "", map[string]*mesh.ResourceMetrics{}, start)
	require.Same(t, newer, c.Get("node"), "partial catalog reads retain fresh health evidence")
	c.Put("node", nil, start.Add(time.Second))
	require.Nil(t, c.Get("node"), "a newer explicit unknown replaces the old observation")
	c.Put("stale", older, start.Add(-3*constants.HealthCheckInterval))
	require.Nil(t, c.Get("stale"), "unreachable nodes cannot retain resources indefinitely")
}
