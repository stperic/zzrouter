package prov_apps

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"runtime"
	"slices"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/discovery/gpu"
	obsruns "github.com/stperic/zzrouter/pkg/observability/runs"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
)

// memoryRecoveryToleranceMiB allows minor allocator/accounting variation after release.
const memoryRecoveryToleranceMiB int64 = 64

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

func (m *ProviderAppManager) reclaimPlan(fit *process.MemoryFitError) (claims []instance.IdleStopCandidate, sizes map[string]int64, skipped map[string]string) {
	sizes, skipped = make(map[string]int64), make(map[string]string)
	reliefs := make(map[string]int64)
	for _, holder := range fit.Holders {
		if holder.Ownership != "zzrouter" || holder.Run == "" {
			continue
		}
		mib := int64(0)
		if holder.MemoryMiB != nil {
			mib = max(0, *holder.MemoryMiB)
		}
		sizes[holder.Run] += mib
		if fit.CommitmentOnly {
			mib = holder.UnusedCommitmentMiB
		}
		reliefs[holder.Run] += mib
	}
	var candidates []evictionCandidate
	for id, relief := range reliefs {
		if inst, ok := m.instances.Get(id); ok {
			if reason := m.evictionBlockedReason(inst); reason != "" {
				skipped[id] = reason
				continue
			}
			candidates = append(candidates, evictionCandidate{instance.IdleStopCandidate{Instance: inst, Activity: inst.GetLastActivity()}, relief})
		}
	}
	claims = chooseEvictions(fit.RequiredMiB-fit.FreeMiB, candidates)
	return claims, sizes, skipped
}

// reclaimFor executes the shared plan, claiming idle eligibility again before any stop.
func (m *ProviderAppManager) reclaimFor(ctx context.Context, fit *process.MemoryFitError) ([]instance.EvictedRun, map[string]string) {
	claims, sizes, skipped := m.reclaimPlan(fit)
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
		if ctx.Err() != nil {
			break
		}
		victim := claim.Instance
		mib := sizes[victim.ID]
		if err := m.stopClaimedIdle(ctx, victim, obsruns.StopReasonMemoryPressure); err != nil {
			slog.Warn("failed to reclaim idle GPU run", "instance", victim.ID, "device", fit.Device, "error", err)
			continue
		}
		stopped = append(stopped, instance.EvictedRun{RunID: victim.ID, Model: victim.Model, Provider: victim.Provider, FreedMiB: mib})
		freed += mib
		slog.Info("stopped idle run for GPU memory pressure", "instance", victim.ID, "provider", victim.Provider, "model", victim.Model, "device", fit.Device, "held_mib", mib, "shortfall_mib", fit.RequiredMiB-fit.FreeMiB)
	}
	if len(stopped) > 0 {
		_ = m.waitMemoryRecovery(ctx, fit.Device, fit.FreeMiB+freed-memoryRecoveryToleranceMiB)
	}
	return stopped, skipped
}

// waitMemoryRecovery leaves recovery expiry to fresh admission; only caller cancellation returns an error.
func (m *ProviderAppManager) waitMemoryRecovery(ctx context.Context, device string, target int64) error {
	recoveryCtx, cancel := context.WithTimeout(ctx, constants.MemoryRecoveryTimeout)
	defer cancel()
	ticker := time.NewTicker(constants.MemoryRecoveryInterval)
	defer ticker.Stop()
	for {
		devices, err := m.memorySnapshot(recoveryCtx)
		if err == nil {
			for _, snapshot := range devices {
				if snapshot.UUID == device && snapshot.FreeMiB >= target {
					return ctx.Err()
				}
			}
		}
		select {
		case <-recoveryCtx.Done():
			return ctx.Err() // Recovery expiry leaves the decision to fresh admission.
		case <-ticker.C:
		}
	}
}

// AssessCapacity resolves the same node tiers and weights a launch uses.
func (m *ProviderAppManager) AssessCapacity(ctx context.Context, req EnsureRequest) process.MemoryAssessment {
	features, err := m.ResolveLaunchParameters(ctx, req)
	if err != nil {
		assessment := process.MemoryAssessment{ContextMode: "unknown"}
		assessment.SetUnknown(process.AssessmentFailed, err.Error())
		return assessment
	}
	assessment, _, _ := m.admitMemory(ctx, features, true, nil)
	return assessment
}

// admitWithReclaim permits one reclaim on the first refused device, then one fresh admission.
func admitWithReclaim(evictIdle bool, admit func() error, reclaim func(*process.MemoryFitError) bool) error {
	err := admit()
	var fit *process.MemoryFitError
	if evictIdle && errors.As(err, &fit) && reclaim(fit) {
		err = admit()
	}
	return err
}

type replacementCheck struct {
	run      string
	recovery map[string]int64
}

func (c *replacementCheck) simulateStops(devices []gpu.MemoryDevice, owners []process.RunRef, stops []string, credits map[string]*int64) []process.RunRef {
	for _, run := range stops {
		for i, device := range devices {
			after, bound := process.MemoryAfterStop(device, owners, run)
			if run == c.run {
				credits[device.UUID] = bound
				if after.FreeMiB > device.FreeMiB {
					c.recovery[device.UUID] = after.FreeMiB - memoryRecoveryToleranceMiB
				}
				if bound != nil {
					after.FreeMiB = device.FreeMiB + *bound
				}
			}
			devices[i] = after
		}
		owners = slices.DeleteFunc(owners, func(owner process.RunRef) bool { return owner.ID == run })
	}
	return owners
}

// admitMemory assesses capacity or admits a launch, with at most one real or simulated reclaim.
func (m *ProviderAppManager) admitMemory(ctx context.Context, features LaunchParameters, capacity bool, replacement *replacementCheck) (process.MemoryAssessment, []instance.EvictedRun, error) {
	settings, err := config.ParseRunSettings(features.Params, m.idleEvictionDefault)
	if err != nil {
		return process.MemoryAssessment{}, nil, fmt.Errorf("%w: %w", ErrParameterValidation, err)
	}
	policy := schema.AutoContext{}
	if features.declared != nil {
		policy = features.declared.AutoContext
	}
	if key := features.Config.GetContextParam(); key != "" {
		contextPolicy := features.parameterShapes[key].AutoContext
		policy.AutoMinContextTokens, policy.AutoContextSource = contextPolicy.AutoMinContextTokens, contextPolicy.AutoContextSource
	}
	// Collect best-effort weight/context evidence even when no memory policy sizes the launch.
	need, _ := process.ModelMemoryNeed(features.model, features.Config, features.Params, policy, features.parameterShapes)
	assessment := process.MemoryAssessment{ContextMode: need.ContextMode, ContextTokens: need.ContextTokens, WeightsMiB: need.WeightsMiB}
	assessment.SetUnknown(process.BudgetNotSized, "")
	budget := launchBudget(features.declared)
	if budget == nil {
		return assessment, nil, nil
	}
	assessment.BudgetKind = budget.Kind
	params, environment, commitments := features.Params, features.Environment, features.Commitments
	if capacity || replacement != nil {
		params, environment, commitments = maps.Clone(params), maps.Clone(environment), maps.Clone(commitments)
	}
	var owners []process.RunRef
	if capacity {
		commitments = nil
	}
	visible, err := visibleDeviceEnvironment(environment)
	if err != nil {
		assessment.SetUnknown(process.MemoryEvidenceUnavailable, err.Error())
		return assessment, nil, err
	}
	var evicted []instance.EvictedRun
	var skipped map[string]string
	credits := make(map[string]*int64)
	var devices []gpu.MemoryDevice
	var systemMiB int64
	err = admitWithReclaim(settings.EvictIdle && !capacity, func() error {
		if replacement == nil || devices == nil {
			if !capacity {
				owners = m.memoryRuns()
			}
			devices, systemMiB, err = m.memoryDevices(ctx, budget.Kind, capacity)
			if err != nil {
				err = fmt.Errorf("%w: %w", process.ErrMemoryObservation, err)
				assessment.SetUnknown(process.MemoryEvidenceUnavailable, err.Error())
				return err
			}
			if replacement != nil {
				devices = slices.Clone(devices)
				owners = replacement.simulateStops(devices, owners, []string{replacement.run}, credits)
			}
		}
		assessment, err = process.AdmitMemoryBudget(*budget, need, params, visible, devices, systemMiB, owners, commitments, features.declared.MemoryUsableFraction)
		return err
	}, func(fit *process.MemoryFitError) bool {
		if replacement == nil {
			evicted, skipped = m.reclaimFor(ctx, fit)
			return len(evicted) > 0
		}
		if memoryNeedsStop(fit, credits) {
			return false
		}
		var claims []instance.IdleStopCandidate
		claims, _, skipped = m.reclaimPlan(fit)
		var stops []string
		for _, claim := range claims {
			stops = append(stops, claim.Instance.ID)
		}
		owners = replacement.simulateStops(devices, owners, stops, credits)
		return len(claims) > 0
	})
	if replacement != nil && memoryNeedsStop(err, credits) {
		clear(replacement.recovery)
		err = nil // Unavailable evidence leaves admission until after the stop.
	}
	var fit *process.MemoryFitError
	if errors.As(err, &fit) {
		for i := range fit.Holders {
			fit.Holders[i].ReclaimSkippedReason = skipped[fit.Holders[i].Run]
		}
		if replacement != nil {
			credit := cmp.Or(credits[fit.Device], new(int64(0)))
			if fit.BudgetKind != "" {
				credit = new(int64(0))
			}
			fit.KeptRun, fit.CreditedMiB = replacement.run, credit
		}
	}
	if !capacity && (budget.Kind == schema.MemoryLayers || budget.Kind == schema.MemoryUnified) && errors.Is(err, process.ErrMemoryObservation) && !slices.ContainsFunc(owners, func(run process.RunRef) bool { return len(run.Commitments) > 0 }) {
		err = nil
	}
	if ctx.Err() != nil {
		assessment = process.MemoryAssessment{WeightsMiB: need.WeightsMiB, ContextMode: need.ContextMode, ContextTokens: need.ContextTokens}
		assessment.SetUnknown(process.AssessmentFailed, ctx.Err().Error())
		return assessment, evicted, ctx.Err()
	}
	return assessment, evicted, err
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
			devices = append(devices, gpu.MemoryDevice{UUID: card.UUID, TotalMiB: card.TotalMemoryMiB, FreeMiB: card.TotalMemoryMiB})
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
		devices, err = []gpu.MemoryDevice{{UUID: "unified", TotalMiB: systemMiB, FreeMiB: systemMiB, UsedMiB: new(int64(0)), Unified: true}}, nil
	}
	return devices, systemMiB, err
}
