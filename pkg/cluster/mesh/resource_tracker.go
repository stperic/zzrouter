// Package mesh provides resource tracking for cluster nodes.
// This file implements real-time GPU and RAM resource collection.
package mesh

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/discovery/gpu"
	"github.com/stperic/zzrouter/pkg/discovery/hardware"
	"github.com/stperic/zzrouter/pkg/utils"
)

// gpuProbeTimeout bounds every call into gpu.LiveMetricsContext.
// Short because nvidia-smi on a broken driver can hang.
const gpuProbeTimeout = 5 * time.Second

// ResourceTracker collects and caches resource metrics from the local node
type ResourceTracker struct {
	mu          sync.RWMutex
	metrics     *ResourceMetrics
	nodeName    string
	updateFreq  time.Duration
	stopCh      chan struct{}
	runningOnce sync.Once
	stoppedOnce sync.Once
	started     atomic.Bool
	// wg tracks the runCollector goroutine so Stop blocks until it
	// exits. Without this the tracker can race with the node-level
	// shutdown sequence (resource collection holds stopCh between
	// ticks and the caller assumes Stop is synchronous).
	wg sync.WaitGroup

	// tickedCh is a test hook: when non-nil, runCollector performs a
	// non-blocking send after each ticker-driven collection. Tests wait
	// on it instead of sleeping to observe a collection cycle. Nil in
	// production; wire via SetTickedChForTest before calling Start.
	//
	// The send is coalescing — if the receiver is slow and two ticks
	// land before the test can receive, the second drop silently.
	// Tests requiring N-tick counting must use a mock ticker or drive
	// CollectNow directly rather than depending on this hook.
	tickedCh chan<- struct{}
}

// NewResourceTracker creates a new resource tracker
func NewResourceTracker(nodeName string, updateFrequency time.Duration) *ResourceTracker {
	if updateFrequency <= 0 {
		updateFrequency = constants.ResourceTrackerInterval // Default: 30 seconds
	}

	return &ResourceTracker{
		nodeName:   nodeName,
		updateFreq: updateFrequency,
		stopCh:     make(chan struct{}),
	}
}

// Start begins periodic resource collection
func (rt *ResourceTracker) Start(ctx context.Context) {
	if rt == nil {
		return
	}
	rt.runningOnce.Do(func() {
		rt.started.Store(true)

		// Collect initial metrics
		rt.collectMetrics()

		// Start periodic collection
		rt.wg.Add(1)
		go func() {
			defer rt.wg.Done()
			rt.runCollector(ctx)
		}()
	})
}

// Stop halts periodic resource collection
func (rt *ResourceTracker) Stop() {
	if rt == nil {
		return
	}
	rt.stoppedOnce.Do(func() {
		close(rt.stopCh)
	})
	rt.wg.Wait()
}

// GetMetrics returns the current cached resource metrics
func (rt *ResourceTracker) GetMetrics() *ResourceMetrics {
	rt.mu.RLock()
	defer rt.mu.RUnlock()

	if rt.metrics == nil {
		return nil
	}

	// Return a copy to prevent mutation
	copy := *rt.metrics
	return &copy
}

// CollectNow forces an immediate metrics collection and returns the result
func (rt *ResourceTracker) CollectNow() *ResourceMetrics {
	rt.collectMetrics()
	return rt.GetMetrics()
}

// runCollector runs the periodic collection loop
func (rt *ResourceTracker) runCollector(ctx context.Context) {
	ticker := time.NewTicker(rt.updateFreq)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-rt.stopCh:
			return
		case <-ticker.C:
			rt.collectMetrics()
			if rt.tickedCh != nil {
				// Non-blocking: tests that aren't watching don't stall
				// the collector, and a test waiting on the next tick
				// still observes this signal.
				select {
				case rt.tickedCh <- struct{}{}:
				default:
				}
			}
		}
	}
}

// collectMetrics gathers current resource metrics
func (rt *ResourceTracker) collectMetrics() {
	metrics := &ResourceMetrics{
		NodeName:    rt.nodeName,
		CollectedAt: utils.Now().Unix(),
	}

	// Collect RAM metrics
	rt.collectRAMMetrics(metrics)

	// Collect GPU metrics based on platform. On Linux/Windows
	// one poll of gpu.LiveMetricsContext invokes nvidia-smi and
	// rocm-smi at most once each and returns both vendor's
	// aggregate metrics in a single struct. We then apply the
	// NVIDIA-over-AMD precedence that this tracker has always
	// used — the two helpers it replaced did the same thing via
	// sequential fallback, but needed two separate subprocess
	// calls when the first one failed.
	switch runtime.GOOS {
	case "darwin":
		rt.collectAppleSiliconMetrics(metrics)
	default:
		ctx, cancel := context.WithTimeout(context.Background(), gpuProbeTimeout)
		set := gpu.LiveMetricsContext(ctx)
		cancel()
		switch {
		case set.NVIDIA.Count > 0:
			applyLiveMetrics(metrics, "nvidia", set.NVIDIA)
		case set.AMD.Count > 0:
			applyLiveMetrics(metrics, "amd", set.AMD)
		}
	}

	// Update cached metrics
	rt.mu.Lock()
	rt.metrics = metrics
	rt.mu.Unlock()

	utils.LogDebugf("[ResourceTracker] Collected metrics: GPU=%s (%dMB free), RAM=%dMB available",
		metrics.GPUType, metrics.GPUMemoryFreeMB, metrics.RAMAvailableMB)
}

// applyLiveMetrics copies gpu.LiveMetrics fields into the
// tracker's ResourceMetrics shape. Memory fields are already
// in MiB — gpu.LiveMetrics and ResourceMetrics use compatible
// MB-ish units for memory (both are 1024-based "MB" values).
func applyLiveMetrics(metrics *ResourceMetrics, gpuType string, m gpu.LiveMetrics) {
	metrics.GPUType = gpuType
	metrics.GPUCount = m.Count
	metrics.GPUMemoryTotalMB = m.TotalMemoryMiB
	metrics.GPUMemoryUsedMB = m.UsedMemoryMiB
	metrics.GPUMemoryFreeMB = m.FreeMemoryMiB
	metrics.GPUUtilization = m.UtilizationPct
}

// collectRAMMetrics collects system RAM information via the
// shared pkg/discovery/hardware helper. That's the same
// abstraction pkg/discovery/gpu uses for GPU metrics in this
// file — keeping both reads behind the discovery package keeps
// the cluster tracker free of gopsutil imports and matches the
// "no direct gopsutil calls outside pkg/discovery" invariant
// the GPU consolidation arc established.
func (rt *ResourceTracker) collectRAMMetrics(metrics *ResourceMetrics) {
	ctx, cancel := context.WithTimeout(context.Background(), gpuProbeTimeout)
	defer cancel()

	info, errs := hardware.DiscoverMemory(ctx)
	if len(errs) > 0 || info.TotalRAM == 0 {
		utils.LogDebugf("[ResourceTracker] Failed to get memory info: %v", errs)
		return
	}

	metrics.RAMTotalMB = info.TotalRAM / (1024 * 1024)
	metrics.RAMAvailableMB = info.AvailableRAM / (1024 * 1024)
}

// collectAppleSiliconMetrics collects Apple Silicon GPU metrics.
// Apple Silicon uses unified memory, so GPU memory is a slice of
// system RAM — there is no per-GPU figure to pull from any SMI
// tool, which is why this path doesn't run through
// gpu.LiveMetricsContext like NVIDIA and AMD do.
//
// Detection delegates to gpu.ProbeContext(VendorApple) so the
// GOARCH check and the sysctl brand-string parse live in exactly
// one place (pkg/discovery/gpu/probe_darwin.go). ProbeContext for
// Apple is cheap — one sysctl syscall, no ghw, no subprocess —
// so it's safe to call on the 30s polling interval without
// opening the "pulls the full inventory on every scrape" hole
// that InventoryContext would.
func (rt *ResourceTracker) collectAppleSiliconMetrics(metrics *ResourceMetrics) {
	ctx, cancel := context.WithTimeout(context.Background(), gpuProbeTimeout)
	defer cancel()

	det, _ := gpu.ProbeContext(ctx, gpu.VendorApple)
	if det.State != gpu.StateDriverOK {
		metrics.GPUType = "none"
		metrics.GPUCount = 0
		return
	}

	metrics.GPUType = "apple"
	metrics.GPUCount = 1 // Apple Silicon is integrated

	// Apple Silicon uses unified memory architecture: GPU memory
	// is a share of system RAM. Query via hardware.DiscoverMemory
	// (same helper collectRAMMetrics uses) and scale by a
	// conservative 75% ratio for the portion Apple's Metal / MLX
	// runtimes can realistically use.
	memInfo, errs := hardware.DiscoverMemory(ctx)
	if len(errs) == 0 && memInfo.TotalRAM > 0 {
		const gpuAvailableRatio = 0.75
		metrics.GPUMemoryTotalMB = int64(float64(memInfo.TotalRAM) / (1024 * 1024) * gpuAvailableRatio)
		metrics.GPUMemoryFreeMB = int64(float64(memInfo.AvailableRAM) / (1024 * 1024) * gpuAvailableRatio)
		metrics.GPUMemoryUsedMB = metrics.GPUMemoryTotalMB - metrics.GPUMemoryFreeMB
	}

	// Apple doesn't expose GPU utilization easily; leave it at 0.
	metrics.GPUUtilization = 0
}

// SetActiveModels updates the count of active models
func (rt *ResourceTracker) SetActiveModels(count int) {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	if rt.metrics != nil {
		rt.metrics.ActiveModels = count
	}
}

// CollectResourceMetrics is a standalone function to collect metrics once
// Useful for immediate collection without starting the tracker
func CollectResourceMetrics(nodeName string) *ResourceMetrics {
	tracker := NewResourceTracker(nodeName, 0)
	return tracker.CollectNow()
}
