package inventory

import (
	"context"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/discovery/gpu"
	"github.com/stperic/zzrouter/pkg/discovery/resources"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnifiedPoolIsCountedOnce(t *testing.T) {
	cache := &Cache{ttl: time.Hour, snapshot: resources.Snapshot{At: utils.Now(), Pools: []resources.Pool{
		{Kind: resources.Unified, TotalMiB: new(int64(64 << 10)), AvailableMiB: new(int64(32 << 10))},
	}}}
	inv := gpu.Inventory{Cards: []gpu.Card{{Vendor: gpu.VendorApple, MemoryMiB: 64 << 10}, {Vendor: gpu.VendorApple, MemoryMiB: 64 << 10}}}
	local := GatherGPU(inv, cache.GetSnapshot(t.Context()))
	require.True(t, local.UnifiedMemory)
	assert.Equal(t, float64(64), local.MemoryTotalGB)
	require.Len(t, local.GPUs, 2)
	assert.Equal(t, local.MemoryTotalGB, local.GPUs[0].MemoryTotalGB+local.GPUs[1].MemoryTotalGB)
	assert.Equal(t, float64(32), *local.GPUs[0].MemoryAvailableGB)
	assert.Nil(t, local.GPUs[1].MemoryAvailableGB)
}

func TestNodeResourceCacheProbesOncePerIntervalAndStampsCompletion(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var probes atomic.Int32
	var returnedAt time.Time
	cache := NewCacheWithProbe(time.Hour, func(_ context.Context, _ *resources.Snapshot) resources.Snapshot {
		if probes.Add(1) == 1 {
			close(entered)
			<-release
			returnedAt = utils.Now()
		}
		return resources.Snapshot{Pools: []resources.Pool{
			{Kind: resources.Host, TotalMiB: new(int64(64 << 10)), AvailableMiB: new(int64(32 << 10))},
			{Kind: resources.GPU, Vendor: "nvidia", ID: "GPU-A", TotalMiB: new(int64(4096)), FreeMiB: new(int64(2048))},
		}}
	})
	inv := gpu.Inventory{Cards: []gpu.Card{{Vendor: gpu.VendorNVIDIA, UUID: "GPU-A"}}}
	var workers sync.WaitGroup
	memory := make(chan *NodeMemory, 1)
	gpus := make(chan *NodeGPU, 1)
	workers.Go(func() { memory <- GatherMemory(cache.GetSnapshot(t.Context())) })
	<-entered
	workers.Go(func() { gpus <- GatherGPU(inv, cache.GetSnapshot(t.Context())) })
	close(release)
	workers.Wait()
	ram, gpu := (<-memory).Sample, (<-gpus).GPUs[0].Sample
	require.Equal(t, int32(1), probes.Load())
	require.Equal(t, ram.ObservedAt, gpu.ObservedAt)
	require.False(t, ram.ObservedAt.Before(returnedAt))
	cached := cache.GetSnapshot(t.Context())
	*cached.Pools[0].TotalMiB = 1
	require.Equal(t, 64.0, GatherMemory(cache.GetSnapshot(t.Context())).TotalGB)
	cache.snapshot.At = utils.Now().Add(-2 * time.Hour)
	cache.GetSnapshot(t.Context())
	require.Equal(t, int32(2), probes.Load())
}

func TestMissingNVIDIADriverLeavesInventoryUnknown(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	cache := NewCacheWithProbe(time.Hour, func(ctx context.Context, _ *resources.Snapshot) resources.Snapshot {
		pools, err := resources.NVIDIAPools(ctx)
		return resources.Snapshot{Pools: pools, GPUError: err}
	})
	snapshot := cache.GetSnapshot(t.Context())
	require.ErrorIs(t, snapshot.GPUError, exec.ErrNotFound)
	require.Nil(t, snapshot.Pools)
	inv := gpu.Inventory{Cards: []gpu.Card{{Vendor: gpu.VendorNVIDIA, UUID: "GPU-A"}}}
	result := GatherGPU(inv, cache.GetSnapshot(t.Context()))
	require.Len(t, result.GPUs, 1)
	require.Nil(t, result.GPUs[0].Sample)
	require.Nil(t, result.GPUs[0].MemoryAvailableGB)
}

func TestCanceledResourceProbeDoesNotReplaceThePriorSample(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cache := NewCacheWithProbe(time.Minute, func(context.Context, *resources.Snapshot) resources.Snapshot {
		cancel()
		return resources.Snapshot{}
	})
	at := utils.Now().Add(-time.Hour)
	cache.snapshot = resources.Snapshot{At: at}
	require.Equal(t, at, cache.GetSnapshot(ctx).At)
}
