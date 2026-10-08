package inventory

import (
	"context"
	"sync"
	"time"

	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/discovery/gpu"
	"github.com/stperic/zzrouter/pkg/discovery/hardware"
	"github.com/stperic/zzrouter/pkg/utils"
)

// Cache is a TTL-invalidated wrapper over pkg/discovery/hardware. Hardware
// state (hot-plugged disk, memory pressure) is not observable from in-
// process events, so TTL is the correct freshness signal.
//
// Disk and memory refresh per TTL; CPU info is populated once on first
// call and served for the lifetime of the process (CPU model and core
// count don't change at runtime). The /health handler and /hosts refresh
// loop both read through this cache so each physical hardware query runs
// at most once per TTL window regardless of handler concurrency.
type Cache struct {
	mu         sync.RWMutex
	diskInfo   []hardware.DiskInfo
	memInfo    *hardware.HardwareInfo
	cpuInfo    *hardware.HardwareInfo // Cached once at first call.
	lastUpdate time.Time
	ttl        time.Duration

	// gpuCards is live per-card VRAM. It carries its own stamp because it
	// refreshes on a different trigger than disk and memory: every health
	// scrape wants it, and each refresh costs an nvidia-smi subprocess.
	gpuCards   []gpu.CardMetrics
	gpuCardsAt time.Time
}

// NewCache creates a new hardware inventory cache with the supplied TTL.
func NewCache(ttl time.Duration) *Cache {
	return &Cache{ttl: ttl}
}

// GetGPUCardMetrics returns live per-card VRAM, refreshing at most once per
// TTL window. Every /health and /hosts caller reads through here so a node
// with three handlers polling at once runs one nvidia-smi, not three — and
// so a hung driver blocks one probe rather than one per request.
//
// A nil result means no card was measured this window; callers must render
// that as unknown, never as zero free.
func (c *Cache) GetGPUCardMetrics(ctx context.Context) []gpu.CardMetrics {
	c.mu.RLock()
	if !c.gpuCardsAt.IsZero() && time.Since(c.gpuCardsAt) < c.ttl {
		cards := make([]gpu.CardMetrics, len(c.gpuCards))
		copy(cards, c.gpuCards)
		c.mu.RUnlock()
		return cards
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()

	// Double-check in case another goroutine refreshed while we waited.
	if !c.gpuCardsAt.IsZero() && time.Since(c.gpuCardsAt) < c.ttl {
		cards := make([]gpu.CardMetrics, len(c.gpuCards))
		copy(cards, c.gpuCards)
		return cards
	}

	c.gpuCards = gpu.LiveCardMetricsContext(ctx)
	c.gpuCardsAt = utils.Now()

	cards := make([]gpu.CardMetrics, len(c.gpuCards))
	copy(cards, c.gpuCards)
	return cards
}

// GetDiskInfo returns cached disk info or refreshes if expired.
func (c *Cache) GetDiskInfo(ctx context.Context) []hardware.DiskInfo {
	c.mu.RLock()
	if time.Since(c.lastUpdate) < c.ttl && c.diskInfo != nil {
		diskInfo := make([]hardware.DiskInfo, len(c.diskInfo))
		copy(diskInfo, c.diskInfo)
		c.mu.RUnlock()
		return diskInfo
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()

	// Double-check in case another goroutine refreshed while we waited.
	if time.Since(c.lastUpdate) < c.ttl && c.diskInfo != nil {
		diskInfo := make([]hardware.DiskInfo, len(c.diskInfo))
		copy(diskInfo, c.diskInfo)
		return diskInfo
	}

	diskInfo, _ := hardware.DiscoverDisk(ctx)
	c.diskInfo = diskInfo
	c.lastUpdate = utils.Now()

	utils.LogDebugf("Hardware cache refreshed: %d disks discovered", len(diskInfo))

	result := make([]hardware.DiskInfo, len(diskInfo))
	copy(result, diskInfo)
	return result
}

// GetMemoryInfo returns cached memory info or refreshes if expired.
func (c *Cache) GetMemoryInfo(ctx context.Context) *hardware.HardwareInfo {
	c.mu.RLock()
	if time.Since(c.lastUpdate) < c.ttl && c.memInfo != nil {
		memInfo := *c.memInfo
		c.mu.RUnlock()
		return &memInfo
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()

	if time.Since(c.lastUpdate) < c.ttl && c.memInfo != nil {
		memInfo := *c.memInfo
		return &memInfo
	}

	memInfo, _ := hardware.DiscoverMemory(ctx)
	c.memInfo = &memInfo
	c.lastUpdate = utils.Now()

	totalGB := float64(memInfo.TotalRAM) / constants.BytesPerGB
	utils.LogDebugf("🔄 Memory cache refreshed: %.1f GB total", totalGB)

	result := *c.memInfo
	return &result
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
