package process

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/discovery/gpu"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
)

func TestAutoMemoryUsesLaunchingDeviceHeadroom(t *testing.T) {
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
		{name: "room rounds to zero", devices: []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 100000, FreeMiB: 1005}}, margin: 1000, fail: true},
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
			err := admitBudget(declared, Need{}, params, tc.visibility, tc.devices, nil, nil)
			if tc.fail {
				require.Error(t, err)
				assert.Equal(t, "auto", params["budget"])
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, params["budget"])
		})
	}
	numeric := map[string]string{"budget": "0.5"}
	require.NoError(t, admitBudget(budget, Need{}, numeric, nil, []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: 6000}}, nil, nil))
	assert.Equal(t, "0.5", numeric["budget"], "a numeric budget is checked, never rewritten")
}

func TestAutoMemoryBesideOutsideHolder(t *testing.T) {
	budget := schema.MemoryBudget{Kind: "fraction_total", Parameter: "gpu-memory-utilization"}
	held := int64(25 * 1024)
	devices := []gpu.MemoryDevice{{UUID: "GPU-localai", TotalMiB: 97249, FreeMiB: 71649, Processes: []gpu.MemoryProcess{{PID: 42, MemoryMiB: &held}}}}
	parameters := map[string]string{budget.Parameter: "auto"}
	require.NoError(t, admitBudget(budget, Need{}, parameters, nil, devices, nil, nil))
	assert.Equal(t, "0.6867", parameters[budget.Parameter])
	for _, fraction := range []string{"0.95", "0.99"} {
		parameters[budget.Parameter] = fraction
		var refusal *MemoryFitError
		require.ErrorAs(t, admitBudget(budget, Need{}, parameters, nil, devices, nil, nil), &refusal)
		assert.Equal(t, int64(71649), refusal.FreeMiB)
		require.Len(t, refusal.Holders, 1)
		assert.Equal(t, "other_or_untracked", refusal.Holders[0].Ownership)
		assert.Contains(t, refusal.Error(), "a process zzRouter does not manage (pid 42, 25600 MiB)")
	}
}

// A GPU a running model already fills is a memory refusal naming the run, not an observation failure.
func TestAutoMemoryWithoutHeadroomNamesTheHolder(t *testing.T) {
	budget := schema.MemoryBudget{Kind: "fraction_total", Parameter: "gpu-memory-utilization"}
	held := int64(91 * 1024)
	devices := []gpu.MemoryDevice{{UUID: "GPU-localai", TotalMiB: 97887, FreeMiB: 4000, Processes: []gpu.MemoryProcess{{PID: 7, MemoryMiB: &held}}}}
	parameters := map[string]string{budget.Parameter: "auto"}
	var refusal *MemoryFitError
	require.ErrorAs(t, admitBudget(budget, Need{}, parameters, nil, devices, []RunRef{{PID: 7, ID: "run-qwen", Model: "Qwen/Qwen3-8B"}}, nil), &refusal)
	assert.Equal(t, "auto", parameters[budget.Parameter])
	assert.Equal(t, int64(4000), refusal.FreeMiB)
	assert.Contains(t, refusal.Error(), "zzRouter run run-qwen serving Qwen/Qwen3-8B (pid 7")
	var cause *httperr.EngineFailure
	require.ErrorAs(t, refusal, &cause)
	assert.Equal(t, httperr.FailureMemory, cause.Class)
	assert.NotErrorIs(t, refusal, ErrMemoryObservation)
}

func TestAutoMemoryRefusalReportsQuantizedThreshold(t *testing.T) {
	budget := schema.MemoryBudget{Kind: schema.MemoryFractionTotal, Parameter: "budget", AutoSafetyMarginMiB: 1000}
	for _, tc := range []struct {
		name            string
		total, required int64
	}{
		{"partial step", 10001, 1002},
		{"two MiB step", 20000, 1002},
		{"localai", 97887, 1010},
		{"ten MiB step", 100000, 1010},
	} {
		t.Run(tc.name, func(t *testing.T) {
			device := gpu.MemoryDevice{UUID: "GPU-A", TotalMiB: tc.total, FreeMiB: tc.required - 1}
			params := map[string]string{"budget": "auto"}
			var fit *MemoryFitError
			require.ErrorAs(t, admitBudget(budget, Need{}, params, nil, []gpu.MemoryDevice{device}, nil, nil), &fit)
			assert.Equal(t, tc.required, fit.RequiredMiB)
			assert.Equal(t, "auto", params["budget"])
			device.FreeMiB = fit.RequiredMiB
			require.NoError(t, admitBudget(budget, Need{}, params, nil, []gpu.MemoryDevice{device}, nil, nil))
			assert.Equal(t, "0.0001", params["budget"])
		})
	}
}
