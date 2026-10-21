package prov_apps

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/discovery/gpu"
	"github.com/stperic/zzrouter/pkg/discovery/hardware"
	"github.com/stperic/zzrouter/pkg/modelregistry"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type replacementRig struct {
	*contractRig
	old, other    *instance.Instance
	before, after []gpu.MemoryDevice
	checks        atomic.Int32
	schema        atomic.Pointer[func(string) (*schema.ProviderSchema, error)]
}

func newReplacementRig(t *testing.T, free, held int64, pinned bool, edits ...func(*config.ServiceConfig)) *replacementRig {
	t.Helper()
	t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
	t.Setenv("CUDA_VISIBLE_DEVICES", "GPU-A")
	r := &replacementRig{}
	r.setBudget(&schema.MemoryBudget{Kind: schema.MemoryFractionTotal, Parameter: "budget"})
	r.contractRig = newContractRig(t, false, WithProviderSchema(func(provider string) (*schema.ProviderSchema, error) {
		return (*r.schema.Load())(provider)
	}))
	require.NoError(t, r.m.appsConfig.UpdateApp("generic", func(svc *config.ServiceConfig) error {
		for _, edit := range edits {
			edit(svc)
		}
		return nil
	}))
	r.m.memorySnapshot = func(context.Context) ([]gpu.MemoryDevice, error) {
		return []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: 10000}}, nil
	}
	req := r.request()
	req.Force = false
	if pinned {
		req.Parameters = map[string]string{"keep-alive": "-1"}
	}
	started, err := r.m.EnsureRun(t.Context(), req)
	require.NoError(t, err)
	awaitRunning(t, started.Run)
	r.old = started.Run
	r.other = instance.NewInstance("other-holder", "other", "other-model", 0, -1, 1)
	r.other.SetStatus(instance.StatusRunning)
	r.other.SetProcessInfo(9000001, 0, 0, nil)
	require.NoError(t, r.m.instances.Register(r.other))
	other := 10000 - free - held
	r.before = []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: free, UsedMiB: new(10000 - free), Processes: []gpu.MemoryProcess{
		{PID: r.old.GetProcessID(), MemoryMiB: new(held)}, {PID: r.other.GetProcessID(), MemoryMiB: new(other)},
	}}}
	r.after = []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: free + held, UsedMiB: new(other), Processes: r.before[0].Processes[1:]}}
	r.m.memorySnapshot = r.snapshot
	return r
}

func (r *replacementRig) setBudget(budget *schema.MemoryBudget, fragments ...string) {
	declared := memoryBudgetSchema(func(string) (*schema.MemoryBudget, error) { return budget, nil }, fragments...)
	r.schema.Store(&declared)
}

func (r *replacementRig) request() EnsureRequest {
	return EnsureRequest{Provider: "generic", Model: "shared", Force: true}
}

func (r *replacementRig) snapshot(context.Context) ([]gpu.MemoryDevice, error) {
	if r.old.GetStatus() == instance.StatusStopped {
		return r.after, nil
	}
	r.checks.Add(1)
	return r.before, nil
}

func (r *replacementRig) replace(t *testing.T) (*instance.Instance, error) {
	t.Helper()
	result, err := r.m.EnsureRun(t.Context(), r.request())
	return result.Run, err
}

func TestReplacementRefusalKeepsOldRunServing(t *testing.T) {
	r := newReplacementRig(t, 1000, 2000, true)
	pins := readPins(t)
	_, err := r.replace(t)
	var fit *process.MemoryFitError
	require.ErrorAs(t, err, &fit)
	assert.Equal(t, r.old.ID, fit.KeptRun)
	assert.Equal(t, new(int64(2000)), fit.CreditedMiB)
	assert.Contains(t, fit.Error(), "kept run "+r.old.ID)
	assert.Contains(t, fit.Error(), "free after the stop 3000 MiB")
	assert.Equal(t, int64(3000), fit.FreeMiB)
	require.Len(t, fit.Holders, 1)
	assert.Equal(t, r.other.ID, fit.Holders[0].Run)
	assert.Equal(t, instance.StatusRunning, r.old.GetStatus())
	assert.Zero(t, r.terms(t))
	lease, err := r.old.Acquire(t.Context(), nil)
	require.NoError(t, err)
	lease.Release()
	assert.Equal(t, pins, readPins(t))
}

func TestReplacementUnifiedRefusalKeepsOldRunServing(t *testing.T) {
	t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
	t.Setenv("CUDA_VISIBLE_DEVICES", "unified")
	r := newContractRig(t, false, WithProviderSchema(memoryBudgetSchema(func(string) (*schema.MemoryBudget, error) {
		return &schema.MemoryBudget{Kind: schema.MemoryUnified}, nil
	})))
	r.m.memorySystem = func(context.Context) *hardware.HardwareInfo {
		return &hardware.HardwareInfo{TotalRAM: 10000 * 1024 * 1024}
	}
	r.m.memorySnapshot = func(context.Context) ([]gpu.MemoryDevice, error) {
		t.Error("unified admission must use system totals")
		return nil, process.ErrMemoryObservation
	}
	req := EnsureRequest{Provider: "generic", Model: "shared", Parameters: map[string]string{"keep-alive": "-1"}}
	started, err := r.m.EnsureRun(t.Context(), req)
	require.NoError(t, err)
	awaitRunning(t, started.Run)
	pins := readPins(t)
	replacementWeights(t, 10001)
	r.m.seams.beforeLaunch = func() { t.Error("refused replacement launched a process") }
	req.Force = true
	_, err = r.m.EnsureRun(t.Context(), req)
	var fit *process.MemoryFitError
	require.ErrorAs(t, err, &fit)
	assert.Equal(t, schema.MemoryUnified, fit.BudgetKind)
	assert.Equal(t, int64(10001), fit.RequiredMiB)
	assert.Equal(t, int64(10000), fit.FreeMiB)
	assert.Equal(t, started.Run.ID, fit.KeptRun)
	assert.Equal(t, new(int64(0)), fit.CreditedMiB)
	registered, ok := r.m.instances.Get(started.Run.ID)
	require.True(t, ok)
	assert.Same(t, started.Run, registered)
	assert.Equal(t, instance.StatusRunning, started.Run.GetStatus())
	assert.Zero(t, r.terms(t))
	lease, err := started.Run.Acquire(t.Context(), nil)
	require.NoError(t, err)
	lease.Release()
	assert.Equal(t, pins, readPins(t))
}

func TestReplacementCreditsOldRunHoldings(t *testing.T) {
	r := newReplacementRig(t, 1000, 5000, false)
	run, err := r.replace(t)
	require.NoError(t, err)
	awaitRunning(t, run)
	assert.NotEqual(t, r.old.ID, run.ID)
	assert.Equal(t, instance.StatusStopped, r.old.GetStatus())
	assert.Equal(t, instance.StatusRunning, r.other.GetStatus())
	assert.Empty(t, run.ToInfo("").EvictedRuns)
	assert.Positive(t, r.checks.Load())
}

func TestReplacementCreditsPinnedOldRun(t *testing.T) {
	r := newReplacementRig(t, 1000, 5000, true)
	pins := readPins(t)
	lease, err := r.old.Acquire(t.Context(), nil)
	require.NoError(t, err)
	defer lease.Release()
	r.old.ObserveActivity(true)
	run, err := r.replace(t)
	require.NoError(t, err)
	awaitRunning(t, run)
	assert.True(t, run.IdleSnapshot().Pinned)
	assert.Equal(t, pins, readPins(t))
	assert.Equal(t, instance.StatusStopped, r.old.GetStatus())
}

func TestReplacementReserveCreditsUnusedCommitment(t *testing.T) {
	t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
	t.Setenv("CUDA_VISIBLE_DEVICES", "GPU-A")
	r := reservationRig(t, false)
	started, err := r.m.EnsureRun(t.Context(), EnsureRequest{Provider: "generic"})
	require.NoError(t, err)
	awaitRunning(t, started.Run)
	before := started.Run.Resolved()
	// 2048 MiB is used; the other 6952 MiB of its reservation must also be credited.
	r.m.memorySnapshot = func(context.Context) ([]gpu.MemoryDevice, error) {
		device := gpu.MemoryDevice{UUID: "GPU-A", TotalMiB: 24576, FreeMiB: 10000}
		if started.Run.GetStatus() != instance.StatusStopped {
			device.FreeMiB = 7952
			device.UsedMiB = new(int64(16624))
			device.Processes = []gpu.MemoryProcess{{PID: started.Run.GetProcessID(), MemoryMiB: new(int64(2048))}, {PID: 9000001, MemoryMiB: new(int64(14576))}}
		}
		return []gpu.MemoryDevice{device}, nil
	}
	blocker := instance.NewInstance("reserve-blocker", "other", "blocker", 0, -1, 1)
	blocker.SetStatus(instance.StatusRunning)
	blocker.SetProcessInfo(9000001, 0, 0, nil)
	require.NoError(t, r.m.instances.Register(blocker))
	restarted, err := r.m.RestartInstance(t.Context(), started.Run.ID)
	require.NoError(t, err)
	awaitRunning(t, restarted)
	assert.Equal(t, before.Commitments, restarted.Resolved().Commitments)
	assert.Equal(t, before.Parameters, restarted.Resolved().Parameters)
	assert.Equal(t, before.Environment, restarted.Resolved().Environment)
}

func TestReplacementPlansIdleEvictionWithoutStopping(t *testing.T) {
	for _, enough := range []bool{false, true} {
		t.Run(map[bool]string{false: "insufficient", true: "covered"}[enough], func(t *testing.T) {
			r := newReplacementRig(t, 1000, 2000, false)
			r.m.idleEvictionDefault = true
			// The real lease makes the competing holder ineligible in the refusal case.
			if !enough {
				lease, err := r.other.Acquire(t.Context(), nil)
				require.NoError(t, err)
				defer lease.Release()
			} else {
				r.other = replaceIdleHolder(t, r)
			}
			b := newBarrier()
			t.Cleanup(b.open)
			r.old.TrackGoroutine()
			go func() { defer r.old.GoroutineDone(); <-r.old.Context().Done(); b.hold() }()
			out := r.ensure(t.Context(), r.request())
			if !enough {
				got := collect(t, []<-chan outcome{out})[0]
				var fit *process.MemoryFitError
				require.ErrorAs(t, got.err, &fit)
				assert.Equal(t, instance.StatusRunning, r.old.GetStatus())
				assert.Equal(t, instance.StatusRunning, r.other.GetStatus())
				return
			}
			b.reached(t)
			assert.Equal(t, instance.StatusRunning, r.other.GetStatus())
			lease, err := r.other.Acquire(t.Context(), nil)
			require.NoError(t, err, "planning must not claim an idle victim")
			lease.Release()
			r.m.memorySnapshot = func(ctx context.Context) ([]gpu.MemoryDevice, error) {
				if r.other.GetStatus() == instance.StatusStopped {
					return []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: 10000}}, nil
				}
				return r.snapshot(ctx)
			}
			b.open()
			got := collect(t, []<-chan outcome{out})[0]
			require.NoError(t, got.err)
			awaitRunning(t, got.result.Run)
			assert.Equal(t, instance.StatusStopped, r.old.GetStatus())
			assert.Equal(t, instance.StatusStopped, r.other.GetStatus())
			require.Len(t, got.result.Run.ToInfo("").EvictedRuns, 1)
			assert.Equal(t, r.other.ID, got.result.Run.ToInfo("").EvictedRuns[0].RunID)
		})
	}
}

func TestReplacementEvictIdleOffNeverPlansVictims(t *testing.T) {
	for _, enabledByDefault := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled by default", true: "request disables node default"}[enabledByDefault], func(t *testing.T) {
			r := newReplacementRig(t, 1000, 2000, false)
			r.m.idleEvictionDefault = enabledByDefault
			r.other = replaceIdleHolder(t, r)
			req := r.request()
			if enabledByDefault {
				req.Parameters = map[string]string{"evict-idle-on-memory-pressure": "false"}
			}
			features, err := r.m.ResolveLaunchParameters(t.Context(), req)
			require.NoError(t, err)
			check := &replacementCheck{run: r.old.ID, recovery: make(map[string]int64)}
			_, evicted, err := r.m.admitMemory(t.Context(), features, false, check)
			var fit *process.MemoryFitError
			require.ErrorAs(t, err, &fit, "the idle holder could cover the shortage if eviction were enabled")
			assert.Empty(t, evicted)
			assert.Equal(t, r.old.ID, fit.KeptRun)
			assert.Equal(t, int64(3000), fit.FreeMiB)
			require.Len(t, fit.Holders, 1)
			assert.Equal(t, r.other.ID, fit.Holders[0].Run)
			assert.Equal(t, new(int64(7000)), fit.Holders[0].MemoryMiB)
			for _, run := range []*instance.Instance{r.old, r.other} {
				assert.Equal(t, instance.StatusRunning, run.GetStatus())
				lease, err := run.Acquire(t.Context(), nil)
				require.NoError(t, err)
				lease.Release()
			}
			assert.Zero(t, r.terms(t))
		})
	}
}

func replaceIdleHolder(t *testing.T, r *replacementRig) *instance.Instance {
	t.Helper()
	require.NoError(t, r.m.instances.Remove(r.other.ID))
	idle := instance.NewInstance(r.other.ID, "other", r.other.Model, 0, time.Hour, 1)
	idle.SetStatus(instance.StatusRunning)
	idle.SetProcessInfo(r.other.GetProcessID(), 0, 0, nil)
	require.NoError(t, r.m.instances.Register(idle))
	return idle
}

func TestReplacementUnknownUsedKeepsStopOrder(t *testing.T) {
	r := newReplacementRig(t, 1000, 2000, true)
	r.before[0].UsedMiB = nil
	_, err := r.replace(t)
	var fit *process.MemoryFitError
	require.ErrorAs(t, err, &fit)
	assert.Empty(t, fit.KeptRun)
	assert.Equal(t, instance.StatusStopped, r.old.GetStatus())
	assert.Equal(t, instance.StatusRunning, r.other.GetStatus())
}

func TestReplacementPartialAttributionUsesBound(t *testing.T) {
	for _, evidence := range []string{"missing holder", "missing size", "unreadable ancestry"} {
		t.Run(evidence, func(t *testing.T) {
			r := newReplacementRig(t, 1000, 2000, false)
			switch evidence {
			case "missing holder":
				r.before[0].Processes = r.before[0].Processes[:1]
			case "missing size":
				r.before[0].Processes[1].MemoryMiB = nil
			case "unreadable ancestry":
				r.before[0].Processes[1].PID = 9000002
			}
			features, err := r.m.ResolveLaunchParameters(t.Context(), r.request())
			require.NoError(t, err)
			check := &replacementCheck{run: r.old.ID, recovery: make(map[string]int64)}
			_, _, err = r.m.admitMemory(t.Context(), features, false, check)
			require.NoError(t, err)
			assert.Equal(t, map[string]int64{"GPU-A": 1000 + 2000 - memoryRecoveryToleranceMiB}, check.recovery)
			_, err = r.replace(t)
			var fit *process.MemoryFitError
			require.ErrorAs(t, err, &fit)
			assert.Empty(t, fit.KeptRun)
			assert.Equal(t, instance.StatusStopped, r.old.GetStatus())
			assert.Positive(t, r.checks.Load())
		})
	}
}

func TestReplacementDriverReservedMemory(t *testing.T) {
	for _, known := range []bool{true, false} {
		t.Run(map[bool]string{true: "used zero", false: "used unknown"}[known], func(t *testing.T) {
			r := newReplacementRig(t, 1000, 2000, false)
			device := gpu.MemoryDevice{UUID: "GPU-A", TotalMiB: 97887, FreeMiB: 97248}
			if known {
				device.UsedMiB = new(int64(0))
			}
			r.before, r.after = []gpu.MemoryDevice{device}, []gpu.MemoryDevice{device}
			req := r.request()
			req.Parameters = map[string]string{"budget": "1"}
			_, err := r.m.EnsureRun(t.Context(), req)
			var fit *process.MemoryFitError
			require.ErrorAs(t, err, &fit)
			assert.Empty(t, fit.Holders)
			if known {
				assert.Equal(t, new(int64(0)), fit.CreditedMiB)
				assert.Equal(t, r.old.ID, fit.KeptRun)
				assert.Equal(t, instance.StatusRunning, r.old.GetStatus())
				assert.Zero(t, r.terms(t))
			} else {
				assert.Nil(t, fit.CreditedMiB)
				assert.Empty(t, fit.KeptRun)
				assert.Equal(t, instance.StatusStopped, r.old.GetStatus())
				assert.Equal(t, 1, r.terms(t))
			}
		})
	}
}

func TestReplacementMaximumReleaseBound(t *testing.T) {
	for _, tc := range []struct {
		name, budget string
		used, free   int64
		otherPID     int
		listingFails bool
		kept         bool
		credit       int64
	}{
		{name: "context gap may fit", budget: "0.9933", used: 3714, free: 93535},
		{name: "above all used", budget: "1", used: 3714, free: 93535, kept: true, credit: 3714},
		{name: "another run excluded", budget: "0.985", used: 4637, free: 92610, otherPID: 9000001, kept: true, credit: 3737},
		{name: "unreadable ancestry included", budget: "0.985", used: 4637, free: 92610, otherPID: 9000002},
		{name: "listing failed may fit", budget: "0.9933", used: 3714, free: 93535, listingFails: true},
		{name: "listing failed above used", budget: "1", used: 3714, free: 93535, listingFails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newReplacementRig(t, 1000, 2000, true)
			device := gpu.MemoryDevice{UUID: "GPU-A", TotalMiB: 97887, UsedMiB: new(tc.used), FreeMiB: tc.free,
				Processes: []gpu.MemoryProcess{{PID: r.old.GetProcessID(), MemoryMiB: new(int64(3688))}}}
			if tc.otherPID != 0 {
				device.Processes = append(device.Processes, gpu.MemoryProcess{PID: tc.otherPID, MemoryMiB: new(int64(900))})
			}
			if tc.listingFails {
				device.Processes, device.ProcessReason = nil, "driver listing unavailable"
			}
			r.before = []gpu.MemoryDevice{device}
			// Fresh admission sees only the attributed release, and still refuses.
			r.after = []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 97887, FreeMiB: tc.free + 3688}}
			req := r.request()
			req.Parameters = map[string]string{"budget": tc.budget}
			_, err := r.m.EnsureRun(t.Context(), req)
			var fit *process.MemoryFitError
			require.ErrorAs(t, err, &fit)
			if tc.kept {
				assert.Equal(t, new(tc.credit), fit.CreditedMiB)
				assert.Equal(t, r.old.ID, fit.KeptRun)
				assert.Equal(t, instance.StatusRunning, r.old.GetStatus())
				assert.Zero(t, r.terms(t))
				lease, err := r.old.Acquire(t.Context(), nil)
				require.NoError(t, err)
				lease.Release()
			} else {
				assert.Nil(t, fit.CreditedMiB)
				assert.Empty(t, fit.KeptRun)
				assert.Equal(t, instance.StatusStopped, r.old.GetStatus())
				assert.Equal(t, 1, r.terms(t))
			}
			assert.Equal(t, instance.StatusRunning, r.other.GetStatus())
		})
	}
}

func TestReplacementBoundRecoveryUsesAttributedHoldings(t *testing.T) {
	r := newReplacementRig(t, 1000, 2000, false)
	r.before = []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 97887, FreeMiB: 93535, UsedMiB: new(int64(3714)),
		Processes: []gpu.MemoryProcess{{PID: r.old.GetProcessID(), MemoryMiB: new(int64(3688))}}}}
	features, err := r.m.ResolveLaunchParameters(t.Context(), r.request())
	require.NoError(t, err)
	features.Params["budget"] = "0.9933"
	check := &replacementCheck{run: r.old.ID, recovery: make(map[string]int64)}
	_, _, err = r.m.admitMemory(t.Context(), features, false, check)
	require.NoError(t, err, "a requirement within the release bound passes the pre-check")
	assert.Equal(t, map[string]int64{"GPU-A": 93535 + 3688 - memoryRecoveryToleranceMiB}, check.recovery)
	assert.Equal(t, instance.StatusRunning, r.old.GetStatus())
	assert.Zero(t, r.terms(t))
}

func TestReplacementBoundCoversIdlePlan(t *testing.T) {
	r := newReplacementRig(t, 1000, 2000, false)
	r.m.idleEvictionDefault = true
	r.other = replaceIdleHolder(t, r)
	r.before[0].UsedMiB = new(int64(3714))
	r.before[0].Processes[0].MemoryMiB = new(int64(2000))
	r.before[0].Processes[1].MemoryMiB = new(int64(900))
	// The stop could release 2814 MiB; adding the idle victim covers a 3700 MiB shortfall.
	features, err := r.m.ResolveLaunchParameters(t.Context(), r.request())
	require.NoError(t, err)
	features.Params["budget"] = "0.47"
	check := &replacementCheck{run: r.old.ID, recovery: make(map[string]int64)}
	_, _, err = r.m.admitMemory(t.Context(), features, false, check)
	require.NoError(t, err)
	assert.Equal(t, map[string]int64{"GPU-A": 1000 + 2000 - memoryRecoveryToleranceMiB}, check.recovery)
	assert.Equal(t, instance.StatusRunning, r.other.GetStatus())
	lease, err := r.other.Acquire(t.Context(), nil)
	require.NoError(t, err, "a pre-check never claims a planned victim")
	lease.Release()
}

func TestReplacementUnbudgetedKeepsStopOrder(t *testing.T) {
	r := newReplacementRig(t, 1000, 2000, false)
	r.setBudget(nil)
	r.m.memorySnapshot = func(context.Context) ([]gpu.MemoryDevice, error) {
		t.Error("an unbudgeted engine must not measure memory")
		return nil, nil
	}
	run, err := r.replace(t)
	require.NoError(t, err)
	awaitRunning(t, run)
	assert.Equal(t, instance.StatusStopped, r.old.GetStatus())
}

func TestReplacementCreditIsPerDevice(t *testing.T) {
	for _, failDevice := range []string{"GPU-A", "GPU-B"} {
		t.Run(failDevice, func(t *testing.T) {
			r := newReplacementRig(t, 1000, 5000, false)
			r.setBudget(&schema.MemoryBudget{Kind: schema.MemoryFractionTotal, Parameter: "budget", DeviceCountParameter: "devices"})
			r.before = append(r.before, gpu.MemoryDevice{UUID: "GPU-B", Index: 1, TotalMiB: 10000, FreeMiB: 1000, UsedMiB: new(int64(9000)), Processes: []gpu.MemoryProcess{
				{PID: r.old.GetProcessID(), MemoryMiB: new(int64(5000))}, {PID: r.other.GetProcessID(), MemoryMiB: new(int64(4000))},
			}})
			i := 0
			if failDevice == "GPU-B" {
				i = 1
			}
			r.before[i].Processes[0].MemoryMiB = new(int64(1000))
			r.before[i].Processes[1].MemoryMiB = new(int64(8000))
			req := r.request()
			req.Parameters = map[string]string{"devices": "2"}
			req.EnvVars = map[string]string{"CUDA_VISIBLE_DEVICES": "GPU-A,GPU-B"}
			_, err := r.m.EnsureRun(t.Context(), req)
			var fit *process.MemoryFitError
			require.ErrorAs(t, err, &fit)
			assert.Equal(t, failDevice, fit.Device)
			assert.Equal(t, new(int64(1000)), fit.CreditedMiB)
			assert.Equal(t, r.old.ID, fit.KeptRun)
			assert.Equal(t, instance.StatusRunning, r.old.GetStatus())
		})
	}
}

func TestReplacementIdlePlanRevalidatesAfterStop(t *testing.T) {
	r := newReplacementRig(t, 1000, 2000, false)
	r.m.idleEvictionDefault = true
	r.other = replaceIdleHolder(t, r)
	leased := make(chan *instance.Lease, 1)
	r.old.TrackGoroutine()
	go func() {
		defer r.old.GoroutineDone()
		<-r.old.Context().Done()
		lease, err := r.other.Acquire(t.Context(), nil)
		assert.NoError(t, err)
		leased <- lease
	}()
	_, err := r.replace(t)
	lease := <-leased
	require.NotNil(t, lease)
	defer lease.Release()
	var fit *process.MemoryFitError
	require.ErrorAs(t, err, &fit)
	assert.Empty(t, fit.KeptRun)
	assert.Equal(t, instance.StatusStopped, r.old.GetStatus())
	assert.Equal(t, instance.StatusRunning, r.other.GetStatus())
	assert.Equal(t, "in use", fit.Holders[0].ReclaimSkippedReason)
	assert.Positive(t, r.checks.Load(), "idle coverage was assessed before the old run drained")
	assert.Equal(t, 1, r.terms(t))
}

func TestReplacementAutoFractionMeasuredAfterStop(t *testing.T) {
	engine := filepath.Join(t.TempDir(), "auto-engine")
	script := strings.Replace(contractEngine, "\n", "\nprintf '%s\\n' \"$@\" >\"$0.argv\"\n", 1)
	require.NoError(t, os.WriteFile(engine, []byte(script), 0o700))
	require.NoError(t, os.WriteFile(engine+".ready", nil, 0o600))
	r := newReplacementRig(t, 1000, 5000, false, func(svc *config.ServiceConfig) { svc.Runtime.Execution.Command = engine })
	req := r.request()
	req.Parameters = map[string]string{"budget": "auto"}
	params := maps.Clone(req.Parameters)
	result, err := r.m.EnsureRun(t.Context(), req)
	require.NoError(t, err)
	awaitRunning(t, result.Run)
	argv, err := os.ReadFile(engine + ".argv")
	require.NoError(t, err)
	assert.Contains(t, string(argv), "--budget\n0.3952\n")
	assert.Equal(t, "auto", result.Run.Resolved().Parameters["budget"])
	assert.Equal(t, params, req.Parameters)
	assert.Equal(t, instance.StatusStopped, r.old.GetStatus())
}

func TestReplacementAutoFractionWithZeroCredit(t *testing.T) {
	r := newReplacementRig(t, 2048, 0, false)
	r.m.idleEvictionDefault = true
	r.other = replaceIdleHolder(t, r)
	r.before[0].TotalMiB = 20000
	r.before[0].UsedMiB = new(int64(17952))
	r.before[0].Processes[1].MemoryMiB = new(int64(17952))
	features := LaunchParameters{declared: requireLaunchSchema(t, r.m, "generic"), Params: map[string]string{"budget": "auto"}}
	check := &replacementCheck{run: r.old.ID, recovery: make(map[string]int64)}
	assessment, _, err := r.m.admitMemory(t.Context(), features, false, check)
	require.NoError(t, err, "idle coverage must let the virtual auto-fraction check pass")
	assert.Equal(t, int64(20000), assessment.CapacityMiB, "idle removal exposes the actual free memory in the simulated admission")
	assert.Empty(t, check.recovery, "the replaced run holds nothing on this device")
	assert.Equal(t, "auto", features.Params["budget"])
	assert.Equal(t, instance.StatusRunning, r.old.GetStatus())
	assert.Equal(t, instance.StatusRunning, r.other.GetStatus())
	assert.Zero(t, r.terms(t))
	lease, err := r.other.Acquire(t.Context(), nil)
	require.NoError(t, err, "the pre-check must not claim the planned victim")
	lease.Release()
}

func TestReplacementInferenceReusesOldRunDuringCheck(t *testing.T) {
	r := newReplacementRig(t, 1000, 2000, false)
	b := newBarrier()
	r.m.memorySnapshot = func(ctx context.Context) ([]gpu.MemoryDevice, error) { b.hold(); return r.snapshot(ctx) }
	out := r.ensure(t.Context(), r.request())
	b.reached(t)
	req := r.request()
	req.Force, req.Origin = false, OriginRequest
	reused, err := r.m.EnsureRun(t.Context(), req)
	require.NoError(t, err)
	assert.Same(t, r.old, reused.Run)
	assert.False(t, reused.Started)
	lease, err := reused.Run.Acquire(t.Context(), nil)
	require.NoError(t, err)
	lease.Release()
	b.open()
	var fit *process.MemoryFitError
	require.ErrorAs(t, collect(t, []<-chan outcome{out})[0].err, &fit)
}

func TestReplacementConcurrentRestartsOneCheck(t *testing.T) {
	r := newReplacementRig(t, 1000, 2000, false)
	b := newBarrier()
	r.m.memorySnapshot = func(ctx context.Context) ([]gpu.MemoryDevice, error) { b.hold(); return r.snapshot(ctx) }
	ctxs, reqs := same(t.Context(), r.request())
	outs := r.leaderAndJoiners(t, b, ctxs, reqs)
	require.Eventually(t, func() bool {
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		joined := 0
		for _, stack := range strings.Split(string(buf[:n]), "\n\n") {
			header, _, _ := strings.Cut(stack, "\n")
			if strings.Contains(header, "[select]") && strings.Contains(stack, ").EnsureRun(") {
				joined++
			}
		}
		return joined == contractCallers
	}, contractWait, time.Millisecond, "every caller joined the held flight")
	b.open()
	for _, got := range collect(t, outs) {
		var fit *process.MemoryFitError
		require.ErrorAs(t, got.err, &fit)
		assert.Equal(t, r.old.ID, fit.KeptRun)
	}
	assert.Equal(t, int32(1), r.checks.Load())
	assert.Equal(t, instance.StatusRunning, r.old.GetStatus())
}

func TestReplacementLateStealRefusesAfterStop(t *testing.T) {
	r := newReplacementRig(t, 1000, 5000, true)
	pins := readPins(t)
	var post atomic.Int32
	r.m.memorySnapshot = func(ctx context.Context) ([]gpu.MemoryDevice, error) {
		if r.old.GetStatus() == instance.StatusStopped && post.Add(1) > 1 {
			return []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: 1000}}, nil
		}
		return r.snapshot(ctx)
	}
	_, err := r.replace(t)
	var fit *process.MemoryFitError
	require.ErrorAs(t, err, &fit)
	assert.Empty(t, fit.KeptRun)
	assert.Nil(t, fit.CreditedMiB)
	assert.Equal(t, instance.StatusStopped, r.old.GetStatus())
	assert.Equal(t, pins, readPins(t))
	assert.Equal(t, 1, r.terms(t))
}

func TestReplacementStopFailure(t *testing.T) {
	for _, failure := range []string{"stop cancellation", "recovery timeout", "recovery cancellation"} {
		t.Run(failure, func(t *testing.T) {
			held := int64(2000)
			if failure == "recovery timeout" {
				held = 5000
			}
			r := newReplacementRig(t, 1000, held, false)
			r.m.idleEvictionDefault = true
			r.other = replaceIdleHolder(t, r)
			if failure == "recovery timeout" {
				lease, err := r.other.Acquire(t.Context(), nil)
				require.NoError(t, err)
				defer lease.Release()
			}
			r.m.seams.beforeLaunch = func() { t.Error("replacement launched after stop cancellation or memory refusal") }
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if failure == "stop cancellation" {
				r.old.TrackGoroutine()
				drained := make(chan struct{})
				go func() { defer close(drained); <-r.old.Context().Done(); cancel() }()
				defer func() { r.old.GoroutineDone(); <-drained }()
			} else {
				r.after[0].FreeMiB = 1000
				if failure == "recovery cancellation" {
					r.m.memorySnapshot = func(snapshotCtx context.Context) ([]gpu.MemoryDevice, error) {
						if r.old.GetStatus() == instance.StatusStopped {
							cancel()
						}
						return r.snapshot(snapshotCtx)
					}
				}
			}
			prepared, err := r.m.prepareStart(ctx, r.request())
			require.NoError(t, err)
			ctx, prepared, err = r.m.prepareColdStart(ctx, prepared)
			require.NoError(t, err)
			_, err = r.m.executeStart(ctx, prepared)
			if failure == "recovery timeout" {
				var fit *process.MemoryFitError
				require.ErrorAs(t, err, &fit)
				assert.Equal(t, int64(1000), fit.FreeMiB)
				assert.Equal(t, int64(5500), fit.RequiredMiB)
				assert.Empty(t, fit.KeptRun)
				assert.Nil(t, fit.CreditedMiB)
				require.Len(t, fit.Holders, 1)
				assert.Equal(t, "in use", fit.Holders[0].ReclaimSkippedReason)
				assert.Equal(t, instance.StatusStopped, r.old.GetStatus())
			} else {
				assert.ErrorIs(t, err, context.Canceled)
			}
			assert.Equal(t, instance.StatusRunning, r.other.GetStatus())
			assert.Equal(t, int32(2), r.attempts.Load(), "no replacement reaches process launch")
			for _, run := range r.m.instances.List() {
				assert.NotEqual(t, instance.StatusStarting, run.GetStatus())
			}
		})
	}
}

func TestReplacementRecoveryTimeoutUsesFreshAdmission(t *testing.T) {
	r := newReplacementRig(t, 1000, 5000, false)
	// Enough for admission, but below the credited recovery target.
	r.after[0].FreeMiB = 5500
	run, err := r.replace(t)
	require.NoError(t, err)
	awaitRunning(t, run)
	assert.Equal(t, instance.StatusStopped, r.old.GetStatus())
	assert.Equal(t, instance.StatusRunning, r.other.GetStatus())
	assert.Empty(t, run.ToInfo("").EvictedRuns)
}

func TestReplacementRestoreRefusalKeepsFailedRun(t *testing.T) {
	r := newReplacementRig(t, 1000, 2000, true)
	r.old.MarkFailedError(errors.New("engine failed"))
	// A confirmed terminal run has no footprint; the remaining holder is complete evidence.
	r.before[0].FreeMiB = 3000
	r.before[0].UsedMiB = new(int64(7000))
	r.before[0].Processes = r.before[0].Processes[1:]
	r.m.RestorePinnedRuns()
	eventually(t, func() bool {
		r.m.flightsMu.Lock()
		defer r.m.flightsMu.Unlock()
		return len(r.m.flights) == 0 && r.attempts.Load() > 1
	}, "restore refusal completed")
	registered, ok := r.m.instances.Get(r.old.ID)
	require.True(t, ok)
	assert.Same(t, r.old, registered)
	assert.Equal(t, instance.StatusFailed, r.old.GetStatus())
	assert.Len(t, readPins(t), 1)
}

func TestReplacementCheckClonesMutableMaps(t *testing.T) {
	for _, reserve := range []bool{false, true} {
		t.Run(map[bool]string{false: "auto fraction", true: "reservation"}[reserve], func(t *testing.T) {
			r := newReplacementRig(t, 1000, 5000, false)
			features := LaunchParameters{declared: requireLaunchSchema(t, r.m, "generic"), Params: map[string]string{"budget": "auto"}, Environment: map[string]string{"CUDA_VISIBLE_DEVICES": "GPU-A"}, Commitments: map[string]int64{"GPU-stale": 17}}
			if reserve {
				r.setBudget(&schema.MemoryBudget{Kind: schema.MemoryReserveMiB, Parameter: "gpu-reserve-mib", ComplementParameter: "reserve-vram"})
				features.declared = requireLaunchSchema(t, r.m, "generic")
				features.Params = map[string]string{"gpu-reserve-mib": "5500"}
			}
			resolved := r.old.Resolved()
			resolved.Commitments = map[string]int64{"GPU-A": 5000}
			r.old.SetResolved(resolved)
			params, environment, commitments := maps.Clone(features.Params), maps.Clone(features.Environment), maps.Clone(features.Commitments)
			before := r.old.Resolved()
			check := &replacementCheck{run: r.old.ID, recovery: make(map[string]int64)}
			_, _, err := r.m.admitMemory(t.Context(), features, false, check)
			require.NoError(t, err)
			assert.Equal(t, params, features.Params)
			assert.Equal(t, environment, features.Environment)
			assert.Equal(t, commitments, features.Commitments)
			assert.Equal(t, before, r.old.Resolved())
			assert.Equal(t, int64(1000), r.before[0].FreeMiB)
			require.Len(t, r.before[0].Processes, 2)
		})
	}
}

func replacementWeights(t *testing.T, mib int64) {
	t.Helper()
	root := t.TempDir()
	previous, err := modelregistry.GetModelsRootDir()
	require.NoError(t, err)
	modelregistry.SetModelsRootDirOverride(root)
	t.Cleanup(func() { modelregistry.SetModelsRootDirOverride(previous) })
	dir := filepath.Join(root, "shared")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	weights := filepath.Join(dir, "weights.gguf")
	require.NoError(t, os.WriteFile(weights, nil, 0o600))
	require.NoError(t, os.Truncate(weights, mib*1024*1024))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"num_hidden_layers":32,"num_key_value_heads":8,"num_attention_heads":32,"hidden_size":4096,"torch_dtype":"float16"}`), 0o600))
}

func TestReplacementSizedNeedRefusesBeforeStop(t *testing.T) {
	r := newReplacementRig(t, 1000, 2000, true, func(svc *config.ServiceConfig) {
		svc.Defaults.Metadata = &config.AppMetadata{ContextParam: "max-model-len"}
	})
	r.setBudget(&schema.MemoryBudget{Kind: schema.MemoryFractionTotal, Parameter: "budget"}, "parameters:\n  max-model-len: {type: int}\n")
	replacementWeights(t, 1024)
	// Intrinsic context no-fit remains known even when the release bound is unknown.
	r.before[0].UsedMiB = nil
	req := r.request()
	req.Parameters = map[string]string{"budget": "0.2", "max-model-len": "2048"}
	_, err := r.m.EnsureRun(t.Context(), req)
	var fit *process.MemoryFitError
	require.ErrorAs(t, err, &fit)
	assert.True(t, fit.Sized)
	assert.Equal(t, r.old.ID, fit.KeptRun)
	assert.Equal(t, new(int64(0)), fit.CreditedMiB)
	assert.Contains(t, fit.Error(), "kept run "+r.old.ID)
	assert.Equal(t, instance.StatusRunning, r.old.GetStatus())
	assert.Zero(t, r.terms(t))
}

func TestReplacementUnavailableObservationKeepsStopOrder(t *testing.T) {
	r := newReplacementRig(t, 1000, 2000, false)
	r.m.memorySnapshot = func(context.Context) ([]gpu.MemoryDevice, error) { return nil, process.ErrMemoryObservation }
	_, err := r.replace(t)
	assert.ErrorIs(t, err, process.ErrMemoryObservation)
	assert.Equal(t, instance.StatusStopped, r.old.GetStatus())
}

func TestReplacementRecoveryWaitsForEveryDevice(t *testing.T) {
	r := newReplacementRig(t, 1000, 5000, false)
	r.setBudget(&schema.MemoryBudget{Kind: schema.MemoryFractionTotal, Parameter: "budget", DeviceCountParameter: "devices"})
	second := r.before[0]
	second.UUID, second.Index = "GPU-B", 1
	r.before = append(r.before, second)
	var recovered atomic.Int32
	r.m.memorySnapshot = func(ctx context.Context) ([]gpu.MemoryDevice, error) {
		if r.old.GetStatus() != instance.StatusStopped {
			return r.snapshot(ctx)
		}
		second := r.after[0]
		second.UUID, second.Index = "GPU-B", 1
		if recovered.Add(1) < 3 {
			second.FreeMiB = 1000
		}
		return []gpu.MemoryDevice{r.after[0], second}, nil
	}
	req := r.request()
	req.Parameters = map[string]string{"devices": "2"}
	req.EnvVars = map[string]string{"CUDA_VISIBLE_DEVICES": "GPU-A,GPU-B"}
	result, err := r.m.EnsureRun(t.Context(), req)
	require.NoError(t, err)
	awaitRunning(t, result.Run)
	assert.GreaterOrEqual(t, recovered.Load(), int32(4), "every credited device recovers before fresh admission")
	assert.Equal(t, instance.StatusStopped, r.old.GetStatus())
}

func TestReplacementLayersKeepsOtherCommitments(t *testing.T) {
	for _, evict := range []bool{false, true} {
		t.Run(map[bool]string{false: "keep ComfyUI", true: "evict idle ComfyUI"}[evict], func(t *testing.T) {
			r := newReplacementRig(t, 1000, 5000, false)
			r.setBudget(&schema.MemoryBudget{Kind: schema.MemoryLayers})
			r.m.idleEvictionDefault = evict
			if evict {
				r.other = replaceIdleHolder(t, r)
			}
			resolved := r.other.Resolved()
			resolved.Commitments = map[string]int64{"GPU-A": 6000}
			r.other.SetResolved(resolved)
			r.m.memorySnapshot = func(ctx context.Context) ([]gpu.MemoryDevice, error) {
				if r.other.GetStatus() == instance.StatusStopped {
					return []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: 10000}}, nil
				}
				return r.snapshot(ctx)
			}
			if evict {
				features, err := r.m.ResolveLaunchParameters(t.Context(), r.request())
				require.NoError(t, err)
				check := &replacementCheck{run: r.old.ID, recovery: make(map[string]int64)}
				_, _, err = r.m.admitMemory(t.Context(), features, false, check)
				require.NoError(t, err)
				assert.Equal(t, map[string]int64{"GPU-A": 6000 - memoryRecoveryToleranceMiB}, check.recovery)
				assert.Equal(t, instance.StatusRunning, r.old.GetStatus())
				assert.Equal(t, instance.StatusRunning, r.other.GetStatus())
			}
			run, err := r.replace(t)
			if evict {
				require.NoError(t, err)
				awaitRunning(t, run)
				assert.Equal(t, instance.StatusStopped, r.old.GetStatus())
				assert.Equal(t, instance.StatusStopped, r.other.GetStatus())
				require.Len(t, run.ToInfo("").EvictedRuns, 1)
			} else {
				var fit *process.MemoryFitError
				require.ErrorAs(t, err, &fit)
				assert.Equal(t, r.old.ID, fit.KeptRun)
				assert.Equal(t, new(int64(5000)), fit.CreditedMiB)
				assert.Equal(t, int64(4000), fit.FreeMiB)
				require.Len(t, fit.Holders, 2)
				assert.Equal(t, int64(2000), fit.Holders[1].UnusedCommitmentMiB)
				assert.Equal(t, instance.StatusRunning, r.old.GetStatus())
				assert.Zero(t, r.terms(t))
				lease, err := r.old.Acquire(t.Context(), nil)
				require.NoError(t, err)
				lease.Release()
			}
			assert.Equal(t, resolved, r.other.Resolved())
		})
	}
}

func TestReplacementPlansIdleEvictionsOnBothDevices(t *testing.T) {
	r := newReplacementRig(t, 1000, 2000, false)
	r.m.idleEvictionDefault = true
	r.other = replaceIdleHolder(t, r)
	r.setBudget(&schema.MemoryBudget{Kind: schema.MemoryFractionTotal, Parameter: "budget", DeviceCountParameter: "devices"})
	second := instance.NewInstance("second-holder", "other", "second-model", 0, time.Hour, 1)
	second.SetStatus(instance.StatusRunning)
	second.SetProcessInfo(9000002, 0, 0, nil)
	require.NoError(t, r.m.instances.Register(second))
	r.before = append(r.before, gpu.MemoryDevice{UUID: "GPU-B", Index: 1, TotalMiB: 10000, FreeMiB: 1000, UsedMiB: new(int64(9000)), Processes: []gpu.MemoryProcess{
		{PID: r.old.GetProcessID(), MemoryMiB: new(int64(2000))}, {PID: second.GetProcessID(), MemoryMiB: new(int64(7000))},
	}})
	r.m.memorySnapshot = func(ctx context.Context) ([]gpu.MemoryDevice, error) {
		devices, err := r.snapshot(ctx)
		devices = slices.Clone(devices)
		if r.old.GetStatus() == instance.StatusStopped {
			devices = append(devices, gpu.MemoryDevice{UUID: "GPU-B", Index: 1, TotalMiB: 10000, FreeMiB: 3000, UsedMiB: new(int64(7000)), Processes: r.before[1].Processes[1:]})
		}
		for i, holder := range []*instance.Instance{r.other, second} {
			if holder.GetStatus() == instance.StatusStopped {
				devices[i].FreeMiB = 10000
				devices[i].UsedMiB = new(int64(0))
				devices[i].Processes = nil
			}
		}
		return devices, err
	}
	req := r.request()
	req.Parameters = map[string]string{"devices": "2"}
	req.EnvVars = map[string]string{"CUDA_VISIBLE_DEVICES": "GPU-A,GPU-B"}
	_, err := r.m.EnsureRun(t.Context(), req)
	var fit *process.MemoryFitError
	require.ErrorAs(t, err, &fit)
	assert.Equal(t, r.old.ID, fit.KeptRun)
	assert.Equal(t, "GPU-B", fit.Device)
	assert.Equal(t, new(int64(2000)), fit.CreditedMiB)
	assert.Equal(t, int64(3000), fit.FreeMiB)
	for _, run := range []*instance.Instance{r.old, r.other, second} {
		assert.Equal(t, instance.StatusRunning, run.GetStatus())
		lease, err := run.Acquire(t.Context(), nil)
		require.NoError(t, err)
		lease.Release()
	}
	assert.Zero(t, r.terms(t))
}

func TestReplacementGPUSelectionRefusesBeforeStop(t *testing.T) {
	for _, kind := range []string{schema.MemoryFractionTotal, schema.MemoryLayers} {
		for _, selector := range []string{"0", "GPU-unknown", "GPU-A,GPU-A"} {
			t.Run(kind+"/"+selector, func(t *testing.T) {
				r := newReplacementRig(t, 1000, 5000, true)
				r.setBudget(&schema.MemoryBudget{Kind: kind, Parameter: "budget"})
				if kind == schema.MemoryLayers {
					resolved := r.other.Resolved()
					resolved.Commitments = map[string]int64{"GPU-A": 2000}
					r.other.SetResolved(resolved)
				}
				r.before = append(r.before, gpu.MemoryDevice{UUID: "GPU-B", Index: 1, TotalMiB: 10000, FreeMiB: 10000})
				r.after = append(r.after, r.before[1])
				req := r.request()
				req.EnvVars = map[string]string{"CUDA_VISIBLE_DEVICES": selector}
				if selector == "GPU-A,GPU-A" {
					r.setBudget(&schema.MemoryBudget{Kind: kind, Parameter: "budget", DeviceCountParameter: "devices"})
					req.Parameters = map[string]string{"devices": "2"}
				}
				_, err := r.m.EnsureRun(t.Context(), req)
				assert.ErrorIs(t, err, process.ErrMemorySelection)
				assert.ErrorIs(t, err, process.ErrMemoryObservation)
				assert.Equal(t, instance.StatusRunning, r.old.GetStatus())
				assert.Zero(t, r.terms(t))
				lease, err := r.old.Acquire(t.Context(), nil)
				require.NoError(t, err)
				lease.Release()
			})
		}
	}
}

func TestReplacementFailedProcessListingKeepsStopOrder(t *testing.T) {
	r := newReplacementRig(t, 2000, 5000, false)
	r.setBudget(&schema.MemoryBudget{Kind: schema.MemoryLayers})
	r.other = replaceIdleHolder(t, r)
	resolved := r.other.Resolved()
	resolved.Commitments = map[string]int64{"GPU-A": 2000}
	r.other.SetResolved(resolved)
	r.other.ObserveActivity(true)
	// R uses 5000 MiB, ComfyUI 2500 MiB, and the driver 500 MiB, but the listing failed.
	r.before = []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: 2000, UsedMiB: new(int64(8000)), ProcessReason: "driver listing unavailable"}}
	r.after = []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: 7000, UsedMiB: new(int64(3000)), ProcessReason: "driver listing unavailable"}}
	run, err := r.replace(t)
	require.NoError(t, err)
	awaitRunning(t, run)
	assert.Equal(t, instance.StatusStopped, r.old.GetStatus())
	assert.Equal(t, instance.StatusRunning, r.other.GetStatus())
	assert.Equal(t, resolved, r.other.Resolved())
	assert.Empty(t, run.ToInfo("").EvictedRuns)
	assert.Equal(t, 1, r.terms(t))
}

func TestReplacementCommitmentOnlyUnknownBoundKeepsRun(t *testing.T) {
	for _, evidence := range []string{"unknown used memory", "failed process listing"} {
		for _, evict := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/evict=%t", evidence, evict), func(t *testing.T) {
				r := newReplacementRig(t, 1000, 5000, true)
				r.setBudget(&schema.MemoryBudget{Kind: schema.MemoryLayers})
				r.m.idleEvictionDefault = evict
				comfy := instance.NewInstance("comfy-holder", "comfy", "", 0, time.Hour, 1)
				comfy.SetStatus(instance.StatusRunning)
				comfy.SetProcessInfo(9000003, 0, 0, nil)
				comfy.SetResolved(instance.Resolved{Commitments: map[string]int64{"GPU-A": 2000}})
				comfy.ObserveActivity(true)
				require.NoError(t, r.m.instances.Register(comfy))
				free, required, unused := int64(4000), int64(6000), int64(2000)
				if evidence == "unknown used memory" {
					r.before[0].UsedMiB, r.after[0].UsedMiB = nil, nil
				} else {
					// R uses 1000 MiB beside 2000 MiB of other use, but the listing failed.
					comfy.SetResolved(instance.Resolved{Commitments: map[string]int64{"GPU-A": 4000}})
					r.before = []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: 7000, UsedMiB: new(int64(3000)), ProcessReason: "listing failed"}}
					r.after = []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: 8000, UsedMiB: new(int64(2000)), ProcessReason: "listing failed"}}
					free, required, unused = 6000, 7000, 1000
				}
				pins, resolved := readPins(t), comfy.Resolved()
				_, err := r.replace(t)
				var fit *process.MemoryFitError
				require.ErrorAs(t, err, &fit)
				assert.True(t, fit.CommitmentOnly)
				assert.Equal(t, r.old.ID, fit.KeptRun)
				assert.Equal(t, new(int64(0)), fit.CreditedMiB)
				assert.Contains(t, fit.Error(), "kept run "+r.old.ID+"; replacement credited 0 MiB")
				assert.Equal(t, free, fit.FreeMiB)
				assert.Equal(t, required, fit.RequiredMiB)
				var commitment int64
				for _, holder := range fit.Holders {
					assert.NotEqual(t, r.old.ID, holder.Run)
					if holder.Run == comfy.ID {
						commitment += holder.UnusedCommitmentMiB
					}
				}
				assert.Equal(t, unused, commitment)
				registered, ok := r.m.instances.Get(r.old.ID)
				require.True(t, ok)
				assert.Same(t, r.old, registered)
				for _, run := range []*instance.Instance{r.old, r.other, comfy} {
					assert.Equal(t, instance.StatusRunning, run.GetStatus())
					lease, err := run.Acquire(t.Context(), nil)
					require.NoError(t, err)
					lease.Release()
				}
				assert.Equal(t, resolved, comfy.Resolved())
				assert.Equal(t, pins, readPins(t))
				assert.Zero(t, r.terms(t))
			})
		}
	}
}

func TestReplacementLayersStopsAfterOneEvictionPlan(t *testing.T) {
	testLayersCommitmentReclaim(t, true, true)
}

func TestFreshLayersEvictsOnlyUnusedCommitmentHolder(t *testing.T) {
	testLayersCommitmentReclaim(t, false, true)
}

func TestCommitmentOnlyRefusalWithoutIdleHolderEvictsNothing(t *testing.T) {
	for _, replacing := range []bool{false, true} {
		t.Run(fmt.Sprintf("replacement=%t", replacing), func(t *testing.T) {
			testLayersCommitmentReclaim(t, replacing, false)
		})
	}
}

func testLayersCommitmentReclaim(t *testing.T, replacing, idle bool) {
	t.Helper()
	r := newReplacementRig(t, 2000, 5000, false)
	r.setBudget(&schema.MemoryBudget{Kind: schema.MemoryLayers})
	r.m.idleEvictionDefault = true
	r.other = replaceIdleHolder(t, r)
	restore := utils.SetClock(utils.FixedClock(utils.Now().Add(-time.Hour)))
	r.other.StampActivityWithHints(nil)
	restore()
	comfy := instance.NewInstance("comfy-holder", "comfy", "", 0, time.Hour, 1)
	comfy.SetStatus(instance.StatusRunning)
	comfy.SetProcessInfo(9000003, 0, 0, nil)
	comfy.SetResolved(instance.Resolved{Commitments: map[string]int64{"GPU-A": 2000}})
	comfy.StampActivityWithHints(nil)
	comfy.ObserveActivity(!idle)
	require.NoError(t, r.m.instances.Register(comfy))
	require.True(t, r.other.GetLastActivity().Before(comfy.GetLastActivity()))
	req := r.request()
	if !replacing {
		req.Model, req.Force = "fresh", false
	}
	result, err := r.m.EnsureRun(t.Context(), req)
	if idle {
		require.NoError(t, err)
		awaitRunning(t, result.Run)
		assert.Equal(t, []instance.EvictedRun{{RunID: comfy.ID, Provider: comfy.Provider, FreedMiB: 2000}}, result.Run.ToInfo("").EvictedRuns)
		assert.Equal(t, instance.StatusStopped, comfy.GetStatus())
	} else {
		var fit *process.MemoryFitError
		require.ErrorAs(t, err, &fit)
		assert.True(t, fit.CommitmentOnly)
		if replacing {
			assert.Equal(t, r.old.ID, fit.KeptRun)
			assert.Equal(t, new(int64(5000)), fit.CreditedMiB)
		} else {
			assert.Empty(t, fit.KeptRun)
		}
		assert.Equal(t, instance.StatusRunning, comfy.GetStatus())
	}
	if idle && replacing {
		assert.Equal(t, instance.StatusStopped, r.old.GetStatus())
		assert.Equal(t, 1, r.terms(t))
	} else {
		assert.Equal(t, instance.StatusRunning, r.old.GetStatus())
		lease, err := r.old.Acquire(t.Context(), nil)
		require.NoError(t, err)
		lease.Release()
		assert.Zero(t, r.terms(t))
	}
	assert.Equal(t, instance.StatusRunning, r.other.GetStatus())
	lease, err := r.other.Acquire(t.Context(), nil)
	require.NoError(t, err)
	lease.Release()
}

func TestFreshLayersAndUnifiedGPUSelectionWithoutCommitments(t *testing.T) {
	for _, kind := range []string{schema.MemoryLayers, schema.MemoryUnified} {
		for _, evict := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/evict=%t", kind, evict), func(t *testing.T) {
				t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
				selector := "0"
				if kind == schema.MemoryUnified {
					selector = "GPU-unknown"
				}
				t.Setenv("CUDA_VISIBLE_DEVICES", selector)
				r := newContractRig(t, false, WithIdleMemoryEviction(evict), WithProviderSchema(memoryBudgetSchema(func(string) (*schema.MemoryBudget, error) {
					return &schema.MemoryBudget{Kind: kind}, nil
				})))
				r.m.memorySnapshot = func(context.Context) ([]gpu.MemoryDevice, error) {
					return []gpu.MemoryDevice{
						{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: 9000, UsedMiB: new(int64(1000))},
						{UUID: "GPU-B", Index: 1, TotalMiB: 10000, FreeMiB: 9000, UsedMiB: new(int64(1000))},
					}, nil
				}
				r.m.memorySystem = func(context.Context) *hardware.HardwareInfo {
					return &hardware.HardwareInfo{TotalRAM: 10000 * 1024 * 1024}
				}
				result, err := r.m.EnsureRun(t.Context(), EnsureRequest{Provider: "generic", Model: "shared"})
				require.NoError(t, err)
				awaitRunning(t, result.Run)
				assert.Empty(t, result.Run.ToInfo("").EvictedRuns)
				assert.Zero(t, r.terms(t))
			})
		}
	}
}

func TestReplacementRefusalDropsCreditedHolders(t *testing.T) {
	for _, evidence := range []string{"unknown size", "unreadable ancestry"} {
		t.Run(evidence, func(t *testing.T) {
			r := newReplacementRig(t, 1000, 2000, false)
			// 9000 MiB used plus 1000 MiB free leaves 640 MiB driver reservation.
			r.before[0].TotalMiB = 10640
			r.before[0].Processes[1].MemoryMiB = new(int64(4000))
			ambiguous := gpu.MemoryProcess{PID: os.Getpid()}
			if evidence == "unreadable ancestry" {
				ambiguous = gpu.MemoryProcess{PID: 9000003, MemoryMiB: new(int64(3000))}
			}
			r.before[0].Processes = append(r.before[0].Processes, ambiguous)
			req := r.request()
			req.Parameters = map[string]string{"budget": "0.95"}
			_, err := r.m.EnsureRun(t.Context(), req)
			var fit *process.MemoryFitError
			require.ErrorAs(t, err, &fit)
			assert.Equal(t, r.old.ID, fit.KeptRun)
			assert.Equal(t, new(int64(5000)), fit.CreditedMiB)
			assert.Equal(t, int64(6000), fit.FreeMiB)
			require.Len(t, fit.Holders, 1)
			assert.Equal(t, r.other.ID, fit.Holders[0].Run)
			assert.Equal(t, instance.StatusRunning, r.old.GetStatus())
			assert.Zero(t, r.terms(t))
		})
	}
}
