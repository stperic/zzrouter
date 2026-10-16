package process

import (
	"maps"
	"testing"

	"github.com/stperic/zzrouter/pkg/discovery/gpu"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMemoryAssessmentMatchesAdmission(t *testing.T) {
	budget := schema.MemoryBudget{Kind: schema.MemoryFractionTotal, Parameter: "budget", DeviceCountParameter: "devices"}
	need := Need{WeightsMiB: 2048, KVMiB: 512, OverheadMiB: 2048}
	for _, tc := range []struct {
		name, visibility, count string
		devices                 []gpu.MemoryDevice
		required, available     int64
		status                  string
	}{
		{"rounded boundary", "GPU-A", "1", []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: 7117}}, 7117, 7117, "fit"},
		{"margin short by one", "GPU-A", "1", []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: 7116}}, 7117, 7116, "no_fit"},
		{"parallel boundary", "GPU-A,GPU-B", "2", []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: 6000}, {UUID: "GPU-B", TotalMiB: 20000, FreeMiB: 9370}}, 7322 + 2048, 9370, "fit"},
		{"selected device only", "GPU-B", "1", []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 100, FreeMiB: 0}, {UUID: "GPU-B", TotalMiB: 10000, FreeMiB: 7117}}, 7117, 7117, "fit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			params := map[string]string{"budget": "auto", "devices": tc.count}
			env := map[string]string{"CUDA_VISIBLE_DEVICES": tc.visibility}
			assessment, err := AdmitMemoryBudget(budget, need, params, env, tc.devices, 0, nil, nil, 0)
			require.Equal(t, tc.status, assessment.Status)
			assert.Equal(t, tc.required, assessment.RequiredMiB)
			assert.Equal(t, tc.available, assessment.CapacityMiB)
			if tc.status == "no_fit" {
				var fit *MemoryFitError
				require.ErrorAs(t, err, &fit)
				assert.Equal(t, assessment.RequiredMiB, fit.RequiredMiB)
				assert.Equal(t, assessment.CapacityMiB, fit.FreeMiB)
				assert.Equal(t, "auto", params["budget"])
			} else {
				require.NoError(t, err)
				assert.NotEqual(t, "auto", params["budget"])
			}
		})
	}
}

func TestMemoryAssessmentWeightLowerBound(t *testing.T) {
	for _, tc := range []struct {
		kind       string
		unified    bool
		fraction   string
		ram, reach int64
	}{
		{schema.MemoryFractionTotal, false, "0.5", 99999, 5000},
		{schema.MemoryFractionTotal, false, "auto", 99999, 10000},
		{schema.MemoryLayers, false, "", 20000, 30000},
		{schema.MemoryLayers, true, "", 10000, 10000},
		{schema.MemoryUnified, true, "", 10000, 10000},
	} {
		t.Run(tc.kind+tc.fraction+map[bool]string{true: " unified", false: " discrete"}[tc.unified], func(t *testing.T) {
			budget := schema.MemoryBudget{Kind: tc.kind, Parameter: "budget"}
			params := map[string]string{"budget": tc.fraction}
			devices := []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: 10000, Unified: tc.unified}}
			for _, weight := range []int64{tc.reach, tc.reach + 1} {
				assessment, err := AdmitMemoryBudget(budget, Need{WeightsMiB: weight, Incomplete: true}, maps.Clone(params), nil, devices, tc.ram, nil, nil, 0)
				if weight == tc.reach {
					require.NoError(t, err)
					assert.Equal(t, "unknown", assessment.Status)
				} else {
					var refusal *MemoryFitError
					require.ErrorAs(t, err, &refusal)
					assert.Equal(t, "no_fit", assessment.Status)
					assert.Equal(t, tc.reach, assessment.CapacityMiB)
					assert.Equal(t, weight, assessment.RequiredMiB)
				}
			}
		})
	}
}

func TestCompatibleNodesUnknownEvidence(t *testing.T) {
	devices := []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: 10000}}
	for _, tc := range []struct {
		name, kind, fraction string
		need                 Need
		devices              []gpu.MemoryDevice
	}{
		{"missing size", schema.MemoryFractionTotal, "auto", Need{}, devices},
		{"missing geometry", schema.MemoryFractionTotal, "auto", Need{WeightsMiB: 1000, Incomplete: true}, devices},
		{"numeric budget missing geometry", schema.MemoryFractionTotal, "0.5", Need{WeightsMiB: 1000, Incomplete: true}, devices},
		{"missing budget value", schema.MemoryFractionTotal, "", Need{}, devices},
		{"missing measurement", schema.MemoryFractionTotal, "auto", Need{}, nil},
		{"missing total", schema.MemoryFractionTotal, "auto", Need{}, []gpu.MemoryDevice{{UUID: "GPU-A"}}},
		{"external scheduler", schema.MemoryExternal, "", Need{}, nil},
		{"layers", schema.MemoryLayers, "", Need{WeightsMiB: 1000}, devices},
		{"unified", schema.MemoryUnified, "", Need{WeightsMiB: 1000}, devices},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assessment, _ := AdmitMemoryBudget(schema.MemoryBudget{Kind: tc.kind, Parameter: "budget"}, tc.need, map[string]string{"budget": tc.fraction}, nil, tc.devices, 20000, nil, nil, 0)
			assert.Equal(t, "unknown", assessment.Status)
			assert.NotEmpty(t, assessment.Reason)
		})
	}
}

func TestMemoryAssessmentWeightByteBoundary(t *testing.T) {
	budget := schema.MemoryBudget{Kind: schema.MemoryFractionTotal, Parameter: "budget"}
	for _, size := range []int64{512 * 1024, 512*1024 + 1} {
		need := Need{WeightsMiB: 1, WeightsBytes: size, Incomplete: true}
		assessment, err := AdmitMemoryBudget(budget, need, map[string]string{"budget": "0.5"}, nil, []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 1, FreeMiB: 1}}, 0, nil, nil, 0)
		if size == 512*1024 {
			require.NoError(t, err)
			assert.Equal(t, "unknown", assessment.Status)
		} else {
			var fit *MemoryFitError
			require.ErrorAs(t, err, &fit)
			assert.Equal(t, "no_fit", assessment.Status)
		}
	}
}

func TestWeightLowerBoundExplainsReachAndRoundsUp(t *testing.T) {
	for _, kind := range []string{schema.MemoryFractionTotal, schema.MemoryLayers, schema.MemoryUnified} {
		t.Run(kind, func(t *testing.T) {
			need := Need{WeightsMiB: 1024, WeightsBytes: 1024*1024*1024 + 1, Incomplete: true}
			assessment, err := AdmitMemoryBudget(schema.MemoryBudget{Kind: kind, Parameter: "budget"}, need, map[string]string{"budget": "0.5"}, nil, []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 1024, FreeMiB: 1024, Unified: kind != schema.MemoryFractionTotal}}, 1024, nil, nil, 0)
			var fit *MemoryFitError
			require.ErrorAs(t, err, &fit)
			assert.Equal(t, int64(1025), fit.RequiredMiB)
			assert.Greater(t, fit.RequiredMiB, fit.FreeMiB)
			assert.Contains(t, assessment.Reason, "model weights require 1.0009765625 GiB")
			assert.Contains(t, assessment.Reason, kind+" reach of ")
			assert.Contains(t, assessment.Reason, "GiB (budget)")
			assert.Contains(t, assessment.Reason, "smaller quantization or another node")
		})
	}
}

func TestMemoryAssessmentLayersReachAllVisibleDevices(t *testing.T) {
	devices := []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 5000, FreeMiB: 5000}, {UUID: "GPU-B", TotalMiB: 6000, FreeMiB: 6000}}
	for _, visible := range []string{"", "GPU-A,GPU-B", "GPU-B"} {
		env := map[string]string{}
		reach := int64(14000)
		if visible != "" {
			env["CUDA_VISIBLE_DEVICES"] = visible
		}
		if visible == "GPU-B" {
			reach = 9000
		}
		for _, weights := range []int64{reach, reach + 1} {
			assessment, err := AdmitMemoryBudget(schema.MemoryBudget{Kind: schema.MemoryLayers}, Need{WeightsMiB: weights, Incomplete: true}, nil, env, devices, 3000, nil, nil, 0)
			if weights == reach {
				require.NoError(t, err)
				assert.Equal(t, "unknown", assessment.Status)
			} else {
				var fit *MemoryFitError
				require.ErrorAs(t, err, &fit)
				assert.Equal(t, reach, assessment.CapacityMiB)
			}
		}
	}
}
