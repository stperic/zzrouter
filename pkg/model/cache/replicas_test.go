package cache

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/modelregistry"
	"github.com/stretchr/testify/require"
)

type replicaClusterClient struct {
	mesh.ClusterClient
	calls   atomic.Int32
	fail    bool
	scoped  bool
	entered chan struct{}
	release chan struct{}
}

func (c *replicaClusterClient) Broadcast(context.Context, string, *mesh.QueryParams) (*mesh.BroadcastResponse, error) {
	c.calls.Add(1)
	if c.entered != nil {
		c.entered <- struct{}{}
		<-c.release
	}
	if c.fail && c.scoped {
		return &mesh.BroadcastResponse{Responses: []*mesh.NodeResponse{{NodeName: "worker-1", Error: errors.New("worker unavailable")}}}, errors.New("partial broadcast failure")
	}
	if c.fail {
		return nil, errors.New("worker unavailable")
	}
	body, _ := json.Marshal(map[string]any{"models": []map[string]any{{"name": "qwen", "assigned_app": "llamacpp", "node": "worker-1"}}})
	return &mesh.BroadcastResponse{Responses: []*mesh.NodeResponse{{NodeName: "worker-1", Response: &mesh.Response{StatusCode: 200, Body: body}}}}, nil
}

func TestLookupReplicasWaitsForInvalidatedWorkerCatalog(t *testing.T) {
	for _, invalidated := range []bool{false, true} {
		t.Run(map[bool]string{false: "cold", true: "invalidated"}[invalidated], func(t *testing.T) {
			c := newWorkerCache(t)
			client := &replicaClusterClient{}
			c.clusterClient = func() mesh.ClusterClient { return client }
			if invalidated {
				c.updateModelCache(samplesFor())
				c.Invalidate()
			}
			require.Empty(t, c.GetAllModels(), "the fast path must miss")
			pinned, err := c.LookupReplicas(t.Context(), "llama3", "", "gpu-1", "")
			require.NoError(t, err)
			require.Empty(t, pinned, "explicit pins must not read retained invalidation evidence")
			require.Equal(t, int32(0), client.calls.Load(), "pins do not populate")
			replicas, err := c.LookupReplicas(t.Context(), "QWEN", "", "", "")
			require.NoError(t, err)
			require.Len(t, replicas, 1)
			require.Equal(t, "worker-1", replicas[0].Node)
			require.Equal(t, int32(1), client.calls.Load())
		})
	}
}

func TestLookupReplicasCachesFailedBroadcast(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		t.Run(map[bool]string{false: "unknown scope", true: "known node"}[scoped], func(t *testing.T) {
			c := newWorkerCache(t)
			client := &replicaClusterClient{fail: true, scoped: scoped, entered: make(chan struct{}, 20), release: make(chan struct{})}
			c.clusterClient = func() mesh.ClusterClient { return client }
			c.populateModelCacheFromItems([]map[string]any{{"name": "known", "node": "worker-1", "assigned_app": "llamacpp"}})
			c.Invalidate()
			var requests sync.WaitGroup
			start := make(chan struct{})
			for range 20 {
				requests.Go(func() {
					<-start
					replicas, err := c.LookupReplicas(t.Context(), "unknown", "", "", "")
					require.NoError(t, err)
					require.Empty(t, replicas)
				})
			}
			close(start)
			<-client.entered
			select {
			case <-client.entered:
				t.Error("parallel unknown-name refresh bypassed singleflight")
			case <-time.After(25 * time.Millisecond):
			}
			close(client.release)
			requests.Wait()
			require.Equal(t, int32(1), client.calls.Load(), "concurrent unknown names share one failed broadcast")
			require.NotEmpty(t, c.UnavailableInventories("", ""))
			// Once failed discovery is published, a request must never wait for the worker again.
			client.fail = false
			replicas, err := c.LookupReplicas(t.Context(), "qwen", "", "", "")
			require.NoError(t, err)
			require.Empty(t, replicas)
			require.Equal(t, int32(1), client.calls.Load())
			replicas, err = c.LookupReplicas(t.Context(), "known", "", "", "")
			if scoped {
				require.ErrorContains(t, err, "worker-1")
			} else {
				require.NoError(t, err)
				require.Empty(t, replicas, "unknown failure scope cannot establish a model's node")
			}
			c.mu.Lock()
			c.lastAttempt = time.Now().Add(-inventoryRetryInterval)
			c.mu.Unlock()
			replicas, err = c.LookupReplicas(t.Context(), "qwen", "", "", "")
			require.NoError(t, err)
			require.Len(t, replicas, 1)
			require.Equal(t, int32(2), client.calls.Load())
			require.Empty(t, c.UnavailableInventories("", ""))
		})
	}
}

func TestLookupReplicasAliasesVariantsProviderAndCase(t *testing.T) {
	c := newWorkerCache(t)
	cfg := variantApps(t, true, map[string]config.ModelSpec{"base+shared": {From: "base"}, "base+own": {From: "base", Parameters: map[string]string{"ctx-size": "8192"}}})
	c.appsConfig = func() *config.AppsConfig { return cfg }
	c.populateModelCacheFromItems([]map[string]any{
		{"name": "base", "source_id": "org/source", "node": "worker-1", "assigned_app": "llamacpp"},
		{"name": "BASE", "source_id": "org/source", "node": "WORKER-1", "assigned_app": "llamacpp"},
		{"name": "base", "source_id": "org/source", "node": "worker-2", "assigned_app": "vllm"},
	})
	for _, tc := range []struct {
		name, provider, canonical string
		count                     int
	}{
		{"ORG/SOURCE", "", "base", 2}, {"BaSe", "LLAMACPP", "base", 1}, {"BASE+SHARED", "", "base", 1}, {"base+own", "", "base+own", 1},
	} {
		t.Run(tc.name+tc.provider, func(t *testing.T) {
			got, err := c.LookupReplicas(t.Context(), tc.name, tc.provider, "", "")
			require.NoError(t, err)
			require.Len(t, got, tc.count)
			require.Equal(t, strings.ToLower(tc.canonical), strings.ToLower(got[0].Model))
		})
	}
	got, err := c.LookupReplicas(t.Context(), "base", "vllm", "worker-1", "")
	require.NoError(t, err)
	require.Empty(t, got, "pinning cannot borrow a different provider")
}

func TestLookupReplicasNamesPrecedeSourceAliases(t *testing.T) {
	c := newWorkerCache(t)
	c.populateModelCacheFromItems([]map[string]any{
		{"name": "foo", "source_id": "bar", "node": "one", "assigned_app": "llamacpp"},
		{"name": "bar", "node": "two", "assigned_app": "llamacpp"},
	})
	got, err := c.LookupReplicas(t.Context(), "BAR", "", "", "")
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "bar", got[0].Name)
	require.Equal(t, "two", got[0].Node)
}

func TestCachePopulationFailedRefreshBackoff(t *testing.T) {
	c := newWorkerCache(t)
	var calls atomic.Int32
	c.registry = func() *modelregistry.Registry { calls.Add(1); return nil }
	c.node = &fakeNode{}
	_, first := c.LookupReplicas(t.Context(), "unknown", "", "", "")
	require.Error(t, first)
	require.False(t, c.valid, "failed refresh must not claim a populated catalog")
	require.True(t, c.lastUpdated.IsZero(), "no successful publication armed the interval")
	require.False(t, c.lastAttempt.IsZero())
	var requests sync.WaitGroup
	for range 20 {
		requests.Go(func() {
			_, err := c.LookupReplicas(t.Context(), "unknown", "", "", "")
			require.ErrorIs(t, err, first, "backoff must preserve lookup failure for anonymous cloud admission")
		})
	}
	requests.Wait()
	require.EqualValues(t, 1, calls.Load(), "failed refresh is not retried per request")
	c.mu.Lock()
	c.lastAttempt = time.Now().Add(-inventoryRetryInterval)
	c.mu.Unlock()
	_, err := c.LookupReplicas(t.Context(), "unknown", "", "", "")
	require.Error(t, err)
	require.EqualValues(t, 2, calls.Load(), "expiry retries the failing refresh")
	c.node = &fakeNode{name: "worker-1"}
	c.Invalidate()
	before := calls.Load()
	replicas, err := c.LookupReplicas(t.Context(), "unknown", "", "", "")
	require.NoError(t, err, "invalidation clears backoff and permits immediate recovery")
	require.Empty(t, replicas)
	require.Equal(t, before+1, calls.Load())
	require.True(t, c.valid)
}

type cacheSyncHook func()

func (h cacheSyncHook) SyncAllFromCache() { h() }

func TestPopulationSuccessDoesNotUndoInvalidation(t *testing.T) {
	c := newWorkerCache(t)
	var scans atomic.Int32
	c.registry = func() *modelregistry.Registry { scans.Add(1); return nil }
	var published int
	c.autoRoute = cacheSyncHook(func() {
		published++
		if published == 1 {
			c.mu.Lock()
			c.invalidateLocked()
			c.mu.Unlock()
		}
	})
	require.NoError(t, c.ensurePopulated(t.Context()))
	require.False(t, c.valid)
	require.NoError(t, c.ensurePopulated(t.Context()))
	require.Equal(t, int32(2), scans.Load(), "invalidation after publication must still trigger discovery")
	require.True(t, c.valid)
}

func TestFailedPopulationDoesNotOverwriteNewerPublication(t *testing.T) {
	c := newWorkerCache(t)
	c.node = &fakeNode{}
	var published time.Time
	c.registry = func() *modelregistry.Registry {
		// A concurrent refresh's newer publication is protected even if this one errors.
		c.updateModelCache(nil)
		published = c.lastAttempt
		return nil
	}
	// ensurePopulated captures its attempt before the registry hook publishes.
	require.Error(t, c.ensurePopulated(t.Context()))
	require.Equal(t, published, c.lastAttempt)
	require.Nil(t, c.populateErr, "the newer catalog's successful publication remains authoritative")
	require.NoError(t, c.ensurePopulated(t.Context()))
}
