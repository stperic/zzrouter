package prov_apps

import (
	"context"
	"log/slog"
	"sort"
	"time"

	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
)

const (
	// memoryRecoveryTimeout bounds asynchronous driver release after a process exits.
	memoryRecoveryTimeout = 5 * time.Second
	// memoryRecoveryInterval avoids hammering the driver while its accounting settles.
	memoryRecoveryInterval = 100 * time.Millisecond
	// memoryRecoveryToleranceMiB allows minor allocator/accounting variation after release.
	memoryRecoveryToleranceMiB int64 = 64
)

func (m *ProviderAppManager) evictable(inst *instance.Instance) bool {
	if !inst.Evictable() {
		return false
	}
	for _, waiting := range m.instances.List() {
		if waiting.GetStatus() == instance.StatusStarting && waiting.WaitingOn() == inst.ID {
			return false
		}
	}
	return true
}

func (m *ProviderAppManager) stopClaimedIdle(ctx context.Context, inst *instance.Instance) error {
	stopCtx, cancel := context.WithTimeout(m.shutdownCtx, constants.ClusterActionTimeout)
	defer cancel()
	stopCancel := context.AfterFunc(ctx, cancel)
	defer stopCancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := stopCtx.Err(); err != nil {
		return err
	}
	return m.StopInstance(stopCtx, inst.ID)
}

type memoryVictim struct {
	inst     *instance.Instance
	mib      int64
	activity time.Time
}

func (m *ProviderAppManager) reclaimFor(ctx context.Context, fit *process.MemoryFitError) bool {
	sizes := make(map[string]int64)
	for _, holder := range fit.Holders {
		if holder.Ownership != "zzrouter" || holder.Run == "" {
			continue
		}
		mib := int64(0)
		if holder.MemoryMiB != nil {
			mib = max(0, *holder.MemoryMiB)
		}
		sizes[holder.Run] += mib
	}
	var victims []memoryVictim
	var available int64
	for id, mib := range sizes {
		if inst, ok := m.instances.Get(id); ok && m.evictable(inst) {
			victims = append(victims, memoryVictim{inst, mib, inst.GetLastActivity()})
			available += mib
		}
	}
	shortfall := fit.RequiredMiB - fit.FreeMiB
	if shortfall <= 0 || available < shortfall {
		return false
	}
	sort.Slice(victims, func(i, j int) bool {
		if victims[i].activity.Equal(victims[j].activity) {
			return victims[i].inst.ID < victims[j].inst.ID
		}
		return victims[i].activity.Before(victims[j].activity)
	})
	var claims []instance.IdleStopCandidate
	var selected int64
	for _, victim := range victims {
		claims = append(claims, instance.IdleStopCandidate{Instance: victim.inst, Activity: victim.activity})
		selected += victim.mib
		if selected >= shortfall {
			break
		}
	}
	if ctx.Err() != nil {
		return false
	}
	release, claimed := instance.ClaimIdleStops(claims)
	if !claimed {
		return false
	}
	defer release()
	var freed int64
	stopped := false
	for _, victim := range victims[:len(claims)] {
		if ctx.Err() != nil {
			break
		}
		if err := m.stopClaimedIdle(ctx, victim.inst); err != nil {
			slog.Warn("failed to reclaim idle GPU run", "instance", victim.inst.ID, "device", fit.Device, "error", err)
			continue
		}
		stopped = true
		freed += victim.mib
		slog.Info("stopped idle run for GPU memory pressure", "instance", victim.inst.ID, "provider", victim.inst.Provider, "model", victim.inst.Model, "device", fit.Device, "held_mib", victim.mib, "shortfall_mib", shortfall)
	}
	if stopped {
		m.waitMemoryRecovery(ctx, fit.Device, fit.FreeMiB+freed-memoryRecoveryToleranceMiB)
	}
	return stopped
}

func (m *ProviderAppManager) waitMemoryRecovery(ctx context.Context, device string, target int64) {
	recoveryCtx, cancel := context.WithTimeout(ctx, memoryRecoveryTimeout)
	defer cancel()
	ticker := time.NewTicker(memoryRecoveryInterval)
	defer ticker.Stop()
	for {
		devices, err := m.memorySnapshot(recoveryCtx)
		if err == nil {
			for _, snapshot := range devices {
				if snapshot.UUID == device && snapshot.FreeMiB >= target {
					return
				}
			}
		}
		select {
		case <-recoveryCtx.Done():
			return
		case <-ticker.C:
		}
	}
}
