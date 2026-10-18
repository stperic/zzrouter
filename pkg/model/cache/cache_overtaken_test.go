package cache

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stretchr/testify/require"
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

type noticeCluster struct {
	mesh.ClusterClient
	unicast   func(context.Context, string) (*mesh.Response, error)
	broadcast func(context.Context) (*mesh.BroadcastResponse, error)
}

func (c noticeCluster) Unicast(ctx context.Context, node, path string, params *mesh.QueryParams) (*mesh.Response, error) {
	if path != "/zzrouter/v1/internal/models" || params.Method != "GET" {
		return nil, errors.New("unexpected inventory request")
	}
	return c.unicast(ctx, node)
}

func (c noticeCluster) Broadcast(ctx context.Context, _ string, _ *mesh.QueryParams) (*mesh.BroadcastResponse, error) {
	return c.broadcast(ctx)
}

func noticeResponse(node, name string, metrics *mesh.ResourceMetrics) *mesh.Response {
	body, _ := json.Marshal(map[string]any{
		"models": []map[string]any{{"name": name, "node": node, "assigned_app": "llamacpp"}}, "resources": metrics,
	})
	return &mesh.Response{StatusCode: 200, Body: body}
}

func TestRefreshNodePreservesOtherNodes(t *testing.T) {
	c := newWorkerCache(t)
	c.node = &fakeNode{name: "coordinator"}
	cfg := variantApps(t, true, map[string]config.ModelSpec{"fast": {From: "base"}})
	c.appsConfig = func() *config.AppsConfig { return cfg }
	c.nodeCache = NewNodeResourceCache()
	bMetrics := &mesh.ResourceMetrics{NodeName: "B"}
	aMetrics := &mesh.ResourceMetrics{NodeName: "A"}
	local := localModelSnapshot{models: []map[string]any{{"name": "base"}}}
	require.True(t, c.publishModelItems([]map[string]any{
		{"name": "base", "node": c.node.Nodename(), "assigned_app": "llamacpp"},
		{"name": "local/shared", "node": c.node.Nodename(), "assigned_app": "llamacpp"},
		{"name": "old", "node": "A", "assigned_app": "llamacpp"},
		{"name": "kept", "node": "B", "assigned_app": "llamacpp"},
	}, c.generation, []InventoryUnavailable{{Node: "A", Reason: "old failure"}, {Node: "B", Reason: "kept failure"}, {Reason: "broadcast failure"}}, local, "", map[string]*mesh.ResourceMetrics{"B": bMetrics}))
	attempt, populationErr := c.lastAttempt, errors.New("previous population error")
	c.lastAttempt = attempt.Add(-2 * inventoryRetryInterval)
	c.populateErr = populationErr
	var reconciled int
	c.autoRoute = cacheSyncHook(func() { reconciled++ })
	client := noticeCluster{
		unicast: func(_ context.Context, node string) (*mesh.Response, error) {
			require.Equal(t, "A", node)
			return &mesh.Response{StatusCode: 200, Body: []byte(`{"models":[{"name":"FAST","node":"A","assigned_app":"llamacpp"},{"name":"remote/shared","node":"A","assigned_app":"llamacpp"}],"resources":{"node_name":"A"}}`)}, nil
		},
		broadcast: func(context.Context) (*mesh.BroadcastResponse, error) {
			t.Error("a notice broadcast to unrelated nodes")
			return &mesh.BroadcastResponse{}, nil
		},
	}
	c.clusterClient = func() mesh.ClusterClient { return client }
	require.NoError(t, c.RefreshNode(t.Context(), "A"))
	require.False(t, c.HasModel("old"))
	require.True(t, c.HasModel("FAST"))
	require.True(t, c.HasModel("kept"))
	require.Equal(t, []InventoryUnavailable{{Node: "B", Reason: "kept failure"}}, c.UnavailableInventories("B", ""))
	require.Empty(t, c.UnavailableInventories("A", ""))
	require.Contains(t, c.UnavailableInventories("", ""), InventoryUnavailable{Reason: "broadcast failure"})
	require.Same(t, bMetrics, c.nodeCache.Get("B"))
	require.Equal(t, aMetrics, c.nodeCache.Get("A"))
	require.Equal(t, local, c.localSnapshot)
	require.Equal(t, attempt.Add(-2*inventoryRetryInterval), c.lastAttempt, "scoped success preserves even an old attempt")
	require.ErrorIs(t, c.populateErr, populationErr)
	require.Equal(t, 1, reconciled)
	c.lastAttempt = attempt // These lookups exercise precedence without retrying the other nodes' old failures.
	c.populateErr = nil
	got, err := c.LookupReplicas(t.Context(), "shared", "", "", "")
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, c.node.Nodename(), got[0].Node, "coordinator-held name keeps precedence")
	require.ErrorIs(t, c.CheckModelAdmission(t.Context(), "fast"), config.ErrModelNameConflict)
}

func TestRefreshNodeOvertakesHungBroadcast(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	c := newWorkerCache(t)
	c.nodeCache = NewNodeResourceCache()
	c.populateModelCacheFromItems([]map[string]any{{"name": "old", "node": "A"}})
	entered, release := make(chan struct{}), make(chan struct{})
	var broadcasts atomic.Int32
	client := noticeCluster{
		unicast: func(context.Context, string) (*mesh.Response, error) {
			return noticeResponse("A", "fresh", &mesh.ResourceMetrics{NodeName: "A", CollectedAt: 2}), nil
		},
		broadcast: func(ctx context.Context) (*mesh.BroadcastResponse, error) {
			name := "fresh"
			if broadcasts.Add(1) == 1 {
				name = "stale"
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			} else {
				require.True(t, c.HasModel("fresh"), "an overtaken build must leave the notice published")
				require.Equal(t, int64(2), c.nodeCache.Get("A").CollectedAt, "an overtaken build cannot overwrite metrics")
			}
			return &mesh.BroadcastResponse{Responses: []*mesh.NodeResponse{{NodeName: "A", Response: noticeResponse("A", name, &mesh.ResourceMetrics{NodeName: "A", CollectedAt: map[string]int64{"fresh": 2, "stale": 1}[name]})}}}, nil
		},
	}
	c.clusterClient = func() mesh.ClusterClient { return client }
	full := make(chan error, 1)
	go func() { full <- c.RefreshCacheSync(ctx) }()
	<-entered
	require.NoError(t, c.RefreshNode(ctx, "A"))
	select {
	case err := <-full:
		t.Fatalf("full refresh ended before release: %v", err)
	default:
	}
	select {
	case <-release:
		t.Fatal("notice waited for the hung broadcast")
	default:
	}
	require.True(t, c.HasModel("fresh"))
	close(release)
	require.NoError(t, <-full)
	require.False(t, c.HasModel("stale"))
	require.True(t, c.HasModel("fresh"))
	require.Equal(t, int64(2), c.nodeCache.Get("A").CollectedAt)
}

func TestRefreshNodeFailureRetainsUnavailableEvidence(t *testing.T) {
	for _, failure := range []string{"transport", "status", "invalid JSON", "provider inventory"} {
		t.Run(failure, func(t *testing.T) {
			c := newWorkerCache(t)
			c.populateModelCacheFromItems([]map[string]any{{"name": "known", "node": "A", "assigned_app": "llamacpp"}})
			attempt := c.lastAttempt.Add(-2 * inventoryRetryInterval)
			populationErr := errors.New("previous population error")
			c.lastAttempt, c.populateErr = attempt, populationErr
			var broadcasts int
			client := noticeCluster{
				unicast: func(context.Context, string) (*mesh.Response, error) {
					switch failure {
					case "transport":
						return nil, errors.New("unreachable")
					case "status":
						return &mesh.Response{StatusCode: 503}, nil
					case "provider inventory":
						return &mesh.Response{StatusCode: 200, Body: []byte(`{"models":[],"inventory_unavailable":[{"provider":"llamacpp","reason":"stopped"}]}`)}, nil
					default:
						return &mesh.Response{StatusCode: 200, Body: []byte("{")}, nil
					}
				},
				broadcast: func(context.Context) (*mesh.BroadcastResponse, error) {
					broadcasts++
					return &mesh.BroadcastResponse{}, nil
				},
			}
			c.clusterClient = func() mesh.ClusterClient { return client }
			err := c.RefreshNode(t.Context(), "A")
			if failure == "provider inventory" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			require.True(t, c.lastAttempt.After(attempt), "a scoped failure starts a new retry window")
			require.ErrorIs(t, c.populateErr, populationErr)
			require.ErrorIs(t, c.ensurePopulated(t.Context()), populationErr)
			require.Zero(t, broadcasts, "the next reader must not broadcast to hung workers")
			require.True(t, c.HasModel("known"), "failed inventory remains evidence")
			require.Len(t, c.UnavailableInventories("A", ""), 1)
			c.populateErr = nil
			got, err := c.LookupReplicas(t.Context(), "known", "", "", "", ReplicaSelection{Exact: failure == "provider inventory"})
			require.Error(t, err)
			require.Empty(t, got, "an unavailable node cannot be routed to")
		})
	}
}

func TestRefreshNodeUsesRequestedIdentity(t *testing.T) {
	for _, tc := range []struct{ name, host, resourceNode string }{
		{"unresolved host", "unregistered.invalid:9090", "A"},
		{"different resource spelling", "192.0.2.10:9090", "a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newWorkerCache(t)
			c.resolveEndpointToNodename = func(host string) string {
				if host == "192.0.2.10:9090" {
					return "A"
				}
				return ""
			}
			metrics := &mesh.ResourceMetrics{NodeName: tc.resourceNode, CollectedAt: 2}
			var reads int
			client := noticeCluster{
				broadcast: func(context.Context) (*mesh.BroadcastResponse, error) {
					return &mesh.BroadcastResponse{Responses: []*mesh.NodeResponse{{NodeName: "A", Response: noticeResponse("192.0.2.10:9090", "old", &mesh.ResourceMetrics{NodeName: "A"})}}}, nil
				},
				unicast: func(_ context.Context, node string) (*mesh.Response, error) {
					require.Equal(t, "A", node)
					reads++
					return noticeResponse(tc.host, map[int]string{1: "fresh", 2: "replacement"}[reads], metrics), nil
				},
			}
			c.clusterClient = func() mesh.ClusterClient { return client }
			require.NoError(t, c.RefreshCacheSync(t.Context()))
			_, found := c.LookupByNode("old", "A")
			require.True(t, found, "the matching full-read host resolves to the registered name")
			require.NoError(t, c.RefreshNode(t.Context(), "A"))
			require.False(t, c.HasModel("old"))
			fresh, found := c.LookupByNode("fresh", "A")
			require.True(t, found)
			require.Equal(t, tc.host, fresh.Extra["ip_address"])
			require.Equal(t, metrics, c.nodeCache.Get("A"), "metrics are keyed by the requested node")
			require.NoError(t, c.RefreshNode(t.Context(), "A"))
			require.False(t, c.HasModel("fresh"), "the reported host must not preserve stale entries as another node")
			require.True(t, c.HasModel("replacement"))
			require.Len(t, c.GetAllModels(), 1, "repeated notices must replace the same node's inventory")
			require.Empty(t, c.UnavailableInventories("A", ""))
		})
	}
}

func TestWarmLocalCachePreservesNodeResources(t *testing.T) {
	c := newWorkerCache(t)
	metrics := &mesh.ResourceMetrics{NodeName: "A"}
	c.nodeCache.Put("A", metrics)
	c.warmLocalCache()
	require.Same(t, metrics, c.nodeCache.Get("A"))
	require.NoError(t, c.RefreshCacheSync(t.Context()))
	require.Nil(t, c.nodeCache.Get("A"), "a full read without workers still replaces resource metrics")
}

func TestRefreshNodeUnpublishedAndInvalidated(t *testing.T) {
	for _, midRead := range []bool{false, true} {
		t.Run(map[bool]string{false: "unpublished", true: "invalidated during read"}[midRead], func(t *testing.T) {
			c := newWorkerCache(t)
			if midRead {
				c.populateModelCacheFromItems([]map[string]any{{"name": "old", "node": "A"}})
			}
			var reads, broadcasts int
			client := noticeCluster{
				unicast: func(context.Context, string) (*mesh.Response, error) {
					reads++
					c.Invalidate()
					return noticeResponse("A", "stale", nil), nil
				},
				broadcast: func(context.Context) (*mesh.BroadcastResponse, error) {
					broadcasts++
					return &mesh.BroadcastResponse{Responses: []*mesh.NodeResponse{{NodeName: "A", Response: noticeResponse("A", "fresh", nil)}}}, nil
				},
			}
			c.clusterClient = func() mesh.ClusterClient { return client }
			require.NoError(t, c.RefreshNode(t.Context(), "A"))
			require.Empty(t, c.GetAllModels())
			require.False(t, c.HasModel("stale"))
			require.Equal(t, map[bool]int{false: 0, true: 1}[midRead], reads)
			require.Zero(t, broadcasts)
			got, err := c.LookupReplicas(t.Context(), "fresh", "", "", "")
			require.NoError(t, err)
			require.Len(t, got, 1)
			require.Equal(t, 1, broadcasts, "the next read rebuilds the worker's inventory")
		})
	}
}

func TestRefreshNodeConcurrentNotices(t *testing.T) {
	c := newWorkerCache(t)
	c.nodeCache = NewNodeResourceCache()
	c.populateModelCacheFromItems([]map[string]any{{"name": "old-A", "node": "A"}, {"name": "old-B", "node": "B"}})
	entered, release := make(chan string, 2), make(chan struct{})
	var mu sync.Mutex
	calls := map[string]int{}
	client := noticeCluster{unicast: func(_ context.Context, node string) (*mesh.Response, error) {
		mu.Lock()
		calls[node]++
		first := calls[node] == 1
		mu.Unlock()
		if first {
			entered <- node
			<-release
		}
		return noticeResponse(node, "fresh-"+node, &mesh.ResourceMetrics{NodeName: node}), nil
	}}
	c.clusterClient = func() mesh.ClusterClient { return client }
	results := make(chan error, 2)
	for _, node := range []string{"A", "B"} {
		go func() { results <- c.RefreshNode(t.Context(), node) }()
	}
	<-entered
	<-entered
	close(release)
	require.NoError(t, <-results)
	require.NoError(t, <-results)
	for _, node := range []string{"A", "B"} {
		require.False(t, c.HasModel("old-"+node))
		require.True(t, c.HasModel("fresh-"+node))
		require.Equal(t, node, c.nodeCache.Get(node).NodeName)
	}
}
