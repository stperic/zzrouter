package inventory

import (
	"math"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/discovery/gpu"
	"github.com/stperic/zzrouter/pkg/discovery/hardware"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReportedMemoryIsNotTotalMinusAvailable(t *testing.T) {
	now := utils.Now()
	used := int64(17 << 30)
	cache := &Cache{ttl: time.Hour, memoryAt: now.Add(-10 * time.Second),
		memInfo: &hardware.HardwareInfo{TotalRAM: 64 << 30, AvailableRAM: 32 << 30, UsedRAM: &used}}
	memory := GatherMemory(t.Context(), cache)
	require.NotNil(t, memory.Sample)
	assert.Equal(t, used, *memory.Sample.UsedBytes)
	assert.Equal(t, cache.memoryAt.UTC(), memory.Sample.ObservedAt)
	assert.GreaterOrEqual(t, memory.Sample.AgeSeconds, float64(10))
	assert.NotEqual(t, int64((memory.TotalGB-memory.AvailableGB)*(1<<30)), *memory.Sample.UsedBytes)
	cache.memInfo.UsedRAM = nil
	assert.Nil(t, GatherMemory(t.Context(), cache).Sample.UsedBytes)
	zero := int64(0)
	cache.memInfo.UsedRAM = &zero
	assert.Equal(t, &zero, GatherMemory(t.Context(), cache).Sample.UsedBytes)
}

func TestDiskRefreshDoesNotRenewMemorySample(t *testing.T) {
	old := utils.Now().Add(-time.Hour)
	cache := &Cache{ttl: time.Minute, diskAt: utils.Now(), memoryAt: old,
		diskInfo: []hardware.DiskInfo{{TotalGB: 1}}, memInfo: &hardware.HardwareInfo{TotalRAM: 1}}
	cache.GetDiskInfo(t.Context())
	value, at := cache.GetMemoryInfo(t.Context())
	require.True(t, at.After(old), "a fresh disk cache must not mask expired RAM")
	require.NotEqual(t, int64(1), value.TotalRAM)
	_, again := cache.GetMemoryInfo(t.Context())
	assert.Equal(t, at, again, "reading a cached sample must not renew it")
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
	cache := &Cache{ttl: time.Hour, gpuCardsAt: utils.Now(), gpuCards: []gpu.CardMetrics{
		{UUID: "B", TotalMemoryMiB: 8192, UsedMemoryMiB: &used},
		{UUID: "A", TotalMemoryMiB: 8192, UsedMemoryMiB: &zero, FreeMemoryMiB: &zero},
	}}
	inv := gpu.Inventory{Cards: []gpu.Card{
		{Vendor: gpu.VendorNVIDIA, UUID: "A", PCIAddress: "0000:01:00.0", MemoryMiB: 8192},
		{Vendor: gpu.VendorNVIDIA, UUID: "B", PCIAddress: "0000:02:00.0", MemoryMiB: 8192},
	}}
	value := GatherGPU(t.Context(), inv, cache)
	require.Len(t, value.GPUs, 2)
	require.NotNil(t, value.GPUs[0].Sample)
	assert.Equal(t, int64(0), *value.GPUs[0].Sample.UsedBytes)
	assert.Equal(t, int64(1<<30), *value.GPUs[1].Sample.UsedBytes)
	assert.Nil(t, value.GPUs[1].MemoryAvailableGB)
	assert.Equal(t, &zero, value.GPUs[0].Sample.UsedBytes)
}
