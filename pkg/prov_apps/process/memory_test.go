package process

import (
	"errors"
	"os"
	"testing"

	"github.com/stperic/zzrouter/pkg/discovery/gpu"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMemoryFitErrorAttributesDescendants(t *testing.T) {
	parents := map[int32]int32{44: 43, 43: 42, 99: 1, 1: 0, 100: 101, 101: 100}
	read := parentPID
	t.Cleanup(func() { parentPID = read })
	parentPID = func(pid int32) (int32, error) {
		parent, ok := parents[pid]
		if !ok {
			return 0, os.ErrPermission
		}
		return parent, nil
	}
	for _, tc := range []struct {
		name string
		pid  int
		run  string
	}{
		{name: "run process", pid: 42, run: "run-owned"},
		{name: "child", pid: 43, run: "run-owned"},
		{name: "grandchild", pid: 44, run: "run-owned"},
		{name: "unrelated", pid: 99},
		{name: "unreadable", pid: 98},
		{name: "cycle", pid: 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			held := int64(3000)
			device := gpu.MemoryDevice{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: 6000, Processes: []gpu.MemoryProcess{{PID: tc.pid, MemoryMiB: &held}}}
			failure := memoryFitError("budget", device, 9500, []RunRef{{PID: 42, ID: "run-owned", Model: "org/owned-model"}})
			require.Len(t, failure.Holders, 1)
			holder := failure.Holders[0]
			assert.Equal(t, tc.pid, holder.PID)
			assert.Equal(t, tc.run, holder.Run)
			if tc.run != "" {
				assert.Equal(t, "zzrouter", holder.Ownership)
				assert.Contains(t, failure.Error(), "held by zzRouter run run-owned")
			} else {
				assert.Equal(t, "other_or_untracked", holder.Ownership)
				assert.Contains(t, failure.Error(), "held by a process zzRouter does not manage")
			}
		})
	}
}

func TestMemoryBudgetUsesPerDeviceFreeMemory(t *testing.T) {
	budget := schema.MemoryBudget{Kind: "fraction_total", Parameter: "budget", DeviceCountParameter: "devices"}
	amount := int64(3000)
	devices := []gpu.MemoryDevice{{UUID: "GPU-A", Index: 0, TotalMiB: 10000, FreeMiB: 6000, Processes: []gpu.MemoryProcess{{PID: 42, MemoryMiB: &amount}, {PID: 43}}}}
	require.NoError(t, admitBudget(budget, Need{}, map[string]string{"budget": "0.55"}, nil, devices, nil, nil))
	err := admitBudget(budget, Need{}, map[string]string{"budget": "0.95"}, nil, devices, []RunRef{{PID: 42, ID: "run-owned", Model: "org/owned-model"}}, nil)
	var failure *MemoryFitError
	require.ErrorAs(t, err, &failure)
	assert.Equal(t, int64(9500), failure.RequiredMiB)
	assert.Equal(t, int64(6000), failure.FreeMiB)
	require.Len(t, failure.Holders, 2)
	assert.Equal(t, "zzrouter", failure.Holders[0].Ownership)
	assert.Equal(t, "run-owned", failure.Holders[0].Run)
	assert.Equal(t, "other_or_untracked", failure.Holders[1].Ownership)
	assert.Nil(t, failure.Holders[1].MemoryMiB)
	assert.Contains(t, failure.Error(), "held by zzRouter run run-owned serving org/owned-model (pid 42, 3000 MiB), a process zzRouter does not manage (pid 43, size unknown)",
		"the message alone tells a stoppable run from a workload only the operator can release")
	// An aggregate free total must not hide one undersized shard device.
	devices = append(devices, gpu.MemoryDevice{UUID: "GPU-B", Index: 1, TotalMiB: 10000, FreeMiB: 5000})
	err = admitBudget(budget, Need{}, map[string]string{"budget": "0.55", "devices": "2"}, map[string]string{"CUDA_VISIBLE_DEVICES": "GPU-A,GPU-B"}, devices, nil, nil)
	require.ErrorAs(t, err, &failure)
	assert.Equal(t, "GPU-B", failure.Device)
	assert.Equal(t, int64(5500), failure.RequiredMiB)
	assert.Equal(t, int64(5000), failure.FreeMiB)
	for _, selector := range []string{"", "0", "GPU-A,GPU-A", "MIG-unknown"} {
		err := admitBudget(budget, Need{}, map[string]string{"budget": "0.55", "devices": "2"}, map[string]string{"CUDA_VISIBLE_DEVICES": selector}, devices, nil, nil)
		assert.ErrorIs(t, err, ErrMemoryObservation)
	}
	for _, fraction := range []string{"NaN", "Inf", "0", "1.5", "unknown"} {
		assert.ErrorIs(t, admitBudget(budget, Need{}, map[string]string{"budget": fraction}, nil, devices, nil, nil), ErrMemoryBudgetInvalid)
	}
	assert.True(t, errors.Is(admitBudget(budget, Need{}, map[string]string{"budget": "0.55"}, nil, devices, nil, nil), ErrMemoryObservation))
}

func admitBudget(budget schema.MemoryBudget, need Need, params, env map[string]string, devices []gpu.MemoryDevice, owners []RunRef, commitments map[string]int64) error {
	_, err := AdmitMemoryBudget(budget, need, params, env, devices, 0, owners, commitments)
	return err
}
