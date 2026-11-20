package inventory

import (
	"context"
	"sync"
	"time"

	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/discovery/hardware"
	"github.com/stperic/zzrouter/pkg/discovery/resources"
	"github.com/stperic/zzrouter/pkg/utils"
)

// Cache shares one completed resource probe per TTL alongside disk and static CPU inventory.
type Cache struct {
	mu       sync.RWMutex
	diskInfo []hardware.DiskInfo
	cpuInfo  *hardware.HardwareInfo
	diskAt   time.Time
	ttl      time.Duration

	resourceMu sync.Mutex
	snapshot   resources.Snapshot
	probe      func(context.Context, *resources.Snapshot) resources.Snapshot
}

// NewCache creates a node cache using the local resource sampler.
func NewCache(ttl time.Duration) *Cache {
	return NewCacheWithProbe(ttl, resources.SampleNode)
}

// NewCacheWithProbe lets snapshot consumers exercise the cache with a controlled resource probe.
func NewCacheWithProbe(ttl time.Duration, probe func(context.Context, *resources.Snapshot) resources.Snapshot) *Cache {
	return &Cache{ttl: ttl, probe: probe}
}

// GetSnapshot keeps concurrent views on the same source sample until its TTL expires.
func (c *Cache) GetSnapshot(ctx context.Context) resources.Snapshot {
	return c.readSnapshot(ctx, false)
}

// RefreshSnapshot re-probes and publishes the result for ordinary cache readers.
func (c *Cache) RefreshSnapshot(ctx context.Context) resources.Snapshot {
	return c.readSnapshot(ctx, true)
}

func (c *Cache) readSnapshot(ctx context.Context, refresh bool) resources.Snapshot {
	c.resourceMu.Lock()
	defer c.resourceMu.Unlock()
	if (refresh || time.Since(c.snapshot.At) >= c.ttl) && ctx.Err() == nil {
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		snapshot := c.probe(probeCtx, &c.snapshot)
		if ctx.Err() == nil {
			snapshot.At = utils.Now()
			c.snapshot = snapshot
		}
	}
	return c.snapshot.Clone()
}

// GetDiskInfo returns cached disk info or refreshes if expired.
func (c *Cache) GetDiskInfo(ctx context.Context) []hardware.DiskInfo {
	c.mu.RLock()
	if time.Since(c.diskAt) < c.ttl && c.diskInfo != nil {
		diskInfo := make([]hardware.DiskInfo, len(c.diskInfo))
		copy(diskInfo, c.diskInfo)
		c.mu.RUnlock()
		return diskInfo
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()

	// Double-check in case another goroutine refreshed while we waited.
	if time.Since(c.diskAt) < c.ttl && c.diskInfo != nil {
		diskInfo := make([]hardware.DiskInfo, len(c.diskInfo))
		copy(diskInfo, c.diskInfo)
		return diskInfo
	}

	c.diskAt = utils.Now()
	diskInfo, _ := hardware.DiscoverDisk(ctx)
	c.diskInfo = diskInfo

	utils.LogDebugf("Hardware cache refreshed: %d disks discovered", len(diskInfo))

	result := make([]hardware.DiskInfo, len(diskInfo))
	copy(result, diskInfo)
	return result
}

// GetCPUInfo returns cached CPU info, populating it on first call. Unlike
// disk and memory, the CPU result is cached for the lifetime of the
// process — CPU model, physical core count, and logical thread count do
// not change at runtime, so re-probing would burn cycles on an answer
// that is already known.
func (c *Cache) GetCPUInfo(ctx context.Context) *hardware.HardwareInfo {
	c.mu.RLock()
	if c.cpuInfo != nil {
		cpuInfo := *c.cpuInfo
		c.mu.RUnlock()
		return &cpuInfo
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.cpuInfo != nil {
		cpuInfo := *c.cpuInfo
		return &cpuInfo
	}

	cpuInfo, _ := hardware.DiscoverCPU(ctx)
	c.cpuInfo = &cpuInfo

	utils.LogDebugf("CPU cache populated: %s", cpuInfo.CPUInfo)

	result := *c.cpuInfo
	return &result
}

// Default returns the process-global Cache, lazily constructed with
// constants.HealthCheckInterval TTL. Prefer passing a *Cache explicitly;
// the default exists for callers that historically relied on the server's
// package-level singleton.
var (
	defaultCache     *Cache
	defaultCacheOnce sync.Once
)

func Default() *Cache {
	defaultCacheOnce.Do(func() {
		defaultCache = NewCache(constants.HealthCheckInterval)
	})
	return defaultCache
}
