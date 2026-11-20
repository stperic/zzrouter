package process

import (
	"testing"

	"github.com/stperic/zzrouter/pkg/discovery/gpu"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func absoluteKVBudget() schema.MemoryBudget {
	return schema.MemoryBudget{Kind: schema.MemoryFractionTotal, Parameter: "gpu-memory-utilization", KVBytesParameter: "kv-cache-memory-bytes"}
}

func TestAbsoluteKVLabStartupAndHold(t *testing.T) {
	budget := absoluteKVBudget()
	params := map[string]string{budget.Parameter: "0.2", budget.KVBytesParameter: "2147483648"}
	devices := []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 97887, FreeMiB: 19578}}
	commitments := map[string]int64{"other": 1024}
	need := Need{WeightsMiB: 952, KVMiB: 50000, ContextMode: "auto"}
	assessment, err := AdmitMemoryBudget(budget, need, params, nil, devices, 0, nil, commitments, 0)
	require.NoError(t, err)
	assert.Equal(t, "fit", assessment.Status)
	assert.Equal(t, int64(7895), assessment.RequiredMiB)
	assert.Equal(t, int64(19578), assessment.CapacityMiB)
	assert.Equal(t, map[string]int64{"other": 1024}, commitments)

	devices[0].FreeMiB--
	assessment, err = AdmitMemoryBudget(budget, need, params, nil, devices, 0, nil, commitments, 0)
	var fit *MemoryFitError
	require.ErrorAs(t, err, &fit)
	assert.Equal(t, "no_fit", assessment.Status)
	assert.Equal(t, budget.Parameter, fit.Parameter)
	assert.Equal(t, int64(19578), fit.RequiredMiB)
	assert.Equal(t, int64(19577), fit.FreeMiB)
	assert.Contains(t, fit.Error(), budget.Parameter)
}

func TestAbsoluteKVAllowsWeightsAboveFractionCeiling(t *testing.T) {
	budget := absoluteKVBudget()
	params := map[string]string{budget.Parameter: "0.1", budget.KVBytesParameter: "2147483648"}
	devices := []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 97887, FreeMiB: 27423}}
	need := autoNeed(t, denseAttentionConfig())
	need.WeightsMiB = 20480
	need.WeightsBytes = 20480 * 1024 * 1024
	assessment, err := AdmitMemoryBudget(budget, need, params, nil, devices, 0, nil, nil, 0)
	require.NoError(t, err)
	assert.Equal(t, "fit", assessment.Status)
	assert.Equal(t, int64(27423), assessment.RequiredMiB)
	assert.Zero(t, assessment.AffordableContextTokens)

	devices[0].FreeMiB--
	assessment, err = AdmitMemoryBudget(budget, need, params, nil, devices, 0, nil, nil, 0)
	var fit *MemoryFitError
	require.ErrorAs(t, err, &fit)
	assert.Equal(t, "no_fit", assessment.Status)
	assert.Equal(t, budget.KVBytesParameter, fit.Parameter)
	assert.Equal(t, int64(27423), fit.RequiredMiB)
	assert.Equal(t, int64(27422), fit.FreeMiB)
	assert.Contains(t, fit.Error(), budget.KVBytesParameter)
}

func TestAbsoluteKVUsesPerDeviceHoldAndAutoStartup(t *testing.T) {
	budget := absoluteKVBudget()
	budget.DeviceCountParameter = "tensor-parallel-size"
	params := map[string]string{budget.Parameter: "auto", budget.KVBytesParameter: "1048577", budget.DeviceCountParameter: "2"}
	env := map[string]string{"CUDA_VISIBLE_DEVICES": "GPU-A,GPU-B"}
	devices := []gpu.MemoryDevice{
		{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: 3001},
		{UUID: "GPU-B", TotalMiB: 60000, FreeMiB: 3502},
	}
	need := Need{WeightsMiB: 999, KVMiB: 100000, ContextMode: "numeric"}
	assessment, err := AdmitMemoryBudget(budget, need, params, env, devices, 0, nil, nil, 0)
	require.NoError(t, err)
	assert.Equal(t, "0.0083", params[budget.Parameter])
	assert.Equal(t, int64(3502), assessment.RequiredMiB)
	assert.Equal(t, int64(3502), assessment.CapacityMiB)

	devices[1].FreeMiB--
	_, err = AdmitMemoryBudget(budget, need, params, env, devices, 0, nil, nil, 0)
	var fit *MemoryFitError
	require.ErrorAs(t, err, &fit)
	assert.Equal(t, "GPU-B", fit.Device)
	assert.Equal(t, int64(3502), fit.RequiredMiB)
	assert.Equal(t, budget.KVBytesParameter, fit.Parameter)
}

func TestAbsoluteKVUnknownWeightsOnlyChecksStartup(t *testing.T) {
	budget := absoluteKVBudget()
	params := map[string]string{budget.Parameter: "0.2", budget.KVBytesParameter: "10737418240"}
	devices := []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: 2000}}
	assessment, err := AdmitMemoryBudget(budget, Need{}, params, nil, devices, 0, nil, nil, 0)
	require.NoError(t, err)
	assert.Equal(t, "unknown", assessment.Status)
	assert.Equal(t, WeightsUnavailable, assessment.UnknownCause)
	assert.Zero(t, assessment.RequiredMiB)
	assert.Zero(t, assessment.AffordableContextTokens)

	devices[0].FreeMiB--
	assessment, err = AdmitMemoryBudget(budget, Need{}, params, nil, devices, 0, nil, nil, 0)
	var fit *MemoryFitError
	require.ErrorAs(t, err, &fit)
	assert.Equal(t, budget.Parameter, fit.Parameter)
	assert.Equal(t, "no_fit", assessment.Status)
}

func TestAbsoluteKVInvalidBytesStayBudgetErrors(t *testing.T) {
	budget := absoluteKVBudget()
	devices := []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: 0}}
	for _, value := range []string{"", "0", "-1", "9223372036854775808"} {
		t.Run(value, func(t *testing.T) {
			params := map[string]string{budget.Parameter: "0.2", budget.KVBytesParameter: value}
			_, err := AdmitMemoryBudget(budget, Need{}, params, nil, devices, 0, nil, nil, 0)
			require.ErrorIs(t, err, ErrMemoryBudgetInvalid)
			assert.ErrorContains(t, err, budget.KVBytesParameter)
		})
	}
}
