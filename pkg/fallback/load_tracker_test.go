package fallback

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadTracker_AcquireRelease(t *testing.T) {
	tracker := NewLoadTracker()

	assert.Equal(t, int64(0), tracker.InFlight("dep-a"))

	tracker.Acquire("dep-a")
	assert.Equal(t, int64(1), tracker.InFlight("dep-a"))

	tracker.Acquire("dep-a")
	assert.Equal(t, int64(2), tracker.InFlight("dep-a"))

	tracker.Release("dep-a")
	assert.Equal(t, int64(1), tracker.InFlight("dep-a"))

	tracker.Release("dep-a")
	assert.Equal(t, int64(0), tracker.InFlight("dep-a"))
}

func TestLoadTracker_ReleaseNeverNegative(t *testing.T) {
	tracker := NewLoadTracker()
	tracker.Release("dep-a")
	assert.Equal(t, int64(0), tracker.InFlight("dep-a"))
}

func TestLoadTracker_MultipleDeployments(t *testing.T) {
	tracker := NewLoadTracker()

	tracker.Acquire("dep-a")
	tracker.Acquire("dep-a")
	tracker.Acquire("dep-b")

	assert.Equal(t, int64(2), tracker.InFlight("dep-a"))
	assert.Equal(t, int64(1), tracker.InFlight("dep-b"))
	assert.Equal(t, int64(0), tracker.InFlight("dep-c"))
}

func TestLoadTracker_Concurrent(t *testing.T) {
	tracker := NewLoadTracker()
	const goroutines = 100

	var wg sync.WaitGroup
	wg.Add(goroutines * 2)

	for range goroutines {
		go func() {
			defer wg.Done()
			tracker.Acquire("dep-a")
		}()
	}
	for range goroutines {
		go func() {
			defer wg.Done()
			tracker.Acquire("dep-b")
		}()
	}

	wg.Wait()
	require.Equal(t, int64(goroutines), tracker.InFlight("dep-a"))
	require.Equal(t, int64(goroutines), tracker.InFlight("dep-b"))
}
