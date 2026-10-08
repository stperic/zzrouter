package process

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/discovery/gpu"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
)

func TestResolveAutoMemoryUsesLaunchingDeviceHeadroom(t *testing.T) {
	budget := schema.MemoryBudget{Kind: "fraction_total", Parameter: "budget", DeviceCountParameter: "count"}
	for _, tc := range []struct {
		name        string
		devices     []gpu.MemoryDevice
		visibility  map[string]string
		count, want string
		margin      int64
		fail        bool
	}{
		{name: "one", devices: []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: 6000}}, want: "0.3952"},
		{name: "fresh measurement", devices: []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: 4000}}, want: "0.1952"},
		{name: "round down", devices: []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 12345, FreeMiB: 5678}}, want: "0.2940"},
		{name: "least selected", devices: []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: 6000}, {UUID: "GPU-B", TotalMiB: 20000, FreeMiB: 8000}}, visibility: map[string]string{"CUDA_VISIBLE_DEVICES": "GPU-A,GPU-B"}, count: "2", want: "0.2975"},
		{name: "captured worker-1 OOM", devices: []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 97887, FreeMiB: 67799}}, want: "0.6426"},
		{name: "configured margin", devices: []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: 6000}}, margin: 1000, want: "0.5000"},
		{name: "negative margin", margin: -1, fail: true},
		{name: "configured reserve exceeds free", devices: []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: 6000}}, margin: 6000, fail: true},
		{name: "no data", fail: true},
		{name: "ambiguous", devices: []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: 6000}, {UUID: "GPU-B", TotalMiB: 10000, FreeMiB: 6000}}, fail: true},
		{name: "no headroom", devices: []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: 2048}}, fail: true},
		{name: "invalid total", devices: []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 0, FreeMiB: 6000}}, fail: true},
		{name: "bad count", devices: []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: 6000}}, count: "NaN", fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			params := map[string]string{"budget": "auto"}
			if tc.count != "" {
				params["count"] = tc.count
			}
			declared := budget
			declared.AutoSafetyMarginMiB = tc.margin
			err := ResolveAutoMemory(declared, params, tc.visibility, tc.devices)
			if tc.fail {
				require.Error(t, err)
				assert.Equal(t, "auto", params["budget"])
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, params["budget"])
			require.NoError(t, CheckMemoryBudget(budget, params, tc.visibility, tc.devices, nil))
		})
	}
	numeric := map[string]string{"budget": "0.9"}
	require.NoError(t, ResolveAutoMemory(budget, numeric, nil, nil))
	assert.Equal(t, "0.9", numeric["budget"])
}
