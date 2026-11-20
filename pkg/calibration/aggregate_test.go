package calibration

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNearestRankPercentiles(t *testing.T) {
	for _, tt := range []struct {
		values []float64
		want   *Percentiles
	}{
		{nil, nil},
		{[]float64{3}, &Percentiles{3, 3, 3, 3, 3, 1}},
		{[]float64{8, 2}, &Percentiles{2, 8, 8, 2, 8, 2}},
		{[]float64{9, 0, 3}, &Percentiles{3, 9, 9, 0, 9, 3}},
		{[]float64{10, 7, 3, 6, 4, 5, 9, 2, 8, 1}, &Percentiles{5, 9, 10, 1, 10, 10}},
	} {
		require.Equal(t, tt.want, percentiles(tt.values))
	}
}

func TestLatencyAndThroughputWindows(t *testing.T) {
	const second = time.Second
	a := measured(second, 2*second, 3*second, 3500*time.Millisecond, 4*second, 10, 3)
	b := measured(2*second, 3*second, 4*second, 4*second, 5*second, 14, 1)
	run := testStepRun(Chat, 1, a, b)
	run.Run.OutputForced = false
	step := summarizeStep(0, 8*second, []stepRun{run}, stepObservations{}, nil, false, nil)
	result := step.Runs[0]
	require.Equal(t, &Percentiles{1000, 1000, 1000, 1000, 1000, 2}, result.SlotWaitMS)
	require.Equal(t, &Percentiles{1000, 1000, 1000, 1000, 1000, 2}, result.TimeToFirstTokenMS)
	require.Equal(t, &Percentiles{500, 500, 500, 500, 500, 1}, result.InterTokenLatencyMS)
	require.Equal(t, RunRates{new(6.0), new(1.0), new(0.5)}, result.Throughput)
	require.Equal(t, StepRates{new(0.5)}, step.Throughput)
	require.Equal(t, 1, *result.ConcurrentStreamsPeak, "response tails are not content streams")
	require.Equal(t, &TokenRange{10, 14}, result.InputTokens)
	require.Equal(t, &TokenRange{1, 3}, result.OutputTokens)

	result, _ = summarizeRun(testStepRun(Chat, 1, b), stepObservations{}, false, false)
	require.Nil(t, result.InterTokenLatencyMS)
	require.Equal(t, 1, *result.ConcurrentStreamsPeak)
}

func TestConcurrentStreamsPeak(t *testing.T) {
	a := measured(0, 0, 1, 4, 8, 10, 2)
	b := measured(0, 0, 2, 3, 8, 10, 2)
	c := measured(0, 0, 3, 3, 8, 10, 1)
	run := testStepRun(Chat, 3, a, b, c)
	run.Run.OutputForced = false
	result, _ := summarizeRun(run, stepObservations{}, false, false)
	require.Equal(t, 3, *result.ConcurrentStreamsPeak)
	require.Equal(t, verified, result.Outcome)
}

func TestNullUsageWithKnownTiming(t *testing.T) {
	a := measured(0, 0, time.Second, time.Second, 2*time.Second, 10, 2)
	a.InputTokens, a.OutputTokens = nil, nil
	b := measured(0, 0, time.Second, time.Second, 2*time.Second, 10, 2)
	run := testStepRun(Chat, 2, a, b)
	step := summarizeStep(0, 2*time.Second, []stepRun{run}, stepObservations{}, nil, false, nil)
	result := step.Runs[0]
	requireVerdict(t, "inconclusive", "usage_unavailable", result.Outcome, result.Reason)
	require.Equal(t, &TokenRange{10, 10}, result.InputTokens)
	require.Equal(t, 2, result.TimeToFirstTokenMS.Count)
	require.Equal(t, 1, result.InterTokenLatencyMS.Count)
	require.Nil(t, result.Throughput.InputTokensPerSecond)
	require.Nil(t, result.Throughput.OutputTokensPerSecond)
	require.Nil(t, step.Throughput.OutputTokensPerSecond)
}

func TestPoolAndRunObservations(t *testing.T) {
	targets := map[string]Target{"a": {}, "resident": {}}
	listed := Holder{PID: 13, MemoryMiB: new(int64(30)), RunID: new("a")}
	resident := Holder{PID: 14, MemoryMiB: new(int64(5)), RunID: new("resident")}
	other := Holder{PID: 15, MemoryMiB: new(int64(20))}
	snapshots := []Snapshot{
		{
			Pools: []PoolSample{
				{ID: "gpu", Kind: "gpu", UsedMiB: new(int64(50)), FreeMiB: new(int64(70)), UtilizationPct: new(100.0), PowerW: new(200.0), Holders: []Holder{listed, resident}},
				{ID: "host", Kind: "host", AvailableMiB: new(int64(200)), SwapUsedMiB: new(int64(0)), CPUPct: new(2.0), HostMemoryMiB: map[string]*int64{"a": new(int64(50))}},
				{ID: "unified", Kind: "unified", AvailableMiB: new(int64(400))},
			},
			Runs: map[string]RunSample{"a": {Running: new(2), Waiting: new(0), KVCacheUsage: new(0.5), CPUPct: new(92.0)}},
		},
		{
			Pools: []PoolSample{
				{ID: "gpu", Kind: "gpu", UsedMiB: new(int64(60)), FreeMiB: new(int64(60)), UtilizationPct: new(0.0), Holders: []Holder{listed, listed, resident, other}},
				{ID: "host", Kind: "host", AvailableMiB: new(int64(180)), SwapUsedMiB: new(int64(0)), CPUPct: new(4.0), HostMemoryMiB: map[string]*int64{"a": new(int64(60))}},
				{ID: "unified", Kind: "unified", AvailableMiB: new(int64(350)), HostMemoryMiB: map[string]*int64{"a": new(int64(30))}},
			},
			Runs: map[string]RunSample{"a": {Running: new(1), KVCacheUsage: new(0.9), CPUPct: new(110.0)}},
		},
	}
	step := summarizeStep(0, time.Second, []stepRun{testStepRun(Chat, 1, measured(0, 0, 1, 2, 3, 10, 2))}, stepObservations{snapshots: snapshots}, map[string]RunSample{}, false, targets)
	require.Len(t, step.Resources, 3)
	gpu, host, unified := step.Resources[0], step.Resources[1], step.Resources[2]
	require.Equal(t, &GPUStats{new(int64(60)), new(int64(60)), &MeanPeak{50, 100}, &MeanPeak{200, 200}, new(int64(20))}, gpu.GPUStats)
	require.Equal(t, &SystemStats{new(int64(180)), new(int64(0)), &MeanPeak{3, 4}}, host.SystemStats)
	require.Equal(t, &SystemStats{MinimumAvailableMiB: new(int64(350))}, unified.SystemStats)
	run := step.Runs[0]
	require.Equal(t, map[string]*int64{"gpu": new(int64(60)), "host": new(int64(60)), "unified": nil}, run.MemoryPeakMiB)
	require.Equal(t, &MeanPeak{101, 110}, run.CPUPct)
	require.Equal(t, EnginePeak{new(2), new(0), new(0.9)}, run.Engine)
	require.Equal(t, new("verified"), DescribePools(snapshots[0].Pools, targets)[0].Ownership)
	require.Equal(t, new("shared"), DescribePools(snapshots[1].Pools, targets)[0].Ownership)
	for _, holders := range [][]Holder{nil, {{PID: 16, RunID: new("a")}}} {
		pool := PoolSample{ID: "gpu", Kind: "gpu", Holders: holders}
		require.Equal(t, new("unknown"), DescribePools([]PoolSample{pool}, targets)[0].Ownership)
	}
	empty := PoolSample{ID: "gpu", Kind: "gpu", Holders: []Holder{}}
	require.Equal(t, new("verified"), DescribePools([]PoolSample{empty}, targets)[0].Ownership)
}

func TestSharedDaemonGPUHolderContributesOnce(t *testing.T) {
	targets := map[string]Target{"a": {}, "b": {}}
	pool := PoolSample{Kind: "gpu", Holders: []Holder{{PID: 9, MemoryMiB: new(int64(80)), ListedRuns: []string{"a", "b"}}}}
	require.Equal(t, "verified", gpuOwnership(pool, targets))
	for id := range targets {
		require.Equal(t, new(int64(80)), gpuMemory(pool, func(owner string) bool { return owner == id }))
	}
	other := func(id string) bool { _, ok := targets[id]; return !ok }
	require.Equal(t, new(int64(0)), gpuMemory(pool, other))
	pool.Holders[0].ListedRuns = nil
	require.Equal(t, "shared", gpuOwnership(pool, targets))
	require.Equal(t, new(int64(80)), gpuMemory(pool, other))
}
