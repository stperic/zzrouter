package prov_apps

import (
	"context"
	"log/slog"
	"time"

	"github.com/stperic/zzrouter/pkg/constants"
	obsruns "github.com/stperic/zzrouter/pkg/observability/runs"
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
	return m.stopInstance(stopCtx, inst.ID, obsruns.StopReasonMemoryPressure)
}

// reclaimFor stops the least recently active idle runs covering fit's
// shortfall and returns the runs it stopped.
func (m *ProviderAppManager) reclaimFor(ctx context.Context, fit *process.MemoryFitError) []instance.EvictedRun {
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
	var candidates []evictionCandidate
	inputs := make(map[string]instance.IdleStopCandidate)
	for id, mib := range sizes {
		if inst, ok := m.instances.Get(id); ok && m.evictable(inst) {
			activity := inst.GetLastActivity()
			candidates = append(candidates, evictionCandidate{id, mib, activity})
			inputs[id] = instance.IdleStopCandidate{Instance: inst, Activity: activity}
		}
	}
	shortfall := fit.RequiredMiB - fit.FreeMiB
	var claims []instance.IdleStopCandidate
	for _, id := range chooseEvictions(shortfall, candidates) {
		claims = append(claims, inputs[id])
	}
	if ctx.Err() != nil {
		return nil
	}
	release, claimed := instance.ClaimIdleStops(claims)
	if !claimed {
		return nil
	}
	defer release()
	var freed int64
	var stopped []instance.EvictedRun
	for _, claim := range claims {
		victim := claim.Instance
		mib := sizes[victim.ID]
		if ctx.Err() != nil {
			break
		}
		if err := m.stopClaimedIdle(ctx, victim); err != nil {
			slog.Warn("failed to reclaim idle GPU run", "instance", victim.ID, "device", fit.Device, "error", err)
			continue
		}
		stopped = append(stopped, instance.EvictedRun{RunID: victim.ID, Model: victim.Model, FreedMiB: mib})
		freed += mib
		slog.Info("stopped idle run for GPU memory pressure", "instance", victim.ID, "provider", victim.Provider, "model", victim.Model, "device", fit.Device, "held_mib", mib, "shortfall_mib", shortfall)
	}
	if len(stopped) > 0 {
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
