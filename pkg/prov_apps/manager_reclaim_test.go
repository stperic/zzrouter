package prov_apps

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/constants"
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

func newReclaimFixture(t *testing.T, enabled bool, keepAlive ...time.Duration) *reclaimFixture {
	t.Helper()
	mgr, err := NewProviderAppManager(testAppsConfig(), WithIdleMemoryEviction(enabled), WithProviderSchema(memoryBudgetSchema(func(string) (*schema.MemoryBudget, error) {
		return &schema.MemoryBudget{Kind: "fraction_total", Parameter: "budget"}, nil
	})))
	require.NoError(t, err)
	cleanupProviderManager(t, mgr)
	limit := 24 * time.Hour
	if len(keepAlive) > 0 {
		limit = keepAlive[0]
	}
	old := instance.NewInstance("older", "vllm", "old-model", 0, limit, 10)
	old.SetStatus(instance.StatusRunning)
	old.SetProcessInfo(9000001, 0, 0, nil)
	restore := utils.SetClock(utils.FixedClock(utils.Now().Add(-time.Hour)))
	old.StampActivityWithHints(nil)
	restore()
	newer := instance.NewInstance("newer", "vllm", "new-model", 0, 24*time.Hour, 10)
	newer.SetStatus(instance.StatusRunning)
	newer.SetProcessInfo(9000002, 0, 0, nil)
	newer.StampActivityWithHints(nil)
	require.NoError(t, mgr.instances.Register(old))
	require.NoError(t, mgr.instances.Register(newer))
	f := &reclaimFixture{mgr: mgr, older: old, newer: newer, features: LaunchParameters{declared: requireLaunchSchema(t, mgr, "vllm"), Params: map[string]string{"budget": "0.5"}}, device: gpu.MemoryDevice{
		UUID: "GPU-A", TotalMiB: 10000, FreeMiB: 1000, UsedMiB: new(int64(9000)),
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
	_, evicted, err := f.mgr.admitMemory(t.Context(), f.features, false, nil)
	return evicted, err
}

func (f *reclaimFixture) assertOriginalFit(t *testing.T, err error, skipped map[string]string) {
	t.Helper()
	var fit *process.MemoryFitError
	require.ErrorAs(t, err, &fit)
	assert.Equal(t, &process.MemoryFitError{Parameter: "budget", Device: "GPU-A", RequiredMiB: 5000, FreeMiB: f.device.FreeMiB,
		Holders: []process.MemoryHolder{
			{PID: f.device.Processes[0].PID, MemoryMiB: f.device.Processes[0].MemoryMiB, Run: f.older.ID, Model: f.older.Model, Ownership: "zzrouter", ReclaimSkippedReason: skipped[f.older.ID]},
			{PID: f.device.Processes[1].PID, MemoryMiB: f.device.Processes[1].MemoryMiB, Run: f.newer.ID, Model: f.newer.Model, Ownership: "zzrouter", ReclaimSkippedReason: skipped[f.newer.ID]},
		}}, fit)
}

// assertNothingEvicted admits and expects the original fit error with no run evicted.
func (f *reclaimFixture) assertNothingEvicted(t *testing.T, skipped map[string]string) {
	t.Helper()
	evicted, err := f.admit(t)
	f.assertOriginalFit(t, err, skipped)
	assert.Empty(t, evicted)
}

func TestMemoryReclaimDisabled(t *testing.T) {
	f := newReclaimFixture(t, false)
	f.assertNothingEvicted(t, nil)
	assert.Equal(t, instance.StatusRunning, f.older.GetStatus())
	assert.Equal(t, instance.StatusRunning, f.newer.GetStatus())
}

func TestMemoryReclaimOldestMinimum(t *testing.T) {
	f := newReclaimFixture(t, true)
	evicted, err := f.admit(t)
	require.NoError(t, err)
	assert.Equal(t, []instance.EvictedRun{{RunID: f.older.ID, Model: f.older.Model, Provider: f.older.Provider, FreedMiB: 4500}}, evicted)
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
	f.assertNothingEvicted(t, map[string]string{f.older.ID: "in use"})
	assert.Equal(t, instance.StatusRunning, f.older.GetStatus())
	assert.Equal(t, instance.StatusRunning, f.newer.GetStatus())
}

func TestMemoryReclaimInsufficientStopsNothing(t *testing.T) {
	f := newReclaimFixture(t, true)
	*f.device.Processes[0].MemoryMiB = 1500
	*f.device.Processes[1].MemoryMiB = 1500
	f.assertNothingEvicted(t, nil)
	assert.Equal(t, instance.StatusRunning, f.older.GetStatus())
	assert.Equal(t, instance.StatusRunning, f.newer.GetStatus())
}

func TestMemoryReclaimAutoFractionInsufficientStopsNothing(t *testing.T) {
	f := newReclaimFixture(t, true)
	f.features.Params["budget"] = "auto"
	f.device.TotalMiB, f.device.FreeMiB, f.device.UsedMiB = 20000, 2048, new(int64(17952))
	*f.device.Processes[0].MemoryMiB, *f.device.Processes[1].MemoryMiB = 1, 0
	f.mgr.memorySnapshot = func(context.Context) ([]gpu.MemoryDevice, error) {
		device := f.device
		if f.older.GetStatus() == instance.StatusStopped {
			device.FreeMiB++
		}
		return []gpu.MemoryDevice{device}, nil
	}
	evicted, err := f.admit(t)
	var fit *process.MemoryFitError
	require.ErrorAs(t, err, &fit)
	assert.Equal(t, int64(2050), fit.RequiredMiB)
	assert.Equal(t, int64(2048), fit.FreeMiB)
	assert.Empty(t, evicted, "one MiB covers margin+1, but cannot yield a positive auto fraction")
	assert.Equal(t, instance.StatusRunning, f.older.GetStatus())
	assert.Equal(t, instance.StatusRunning, f.newer.GetStatus())
}

func TestMemoryReclaimDoesNotCreditUnaccountedMemory(t *testing.T) {
	f := newReclaimFixture(t, true)
	*f.device.Processes[0].MemoryMiB = 2000
	*f.device.Processes[1].MemoryMiB = 2000
	evicted, err := f.admit(t)
	require.NoError(t, err)
	assert.Equal(t, []instance.EvictedRun{
		{RunID: f.older.ID, Model: f.older.Model, Provider: f.older.Provider, FreedMiB: 2000},
		{RunID: f.newer.ID, Model: f.newer.Model, Provider: f.newer.Provider, FreedMiB: 2000},
	}, evicted, "unaccounted use cannot replace either victim's attributed release")
	assert.Equal(t, instance.StatusStopped, f.older.GetStatus())
	assert.Equal(t, instance.StatusStopped, f.newer.GetStatus())
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
				assert.GreaterOrEqual(t, utils.Now().Sub(started), constants.MemoryRecoveryTimeout)
			}
			assert.Equal(t, 2, attempts, "exactly one retry after reclamation")
			assert.Less(t, utils.Now().Sub(started), constants.MemoryRecoveryTimeout+2*time.Second)
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
				waiting := instance.NewInstance("waiting", "vllm", "waiting-model", 0, 24*time.Hour, 10)
				waiting.SetWaitingOn(f.older.ID)
				require.NoError(t, f.mgr.instances.Register(waiting))
			case "foreign":
				f.older.SetProcessInfo(0, 0, 0, nil)
			case "unknown":
				f.device.Processes[0].MemoryMiB = nil
			}
			var fit *process.MemoryFitError
			evicted, err := f.admit(t)
			require.ErrorAs(t, err, &fit)
			assert.Empty(t, evicted)
			reason := map[string]string{"starting": "not idle", "waiter": "in use"}[state]
			assert.Equal(t, reason, fit.Holders[0].ReclaimSkippedReason)
			if reason != "" {
				assert.Contains(t, fit.Error(), "reclaim skipped: "+reason)
			}
			assert.NotEqual(t, instance.StatusStopped, f.older.GetStatus())
			assert.Equal(t, instance.StatusRunning, f.newer.GetStatus())
		})
	}
}

func TestMemoryReclaimAggregatesHolders(t *testing.T) {
	f := newReclaimFixture(t, true)
	*f.device.Processes[0].MemoryMiB = 2000
	fit := &process.MemoryFitError{Device: "GPU-A", FreeMiB: 1000, RequiredMiB: 5000, Holders: []process.MemoryHolder{
		{Run: f.older.ID, Ownership: "zzrouter", MemoryMiB: new(int64(2000))},
		{Run: f.older.ID, Ownership: "zzrouter", MemoryMiB: new(int64(2500))},
	}}
	evicted, skipped := f.mgr.reclaimFor(t.Context(), fit)
	assert.Equal(t, []instance.EvictedRun{{RunID: f.older.ID, Model: f.older.Model, Provider: f.older.Provider, FreedMiB: 4500}}, evicted)
	assert.Empty(t, skipped)
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
	require.NoError(t, f.mgr.waitMemoryRecovery(t.Context(), "GPU-A", 1000+4500-memoryRecoveryToleranceMiB))
	assert.Equal(t, 3, polls, "wait for held sizes, not merely enough memory for admission")
}

func TestMemoryRecoveryCallerCancellation(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(cause.Error(), func(t *testing.T) {
			for _, recovered := range []bool{false, true} {
				t.Run(map[bool]string{false: "pending", true: "recovered"}[recovered], func(t *testing.T) {
					f := newReclaimFixture(t, true)
					ctx, cancel := context.WithCancel(t.Context())
					if cause == context.DeadlineExceeded {
						cancel()
						ctx, cancel = context.WithDeadline(t.Context(), time.Unix(0, 0))
					}
					cancel()
					target := f.device.FreeMiB + 1
					if recovered {
						target = f.device.FreeMiB
					}
					assert.ErrorIs(t, f.mgr.waitMemoryRecovery(ctx, f.device.UUID, target), cause)
				})
			}
		})
	}
}

func TestIdleStopAndLeaseClaim(t *testing.T) {
	f := newReclaimFixture(t, true)
	lease, err := f.older.Acquire(t.Context(), nil)
	require.NoError(t, err)
	release, refusal := instance.ClaimIdleStops([]instance.IdleStopCandidate{{Instance: f.older, Activity: f.older.GetLastActivity()}})
	assert.Nil(t, release)
	assert.Equal(t, &instance.IdleStopRefusal{RunID: f.older.ID, Reason: "in use"}, refusal)
	lease.Release()
	release, refusal = instance.ClaimIdleStops([]instance.IdleStopCandidate{{Instance: f.older, Activity: f.older.GetLastActivity()}})
	require.NotNil(t, release)
	require.Nil(t, refusal)
	defer release()
	_, err = f.older.Acquire(t.Context(), nil)
	assert.ErrorIs(t, err, instance.ErrStopping)
}

func TestMemoryReclaimUnknownHolderCountsZero(t *testing.T) {
	f := newReclaimFixture(t, true)
	f.device.Processes[0].MemoryMiB = nil
	evicted, err := f.admit(t)
	require.NoError(t, err)
	assert.Equal(t, []instance.EvictedRun{{RunID: f.newer.ID, Model: f.newer.Model, Provider: f.newer.Provider, FreedMiB: 4500}}, evicted)
	assert.Equal(t, instance.StatusRunning, f.older.GetStatus(), "an unknown-size holder offers no relief")
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
	_, _, err := f.mgr.admitMemory(ctx, f.features, false, nil)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 2, attempts)
	assert.Equal(t, instance.StatusStopped, f.older.GetStatus())
}

func TestMemoryReclaimCancellationStopsDrain(t *testing.T) {
	f := newReclaimFixture(t, true)
	*f.device.Processes[0].MemoryMiB = 2000
	*f.device.Processes[1].MemoryMiB = 2500
	log := capturePinLog(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.older.TrackGoroutine()
	go func() {
		defer f.older.GoroutineDone()
		<-f.older.Context().Done()
		cancel()
	}()
	_, _, err := f.mgr.admitMemory(ctx, f.features, false, nil)
	require.Error(t, err)
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	assert.Equal(t, instance.StatusRunning, f.newer.GetStatus())
	lease, err := f.newer.Acquire(t.Context(), nil)
	require.NoError(t, err, "cancellation releases the remaining idle claim")
	lease.Release()
	log.mu.Lock()
	defer log.mu.Unlock()
	for _, record := range log.records {
		if record.Message == "failed to reclaim idle GPU run" {
			record.Attrs(func(attr slog.Attr) bool {
				if attr.Key == "instance" {
					assert.NotEqual(t, f.newer.ID, attr.Value.String(), "remaining victims are not attempted after cancellation")
				}
				return true
			})
		}
	}
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
				assert.Equal(t, []instance.EvictedRun{{RunID: f.older.ID, Model: f.older.Model, Provider: f.older.Provider, FreedMiB: 4500}}, evicted)
				assert.Equal(t, instance.StatusStopped, f.older.GetStatus())
			} else {
				f.assertOriginalFit(t, err, nil)
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
				waiting := instance.NewInstance("waiting", "vllm", "pending", 0, 24*time.Hour, 1)
				waiting.SetWaitingOn(f.older.ID)
				require.NoError(t, f.mgr.instances.Register(waiting))
				unblock = func() { waiting.Admit() }
			} else {
				release, refusal := instance.ClaimIdleStops([]instance.IdleStopCandidate{{Instance: f.older, Activity: f.older.GetLastActivity()}})
				require.NotNil(t, release)
				require.Nil(t, refusal)
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

func TestMemoryReclaimRetryKeepsSkippedDecision(t *testing.T) {
	f := newReclaimFixture(t, true)
	*f.device.Processes[0].MemoryMiB = 6000
	*f.device.Processes[1].MemoryMiB = 1000
	f.newer.SetResolved(instance.Resolved{Commitments: map[string]int64{"GPU-A": 2000}})
	f.newer.ObserveActivity(true)
	attempts := 0
	f.mgr.memorySnapshot = func(ctx context.Context) ([]gpu.MemoryDevice, error) {
		device := f.device
		if ctx != t.Context() {
			device.FreeMiB = 9000
			f.newer.ObserveActivity(false)
		} else {
			attempts++
			if attempts == 2 {
				device.FreeMiB = 4000
				device.Processes = device.Processes[1:]
			}
		}
		return []gpu.MemoryDevice{device}, nil
	}
	evicted, err := f.admit(t)
	var fit *process.MemoryFitError
	require.ErrorAs(t, err, &fit)
	require.Len(t, evicted, 1)
	assert.Equal(t, f.older.ID, evicted[0].RunID)
	assert.Equal(t, 2, attempts)
	assert.Equal(t, int64(3000), fit.FreeMiB, "keep the retry's fresh memory accounting")
	require.Len(t, fit.Holders, 2)
	for _, holder := range fit.Holders {
		assert.Equal(t, f.newer.ID, holder.Run)
		assert.Equal(t, "busy", holder.ReclaimSkippedReason, "process and commitment retain the actual reclaim decision")
	}
	assert.Equal(t, int64(1000), fit.Holders[1].UnusedCommitmentMiB)
	assert.True(t, f.newer.Evictable(), "the run became idle after reclaim made its decision")
	assert.Contains(t, fit.Error(), "reclaim skipped: busy")
}

func TestPinnedRunRefusesMemoryLaunch(t *testing.T) {
	for _, hint := range []time.Duration{-1, 0, time.Minute} {
		t.Run(hint.String(), func(t *testing.T) {
			f := newReclaimFixture(t, true, -1)
			*f.device.Processes[1].MemoryMiB = 100
			lease, err := f.older.Acquire(t.Context(), &keepalive.Override{Duration: &hint})
			require.NoError(t, err)
			lease.Release()
			before := f.older.ToInfo("")
			f.assertNothingEvicted(t, map[string]string{f.older.ID: "pinned"})
			assert.Equal(t, before, f.older.ToInfo(""))
			assert.Equal(t, instance.StatusRunning, f.newer.GetStatus())
			assert.Empty(t, f.mgr.auditEvents, "refusal must issue zero stops")
			current, exists := f.mgr.instances.Get(f.older.ID)
			require.True(t, exists)
			assert.Same(t, f.older, current)
		})
	}
}

func TestMemoryReclaimSkipsPinnedAndEvictsUnpinned(t *testing.T) {
	for _, sufficient := range []bool{true, false} {
		t.Run(map[bool]string{true: "sufficient", false: "insufficient"}[sufficient], func(t *testing.T) {
			f := newReclaimFixture(t, true, -1)
			if !sufficient {
				*f.device.Processes[1].MemoryMiB = 100
				f.assertNothingEvicted(t, map[string]string{f.older.ID: "pinned"})
				assert.Equal(t, instance.StatusRunning, f.newer.GetStatus())
				assert.Empty(t, f.mgr.auditEvents)
			} else {
				evicted, err := f.admit(t)
				require.NoError(t, err)
				assert.Equal(t, []instance.EvictedRun{{RunID: f.newer.ID, Model: f.newer.Model, Provider: f.newer.Provider, FreedMiB: 4500}}, evicted)
				assert.Equal(t, instance.StatusStopped, f.newer.GetStatus())
			}
			assert.Equal(t, instance.StatusRunning, f.older.GetStatus())
			assert.True(t, f.mgr.instances.Has(f.older.ID))
		})
	}
}

func TestNegativeInferenceHintDoesNotPin(t *testing.T) {
	f := newReclaimFixture(t, true)
	negative := time.Duration(-1)
	// Backdate the hinted use: a coarse clock (Windows) can stamp both runs alike.
	restore := utils.SetClock(utils.FixedClock(utils.Now().Add(-time.Hour)))
	lease, err := f.older.Acquire(t.Context(), &keepalive.Override{Duration: &negative})
	require.NoError(t, err)
	lease.Release()
	restore()
	assert.Negative(t, f.older.GetKeepAlive())
	assert.False(t, f.older.IdleSnapshot().Pinned)
	assert.True(t, f.older.Evictable())
	*f.device.Processes[1].MemoryMiB = 100
	evicted, err := f.admit(t)
	require.NoError(t, err)
	require.Len(t, evicted, 1)
	assert.Equal(t, f.older.ID, evicted[0].RunID)
	assert.Equal(t, instance.StatusStopped, f.older.GetStatus())
}

func TestPinnedRunNeverReapedAfterInferenceHints(t *testing.T) {
	for _, hint := range []time.Duration{0, time.Millisecond} {
		t.Run(hint.String(), func(t *testing.T) {
			f := newReclaimFixture(t, false, -1)
			lease, err := f.older.Acquire(t.Context(), &keepalive.Override{Duration: &hint})
			require.NoError(t, err)
			lease.Release()
			assert.Equal(t, hint, f.older.GetKeepAlive(), "hints still update the idle timer")
			assert.False(t, Expired(f.older.IdleSnapshot(), utils.Now().Add(time.Hour)))
			// The unpinned control proves the reaper has processed the expired timers.
			zero := time.Duration(0)
			control, err := f.newer.Acquire(t.Context(), &keepalive.Override{Duration: &zero})
			require.NoError(t, err)
			control.Release()
			f.mgr.Start(t.Context())
			eventually(t, func() bool { return !f.mgr.instances.Has(f.newer.ID) }, "control must expire")
			assert.Equal(t, instance.StatusRunning, f.older.GetStatus())
			assert.True(t, f.mgr.instances.Has(f.older.ID))
		})
	}
}
