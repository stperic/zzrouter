package cache

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/modelregistry"
)

// invalidatingCluster invalidates the cache during its first broadcast, the
// way a run stopping mid-refresh does.
type invalidatingCluster struct {
	mesh.ClusterClient
	cache *Cache
	calls atomic.Int32
}

func (c *invalidatingCluster) Broadcast(context.Context, string, *mesh.QueryParams) (*mesh.BroadcastResponse, error) {
	if c.calls.Add(1) == 1 {
		c.cache.Invalidate()
	}
	return &mesh.BroadcastResponse{}, nil
}

// A read whose refresh an invalidation overtook refreshes again; it used to
// answer 500 "model catalog changed during refresh" (seen by an agent right
// after it stopped a run).
func TestEnsurePopulated_RefreshesAgainWhenOvertaken(t *testing.T) {
	cluster := &invalidatingCluster{}
	cfg := &pkgConfig.NodeConfig{}
	c := New(Config{
		Node:                      &fakeNode{name: "coord"},
		Registry:                  func() *modelregistry.Registry { return nil },
		AppsConfig:                func() *pkgConfig.AppsConfig { return nil },
		ClusterClient:             func() mesh.ClusterClient { return cluster },
		NodeCache:                 NewNodeResourceCache(),
		NodeConfig:                cfg,
		ResolveEndpointToNodename: func(s string) string { return s },
	})
	cluster.cache = c

	_, err := c.ListModels(context.Background(), "", "", "", "")
	require.NoError(t, err)
	require.EqualValues(t, 2, cluster.calls.Load(), "the overtaken refresh ran again")
}
