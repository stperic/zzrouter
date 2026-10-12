package prov_apps

import (
	"context"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/discovery/gpu"
	obsruns "github.com/stperic/zzrouter/pkg/observability/runs"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/keepalive"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type reclaimFixture struct {
	mgr      *ProviderAppManager
	older    *instance.Instance
	newer    *instance.Instance
	features LaunchParameters
	device   gpu.MemoryDevice
}

func newReclaimFixture(t *testing.T, enabled bool) *reclaimFixture {
	t.Helper()
	mgr, err := NewProviderAppManager(testAppsConfig(), WithIdleMemoryEviction(enabled), WithMemoryBudget(func(string) (*schema.MemoryBudget, error) {
		return &schema.MemoryBudget{Kind: "fraction_total", Parameter: "budget"}, nil
	}))
	require.NoError(t, err)
	cleanupProviderManager(t, mgr)
	old := instance.NewInstance("older", "vllm", "old-model", 0, -1, 10)
	old.SetStatus(instance.StatusRunning)
	old.SetProcessInfo(9000001, 0, nil)
	restore := utils.SetClock(utils.FixedClock(utils.Now().Add(-time.Hour)))
	old.StampActivityWithHints(nil)
	restore()
	newer := instance.NewInstance("newer", "vllm", "new-model", 0, -1, 10)
	newer.SetStatus(instance.StatusRunning)
	newer.SetProcessInfo(9000002, 0, nil)
	newer.StampActivityWithHints(nil)
	require.NoError(t, mgr.instances.Register(old))
	require.NoError(t, mgr.instances.Register(newer))
	f := &reclaimFixture{mgr: mgr, older: old, newer: newer, features: LaunchParameters{Params: map[string]string{"budget": "0.5"}}, device: gpu.MemoryDevice{
		UUID: "GPU-A", TotalMiB: 10000, FreeMiB: 1000,
		Processes: []gpu.MemoryProcess{{PID: 9000001, MemoryMiB: new(int64(4500))}, {PID: 9000002, MemoryMiB: new(int64(4500))}},
	}}
	mgr.memorySnapshot = func(context.Context) ([]gpu.MemoryDevice, error) {
		device := f.device
		if old.GetStatus() == instance.StatusStopped || newer.GetStatus() == instance.StatusStopped {
			device.FreeMiB = 5500
		}
		return []gpu.MemoryDevice{device}, nil
	}
	return f
}

func (f *reclaimFixture) admit(t *testing.T) ([]instance.EvictedRun, error) {
	t.Helper()
	f.mgr.memoryAdmission <- struct{}{}
	defer func() { <-f.mgr.memoryAdmission }()
	return f.mgr.admitMemory(t.Context(), "vllm", f.features)
}

func (f *reclaimFixture) assertOriginalFit(t *testing.T, err error) {
	t.Helper()
	var fit *process.MemoryFitError
	require.ErrorAs(t, err, &fit)
	assert.Equal(t, &process.MemoryFitError{Parameter: "budget", Device: "GPU-A", RequiredMiB: 5000, FreeMiB: f.device.FreeMiB,
		Holders: []process.MemoryHolder{
			{MemoryProcess: f.device.Processes[0], Run: f.older.ID, Model: f.older.Model, Ownership: "zzrouter"},
			{MemoryProcess: f.device.Processes[1], Run: f.newer.ID, Model: f.newer.Model, Ownership: "zzrouter"},
		}}, fit)
}

// assertNothingEvicted admits and expects the original fit error with no run evicted.
func (f *reclaimFixture) assertNothingEvicted(t *testing.T) {
	t.Helper()
	evicted, err := f.admit(t)
	f.assertOriginalFit(t, err)
	assert.Empty(t, evicted)
}

func TestMemoryReclaimDisabled(t *testing.T) {
	f := newReclaimFixture(t, false)
	f.assertNothingEvicted(t)
	assert.Equal(t, instance.StatusRunning, f.older.GetStatus())
	assert.Equal(t, instance.StatusRunning, f.newer.GetStatus())
}

func TestMemoryReclaimOldestMinimum(t *testing.T) {
	f := newReclaimFixture(t, true)
	evicted, err := f.admit(t)
	require.NoError(t, err)
	assert.Equal(t, []instance.EvictedRun{{RunID: f.older.ID, Model: f.older.Model, FreedMiB: 4500}}, evicted)
	assert.Equal(t, instance.StatusStopped, f.older.GetStatus())
	assert.Equal(t, instance.StatusRunning, f.newer.GetStatus())
	assert.False(t, f.mgr.instances.Has(f.older.ID))
	var events []EventType
	var reason obsruns.StopReason
	for len(f.mgr.auditEvents) > 0 {
		event := <-f.mgr.auditEvents
		if event.Instance == f.older.ID {
			events = append(events, event.Type)
			reason = event.Reason
		}
	}
	assert.Equal(t, []EventType{EventInstanceStopping, EventInstanceStopped}, events)
	assert.Equal(t, obsruns.StopReasonMemoryPressure, reason, "the stopped event says why the run ended")
}

func TestMemoryReclaimProtectsLease(t *testing.T) {
	f := newReclaimFixture(t, true)
	lease, err := f.older.Acquire(t.Context(), nil)
	require.NoError(t, err)
	defer lease.Release()
	*f.device.Processes[1].MemoryMiB = 100
	f.assertNothingEvicted(t)
	assert.Equal(t, instance.StatusRunning, f.older.GetStatus())
	assert.Equal(t, instance.StatusRunning, f.newer.GetStatus())
}

func TestMemoryReclaimInsufficientStopsNothing(t *testing.T) {
	f := newReclaimFixture(t, true)
	*f.device.Processes[0].MemoryMiB = 1500
	*f.device.Processes[1].MemoryMiB = 1500
	f.assertNothingEvicted(t)
	assert.Equal(t, instance.StatusRunning, f.older.GetStatus())
	assert.Equal(t, instance.StatusRunning, f.newer.GetStatus())
}

func TestMemoryReclaimWaitsForRecovery(t *testing.T) {
	for _, recoverMemory := range []bool{true, false} {
		t.Run(map[bool]string{true: "late", false: "never"}[recoverMemory], func(t *testing.T) {
			f := newReclaimFixture(t, true)
			attempts, polls := 0, 0
			f.mgr.memorySnapshot = func(ctx context.Context) ([]gpu.MemoryDevice, error) {
				device := f.device
				if ctx == t.Context() {
					attempts++
				} else {
					polls++
				}
				if recoverMemory && polls >= 3 {
					device.FreeMiB = 5500
				}
				return []gpu.MemoryDevice{device}, nil
			}
			started := utils.Now()
			_, err := f.admit(t)
			if recoverMemory {
				require.NoError(t, err)
				assert.GreaterOrEqual(t, polls, 3)
			} else {
				var fit *process.MemoryFitError
				require.ErrorAs(t, err, &fit)
				assert.Equal(t, int64(1000), fit.FreeMiB)
				assert.Equal(t, int64(5000), fit.RequiredMiB)
				assert.GreaterOrEqual(t, utils.Now().Sub(started), memoryRecoveryTimeout)
			}
			assert.Equal(t, 2, attempts, "exactly one retry after reclamation")
			assert.Less(t, utils.Now().Sub(started), memoryRecoveryTimeout+2*time.Second)
			assert.Equal(t, instance.StatusStopped, f.older.GetStatus())
			assert.Equal(t, instance.StatusRunning, f.newer.GetStatus())
		})
	}
}

func TestMemoryReclaimEligibility(t *testing.T) {
	for _, state := range []string{"starting", "waiter", "foreign", "unknown"} {
		t.Run(state, func(t *testing.T) {
			f := newReclaimFixture(t, true)
			*f.device.Processes[1].MemoryMiB = 100
			switch state {
			case "starting":
				f.older.SetStatus(instance.StatusStarting)
			case "waiter":
				waiting := instance.NewInstance("waiting", "vllm", "waiting-model", 0, -1, 10)
				waiting.SetWaitingOn(f.older.ID)
				require.NoError(t, f.mgr.instances.Register(waiting))
			case "foreign":
				f.older.SetProcessInfo(0, 0, nil)
			case "unknown":
				f.device.Processes[0].MemoryMiB = nil
			}
			var fit *process.MemoryFitError
			evicted, err := f.admit(t)
			require.ErrorAs(t, err, &fit)
			assert.Empty(t, evicted)
			assert.NotEqual(t, instance.StatusStopped, f.older.GetStatus())
			assert.Equal(t, instance.StatusRunning, f.newer.GetStatus())
		})
	}
}

func TestMemoryReclaimAggregatesHolders(t *testing.T) {
	f := newReclaimFixture(t, true)
	*f.device.Processes[0].MemoryMiB = 2000
	fit := &process.MemoryFitError{Device: "GPU-A", FreeMiB: 1000, RequiredMiB: 5000, Holders: []process.MemoryHolder{
		{Run: f.older.ID, Ownership: "zzrouter", MemoryProcess: gpu.MemoryProcess{MemoryMiB: new(int64(2000))}},
		{Run: f.older.ID, Ownership: "zzrouter", MemoryProcess: gpu.MemoryProcess{MemoryMiB: new(int64(2500))}},
	}}
	assert.Equal(t, []instance.EvictedRun{{RunID: f.older.ID, Model: f.older.Model, FreedMiB: 4500}}, f.mgr.reclaimFor(t.Context(), fit))
	assert.Equal(t, instance.StatusStopped, f.older.GetStatus())
	assert.Equal(t, instance.StatusRunning, f.newer.GetStatus())
}

func TestIdleReaperUsesEvictable(t *testing.T) {
	f := newReclaimFixture(t, false)
	lease, err := f.newer.Acquire(t.Context(), nil)
	require.NoError(t, err)
	defer lease.Release()
	zero := time.Duration(0)
	f.older.StampActivityWithHints(&keepalive.Override{Duration: &zero})
	f.newer.StampActivityWithHints(&keepalive.Override{Duration: &zero})
	f.mgr.Start(t.Context())
	leaseOld, err := f.older.Acquire(t.Context(), nil)
	require.NoError(t, err)
	leaseOld.Release()
	eventually(t, func() bool { return !f.mgr.instances.Has(f.older.ID) }, "expired idle run stopped")
	assert.Equal(t, instance.StatusRunning, f.newer.GetStatus())
}

func TestMemoryRecoveryUsesDeviceAndBaseline(t *testing.T) {
	f := newReclaimFixture(t, true)
	polls := 0
	f.mgr.memorySnapshot = func(context.Context) ([]gpu.MemoryDevice, error) {
		polls++
		free := int64(5000)
		if polls >= 3 {
			free = 5500
		}
		return []gpu.MemoryDevice{{UUID: "GPU-other", FreeMiB: 10000}, {UUID: "GPU-A", FreeMiB: free}}, nil
	}
	f.mgr.waitMemoryRecovery(t.Context(), "GPU-A", 1000+4500-memoryRecoveryToleranceMiB)
	assert.Equal(t, 3, polls, "wait for held sizes, not merely enough memory for admission")
}

func TestIdleStopAndLeaseClaim(t *testing.T) {
	f := newReclaimFixture(t, true)
	lease, err := f.older.Acquire(t.Context(), nil)
	require.NoError(t, err)
	_, claimed := instance.ClaimIdleStops([]instance.IdleStopCandidate{{Instance: f.older, Activity: f.older.GetLastActivity()}})
	assert.False(t, claimed)
	lease.Release()
	release, claimed := instance.ClaimIdleStops([]instance.IdleStopCandidate{{Instance: f.older, Activity: f.older.GetLastActivity()}})
	require.True(t, claimed)
	defer release()
	_, err = f.older.Acquire(t.Context(), nil)
	assert.ErrorIs(t, err, instance.ErrStopping)
}

func TestMemoryReclaimUnknownHolderCountsZero(t *testing.T) {
	f := newReclaimFixture(t, true)
	f.device.Processes[0].MemoryMiB = nil
	_, err := f.admit(t)
	require.NoError(t, err)
	assert.Equal(t, instance.StatusStopped, f.older.GetStatus())
	assert.Equal(t, instance.StatusStopped, f.newer.GetStatus(), "unknown oldest holder cannot cover the shortfall")
}

func TestMemoryReclaimCancellationBoundsRecovery(t *testing.T) {
	f := newReclaimFixture(t, true)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	attempts := 0
	f.mgr.memorySnapshot = func(snapshotCtx context.Context) ([]gpu.MemoryDevice, error) {
		if snapshotCtx == ctx {
			attempts++
			return []gpu.MemoryDevice{f.device}, nil
		}
		cancel()
		<-snapshotCtx.Done()
		return nil, snapshotCtx.Err()
	}
	f.mgr.memoryAdmission <- struct{}{}
	defer func() { <-f.mgr.memoryAdmission }()
	_, err := f.mgr.admitMemory(ctx, "vllm", f.features)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 2, attempts)
	assert.Equal(t, instance.StatusStopped, f.older.GetStatus())
}

func TestMemoryReclaimReservesWholeSetBeforeDrain(t *testing.T) {
	f := newReclaimFixture(t, true)
	*f.device.Processes[0].MemoryMiB = 2000
	*f.device.Processes[1].MemoryMiB = 2500
	attempted := make(chan error, 1)
	f.older.TrackGoroutine()
	go func() {
		defer f.older.GoroutineDone()
		<-f.older.Context().Done()
		lease, err := f.newer.Acquire(t.Context(), nil)
		if lease != nil {
			lease.Release()
		}
		attempted <- err
	}()
	_, err := f.admit(t)
	require.NoError(t, err)
	assert.ErrorIs(t, <-attempted, instance.ErrStopping, "the later victim is reserved while the first drains")
	assert.Equal(t, instance.StatusStopped, f.older.GetStatus())
	assert.Equal(t, instance.StatusStopped, f.newer.GetStatus())
}

func TestMemoryReclaimSettingOverridesLegacyDefault(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "off", true: "on"}[enabled], func(t *testing.T) {
			f := newReclaimFixture(t, !enabled)
			f.features.Params["evict-idle-on-memory-pressure"] = map[bool]string{false: "false", true: "true"}[enabled]
			evicted, err := f.admit(t)
			if enabled {
				require.NoError(t, err)
				assert.Equal(t, []instance.EvictedRun{{RunID: f.older.ID, Model: f.older.Model, FreedMiB: 4500}}, evicted)
				assert.Equal(t, instance.StatusStopped, f.older.GetStatus())
			} else {
				f.assertOriginalFit(t, err)
				assert.Empty(t, evicted)
				assert.Equal(t, instance.StatusRunning, f.older.GetStatus())
			}
		})
	}
}

func TestIdleReaperUnblockedByDependantAndClaim(t *testing.T) {
	for _, block := range []string{"dependant", "claim"} {
		t.Run(block, func(t *testing.T) {
			f := newReclaimFixture(t, false)
			zero := time.Duration(0)
			lease, err := f.older.Acquire(t.Context(), &keepalive.Override{Duration: &zero})
			require.NoError(t, err)
			lease.Release()
			var unblock func()
			if block == "dependant" {
				waiting := instance.NewInstance("waiting", "vllm", "pending", 0, -1, 1)
				waiting.SetWaitingOn(f.older.ID)
				require.NoError(t, f.mgr.instances.Register(waiting))
				unblock = func() { waiting.Admit() }
			} else {
				release, claimed := instance.ClaimIdleStops([]instance.IdleStopCandidate{{Instance: f.older, Activity: f.older.GetLastActivity()}})
				require.True(t, claimed)
				unblock = release
			}
			f.mgr.Start(t.Context())
			time.Sleep(25 * time.Millisecond)
			assert.Equal(t, instance.StatusRunning, f.older.GetStatus())
			unblock()
			eventually(t, func() bool { return !f.mgr.instances.Has(f.older.ID) }, "removing idle protection must wake expiry")
		})
	}
}
