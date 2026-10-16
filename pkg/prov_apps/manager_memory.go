package prov_apps

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"runtime"
	"slices"
	"time"

	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/discovery/gpu"
	obsruns "github.com/stperic/zzrouter/pkg/observability/runs"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
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
	return m.evictionBlockedReason(inst) == ""
}

func (m *ProviderAppManager) evictionBlockedReason(inst *instance.Instance) string {
	idle := inst.IdleSnapshot()
	for _, waiting := range m.instances.List() {
		if waiting.GetStatus() == instance.StatusStarting && waiting.WaitingOn() == inst.ID {
			idle.Leased = true
			break
		}
	}
	return idle.EvictionBlockedReason()
}

func (m *ProviderAppManager) stopClaimedIdle(ctx context.Context, inst *instance.Instance, reason obsruns.StopReason) error {
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
	return m.stopInstance(stopCtx, inst.ID, reason)
}

// reclaimFor stops the least recently active idle runs covering fit's
// shortfall and returns the runs it stopped and the reasons it skipped others.
func (m *ProviderAppManager) reclaimFor(ctx context.Context, fit *process.MemoryFitError) ([]instance.EvictedRun, map[string]string) {
	skipped := make(map[string]string)
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
		if inst, ok := m.instances.Get(id); ok {
			if reason := m.evictionBlockedReason(inst); reason != "" {
				skipped[id] = reason
				continue
			}
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
		return nil, skipped
	}
	release, refusal := instance.ClaimIdleStops(claims)
	if release == nil {
		if refusal != nil {
			skipped[refusal.RunID] = refusal.Reason
		}
		return nil, skipped
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
		if err := m.stopClaimedIdle(ctx, victim, obsruns.StopReasonMemoryPressure); err != nil {
			slog.Warn("failed to reclaim idle GPU run", "instance", victim.ID, "device", fit.Device, "error", err)
			continue
		}
		stopped = append(stopped, instance.EvictedRun{RunID: victim.ID, Model: victim.Model, Provider: victim.Provider, FreedMiB: mib})
		freed += mib
		slog.Info("stopped idle run for GPU memory pressure", "instance", victim.ID, "provider", victim.Provider, "model", victim.Model, "device", fit.Device, "held_mib", mib, "shortfall_mib", shortfall)
	}
	if len(stopped) > 0 {
		m.waitMemoryRecovery(ctx, fit.Device, fit.FreeMiB+freed-memoryRecoveryToleranceMiB)
	}
	return stopped, skipped
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

// AssessCapacity resolves the same node tiers and weights a launch uses.
func (m *ProviderAppManager) AssessCapacity(ctx context.Context, req EnsureRequest) process.MemoryAssessment {
	features, err := m.ResolveLaunchParameters(ctx, req)
	if err != nil {
		return process.MemoryAssessment{ContextMode: "unknown", Status: "unknown", Reason: err.Error()}
	}
	assessment, _ := m.checkMemory(ctx, features, true)
	return assessment
}

func (m *ProviderAppManager) checkMemory(ctx context.Context, features LaunchParameters, capacity bool) (process.MemoryAssessment, error) {
	assessment := process.MemoryAssessment{ContextMode: "unknown", Status: "unknown", Reason: "no launch memory policy"}
	budget := launchBudget(features.declared)
	if budget == nil {
		return assessment, nil
	}
	assessment.BudgetKind = budget.Kind
	params, environment, commitments := features.Params, features.Environment, features.Commitments
	var owners []process.RunRef
	if capacity {
		params, environment, commitments = maps.Clone(params), maps.Clone(environment), nil
	} else {
		owners = m.memoryRuns()
	}
	visible, err := visibleDeviceEnvironment(environment)
	if err != nil {
		assessment.Reason = err.Error()
		return assessment, err
	}
	devices, systemMiB, err := m.memoryDevices(ctx, budget.Kind, capacity)
	if err == nil {
		if capacity {
			devices = slices.Clone(devices)
			for i := range devices {
				devices[i].FreeMiB, devices[i].Processes = devices[i].TotalMiB, nil
			}
		}
		need, _ := process.ModelMemoryNeed(features.model, features.Config, params, features.contextPolicy)
		fraction := environment[features.declared.MemoryFractionEnvironment]
		assessment, err = process.AdmitMemoryBudget(*budget, need, params, visible, devices, systemMiB, owners, commitments, fraction)
	} else {
		err = fmt.Errorf("%w: %w", process.ErrMemoryObservation, err)
		assessment.Reason = err.Error()
	}
	if !capacity && (budget.Kind == schema.MemoryLayers || budget.Kind == schema.MemoryUnified) && errors.Is(err, process.ErrMemoryObservation) && !slices.ContainsFunc(owners, func(run process.RunRef) bool { return len(run.Commitments) > 0 }) {
		err = nil
	}
	if ctx.Err() != nil {
		return process.MemoryAssessment{ContextMode: "unknown", Status: "unknown", Reason: ctx.Err().Error()}, ctx.Err()
	}
	return assessment, err
}

// memoryTotals shares the cached device observation used by reservation preparation.
func (m *ProviderAppManager) memoryTotals(ctx context.Context) ([]gpu.MemoryDevice, error) {
	cards := m.memoryInventory(ctx)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	devices := make([]gpu.MemoryDevice, 0, len(cards))
	for _, card := range cards {
		if card.UUID != "" && card.TotalMemoryMiB > 0 {
			devices = append(devices, gpu.MemoryDevice{UUID: card.UUID, TotalMiB: card.TotalMemoryMiB})
		}
	}
	if len(devices) == 0 {
		return nil, fmt.Errorf("%w: NVIDIA GPU totals unavailable", process.ErrMemoryObservation)
	}
	return devices, nil
}

func (m *ProviderAppManager) memoryDevices(ctx context.Context, kind string, capacity bool) ([]gpu.MemoryDevice, int64, error) {
	var systemMiB int64
	if kind == schema.MemoryLayers || kind == schema.MemoryUnified {
		if memory := m.memorySystem(ctx); memory != nil {
			systemMiB = memory.TotalRAM / (1024 * 1024)
		}
	}
	var devices []gpu.MemoryDevice
	var err error
	if kind != schema.MemoryUnified {
		if capacity {
			devices, err = m.memoryTotals(ctx)
		} else {
			devices, err = m.memorySnapshot(ctx)
		}
	}
	if kind == schema.MemoryUnified || (err != nil && kind == schema.MemoryLayers && runtime.GOOS == "darwin" && runtime.GOARCH == "arm64") {
		if systemMiB <= 0 {
			return nil, systemMiB, process.ErrMemoryObservation
		}
		devices, err = []gpu.MemoryDevice{{UUID: "unified", TotalMiB: systemMiB, FreeMiB: systemMiB, Unified: true}}, nil
	}
	return devices, systemMiB, err
}
