package prov_apps

import (
	"context"
	"runtime"
	"testing"

	"github.com/stperic/zzrouter/pkg/discovery/gpu"
	"github.com/stperic/zzrouter/pkg/discovery/hardware"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stretchr/testify/require"
)

func TestUnifiedCapacityUsesTotalsAndAdmissionUsesFreshAvailability(t *testing.T) {
	r := reservationRig(t, false)
	r.m.memorySystem = func(context.Context) *hardware.HardwareInfo {
		return &hardware.HardwareInfo{TotalRAM: 32 << 30, AvailableRAM: 31 << 30}
	}
	observations := 0
	r.m.memorySnapshot = func(context.Context) ([]gpu.MemoryDevice, error) {
		observations++
		device, err := gpu.UnifiedMemoryDevice(32<<30, 6<<30)
		return []gpu.MemoryDevice{device}, err
	}
	capacity, _, err := r.m.memoryDevices(t.Context(), schema.MemoryUnified, true)
	require.NoError(t, err)
	require.Equal(t, int64(32768), capacity[0].FreeMiB)
	require.Zero(t, observations)
	current, _, err := r.m.memoryDevices(t.Context(), schema.MemoryUnified, false)
	require.NoError(t, err)
	require.Equal(t, int64(6144), current[0].FreeMiB)
	require.Nil(t, current[0].UsedMiB)
	require.Equal(t, 1, observations)

	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		t.Setenv("CUDA_VISIBLE_DEVICES", "GPU-foreign")
		r.m.memoryInventory = func(context.Context) []gpu.CardMetrics { return nil }
		features, err := r.m.ResolveLaunchParameters(t.Context(), EnsureRequest{Provider: "generic"})
		require.NoError(t, err)
		require.Equal(t, map[string]int64{"unified": 9000}, features.Commitments)
		require.Equal(t, "GPU-foreign", features.Environment["CUDA_VISIBLE_DEVICES"], "unified preparation must not rewrite a CUDA selector")
		require.Equal(t, "23.2109375", features.Params["reserve-vram"])
		_, _, err = r.m.admitMemory(t.Context(), features, false, nil)
		var fit *process.MemoryFitError
		require.ErrorAs(t, err, &fit)
		require.Equal(t, int64(6144), fit.FreeMiB)
		require.Empty(t, r.m.instances.List(), "a refused reservation never starts a process")
	}
}

func TestUnifiedReclaimReleasesCommitmentWithoutClaimingFreedRAM(t *testing.T) {
	f := newReclaimFixture(t, true)
	f.mgr.providerSchema = memoryBudgetSchema(func(string) (*schema.MemoryBudget, error) {
		return &schema.MemoryBudget{Kind: schema.MemoryReserveMiB, Parameter: "budget"}, nil
	})
	f.features.declared = requireLaunchSchema(t, f.mgr, "vllm")
	f.features.Params = map[string]string{"budget": "6144"}
	f.older.SetResolved(instance.Resolved{Commitments: map[string]int64{"unified": 12288}})
	observations := 0
	f.mgr.memorySnapshot = func(context.Context) ([]gpu.MemoryDevice, error) {
		observations++
		device, err := gpu.UnifiedMemoryDevice(32<<30, 16<<30)
		return []gpu.MemoryDevice{device}, err
	}
	evicted, err := f.admit(t)
	require.NoError(t, err)
	require.Equal(t, []instance.EvictedRun{{RunID: f.older.ID, Model: f.older.Model, Provider: f.older.Provider, FreedMiB: 0}}, evicted)
	require.Equal(t, instance.StatusStopped, f.older.GetStatus())
	require.Equal(t, instance.StatusRunning, f.newer.GetStatus())
	require.Equal(t, 2, observations, "commitment-only reclaim needs initial and post-drain admission, never a physical-memory recovery poll")
}
