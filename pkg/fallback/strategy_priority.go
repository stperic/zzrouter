package fallback

// PriorityStrategy selects deployments by priority (ascending — 1 = first, 2 = fallback).
// Candidates are already sorted by priority in GroupStore, so this
// strategy simply returns them as-is.
type PriorityStrategy struct{}

func (s *PriorityStrategy) Select(candidates []Candidate) []Candidate {
	// Already sorted by priority ascending in GroupStore.
	// Copy to protect input from caller mutations.
	result := make([]Candidate, len(candidates))
	copy(result, candidates)
	return result
}

func (s *PriorityStrategy) Name() string { return "priority" }
