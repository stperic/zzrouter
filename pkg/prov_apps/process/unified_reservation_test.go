package process

import (
	"maps"
	"testing"

	"github.com/stperic/zzrouter/pkg/discovery/gpu"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stretchr/testify/require"
)

func TestUnifiedReservationDoesNotPinCUDAVisibility(t *testing.T) {
	device, err := gpu.UnifiedMemoryDevice(16<<30, 12<<30)
	require.NoError(t, err)
	for _, environment := range []map[string]string{{}, {"CUDA_VISIBLE_DEVICES": "GPU-other,1"}, {"CUDA_VISIBLE_DEVICES": ""}} {
		before := maps.Clone(environment)
		params := map[string]string{"gpu-reserve-mib": "8192"}
		reserved, err := PrepareMemoryBudget(schema.MemoryBudget{Kind: schema.MemoryReserveMiB, Parameter: "gpu-reserve-mib", ComplementParameter: "reserve-vram"}, params, environment, []gpu.MemoryDevice{device})
		require.NoError(t, err)
		require.Equal(t, map[string]int64{"unified": 8192}, reserved)
		require.Equal(t, "8", params["reserve-vram"])
		require.Equal(t, before, environment, "Apple's pool must not gain a CUDA selector")
	}
}

func TestUnifiedUnknownUsagePreservesEveryCommitment(t *testing.T) {
	device, err := gpu.UnifiedMemoryDevice(32<<30, 20<<30)
	require.NoError(t, err)
	owners := []RunRef{
		{ID: "first", Commitments: map[string]int64{"unified": 6144}},
		{ID: "second", Commitments: map[string]int64{"unified": 7168}},
	}
	for _, kind := range []string{schema.MemoryReserveMiB, schema.MemoryUnified, schema.MemoryLayers} {
		t.Run(kind, func(t *testing.T) {
			_, err := AdmitMemoryBudget(schema.MemoryBudget{Kind: kind, Parameter: "budget"}, Need{}, map[string]string{"budget": "8192"}, nil, []gpu.MemoryDevice{device}, 32768, owners, nil, 0.75)
			var fit *MemoryFitError
			require.ErrorAs(t, err, &fit)
			require.Equal(t, "unified", fit.Device)
			require.Equal(t, int64(7168), fit.FreeMiB)
			require.True(t, fit.CommitmentOnly)
			require.Len(t, fit.Holders, 2)
			require.Nil(t, fit.Holders[0].MemoryMiB)
			require.Nil(t, fit.Holders[1].MemoryMiB)
			require.Equal(t, int64(6144), fit.Holders[0].UnusedCommitmentMiB)
			require.Equal(t, int64(7168), fit.Holders[1].UnusedCommitmentMiB)
			require.Contains(t, fit.Error(), "usage unknown")
		})
	}
	device.FreeMiB = 4096
	_, err = AdmitMemoryBudget(schema.MemoryBudget{Kind: schema.MemoryReserveMiB, Parameter: "budget"}, Need{}, map[string]string{"budget": "8192"}, nil, []gpu.MemoryDevice{device}, 32768, owners, nil, 0)
	var fit *MemoryFitError
	require.ErrorAs(t, err, &fit)
	require.False(t, fit.CommitmentOnly, "a physical shortage is not repaired by removing an unknown reservation")
	after, release := MemoryAfterStop(device, owners, "first")
	require.Nil(t, release)
	require.Equal(t, device.FreeMiB, after.FreeMiB)
}
