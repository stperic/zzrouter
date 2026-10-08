package server

import (
	"github.com/gin-gonic/gin"
	modelgroup "github.com/stperic/zzrouter/pkg/model/group"
)

// replicaState is the per-replica live view emitted by /state. Fields
// not sourced (nil tracker) are omitted via omitempty so a minimal
// deployment that hasn't wired the load/latency/health trackers still
// returns a clean shape.
//
// Cut from the v2 plan but advertised in /schema:
//   - breaker_state          (BreakerManager is keyed by URL not replica)
//   - ratelimit_remaining/reset (RateLimitTracker is keyed by provider)
//   - last_dispatch_at       (no per-replica last-dispatch tracker)
//
// These land in a follow-up arc once the replica→key plumbing exists.
type replicaState struct {
	Name                     string  `json:"name"`
	CooldownRemainingSeconds float64 `json:"cooldown_remaining_seconds,omitempty"`
	CooldownReason           string  `json:"cooldown_reason,omitempty"`
	HealthState              string  `json:"health_state,omitempty"`
	InFlight                 int64   `json:"in_flight,omitempty"`
	LatencyAvgMs             int64   `json:"latency_avg_ms,omitempty"`
	LatencySamples           int     `json:"latency_samples,omitempty"`
}

// modelGroupStateResponse is the live snapshot returned by
// GET /model-groups/:name/state. Sorted by replica priority via the
// underlying GroupStore which sorts replicas at load time.
type modelGroupStateResponse struct {
	Name     string         `json:"name"`
	Replicas []replicaState `json:"replicas"`
}

// GetModelGroupState handles GET /zzrouter/v1/model-groups/:name/state.
//
// Live derived fields only — every value is a snapshot of tracker
// state at request time. ETag does NOT apply (the body changes on
// every poll by design); Cache-Control: no-store guards against
// well-meaning intermediaries caching transient state.
func (ctrl *ModelGroupsController) GetModelGroupState(c *gin.Context) {
	name := c.Param("name")
	group := ctrl.groupStore.Get(name)
	if group == nil {
		NotFound(c, "model group not found")
		return
	}

	reps := make([]replicaState, 0, len(group.Replicas))
	for _, r := range group.Replicas {
		reps = append(reps, ctrl.buildReplicaState(r))
	}

	c.Header("Cache-Control", "no-store")
	respondSuccess(c, "Model group state retrieved successfully", modelGroupStateResponse{
		Name:     name,
		Replicas: reps,
	})
}

func (ctrl *ModelGroupsController) buildReplicaState(r modelgroup.Replica) replicaState {
	s := replicaState{Name: r.Name}
	if ctrl.cooldowns != nil {
		if remaining, reason := ctrl.cooldowns.GetEntry(r.Name); remaining > 0 {
			s.CooldownRemainingSeconds = remaining.Seconds()
			s.CooldownReason = reason
		}
	}
	if ctrl.healthChecker != nil {
		s.HealthState = ctrl.healthChecker.HealthState(r.Name)
	}
	if ctrl.loadTracker != nil {
		s.InFlight = ctrl.loadTracker.InFlight(r.Name)
	}
	if ctrl.latencyTracker != nil {
		avg, samples := ctrl.latencyTracker.Snapshot(r.Name)
		s.LatencyAvgMs = avg.Milliseconds()
		s.LatencySamples = samples
	}
	return s
}
