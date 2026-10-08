package server

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSyncExecutor_InflightDedupe asserts a second claim for the same
// key returns the existing job ID and releasing unblocks new claims.
func TestSyncExecutor_InflightDedupe(t *testing.T) {
	e := &SyncExecutor{inflight: map[string]string{}}

	prior, ok := e.claimInflightDeploy("llama3|gguf", "job-1")
	require.True(t, ok)
	require.Empty(t, prior)

	prior, ok = e.claimInflightDeploy("llama3|gguf", "job-2")
	assert.False(t, ok)
	assert.Equal(t, "job-1", prior)

	prior, ok = e.claimInflightDeploy("llama3|safetensors", "job-3")
	assert.True(t, ok)
	assert.Empty(t, prior)

	e.releaseInflightDeploy("llama3|gguf")
	prior, ok = e.claimInflightDeploy("llama3|gguf", "job-4")
	assert.True(t, ok)
	assert.Empty(t, prior)
}

// TestSyncExecutor_SemaphoreBounds asserts acquireSlot is non-blocking
// and rejects over-cap requests so callers can return 429.
func TestSyncExecutor_SemaphoreBounds(t *testing.T) {
	e := &SyncExecutor{sem: make(chan struct{}, maxConcurrentSyncOps)}

	// Acquire exactly maxConcurrentSyncOps slots.
	for i := 0; i < maxConcurrentSyncOps; i++ {
		assert.True(t, e.acquireSlot(), "slot %d should acquire", i)
	}

	// The next acquire must fail non-blockingly.
	assert.False(t, e.acquireSlot(), "slot beyond cap should not acquire")

	// Releasing one slot lets exactly one more acquire in.
	e.releaseSlot()
	assert.True(t, e.acquireSlot())
	assert.False(t, e.acquireSlot())
}

// TestSyncExecutor_ClaimRaceSafety asserts that under a goroutine storm
// on the same key, at most one claim is held simultaneously. Race
// detector catches torn accesses; this test catches claim/release
// ordering bugs.
func TestSyncExecutor_ClaimRaceSafety(t *testing.T) {
	e := &SyncExecutor{inflight: map[string]string{}}

	const goroutines = 32
	var wg sync.WaitGroup
	wg.Add(goroutines)

	var concurrent atomic.Int32
	var maxConcurrent atomic.Int32

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			_, ok := e.claimInflightDeploy("shared-key", "job")
			if !ok {
				return
			}
			n := concurrent.Add(1)
			for {
				m := maxConcurrent.Load()
				if n <= m || maxConcurrent.CompareAndSwap(m, n) {
					break
				}
			}
			concurrent.Add(-1)
			e.releaseInflightDeploy("shared-key")
		}()
	}
	wg.Wait()

	assert.Equal(t, int32(1), maxConcurrent.Load(),
		"at most one goroutine may hold the claim at a time")
}
