package mesh

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stperic/zzrouter/pkg/discovery/resources"
	"github.com/stperic/zzrouter/pkg/host/inventory"
	"github.com/stretchr/testify/require"
)

func TestResourceTrackerCreation(t *testing.T) {
	t.Parallel()
	tracker := NewResourceTracker("test-node", 30*time.Second)

	if tracker == nil {
		t.Fatal("NewResourceTracker returned nil")
	}

	if tracker.nodeName != "test-node" {
		t.Errorf("nodeName = %q, want %q", tracker.nodeName, "test-node")
	}

	if tracker.updateFreq != 30*time.Second {
		t.Errorf("updateFreq = %v, want %v", tracker.updateFreq, 30*time.Second)
	}
}

func TestResourceTrackerDefaultFrequency(t *testing.T) {
	t.Parallel()
	// Zero frequency should default to 30 seconds
	tracker := NewResourceTracker("test-node", 0)

	if tracker.updateFreq != 30*time.Second {
		t.Errorf("updateFreq = %v, want default 30s", tracker.updateFreq)
	}
}

func TestResourceTrackerCollectNow(t *testing.T) {
	t.Parallel()
	tracker := NewResourceTracker("test-node", 30*time.Second)

	metrics := tracker.CollectNow()

	if metrics == nil {
		t.Fatal("CollectNow returned nil")
	}

	// Verify node name is set
	if metrics.NodeName != "test-node" {
		t.Errorf("NodeName = %q, want %q", metrics.NodeName, "test-node")
	}

	// Verify collection timestamp is recent
	collectedAt := time.Unix(metrics.CollectedAt, 0)
	if time.Since(collectedAt) > 5*time.Second {
		t.Error("CollectedAt is not recent")
	}

	// RAM should always be populated
	if metrics.RAMTotalMB == nil || *metrics.RAMTotalMB <= 0 {
		t.Error("RAMTotalMB should be > 0")
	}

	if metrics.RAMAvailableMB == nil || *metrics.RAMAvailableMB <= 0 {
		t.Error("RAMAvailableMB should be > 0")
	}

	// RAM available should be <= total
	require.NotNil(t, metrics.RAMAvailableMB)
	require.NotNil(t, metrics.RAMTotalMB)
	require.LessOrEqual(t, *metrics.RAMAvailableMB, *metrics.RAMTotalMB)
}

func TestResourceTrackerStartStop(t *testing.T) {
	t.Parallel()
	tracker := NewResourceTracker("test-node", 100*time.Millisecond)

	ctx := t.Context()

	// Start collects initial metrics synchronously before spawning the
	// ticker goroutine; no wait needed to observe the first snapshot.
	tracker.Start(ctx)

	metrics := tracker.GetMetrics()
	if metrics == nil {
		t.Error("GetMetrics returned nil after Start")
	}

	// Stop the tracker
	tracker.Stop()

	// Should still be able to get cached metrics after stop
	metrics = tracker.GetMetrics()
	if metrics == nil {
		t.Error("GetMetrics returned nil after Stop")
	}
}

func TestResourceTrackerSetActiveModels(t *testing.T) {
	t.Parallel()
	tracker := NewResourceTracker("test-node", 30*time.Second)

	// Collect initial metrics
	tracker.CollectNow()

	// Set active models count
	tracker.SetActiveModels(3)

	metrics := tracker.GetMetrics()
	if metrics == nil {
		t.Fatal("GetMetrics returned nil")
	}

	if metrics.ActiveModels != 3 {
		t.Errorf("ActiveModels = %d, want 3", metrics.ActiveModels)
	}
}

func TestCollectResourceMetricsStandalone(t *testing.T) {
	t.Parallel()
	// Test the standalone function
	metrics := CollectResourceMetrics("standalone-test")

	if metrics == nil {
		t.Fatal("CollectResourceMetrics returned nil")
	}

	if metrics.NodeName != "standalone-test" {
		t.Errorf("NodeName = %q, want %q", metrics.NodeName, "standalone-test")
	}

	// Should have valid RAM metrics
	if metrics.RAMTotalMB == nil || *metrics.RAMTotalMB <= 0 {
		t.Error("RAMTotalMB should be > 0")
	}
}

func TestResourceMetricsCopy(t *testing.T) {
	t.Parallel()
	tracker := NewResourceTracker("test-node", 30*time.Second)
	tracker.CollectNow()

	// Get metrics twice
	metrics1 := tracker.GetMetrics()
	metrics2 := tracker.GetMetrics()

	// Should be separate copies
	metrics1.ActiveModels = 99

	if metrics2.ActiveModels == 99 {
		t.Error("GetMetrics should return a copy, not the same reference")
	}
}

func TestResourceTrackerPeriodicUpdate(t *testing.T) {
	t.Parallel()
	tracker := NewResourceTracker("test-node", 50*time.Millisecond)

	// Buffered capacity 1 so a single tick doesn't block the collector
	// if the test hasn't yet entered the receive.
	ticked := make(chan struct{}, 1)
	tracker.SetTickedChForTest(ticked)

	ctx := t.Context()
	tracker.Start(ctx)
	t.Cleanup(tracker.Stop)

	// Wait for one ticker-driven collection — proves periodic update
	// actually fires without relying on wall-clock sleep.
	select {
	case <-ticked:
	case <-time.After(2 * time.Second):
		t.Fatal("tracker never reported a ticker-driven collection")
	}

	metrics := tracker.GetMetrics()
	if metrics == nil {
		t.Fatal("GetMetrics returned nil")
	}

	collectedAt := time.Unix(metrics.CollectedAt, 0)
	if time.Since(collectedAt) > 2*time.Second {
		t.Errorf("Metrics should be periodically updated, but last collection was %v ago", time.Since(collectedAt))
	}

	if metrics.RAMTotalMB == nil || *metrics.RAMTotalMB <= 0 {
		t.Error("Expected RAM metrics to be collected during periodic update")
	}
}

func TestResourceTrackerUsesUnifiedAvailableAndReportsUnknownGPUUsage(t *testing.T) {
	pool := resources.Pool{
		Kind: resources.Unified, ID: "unified", TotalMiB: new(int64(65536)), AvailableMiB: new(int64(19234)),
	}
	tracker := NewResourceTracker("apple", 0)
	tracker.cache = inventory.NewCacheWithProbe(time.Hour, func(context.Context, *resources.Snapshot) resources.Snapshot {
		return resources.Snapshot{Pools: []resources.Pool{pool}}
	})
	reading := tracker.CollectNow()
	require.Equal(t, "apple", reading.GPUType)
	require.Equal(t, 1, reading.GPUCount)
	require.Equal(t, new(int64(65536)), reading.GPUMemoryTotalMB)
	require.Equal(t, new(int64(19234)), reading.GPUMemoryFreeMB)
	require.Nil(t, reading.GPUMemoryUsedMB)
	require.Equal(t, new(int64(19234)), reading.RAMAvailableMB)
	*reading.GPUMemoryFreeMB = 0
	require.Equal(t, new(int64(19234)), tracker.GetMetrics().GPUMemoryFreeMB)
}

func TestResourceTrackerPreservesNullAndMeasuredZeroInGPUFields(t *testing.T) {
	snapshot := resources.Snapshot{Pools: []resources.Pool{
		{Kind: resources.GPU, Vendor: "nvidia", ID: "A", TotalMiB: new(int64(65536)), FreeMiB: new(int64(0))},
	}}
	reading := resourceMetrics("nvidia", snapshot)
	wire, err := json.Marshal(reading)
	require.NoError(t, err)
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(wire, &fields))
	require.JSONEq(t, "null", string(fields["gpu_memory_used_mb"]))
	require.JSONEq(t, "0", string(fields["gpu_memory_free_mb"]))
	require.JSONEq(t, "65536", string(fields["gpu_memory_total_mb"]))
}

func TestResourceTrackerReprobesWithinCacheTTL(t *testing.T) {
	for _, trigger := range []string{"tick", "collect now", "refresh request"} {
		t.Run(trigger, func(t *testing.T) {
			const ttl = time.Minute
			var calls atomic.Int64
			interval := time.Hour
			if trigger == "tick" {
				interval = 5 * time.Millisecond
			}
			tracker := NewResourceTracker("node", interval)
			tracker.cache = inventory.NewCacheWithProbe(ttl, func(context.Context, *resources.Snapshot) resources.Snapshot {
				free := calls.Add(1)
				return resources.Snapshot{Pools: []resources.Pool{
					{Kind: resources.GPU, Vendor: "nvidia", FreeMiB: &free},
				}}
			})
			ticked := make(chan struct{}, 1)
			tracker.SetTickedChForTest(ticked)
			before := tracker.cache.GetSnapshot(t.Context())
			switch trigger {
			case "tick":
				tracker.Start(t.Context())
				defer tracker.Stop()
				initial := *before.Pools[0].FreeMiB + 1
				select {
				case <-ticked:
				case <-time.After(2 * time.Second):
					t.Fatal("collector did not tick")
				}
				tracker.Stop()
				require.Greater(t, *tracker.GetMetrics().GPUMemoryFreeMB, initial)
			case "collect now":
				require.Equal(t, new(int64(2)), tracker.CollectNow().GPUMemoryFreeMB)
			case "refresh request":
				tracker.Start(t.Context())
				defer tracker.Stop()
				initial := calls.Load()
				tracker.RequestRefresh()
				require.Eventually(t, func() bool {
					return *tracker.GetMetrics().GPUMemoryFreeMB > initial
				}, time.Second, time.Millisecond)
			}
			probes := calls.Load()
			after := tracker.cache.GetSnapshot(t.Context())
			require.Equal(t, probes, calls.Load(), "ordinary read must reuse the observation within the TTL")
			require.Greater(t, *after.Pools[0].FreeMiB, *before.Pools[0].FreeMiB)
			require.Equal(t, probes, *after.Pools[0].FreeMiB)
			require.Equal(t, after.Pools[0].FreeMiB, tracker.GetMetrics().GPUMemoryFreeMB)
		})
	}
}

func TestResourceTrackerKeepsNewestProbeWithEqualTimestamps(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tracker := NewResourceTracker("node", 0)
		var probes int64
		tracker.cache = inventory.NewCacheWithProbe(time.Minute, func(context.Context, *resources.Snapshot) resources.Snapshot {
			probes++
			return resources.Snapshot{Pools: []resources.Pool{{Kind: resources.GPU, Vendor: "nvidia", FreeMiB: new(probes)}}}
		})
		older, olderSequence := tracker.cache.RefreshSnapshot(t.Context())
		newer, newerSequence := tracker.cache.RefreshSnapshot(t.Context())
		require.Equal(t, older.At, newer.At)

		tracker.publishMetrics(resourceMetrics(tracker.nodeName, newer), newerSequence)
		tracker.publishMetrics(resourceMetrics(tracker.nodeName, older), olderSequence)
		require.Equal(t, new(int64(2)), tracker.GetMetrics().GPUMemoryFreeMB)
	})
}

func TestResourceTrackerPreservesUnknownRAMAndUtilization(t *testing.T) {
	reading := resourceMetrics("node", resources.Snapshot{Pools: []resources.Pool{
		{Kind: resources.Host, AvailableMiB: new(int64(0))},
		{Kind: resources.GPU, Vendor: "nvidia"},
	}})
	wire, err := json.Marshal(reading)
	require.NoError(t, err)
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(wire, &fields))
	require.JSONEq(t, "null", string(fields["ram_total_mb"]))
	require.JSONEq(t, "0", string(fields["ram_available_mb"]))
	require.JSONEq(t, "null", string(fields["gpu_utilization"]))
	require.False(t, reading.CanFitModel(0, true))
	require.True(t, reading.CanFitModel(0, false))
	reading.RAMAvailableMB = nil
	require.False(t, reading.CanFitModel(0, false))
	wire, err = json.Marshal(reading)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(wire, &fields))
	require.JSONEq(t, "null", string(fields["ram_available_mb"]))
}
