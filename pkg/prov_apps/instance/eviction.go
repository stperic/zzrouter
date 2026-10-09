package instance

import (
	"slices"
	"sort"
	"sync"
	"time"
)

// IdleStopCandidate carries the activity observed when deciding to stop a run.
type IdleStopCandidate struct {
	Instance *Instance
	Activity time.Time
}

// ClaimIdleStops reserves a complete idle set against new leases until release.
func ClaimIdleStops(candidates []IdleStopCandidate) (release func(), claimed bool) {
	if len(candidates) == 0 {
		return nil, false
	}
	ordered := slices.Clone(candidates)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Instance.ID < ordered[j].Instance.ID })
	for n, candidate := range ordered {
		if n > 0 && candidate.Instance.ID == ordered[n-1].Instance.ID {
			return nil, false
		}
	}
	// Lock in ID order because competing reclaims may reserve overlapping sets.
	for _, candidate := range ordered {
		candidate.Instance.mu.Lock()
		defer candidate.Instance.mu.Unlock()
	}
	for _, candidate := range ordered {
		inst := candidate.Instance
		if !inst.evictableLocked() || !inst.lastActivityTime().Equal(candidate.Activity) {
			return nil, false
		}
	}
	for _, candidate := range ordered {
		candidate.Instance.idleStopClaimed = true
	}
	return sync.OnceFunc(func() {
		for _, candidate := range ordered {
			inst := candidate.Instance
			inst.mu.Lock()
			inst.idleStopClaimed = false
			inst.mu.Unlock()
		}
	}), true
}
