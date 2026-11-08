package cache

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stretchr/testify/require"
)

type conditionalRequestCluster struct {
	mesh.ClusterClient
	read func(*mesh.QueryParams) (*mesh.Response, error)
}

func (c conditionalRequestCluster) Unicast(_ context.Context, _, _ string, q *mesh.QueryParams) (*mesh.Response, error) {
	return c.read(q)
}

func TestConditionalNodePublicationLifecycle(t *testing.T) {
	c := newWorkerCache(t)
	initial := &conditionalWarmCluster{}
	c.clusterClient = func() mesh.ClusterClient { return initial }
	require.NoError(t, c.RefreshCacheSync(t.Context()))
	newer := &mesh.ResourceMetrics{NodeName: "A", GPUMemoryFreeMB: 8192}
	c.nodeCache.Put("A", newer, utils.Now())
	phase := 0
	client := conditionalRequestCluster{read: func(q *mesh.QueryParams) (*mesh.Response, error) {
		switch phase {
		case 0:
			require.Equal(t, `"E1"`, q.Headers.Get("If-None-Match"))
			return &mesh.Response{StatusCode: 304, Headers: http.Header{"Etag": {`"E1"`}}}, nil
		case 1:
			return &mesh.Response{StatusCode: 200, Headers: http.Header{"Etag": {`"E2"`}}, Body: []byte("invalid JSON")}, nil
		case 2:
			require.Empty(t, q.Headers.Get("If-None-Match"), "failed evidence invalidates its validator")
			return conditionalResponse("new", `"E3"`), nil
		case 3:
			return nil, errors.New("network timeout")
		default:
			return &mesh.Response{StatusCode: 304, Headers: http.Header{"Etag": {`"E3"`}}}, nil
		}
	}}
	c.clusterClient = func() mesh.ClusterClient { return client }
	require.NoError(t, c.RefreshNode(t.Context(), "A"))
	require.True(t, c.HasModel("old"))
	require.Same(t, newer, c.nodeCache.Get("A"), "304 must not replay resource samples")
	phase = 1
	require.Error(t, c.RefreshNode(t.Context(), "A"))
	require.Empty(t, c.workerETags["A"])
	phase = 2
	require.NoError(t, c.RefreshNode(t.Context(), "A"))
	require.True(t, c.HasModel("new"))
	require.False(t, c.HasModel("old"))
	phase = 3
	require.Error(t, c.RefreshNode(t.Context(), "A"))
	require.Empty(t, c.workerETags["A"])
	phase = 4
	require.Error(t, c.RefreshNode(t.Context(), "A"), "304 cannot acknowledge missing published evidence")
	c.Invalidate()
	require.Empty(t, c.workerETags)
}

type conditionalCluster struct {
	mesh.ClusterClient
	entered, release     chan struct{}
	broadcasts, unicasts atomic.Int32
}

func conditionalResponse(name, etag string) *mesh.Response {
	r := noticeResponse("A", name, nil)
	r.Headers = http.Header{"Etag": []string{etag}}
	return r
}
func (c *conditionalCluster) Broadcast(context.Context, string, *mesh.QueryParams) (*mesh.BroadcastResponse, error) {
	name, tag := "old", `"E1"`
	if c.broadcasts.Add(1) > 1 {
		name, tag = "new", `"E2"`
	}
	return &mesh.BroadcastResponse{Responses: []*mesh.NodeResponse{{NodeName: "A", Response: conditionalResponse(name, tag)}}}, nil
}
func (c *conditionalCluster) Unicast(ctx context.Context, node, path string, q *mesh.QueryParams) (*mesh.Response, error) {
	if c.unicasts.Add(1) == 1 {
		close(c.entered)
		select {
		case <-c.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return &mesh.Response{StatusCode: 304, Headers: http.Header{"Etag": []string{`"E1"`}}}, nil
	}
	return conditionalResponse("new", `"E2"`), nil
}
func TestConcurrent304KeepsMatchingValidator(t *testing.T) {
	c := newWorkerCache(t)
	cluster := &conditionalCluster{entered: make(chan struct{}), release: make(chan struct{})}
	c.clusterClient = func() mesh.ClusterClient { return cluster }
	require.NoError(t, c.RefreshCacheSync(t.Context()))
	done := make(chan error, 1)
	go func() { done <- c.RefreshNode(t.Context(), "A") }()
	<-cluster.entered
	require.NoError(t, c.RefreshCacheSync(t.Context()))
	close(cluster.release)
	require.NoError(t, <-done)
	require.True(t, c.HasModel("new"))
	require.Equal(t, `"E2"`, c.workerETags["A"], "the E2 model snapshot cannot retain E1's validator")
}

type conditionalWarmCluster struct{ mesh.ClusterClient }

func (c *conditionalWarmCluster) Broadcast(context.Context, string, *mesh.QueryParams) (*mesh.BroadcastResponse, error) {
	return &mesh.BroadcastResponse{Responses: []*mesh.NodeResponse{{NodeName: "A", Response: conditionalResponse("old", `"E1"`)}}}, nil
}
func (c *conditionalWarmCluster) Unicast(ctx context.Context, node, path string, q *mesh.QueryParams) (*mesh.Response, error) {
	if q.Headers.Get("If-None-Match") == `"E1"` {
		return &mesh.Response{StatusCode: 304, Headers: http.Header{"Etag": []string{`"E1"`}}}, nil
	}
	return conditionalResponse("old", `"E1"`), nil
}
func TestWarmCannotLeaveOrphanedValidator(t *testing.T) {
	c := newWorkerCache(t)
	cluster := &conditionalWarmCluster{}
	c.clusterClient = func() mesh.ClusterClient { return cluster }
	require.NoError(t, c.RefreshCacheSync(t.Context()))
	require.True(t, c.HasModel("old"))
	c.warmLocalCache()
	require.False(t, c.HasModel("old"))
	require.NoError(t, c.RefreshNode(t.Context(), "A"))
	require.True(t, c.HasModel("old"), "a 304 cannot stand in for a worker snapshot discarded by warm")
}
