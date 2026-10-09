package cache

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/cluster/mesh"
)

type publicationCluster struct {
	mesh.ClusterClient
	calls     atomic.Int32
	broadcast func(context.Context, int32) (*mesh.BroadcastResponse, error)
}

// Broadcast supplies controlled snapshots during publication races.
func (c *publicationCluster) Broadcast(ctx context.Context, _ string, _ *mesh.QueryParams) (*mesh.BroadcastResponse, error) {
	return c.broadcast(ctx, c.calls.Add(1))
}

func TestEnsurePopulated_RefreshesAgainWhenOvertaken(t *testing.T) {
	c := newWorkerCache(t)
	cluster := &publicationCluster{broadcast: func(_ context.Context, call int32) (*mesh.BroadcastResponse, error) {
		if call <= 5 {
			c.Invalidate()
		}
		return &mesh.BroadcastResponse{}, nil
	}}
	c.clusterClient = func() mesh.ClusterClient { return cluster }

	_, err := c.ListModels(t.Context(), "", "", "", "")
	require.NoError(t, err)
	require.EqualValues(t, 6, cluster.calls.Load())
}

func TestRefreshCacheSync_ConcurrentPublications(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	c := newWorkerCache(t)
	entered, release := make(chan struct{}), make(chan struct{})
	cluster := &publicationCluster{broadcast: func(ctx context.Context, call int32) (*mesh.BroadcastResponse, error) {
		body := []byte(`{"models":[{"name":"new","node":"worker-1"}]}`)
		if call == 1 {
			body = []byte(`{"models":[{"name":"old","node":"worker-1"}]}`)
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return &mesh.BroadcastResponse{Responses: []*mesh.NodeResponse{
			{NodeName: "worker-1", Response: &mesh.Response{StatusCode: 200, Body: body}},
		}}, nil
	}}
	c.clusterClient = func() mesh.ClusterClient { return cluster }
	first, second := make(chan error, 1), make(chan error, 1)
	c.Invalidate()
	go func() { first <- c.RefreshCacheSync(ctx) }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	c.Invalidate()
	go func() { second <- c.RefreshCacheSync(ctx) }()
	require.NoError(t, <-second)
	require.True(t, c.HasModel("new"))
	close(release)
	require.NoError(t, <-first)
	require.True(t, c.HasModel("new"))
	require.False(t, c.HasModel("old"), "the older snapshot cannot replace the newer publication")
}

func TestRefreshCacheSync_RepeatedSupersession(t *testing.T) {
	c := newWorkerCache(t)
	cluster := &publicationCluster{broadcast: func(_ context.Context, call int32) (*mesh.BroadcastResponse, error) {
		if call <= 5 {
			c.Invalidate()
		}
		return &mesh.BroadcastResponse{}, nil
	}}
	c.clusterClient = func() mesh.ClusterClient { return cluster }
	var reconciled int
	c.autoRoute = cacheSyncHook(func() { reconciled++ })

	require.NoError(t, c.RefreshCacheSync(t.Context()))
	require.EqualValues(t, 6, cluster.calls.Load())
	require.True(t, c.valid)
	require.Equal(t, 1, reconciled, "only an accepted publication reconciles routes")
}

func TestRefreshCacheSync_SupersessionStopsOnFailure(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(map[bool]string{false: "refresh error", true: "context canceled"}[canceled], func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			c := newWorkerCache(t)
			cluster := &publicationCluster{broadcast: func(_ context.Context, call int32) (*mesh.BroadcastResponse, error) {
				c.Invalidate()
				if canceled {
					if call == 5 {
						cancel()
					}
				} else {
					c.node = &fakeNode{}
				}
				return &mesh.BroadcastResponse{}, nil
			}}
			c.clusterClient = func() mesh.ClusterClient { return cluster }

			err := c.RefreshCacheSync(ctx)
			if canceled {
				require.ErrorIs(t, err, context.Canceled)
				require.EqualValues(t, 5, cluster.calls.Load())
			} else {
				require.ErrorContains(t, err, "hostname not configured")
				require.EqualValues(t, 1, cluster.calls.Load(), "a real build failure must not be retried")
			}
			require.False(t, c.valid)
		})
	}
}
