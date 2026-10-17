package prov_apps

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/utils"
)

// detachedStop invokes StopInstance on a background goroutine so callers
// who themselves hold an inst.TrackGoroutine slot (the watcher) don't
// deadlock on inst.WaitForGoroutines.
//
// Untracked by design: this goroutine is NOT added to manager.wg. A
// server shutdown that races with a cancel-job can therefore return
// before detachedStop finishes. That is safe because the bounded ctx
// ensures exit, and any orphaned process/port state is reclaimed by
// process.CleanupOrphans on next server start. Tracking on manager.wg
// would require restructuring Stop — not worth it for this edge case.
func (m *ProviderAppManager) detachedStop(instID, provider string) {
	ctx, cancel := context.WithTimeout(context.Background(), m.stopBudget(provider))
	defer cancel()
	if err := m.StopInstance(ctx, instID); err != nil && !errors.Is(err, ErrInstanceNotFound) {
		slog.Warn("detachedStop failed",
			"instance", instID, "error", err)
	}
}

// watcherPollInterval controls how often watchInstanceReadiness polls
// inst.GetStatus(). Status is atomic so polling is cheap; 500ms is
// imperceptible against multi-second launches and keeps the hot path
// simple. A status-change signal channel is the obvious optimization
// if sub-100ms latency ever matters.
const watcherPollInterval = 500 * time.Millisecond

// watcherHeartbeatInterval emits a Progress event while the instance
// is still transitioning toward StatusRunning. Serves two purposes:
// (1) clients see a visible "still-loading" signal during multi-minute
// model loads, (2) the pkg/jobs inactivity janitor sees a live producer
// so a genuinely slow load doesn't get reaped before becoming healthy.
const watcherHeartbeatInterval = 30 * time.Second

// watchInstanceReadiness owns a jobs.Handle from EnsureRun through
// first-readiness (Done) or terminal failure (Fail). Exits after the
// terminal event. Post-readiness the instance lifecycle is observed via
// /runs/:id and /runs/:id/health; the launch job is done.
//
// Cancel semantics: DELETE /jobs/:id ends h.Context() with cause
// jobs.ErrCanceled, and this watcher then calls m.StopInstance(inst.ID) so
// the running process is actually terminated — otherwise cancel would be
// cosmetic. Any other end of the job (a reap, a registry stop) leaves the
// run alone. Agents post-readiness should use DELETE /runs/:id directly;
// DELETE /jobs/:id on a terminal handle is a no-op (clean by pkg/jobs semantics).
func (m *ProviderAppManager) watchInstanceReadiness(inst *instance.Instance, h jobs.Handle) {
	result := errors.New("instance readiness interrupted")
	defer func() {
		// The job's terminal event is what a caller waits on, so the catalog reflects the run first.
		if m.publishCatalog != nil && m.shutdownCtx.Err() == nil {
			result = errors.Join(result, m.publishCatalog())
		}
		if evicted := inst.EvictedRuns(); len(evicted) > 0 {
			h.Meta(jobs.Meta{"evicted_runs": evicted})
		}
		if result != nil {
			h.Fail(result)
		} else {
			h.Done()
		}
	}()
	// Opening Progress: agents subscribing immediately see a "launching"
	// (or "queued") event even before the first status transition. Meta MUST be set
	// before Progress — jobs.Handle.Meta patches the sidecar for the
	// NEXT emitted event, so calling Progress first would emit an event
	// carrying only the meta passed to jobs.Start (run_id/provider/etc.)
	// and the "phase" field would arrive one event late.
	queuedOn, isQueued := queuedState(inst)
	phase := "launching"
	if isQueued {
		phase = "queued"
	}
	h.Meta(jobs.Meta{"phase": phase, "waiting_on": queuedOn})
	h.Progress(0, phase, jobs.Bytes{})

	tick := time.NewTicker(watcherPollInterval)
	defer tick.Stop()

	// Per-kind inactivity for KindRun is 2 hours; our own heartbeat is
	// 30s so the janitor only ever trips on a genuinely stuck process.
	heartbeat := time.NewTicker(watcherHeartbeatInterval)
	defer heartbeat.Stop()

	startedAt := utils.Now()

	for {
		select {
		case <-h.Context().Done():
			if !errors.Is(context.Cause(h.Context()), jobs.ErrCanceled) {
				return // ended by a reap or registry stop, not by the caller
			}
			// Cancel via DELETE /jobs/:id — kill the instance so cancel
			// is meaningful (not cosmetic). We cannot call StopInstance
			// synchronously here because it invokes inst.WaitForGoroutines
			// which would deadlock on this very goroutine. Spawn a
			// detached stopper that outlives the watcher; the watcher
			// exits so inst.wg decrements, StopInstance's wait unblocks,
			// and the server-Stop drain proceeds cleanly.
			go m.detachedStop(inst.ID, inst.Provider)
			result = errors.New("cancelled")
			return

		case <-inst.Context().Done():
			// Instance ctx cancelled (StopInstance externally called,
			// not via job cancel). No need to call StopInstance — the
			// caller already did. Just emit terminal and exit so
			// inst.WaitForGoroutines unblocks.
			result = errors.New("instance stopped")
			return

		case <-m.shutdownCtx.Done():
			// Manager is shutting down. runInstanceLifecycle owns process
			// teardown; we just emit Fail so subscribers get a terminal
			// event instead of an indefinite hang.
			result = errors.New("manager shutdown")
			return

		case <-heartbeat.C:
			if !isQueued {
				phase = "waiting-for-health"
			}
			h.Meta(jobs.Meta{"phase": phase, "elapsed_ms": utils.Now().Sub(startedAt).Milliseconds()})
			h.Progress(0, phase, jobs.Bytes{})

		case <-tick.C:
			// A queued run's job says what it waits on, and when it is admitted.
			if on, q := queuedState(inst); q != isQueued || on != queuedOn {
				queuedOn, isQueued, phase = on, q, "launching"
				if q {
					phase = "queued"
				}
				h.Meta(jobs.Meta{"phase": phase, "waiting_on": queuedOn})
				h.Progress(0, phase, jobs.Bytes{})
			}
			status := inst.GetStatus()
			switch status {
			case instance.StatusRunning:
				result = nil
				h.Meta(jobs.Meta{
					"phase":      "running",
					"elapsed_ms": utils.Now().Sub(startedAt).Milliseconds(),
					"port":       inst.Port,
					"health_url": inst.HealthURL,
				})
				return

			case instance.StatusFailed:
				// GetErrorMessage takes the instance mutex — direct field
				// access races with MarkFailed's write even though status
				// is atomic (the status atomic and the field-write are
				// not co-synchronized).
				info := inst.ToInfo("")
				h.Meta(jobs.Meta{"failure": info.Failure})
				result = fmt.Errorf("instance failed: %s", info.ErrorMessage)
				return

			case instance.StatusStopped:
				// Reached Stopped without passing through Running —
				// e.g., process crashed during startup then the reaper
				// set Stopped before the health monitor saw a success.
				result = errors.New("instance stopped before becoming healthy")
				return
			}
			// StatusStarting / StatusUnhealthy / StatusRestarting /
			// StatusStopping are intermediate; keep watching.
		}
	}
}

// queuedState reports whether a run still waits to be admitted, and on which run.
func queuedState(inst *instance.Instance) (waitingOn string, queued bool) {
	if inst.GetStatus() != instance.StatusStarting || inst.Admitted() {
		return "", false
	}
	return inst.WaitingOn(), true
}
