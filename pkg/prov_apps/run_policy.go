package prov_apps

import (
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/protocol"
)

// RunLifecycle describes how a provider makes a model ready.
type RunLifecycle int

const (
	// LifecycleProcess starts a process per model.
	LifecycleProcess RunLifecycle = iota
	// LifecycleServed loads a model through its endpoint.
	LifecycleServed
	// LifecycleNone needs only catalog evidence.
	LifecycleNone
)

// Lifecycle derives run ownership from config, with process ownership first.
func Lifecycle(svc config.ServiceConfig) RunLifecycle {
	if svc.LaunchesProcess() {
		return LifecycleProcess
	}
	if svc.HasEndpoint() && protocol.LoadsModels(svc.Mode, svc.Protocol) {
		return LifecycleServed
	}
	return LifecycleNone
}

type runSnapshot struct {
	run                               *instance.Instance
	provider, endpoint, runtime, plan string
	status                            instance.Status
	unconfirmed                       bool
}
type startSnapshot struct {
	local, reachable bool
	lifecycle        RunLifecycle
	run              *runSnapshot
}
type startDecision struct {
	reuse, stop *instance.Instance
	err         error
	ready, load bool
}

func decideStart(req EnsureRequest, snapshot startSnapshot) startDecision {
	switch snapshot.lifecycle {
	case LifecycleNone:
		if !snapshot.reachable {
			return startDecision{err: ErrProviderUnavailable}
		}
		if !snapshot.local {
			return startDecision{err: fmt.Errorf("%w: %s is not in this provider's catalog; download or register it, using POST /zzrouter/v1/deployments for a cloud model", ErrModelNotLocal, req.Model)}
		}
		return startDecision{ready: true}
	case LifecycleServed:
		if !snapshot.reachable {
			return startDecision{err: ErrProviderUnavailable}
		}
		return startDecision{load: true}
	}
	var stop *instance.Instance
	if run := snapshot.run; run != nil && run.endpoint == string(req.Endpoint) {
		if run.status == instance.StatusStopping {
			// Drain may finish between retries; use the request-slot backoff.
			return startDecision{err: &instance.AdmissionError{Reason: instance.ErrStopping, RetryAfter: instance.AdmissionRetryAfter}}
		}
		if req.installSmoke && (!run.status.IsTerminal() || run.unconfirmed) {
			return startDecision{err: ErrInstancesRunning}
		}
		if req.Force || run.status.IsTerminal() {
			stop = run.run
		} else {
			if run.provider != req.Provider {
				return startDecision{err: fmt.Errorf("%w: %s is held by provider %s; requested %s", ErrRunHeldByOtherProvider, req.Model, run.provider, req.Provider)}
			}
			if run.runtime != req.Runtime || run.plan != req.DisposablePlanID {
				return startDecision{err: ErrInstancesRunning}
			}
			return startDecision{reuse: run.run}
		}
	}
	if !snapshot.local {
		return startDecision{err: fmt.Errorf("%w: %s; download it with auto_deploy before starting", ErrModelNotLocal, req.Model)}
	}
	return startDecision{stop: stop}
}

type evictionCandidate struct {
	id       string
	mib      int64
	activity time.Time
}

func chooseEvictions(shortfall int64, candidates []evictionCandidate) []string {
	var available int64
	for _, candidate := range candidates {
		available += candidate.mib
	}
	if shortfall <= 0 || available < shortfall {
		return nil
	}
	candidates = slices.Clone(candidates)
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].activity.Equal(candidates[j].activity) {
			return candidates[i].id < candidates[j].id
		}
		return candidates[i].activity.Before(candidates[j].activity)
	})
	var selected int64
	var ids []string
	for _, candidate := range candidates {
		ids = append(ids, candidate.id)
		selected += candidate.mib
		if selected >= shortfall {
			break
		}
	}
	return ids
}

// idleExpired is Ollama's keep-alive rule: below zero never, zero at once, else after keep-alive idle.
func idleExpired(keepAlive time.Duration, lastActivity, now time.Time) bool {
	return keepAlive == 0 || (keepAlive > 0 && !lastActivity.IsZero() && now.Sub(lastActivity) > keepAlive)
}
