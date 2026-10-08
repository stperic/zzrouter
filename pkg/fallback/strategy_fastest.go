package fallback

import (
	"sort"
	"time"
)

// FastestStrategy selects deployments by lowest observed average latency.
// Deployments with no latency data fall to the end (encouraging exploration
// of new deployments without starving them entirely).
// Tiebreaker: higher priority wins.
type FastestStrategy struct {
	tracker *LatencyTracker
}

// NewFastestStrategy creates a strategy backed by the given latency tracker.
func NewFastestStrategy(tracker *LatencyTracker) *FastestStrategy {
	return &FastestStrategy{tracker: tracker}
}

func (s *FastestStrategy) Select(candidates []Candidate) []Candidate {
	result := make([]Candidate, len(candidates))
	copy(result, candidates)

	// Snapshot latency data for consistent sort ordering
	type latencyInfo struct {
		hasData bool
		avg     time.Duration
	}
	latencies := make(map[string]latencyInfo, len(result))
	for _, c := range result {
		latencies[c.Name] = latencyInfo{
			hasData: s.tracker.HasData(c.Name),
			avg:     s.tracker.AverageLatency(c.Name),
		}
	}

	sort.SliceStable(result, func(i, j int) bool {
		li := latencies[result[i].Name]
		lj := latencies[result[j].Name]

		// Deployments with no data go to the end
		if li.hasData != lj.hasData {
			return li.hasData
		}

		// Both have data: sort by average latency ascending
		if li.hasData && lj.hasData && li.avg != lj.avg {
			return li.avg < lj.avg
		}

		// Tiebreaker: lower priority number = higher priority
		return result[i].Priority < result[j].Priority
	})

	return result
}

func (s *FastestStrategy) Name() string { return "fastest" }
