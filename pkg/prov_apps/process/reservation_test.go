package process

import (
	"testing"

	"github.com/stperic/zzrouter/pkg/discovery/gpu"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stretchr/testify/require"
)

func TestReservationBoundsComplementAndAttribution(t *testing.T) {
	budget := schema.MemoryBudget{Kind: "reserve_mib", Parameter: "gpu-reserve-mib", ComplementParameter: "reserve-vram"}
	device := gpu.MemoryDevice{UUID: "GPU-A", TotalMiB: 24576, FreeMiB: 22528}
	for _, value := range []string{"0", "-1", "24577", "NaN", "1.5", "auto"} {
		_, err := PrepareMemoryBudget(budget, map[string]string{budget.Parameter: value}, map[string]string{}, []gpu.MemoryDevice{device})
		require.ErrorIs(t, err, ErrMemoryBudgetInvalid)
	}
	params, env := map[string]string{budget.Parameter: "9000"}, map[string]string{}
	reserved, err := PrepareMemoryBudget(budget, params, env, []gpu.MemoryDevice{device})
	require.NoError(t, err)
	require.Equal(t, map[string]int64{"GPU-A": 9000}, reserved)
	require.Equal(t, "15", params[budget.ComplementParameter])
	require.Equal(t, "GPU-A", env["CUDA_VISIBLE_DEVICES"])
	owners := []RunRef{{ID: "studio", Provider: "studio", PID: 7, Commitments: reserved}}
	device.Processes = []gpu.MemoryProcess{{PID: 7, MemoryMiB: new(int64(2048))}}
	fit := memoryFitError("fraction", device, 18000, owners)
	require.Equal(t, int64(15576), fit.FreeMiB)
	require.Len(t, fit.Holders, 2)
	require.Equal(t, int64(6952), fit.Holders[1].UnusedCommitmentMiB)
	require.Contains(t, fit.Error(), "provider studio")
	var refused *MemoryFitError
	require.ErrorAs(t, AdmitMemoryBudget(schema.MemoryBudget{Kind: "fraction_total", Parameter: "fraction"}, Need{}, map[string]string{"fraction": "0.75"}, nil, []gpu.MemoryDevice{device}, owners, nil), &refused)
	require.Equal(t, fit.FreeMiB, refused.FreeMiB)
	require.ErrorAs(t, AdmitMemoryBudget(schema.MemoryBudget{Kind: "layers"}, Need{}, nil, nil, []gpu.MemoryDevice{device}, owners, nil), &refused)
	owners[0].PID = 0
	device.Processes = nil
	require.Equal(t, int64(13528), memoryFitError("fraction", device, 18000, owners).FreeMiB)
}
