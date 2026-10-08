package fallback

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPriorityStrategy(t *testing.T) {
	s := &PriorityStrategy{}
	assert.Equal(t, "priority", s.Name())

	candidates := []Candidate{
		{Name: "a", Priority: 1},
		{Name: "b", Priority: 2},
		{Name: "c", Priority: 3},
	}

	result := s.Select(candidates)
	require.Len(t, result, 3)
	assert.Equal(t, "a", result[0].Name)
	assert.Equal(t, "b", result[1].Name)
	assert.Equal(t, "c", result[2].Name)
}

func TestPriorityStrategy_DoesNotMutateInput(t *testing.T) {
	s := &PriorityStrategy{}
	candidates := []Candidate{{Name: "a"}, {Name: "b"}}
	result := s.Select(candidates)
	result[0].Name = "modified"
	assert.Equal(t, "a", candidates[0].Name)
}

func TestLeastLoadStrategy(t *testing.T) {
	tracker := NewLoadTracker()
	s := NewLeastLoadStrategy(tracker)
	assert.Equal(t, "least-load", s.Name())

	tracker.Acquire("a")
	tracker.Acquire("a")
	tracker.Acquire("b")

	candidates := []Candidate{
		{Name: "a", Priority: 1}, // 2 in-flight
		{Name: "b", Priority: 2}, // 1 in-flight
		{Name: "c", Priority: 3}, // 0 in-flight
	}

	result := s.Select(candidates)
	require.Len(t, result, 3)
	assert.Equal(t, "c", result[0].Name, "least loaded should be first")
	assert.Equal(t, "b", result[1].Name)
	assert.Equal(t, "a", result[2].Name, "most loaded should be last")
}

func TestLeastLoadStrategy_PriorityTiebreaker(t *testing.T) {
	tracker := NewLoadTracker()
	s := NewLeastLoadStrategy(tracker)

	// Both have 0 in-flight — tiebreak by priority (lower number = higher priority)
	candidates := []Candidate{
		{Name: "low-pri", Priority: 5},
		{Name: "high-pri", Priority: 1},
	}

	result := s.Select(candidates)
	assert.Equal(t, "high-pri", result[0].Name, "lower priority number should win on tie")
}

func TestFastestStrategy(t *testing.T) {
	tracker := NewLatencyTracker(10)
	s := NewFastestStrategy(tracker)
	assert.Equal(t, "fastest", s.Name())

	// Record latencies
	tracker.Record("slow", 500*time.Millisecond)
	tracker.Record("fast", 100*time.Millisecond)

	candidates := []Candidate{
		{Name: "slow", Priority: 1},
		{Name: "fast", Priority: 2},
		{Name: "unknown", Priority: 3},
	}

	result := s.Select(candidates)
	require.Len(t, result, 3)
	assert.Equal(t, "fast", result[0].Name, "fastest should be first")
	assert.Equal(t, "slow", result[1].Name)
	assert.Equal(t, "unknown", result[2].Name, "no data goes to end")
}

func TestFastestStrategy_NoData_FallsBackToPriority(t *testing.T) {
	tracker := NewLatencyTracker(10)
	s := NewFastestStrategy(tracker)

	// No latency data at all — should fall back to priority ordering
	candidates := []Candidate{
		{Name: "low-pri", Priority: 5},
		{Name: "high-pri", Priority: 1},
	}

	result := s.Select(candidates)
	assert.Equal(t, "high-pri", result[0].Name, "should use priority when no data")
}

func TestStrategyRegistry(t *testing.T) {
	reg := NewStrategyRegistry()
	reg.Register(&PriorityStrategy{})

	s := reg.Get("priority")
	require.NotNil(t, s)
	assert.Equal(t, "priority", s.Name())

	assert.Nil(t, reg.Get("nonexistent"))
}
