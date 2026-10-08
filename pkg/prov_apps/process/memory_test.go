package process

import (
	"errors"
	"testing"

	"github.com/stperic/zzrouter/pkg/discovery/gpu"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMemoryBudgetUsesPerDeviceFreeMemory(t *testing.T) {
	budget := schema.MemoryBudget{Kind: "fraction_total", Parameter: "budget", DeviceCountParameter: "devices"}
	amount := int64(3000)
	devices := []gpu.MemoryDevice{{UUID: "GPU-A", Index: 0, TotalMiB: 10000, FreeMiB: 6000, Processes: []gpu.MemoryProcess{{PID: 42, MemoryMiB: &amount}, {PID: 43}}}}
	require.NoError(t, CheckMemoryBudget(budget, map[string]string{"budget": "0.55"}, nil, devices, nil))
	err := CheckMemoryBudget(budget, map[string]string{"budget": "0.95"}, nil, devices, map[int]string{42: "run-owned"})
	var failure *MemoryFitError
	require.ErrorAs(t, err, &failure)
	assert.Equal(t, int64(9500), failure.RequiredMiB)
	assert.Equal(t, int64(6000), failure.FreeMiB)
	require.Len(t, failure.Holders, 2)
	assert.Equal(t, "zzrouter", failure.Holders[0].Ownership)
	assert.Equal(t, "run-owned", failure.Holders[0].Run)
	assert.Equal(t, "other_or_untracked", failure.Holders[1].Ownership)
	assert.Nil(t, failure.Holders[1].MemoryMiB)
	// An aggregate free total must not hide one undersized shard device.
	devices = append(devices, gpu.MemoryDevice{UUID: "GPU-B", Index: 1, TotalMiB: 10000, FreeMiB: 5000})
	err = CheckMemoryBudget(budget, map[string]string{"budget": "0.55", "devices": "2"}, map[string]string{"CUDA_VISIBLE_DEVICES": "GPU-A,GPU-B"}, devices, nil)
	require.ErrorAs(t, err, &failure)
	assert.Equal(t, "GPU-B", failure.Device)
	assert.Equal(t, int64(5500), failure.RequiredMiB)
	assert.Equal(t, int64(5000), failure.FreeMiB)
	for _, selector := range []string{"", "0", "GPU-A,GPU-A", "MIG-unknown"} {
		err := CheckMemoryBudget(budget, map[string]string{"budget": "0.55", "devices": "2"}, map[string]string{"CUDA_VISIBLE_DEVICES": selector}, devices, nil)
		assert.ErrorIs(t, err, ErrMemoryObservation)
	}
	for _, fraction := range []string{"NaN", "Inf", "0", "1.5", "unknown"} {
		assert.ErrorIs(t, CheckMemoryBudget(budget, map[string]string{"budget": fraction}, nil, devices, nil), ErrMemoryBudgetInvalid)
	}
	assert.True(t, errors.Is(CheckMemoryBudget(budget, map[string]string{"budget": "0.55"}, nil, devices, nil), ErrMemoryObservation))
}
