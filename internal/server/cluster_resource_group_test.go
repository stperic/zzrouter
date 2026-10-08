package server

import (
	"context"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	"github.com/stretchr/testify/require"
)

func TestClusterResourceGroup_Constructs(t *testing.T) {
	t.Parallel()
	rt := mesh.NewResourceTracker("test-node", 10*time.Millisecond)
	g := newClusterResourceGroup(rt, nil, 9091)
	require.NotNil(t, g)
}

func TestClusterResourceGroup_StartStop_WithNilDiscovery(t *testing.T) {
	t.Parallel()
	rt := mesh.NewResourceTracker("test-node", 10*time.Millisecond)
	g := newClusterResourceGroup(rt, nil, 9091)

	ctx := context.Background()
	require.NoError(t, g.Start(ctx))
	require.NoError(t, g.Stop(ctx))
	// Idempotent Stop — second call must be safe thanks to the
	// underlying subsystems' sync.Once guards.
	require.NoError(t, g.Stop(ctx))
}

func TestClusterResourceGroup_NilReceiver_IsSafe(t *testing.T) {
	t.Parallel()
	var g *clusterResourceGroup
	require.NoError(t, g.Start(context.Background()))
	require.NoError(t, g.Stop(context.Background()))
}
