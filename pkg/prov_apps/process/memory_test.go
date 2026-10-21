package process

import (
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

func TestMemoryFitErrorKeptRunWithoutCredit(t *testing.T) {
	fit := &MemoryFitError{KeptRun: "old", CommitmentOnly: true, Device: "GPU-A", RequiredMiB: 6000, FreeMiB: 4000}
	assert.Contains(t, fit.Error(), "kept run old; replacement credited 0 MiB")
	assert.NotNil(t, fit.Unwrap())
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
		assert.ErrorIs(t, err, ErrMemorySelection)
	}
	for _, fraction := range []string{"NaN", "Inf", "0", "1.5", "unknown"} {
		assert.ErrorIs(t, admitBudget(budget, Need{}, map[string]string{"budget": fraction}, nil, devices, nil, nil), ErrMemoryBudgetInvalid)
	}
	err = admitBudget(budget, Need{}, map[string]string{"budget": "0.55"}, nil, devices, nil, nil)
	assert.ErrorIs(t, err, ErrMemoryObservation)
	assert.ErrorIs(t, err, ErrMemorySelection)
}

func admitBudget(budget schema.MemoryBudget, need Need, params, env map[string]string, devices []gpu.MemoryDevice, owners []RunRef, commitments map[string]int64) error {
	_, err := AdmitMemoryBudget(budget, need, params, env, devices, 0, owners, commitments, 0)
	return err
}

func TestMemoryCommitmentOnlyRefusal(t *testing.T) {
	for _, kind := range []string{schema.MemoryLayers, schema.MemoryUnified} {
		t.Run(kind, func(t *testing.T) {
			device := gpu.MemoryDevice{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: 2000, UsedMiB: new(int64(8000)), Processes: []gpu.MemoryProcess{{PID: 42, MemoryMiB: new(int64(2000))}}}
			owners := []RunRef{{ID: "comfy", PID: 42, Commitments: map[string]int64{"GPU-A": 4000}}}
			err := admitBudget(schema.MemoryBudget{Kind: kind}, Need{}, nil, nil, []gpu.MemoryDevice{device}, owners, nil)
			var fit *MemoryFitError
			require.ErrorAs(t, err, &fit)
			assert.True(t, fit.CommitmentOnly)
			assert.Equal(t, int64(2000), fit.RequiredMiB)
			assert.Zero(t, fit.FreeMiB)
		})
	}
}

func TestMemoryReleaseBound(t *testing.T) {
	read := parentPID
	t.Cleanup(func() { parentPID = read })
	parentPID = func(pid int32) (int32, error) {
		if pid == 99 {
			return 0, os.ErrPermission
		}
		return 0, nil
	}
	for _, tc := range []struct {
		name        string
		processes   []gpu.MemoryProcess
		reason      string
		unaccounted int64
	}{
		{name: "complete", processes: []gpu.MemoryProcess{{PID: 42, MemoryMiB: new(int64(3000))}}},
		{name: "partial", processes: []gpu.MemoryProcess{{PID: 42, MemoryMiB: new(int64(1000))}}, unaccounted: 2000},
		{name: "unknown", unaccounted: 3000},
		{name: "unknown size", processes: []gpu.MemoryProcess{{PID: 42}}, unaccounted: 3000},
		{name: "foreign unknown size", processes: []gpu.MemoryProcess{{PID: 98}}, unaccounted: 3000},
		{name: "ancestry failed", processes: []gpu.MemoryProcess{{PID: 99, MemoryMiB: new(int64(3000))}}, unaccounted: 3000},
		{name: "foreign", processes: []gpu.MemoryProcess{{PID: 98, MemoryMiB: new(int64(3000))}}},
		{name: "driver reason", reason: "driver listing unavailable", unaccounted: 3000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			device := gpu.MemoryDevice{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: 7000, UsedMiB: new(int64(3000)), Processes: tc.processes, ProcessReason: tc.reason}
			owners := []RunRef{{ID: "old", PID: 42}}
			after, bound := MemoryAfterStop(device, owners, "old")
			held := int64(0)
			for _, p := range tc.processes {
				if p.PID == 42 && p.MemoryMiB != nil {
					held += *p.MemoryMiB
				}
			}
			if tc.reason == "" {
				require.NotNil(t, bound)
				assert.Equal(t, held+tc.unaccounted, *bound)
			} else {
				assert.Nil(t, bound, "a failed listing cannot establish release")
			}
			assert.Equal(t, device.FreeMiB+held, after.FreeMiB)
			fit := memoryFitError("budget", after, 8000, nil)
			if tc.name == "foreign" {
				require.Len(t, fit.Holders, 1)
				assert.Empty(t, fit.Holders[0].Run)
				assert.Equal(t, "other_or_untracked", fit.Holders[0].Ownership)
			} else {
				assert.Empty(t, fit.Holders, "all processes inside the release bound leave the simulation")
			}
		})
	}
}

func TestMemoryFootprintUsesDriverUsedMemory(t *testing.T) {
	for _, tc := range []struct {
		name        string
		free        int64
		used        *int64
		unaccounted *int64
	}{
		{name: "driver reserved only", free: 97248, used: new(int64(0)), unaccounted: new(int64(0))},
		{name: "used unknown", free: 97248},
		{name: "used unknown without reservations", free: 97887},
		{name: "unattributed use", free: 97248, used: new(int64(1)), unaccounted: new(int64(1))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			device := gpu.MemoryDevice{UUID: "GPU-A", TotalMiB: 97887, FreeMiB: tc.free, UsedMiB: tc.used}
			_, bound := MemoryAfterStop(device, nil, "old")
			assert.Equal(t, tc.unaccounted, bound)
			fit := memoryFitError("budget", device, 97887, nil)
			assert.Equal(t, tc.free, fit.FreeMiB, "driver reservations remain unavailable for admission")
			assert.Empty(t, fit.Holders)
		})
	}
}
