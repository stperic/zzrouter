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

func TestReservationUnavailableAttributionUsesDeviceLowerBound(t *testing.T) {
	for _, tc := range []struct {
		name       string
		reason     string
		free, want int64
	}{
		{"attributed", "", 22000, 18000},
		{"unavailable below commitment", "unavailable", 22000, 20000},
		{"unavailable above commitment", "unavailable", 17000, 17000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			owners := []RunRef{{ID: "tenant", PID: 7, Commitments: map[string]int64{"GPU-A": 4000}}}
			device := gpu.MemoryDevice{UUID: "GPU-A", TotalMiB: 24000, FreeMiB: tc.free, ProcessReason: tc.reason}
			fit := memoryFitError("budget", device, 20000, owners)
			require.Equal(t, tc.want, fit.FreeMiB)
		})
	}
}

func TestUnknownBudgetOnlyChecksSelectedDevices(t *testing.T) {
	devices := []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 24000, FreeMiB: 24000}, {UUID: "GPU-B", TotalMiB: 24000, FreeMiB: 24000}}
	owners := []RunRef{{ID: "tenant", Commitments: map[string]int64{"GPU-A": 4000}}}
	budget := schema.MemoryBudget{Kind: "layers", Parameter: "gpu-layers", DeviceCountParameter: "devices"}
	require.NoError(t, AdmitMemoryBudget(budget, Need{}, nil, map[string]string{"CUDA_VISIBLE_DEVICES": "GPU-B"}, devices, owners, nil))
	var fit *MemoryFitError
	require.ErrorAs(t, AdmitMemoryBudget(budget, Need{}, nil, map[string]string{"CUDA_VISIBLE_DEVICES": "GPU-A"}, devices, owners, nil), &fit)
	require.Equal(t, "gpu-layers", fit.Parameter)
	require.Equal(t, "GPU-A", fit.Device)
	require.ErrorAs(t, AdmitMemoryBudget(budget, Need{}, map[string]string{"devices": "2"}, map[string]string{"CUDA_VISIBLE_DEVICES": "GPU-B,GPU-A"}, devices, owners, nil), &fit)
}
