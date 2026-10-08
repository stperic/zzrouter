package fallback

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestLatencyTracker_Record_Average(t *testing.T) {
	tracker := NewLatencyTracker(10)

	tracker.Record("dep-a", 100*time.Millisecond)
	tracker.Record("dep-a", 200*time.Millisecond)
	tracker.Record("dep-a", 300*time.Millisecond)

	avg := tracker.AverageLatency("dep-a")
	assert.Equal(t, 200*time.Millisecond, avg)
}

func TestLatencyTracker_NoData(t *testing.T) {
	tracker := NewLatencyTracker(10)

	assert.Equal(t, time.Duration(0), tracker.AverageLatency("unknown"))
	assert.False(t, tracker.HasData("unknown"))
}

func TestLatencyTracker_HasData(t *testing.T) {
	tracker := NewLatencyTracker(10)

	assert.False(t, tracker.HasData("dep-a"))
	tracker.Record("dep-a", 100*time.Millisecond)
	assert.True(t, tracker.HasData("dep-a"))
}

func TestLatencyTracker_WindowOverflow(t *testing.T) {
	tracker := NewLatencyTracker(3)

	// Fill window
	tracker.Record("dep-a", 100*time.Millisecond)
	tracker.Record("dep-a", 200*time.Millisecond)
	tracker.Record("dep-a", 300*time.Millisecond)
	assert.Equal(t, 200*time.Millisecond, tracker.AverageLatency("dep-a"))

	// Overflow: oldest (100ms) replaced by 400ms
	tracker.Record("dep-a", 400*time.Millisecond)
	// Window: [400, 200, 300] → avg = 300ms
	assert.Equal(t, 300*time.Millisecond, tracker.AverageLatency("dep-a"))
}

func TestLatencyTracker_MultipleDeployments(t *testing.T) {
	tracker := NewLatencyTracker(10)

	tracker.Record("fast", 50*time.Millisecond)
	tracker.Record("slow", 500*time.Millisecond)

	assert.Less(t, tracker.AverageLatency("fast"), tracker.AverageLatency("slow"))
}

func TestLatencyTracker_DefaultWindowSize(t *testing.T) {
	tracker := NewLatencyTracker(0) // Should default to 100
	tracker.Record("dep-a", 100*time.Millisecond)
	assert.True(t, tracker.HasData("dep-a"))
}

func TestLatencyTracker_Snapshot(t *testing.T) {
	tr := NewLatencyTracker(10)
	avg, n := tr.Snapshot("nope")
	if avg != 0 || n != 0 {
		t.Fatalf("Snapshot on unknown dep = (%v,%d), want (0,0)", avg, n)
	}

	tr.Record("r1", 100*time.Millisecond)
	tr.Record("r1", 300*time.Millisecond)
	avg, n = tr.Snapshot("r1")
	if avg != 200*time.Millisecond {
		t.Fatalf("Snapshot avg = %v, want 200ms", avg)
	}
	if n != 2 {
		t.Fatalf("Snapshot samples = %d, want 2", n)
	}
}
