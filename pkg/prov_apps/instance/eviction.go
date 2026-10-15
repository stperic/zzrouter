package instance

import (
	"slices"
	"sort"
	"sync"
	"time"
)

// EvictionBlockedReason records why a snapshot cannot be reclaimed, or is empty when idle.
func (s IdleState) EvictionBlockedReason() string {
	switch {
	case (s.Status != StatusRunning && s.Status != StatusUnhealthy) || s.Claimed:
		return "not idle"
	case s.Pinned:
		return "pinned"
	case s.Busy:
		return "busy"
	case s.Leased:
		return "in use"
	default:
		return ""
	}
}

// IdleStopCandidate carries the activity observed when deciding to stop a run.
type IdleStopCandidate struct {
	Instance *Instance
	Activity time.Time
	Idle     *IdleState // expiry claims also revalidate duration and release eligibility
}

// IdleStopRefusal records the decision that prevented claiming a complete idle set.
type IdleStopRefusal struct {
	RunID, Reason string
}

// ClaimIdleStops returns a non-nil release after reserving a complete idle set against new leases.
func ClaimIdleStops(candidates []IdleStopCandidate) (release func(), refusal *IdleStopRefusal) {
	if len(candidates) == 0 {
		return nil, nil
	}
	ordered := slices.Clone(candidates)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Instance.ID < ordered[j].Instance.ID })
	for n, candidate := range ordered {
		if n > 0 && candidate.Instance.ID == ordered[n-1].Instance.ID {
			return nil, nil
		}
	}
	// Lock in ID order because competing reclaims may reserve overlapping sets.
	for _, candidate := range ordered {
		candidate.Instance.mu.Lock()
		defer candidate.Instance.mu.Unlock()
	}
	for _, candidate := range ordered {
		inst := candidate.Instance
		if reason := inst.idleSnapshotLocked().EvictionBlockedReason(); reason != "" {
			return nil, &IdleStopRefusal{RunID: inst.ID, Reason: reason}
		}
		if !inst.LastActivity.Equal(candidate.Activity) || (candidate.Idle != nil && inst.idleSnapshotLocked() != *candidate.Idle) {
			return nil, &IdleStopRefusal{RunID: inst.ID, Reason: "not idle"}
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
			inst.notifyIdleLocked()
			inst.mu.Unlock()
		}
	}), nil
}
