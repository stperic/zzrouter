// Package mesh provides resource tracking for cluster nodes.
// This file implements real-time GPU and RAM resource collection.
package mesh

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/discovery/resources"
	"github.com/stperic/zzrouter/pkg/host/inventory"
)

// ResourceTracker collects and caches resource metrics from the local node
type ResourceTracker struct {
	mu          sync.RWMutex
	metrics     *ResourceMetrics
	sequence    uint64
	cache       *inventory.Cache
	nodeName    string
	updateFreq  time.Duration
	stopCh      chan struct{}
	refreshCh   chan struct{}
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
		cache:      inventory.Default(),
		updateFreq: updateFrequency,
		stopCh:     make(chan struct{}),
		refreshCh:  make(chan struct{}, 1),
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
		rt.collectMetrics(ctx)

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
	for _, value := range []**int64{&copy.GPUMemoryTotalMB, &copy.GPUMemoryUsedMB, &copy.GPUMemoryFreeMB, &copy.RAMTotalMB, &copy.RAMAvailableMB} {
		if *value != nil {
			*value = new(**value)
		}
	}
	if copy.GPUUtilization != nil {
		copy.GPUUtilization = new(*copy.GPUUtilization)
	}
	return &copy
}

// CollectNow re-probes and publishes a fresh resource observation.
func (rt *ResourceTracker) CollectNow() *ResourceMetrics {
	rt.collectMetrics(context.Background())
	return rt.GetMetrics()
}

// RequestRefresh coalesces requests to re-probe onto the owned collector.
func (rt *ResourceTracker) RequestRefresh() {
	select {
	case rt.refreshCh <- struct{}{}:
	default:
	}
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
		case <-rt.refreshCh:
			rt.collectMetrics(ctx)
		case <-ticker.C:
			rt.collectMetrics(ctx)
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

func (rt *ResourceTracker) collectMetrics(ctx context.Context) {
	snapshot, sequence := rt.cache.RefreshSnapshot(ctx)
	rt.publishMetrics(resourceMetrics(rt.nodeName, snapshot), sequence)
}

func (rt *ResourceTracker) publishMetrics(metrics *ResourceMetrics, sequence uint64) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if sequence < rt.sequence {
		return
	}
	if rt.metrics != nil {
		metrics.ActiveModels = rt.metrics.ActiveModels
	}
	rt.metrics, rt.sequence = metrics, sequence
}

func resourceMetrics(nodeName string, snapshot resources.Snapshot) *ResourceMetrics {
	metrics := &ResourceMetrics{NodeName: nodeName, GPUType: "none"}
	if !snapshot.At.IsZero() {
		metrics.CollectedAt = snapshot.At.Unix()
	}
	if pool, ok := snapshot.SystemPool(); ok {
		metrics.RAMTotalMB, metrics.RAMAvailableMB = pool.TotalMiB, pool.AvailableMiB
		if pool.Kind == resources.Unified {
			metrics.GPUType, metrics.GPUCount = "apple", 1
			metrics.GPUMemoryTotalMB, metrics.GPUMemoryFreeMB = pool.TotalMiB, pool.AvailableMiB
			return metrics
		}
	}
	for _, vendor := range []string{"nvidia", "amd"} {
		if stats := snapshot.GPUStats(vendor); stats.Count > 0 {
			metrics.GPUType, metrics.GPUCount = vendor, stats.Count
			metrics.GPUMemoryTotalMB, metrics.GPUMemoryUsedMB, metrics.GPUMemoryFreeMB = stats.TotalMiB, stats.UsedMiB, stats.FreeMiB
			metrics.GPUUtilization = stats.UtilizationPct
			break
		}
	}
	return metrics
}

// SetActiveModels updates the count of active models
func (rt *ResourceTracker) SetActiveModels(count int) {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	if rt.metrics != nil {
		rt.metrics.ActiveModels = count
	}
}

// CollectResourceMetrics reads the shared node snapshot without starting a tracker.
func CollectResourceMetrics(nodeName string) *ResourceMetrics {
	return resourceMetrics(nodeName, inventory.Default().GetSnapshot(context.Background()))
}
