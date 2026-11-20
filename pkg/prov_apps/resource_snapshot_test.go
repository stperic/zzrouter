package prov_apps

import (
	"context"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/discovery/gpu"
	"github.com/stperic/zzrouter/pkg/discovery/resources"
	"github.com/stretchr/testify/require"
)

func poolSnapshot(pools ...resources.Pool) func(context.Context, bool) resources.Snapshot {
	return func(context.Context, bool) resources.Snapshot { return resources.Snapshot{Pools: pools} }
}

func deviceSnapshot(probe func(context.Context) ([]gpu.MemoryDevice, error), hostMiB ...int64) func(context.Context, bool) resources.Snapshot {
	return func(ctx context.Context, _ bool) resources.Snapshot {
		devices, err := probe(ctx)
		snapshot := resources.Snapshot{GPUError: err}
		for _, d := range devices {
			pool := resources.Pool{
				Kind: resources.GPU, ID: d.UUID, Index: new(d.Index), TotalMiB: new(d.TotalMiB),
				FreeMiB: new(d.FreeMiB), UsedMiB: d.UsedMiB, ProcessReason: d.ProcessReason,
			}
			if d.Unified {
				pool.Kind, pool.AvailableMiB = resources.Unified, pool.FreeMiB
			}
			for _, p := range d.Processes {
				pool.Holders = append(pool.Holders, resources.Holder{PID: p.PID, MemoryMiB: p.MemoryMiB})
			}
			snapshot.Pools = append(snapshot.Pools, pool)
		}
		if _, ok := snapshot.SystemPool(); !ok && len(hostMiB) > 0 {
			snapshot.Pools = append(snapshot.Pools, resources.Pool{Kind: resources.Host, TotalMiB: &hostMiB[0]})
		}
		return snapshot
	}
}

func nvidiaSnapshot(ctx context.Context, _ bool) resources.Snapshot {
	pools, err := resources.NVIDIAPools(ctx)
	return resources.Snapshot{Pools: pools, GPUError: err}
}

func TestAdmissionSnapshotIsFreshAndBounded(t *testing.T) {
	m := &ProviderAppManager{}
	calls := int64(0)
	m.memorySnapshot = func(ctx context.Context, fresh bool) resources.Snapshot {
		require.True(t, fresh)
		dl, ok := ctx.Deadline()
		require.True(t, ok)
		require.Positive(t, time.Until(dl))
		require.LessOrEqual(t, time.Until(dl), 10*time.Second)
		require.Greater(t, time.Until(dl), 9*time.Second)
		calls++
		return resources.Snapshot{Pools: []resources.Pool{{
			Kind: resources.GPU, ID: "GPU-A", Index: new(0), TotalMiB: new(int64(10000)), FreeMiB: new(calls),
		}}}
	}
	for i := int64(1); i <= 2; i++ {
		devices, _, err := m.memoryDevices(t.Context(), "", false)
		require.NoError(t, err)
		require.Equal(t, i, devices[0].FreeMiB)
	}
}
