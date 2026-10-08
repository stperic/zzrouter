package fallback

import "sort"

// LeastLoadStrategy selects the deployment with the fewest in-flight requests.
// Tiebreaker: higher priority wins.
type LeastLoadStrategy struct {
	tracker *LoadTracker
}

// NewLeastLoadStrategy creates a strategy backed by the given load tracker.
func NewLeastLoadStrategy(tracker *LoadTracker) *LeastLoadStrategy {
	return &LeastLoadStrategy{tracker: tracker}
}

func (s *LeastLoadStrategy) Select(candidates []Candidate) []Candidate {
	result := make([]Candidate, len(candidates))
	copy(result, candidates)

	// Snapshot in-flight counts for consistent sort ordering
	loads := make(map[string]int64, len(result))
	for _, c := range result {
		loads[c.Name] = s.tracker.InFlight(c.Name)
	}

	sort.SliceStable(result, func(i, j int) bool {
		loadI := loads[result[i].Name]
		loadJ := loads[result[j].Name]
		if loadI != loadJ {
			return loadI < loadJ
		}
		return result[i].Priority < result[j].Priority
	})

	return result
}

func (s *LeastLoadStrategy) Name() string { return "least-load" }
