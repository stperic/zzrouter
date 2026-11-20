package inventory

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/discovery/gpu"
	"github.com/stperic/zzrouter/pkg/discovery/hardware"
	"github.com/stperic/zzrouter/pkg/discovery/resources"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReportedMemoryIsNotTotalMinusAvailable(t *testing.T) {
	now := utils.Now()
	used := int64(17 << 10)
	cache := &Cache{ttl: time.Hour, snapshot: resources.Snapshot{
		At:    now.Add(-10 * time.Second),
		Pools: []resources.Pool{{Kind: resources.Host, TotalMiB: new(int64(64 << 10)), AvailableMiB: new(int64(32 << 10)), UsedMiB: &used}},
	}}
	memory := GatherMemory(cache.GetSnapshot(t.Context()))
	require.NotNil(t, memory.Sample)
	assert.Equal(t, used<<20, *memory.Sample.UsedBytes)
	assert.Equal(t, new(17.0/64*100), memory.UsedPercent)
	assert.Equal(t, cache.snapshot.At.UTC(), memory.Sample.ObservedAt)
	assert.GreaterOrEqual(t, memory.Sample.AgeSeconds, float64(10))
	assert.NotEqual(t, int64((memory.TotalGB-memory.AvailableGB)*(1<<30)), *memory.Sample.UsedBytes)
	cache.snapshot.Pools[0].UsedMiB = nil
	assert.Nil(t, GatherMemory(cache.GetSnapshot(t.Context())).Sample.UsedBytes)
	assert.Nil(t, GatherMemory(cache.GetSnapshot(t.Context())).UsedPercent)
	zero := int64(0)
	cache.snapshot.Pools[0].UsedMiB = &zero
	assert.Equal(t, &zero, GatherMemory(cache.GetSnapshot(t.Context())).Sample.UsedBytes)
}

func TestDiskRefreshDoesNotRenewMemorySample(t *testing.T) {
	old := utils.Now().Add(-time.Hour)
	cache := NewCacheWithProbe(time.Minute, func(context.Context, *resources.Snapshot) resources.Snapshot {
		return resources.Snapshot{Pools: []resources.Pool{{Kind: resources.Host, TotalMiB: new(int64(64 << 10))}}}
	})
	cache.diskAt, cache.snapshot.At = utils.Now(), old
	cache.diskInfo = []hardware.DiskInfo{{TotalGB: 1}}
	cache.GetDiskInfo(t.Context())
	value := cache.GetSnapshot(t.Context())
	require.True(t, value.At.After(old), "a fresh disk cache must not mask expired RAM")
	assert.Equal(t, value.At, cache.GetSnapshot(t.Context()).At, "reading a cached sample must not renew it")
}

func TestMemorySampleUsesLocalClockAndRetainsAge(t *testing.T) {
	start := utils.Now().Add(-4 * time.Second)
	used := int64(0)
	peer := &MemorySample{TotalBytes: 1024, UsedBytes: &used, AgeSeconds: 20, ObservedAt: start.Add(time.Hour)}
	local := ReceiveMemorySample(peer, start)
	require.NotNil(t, local)
	assert.Equal(t, start.Add(-20*time.Second).UTC(), local.ObservedAt)
	assert.GreaterOrEqual(t, local.AgeSeconds, float64(24))
	assert.Equal(t, &used, local.UsedBytes)
	assert.False(t, local.Stale)
	assert.True(t, local.Current(start.Add(time.Minute), false).Stale)
	assert.True(t, local.Current(start, true).Stale)
	assert.False(t, local.Stale, "projection must not modify retained snapshots")
	for _, age := range []float64{-1, math.NaN(), math.Inf(1), 1e20} {
		peer.AgeSeconds = age
		assert.Nil(t, ReceiveMemorySample(peer, start))
	}
	assert.Nil(t, ReceiveMemorySample(nil, start))
}

func TestGPUCardSamplesKeepIdentityAndPartialMeasurements(t *testing.T) {
	used, zero := int64(1024), int64(0)
	cache := &Cache{ttl: time.Hour, snapshot: resources.Snapshot{At: utils.Now(), Pools: []resources.Pool{
		{Kind: resources.GPU, Vendor: "nvidia", ID: "B", TotalMiB: new(int64(8192)), UsedMiB: &used},
		{Kind: resources.GPU, Vendor: "nvidia", ID: "A", TotalMiB: new(int64(8192)), UsedMiB: &zero, FreeMiB: &zero},
	}}}
	inv := gpu.Inventory{Cards: []gpu.Card{
		{Vendor: gpu.VendorNVIDIA, UUID: "A", PCIAddress: "0000:01:00.0", MemoryMiB: 8192},
		{Vendor: gpu.VendorNVIDIA, UUID: "B", PCIAddress: "0000:02:00.0", MemoryMiB: 8192},
	}}
	value := GatherGPU(inv, cache.GetSnapshot(t.Context()))
	require.Len(t, value.GPUs, 2)
	require.NotNil(t, value.GPUs[0].Sample)
	assert.Equal(t, int64(0), *value.GPUs[0].Sample.UsedBytes)
	assert.Equal(t, int64(1<<30), *value.GPUs[1].Sample.UsedBytes)
	assert.Nil(t, value.GPUs[1].MemoryAvailableGB)
	assert.Equal(t, &zero, value.GPUs[0].Sample.UsedBytes)
}
