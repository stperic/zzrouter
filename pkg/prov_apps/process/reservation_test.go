package process

import (
	"encoding/json"
	"strconv"
	"testing"

	"github.com/stperic/zzrouter/pkg/discovery/gpu"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stretchr/testify/require"
)

func TestReservationComplementPreservesExactBudget(t *testing.T) {
	budget := schema.MemoryBudget{Kind: "reserve_mib", Parameter: "gpu-reserve-mib", ComplementParameter: "reserve-vram"}
	device := gpu.MemoryDevice{UUID: "GPU-A", TotalMiB: 97887, FreeMiB: 97887}
	params := map[string]string{budget.Parameter: "16384"}
	reserved, err := PrepareMemoryBudget(budget, params, nil, []gpu.MemoryDevice{device})
	require.NoError(t, err)
	require.Equal(t, "79.5927734375", params[budget.ComplementParameter])
	complement, err := strconv.ParseFloat(params[budget.ComplementParameter], 64)
	require.NoError(t, err)
	require.Equal(t, float64(reserved[device.UUID]), float64(device.TotalMiB)-complement*float64(MiBPerGiB))
}

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
	require.Equal(t, "15.2109375", params[budget.ComplementParameter])
	require.Equal(t, "GPU-A", env["CUDA_VISIBLE_DEVICES"])
	owners := []RunRef{{ID: "studio", Provider: "studio", PID: 7, Commitments: reserved}}
	device.Processes = []gpu.MemoryProcess{{PID: 7, MemoryMiB: new(int64(2048))}}
	fit := memoryFitError("fraction", device, 18000, owners)
	require.Equal(t, int64(15576), fit.FreeMiB)
	require.Len(t, fit.Holders, 2)
	require.Equal(t, int64(6952), fit.Holders[1].UnusedCommitmentMiB)
	encoded, err := json.Marshal(fit)
	require.NoError(t, err)
	var wire struct {
		Holders []map[string]any `json:"holders"`
	}
	require.NoError(t, json.Unmarshal(encoded, &wire))
	require.Equal(t, float64(7), wire.Holders[0]["pid"], "the observed process keeps its pid")
	require.NotContains(t, wire.Holders[1], "pid", "an unused commitment has no process")
	require.Equal(t, float64(6952), wire.Holders[1]["memory_mib"])
	require.Equal(t, float64(6952), wire.Holders[1]["unused_commitment_mib"])
	require.Contains(t, fit.Error(), "zzRouter run studio provider studio (pid 7, 2048 MiB)")
	require.Contains(t, fit.Error(), "zzRouter run studio provider studio (6952 MiB unused commitment)")
	var refused *MemoryFitError
	require.ErrorAs(t, admitBudget(schema.MemoryBudget{Kind: "fraction_total", Parameter: "fraction"}, Need{}, map[string]string{"fraction": "0.75"}, nil, []gpu.MemoryDevice{device}, owners, nil), &refused)
	require.Equal(t, fit.FreeMiB, refused.FreeMiB)
	require.ErrorAs(t, admitBudget(schema.MemoryBudget{Kind: "layers"}, Need{}, nil, nil, []gpu.MemoryDevice{device}, owners, nil), &refused)
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
	require.NoError(t, admitBudget(budget, Need{}, nil, map[string]string{"CUDA_VISIBLE_DEVICES": "GPU-B"}, devices, owners, nil))
	var fit *MemoryFitError
	require.ErrorAs(t, admitBudget(budget, Need{}, nil, map[string]string{"CUDA_VISIBLE_DEVICES": "GPU-A"}, devices, owners, nil), &fit)
	require.Equal(t, "gpu-layers", fit.Parameter)
	require.Equal(t, "GPU-A", fit.Device)
	require.ErrorAs(t, admitBudget(budget, Need{}, map[string]string{"devices": "2"}, map[string]string{"CUDA_VISIBLE_DEVICES": "GPU-B,GPU-A"}, devices, owners, nil), &fit)
}
