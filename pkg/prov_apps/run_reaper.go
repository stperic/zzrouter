package prov_apps

import (
	"container/heap"
	"log/slog"
	"time"

	obsruns "github.com/stperic/zzrouter/pkg/observability/runs"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/utils"
)

type idleDeadline struct {
	run *instance.Instance
	at  time.Time
}
type idleHeap []idleDeadline

func (h idleHeap) Len() int           { return len(h) }
func (h idleHeap) Less(i, j int) bool { return h[i].at.Before(h[j].at) }
func (h idleHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *idleHeap) Push(v any) {
	deadline, ok := v.(idleDeadline)
	if !ok {
		slog.Error("idle heap requires an idle deadline")
		return
	}
	*h = append(*h, deadline)
}
func (h *idleHeap) Pop() any { old := *h; v := old[len(old)-1]; *h = old[:len(old)-1]; return v }

// idleReaper owns one timer and rebuilds its heap from authoritative state on each coalesced wake.
func (m *ProviderAppManager) idleReaper() {
	defer close(m.reaperDone)
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		if m.startsCtx.Err() != nil {
			return
		}
		deadlines := idleHeap{}
		for _, run := range m.instances.List() {
			idle := run.IdleSnapshot()
			if !m.evictable(run) || idle.KeepAlive < 0 || idle.KeepAlive == 0 && !idle.Released || idle.Activity.IsZero() {
				continue
			}
			deadlines = append(deadlines, idleDeadline{run, idle.Activity.Add(idle.KeepAlive)})
		}
		heap.Init(&deadlines)
		timer.Stop()
		var expired <-chan time.Time
		if deadlines.Len() > 0 {
			timer.Reset(max(0, deadlines[0].at.Sub(utils.Now())))
			expired = timer.C
		}
		select {
		case <-m.startsCtx.Done():
			return
		case <-m.instances.Changes():
			continue
		case <-expired:
			for deadlines.Len() > 0 && !utils.Now().Before(deadlines[0].at) {
				run := deadlines[0].run
				heap.Pop(&deadlines)
				current, exists := m.instances.Get(run.ID)
				idle := run.IdleSnapshot()
				if !exists || current != run || !m.evictable(run) || !Expired(idle, utils.Now()) {
					continue
				}
				release, _ := instance.ClaimIdleStops([]instance.IdleStopCandidate{{Instance: run, Activity: idle.Activity, Idle: &idle}})
				if release == nil {
					continue
				}
				if err := m.stopInstance(m.startsCtx, run.ID, obsruns.StopReasonIdle); err != nil {
					slog.Warn("failed to stop idle run", "run", run.ID, "error", err)
				}
				release()
			}
		}
	}
}
