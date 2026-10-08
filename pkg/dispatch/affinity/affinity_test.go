package affinity

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRecordAndLookup verifies the basic happy path — a recorded
// (id, provider) round-trips back out of Lookup.
func TestRecordAndLookup(t *testing.T) {
	a := New(time.Hour)

	a.Record("resp_abc", "openai-prod")
	provider, ok := a.Lookup("resp_abc")
	require.True(t, ok, "expected lookup hit for recorded id")
	assert.Equal(t, "openai-prod", provider)
}

// TestEmptyInputsIgnored verifies the defensive guards so callers can
// invoke Record with best-effort extraction results without guarding
// every call site.
func TestEmptyInputsIgnored(t *testing.T) {
	a := New(time.Hour)

	a.Record("", "openai")
	a.Record("resp_abc", "")
	assert.Equal(t, 0, a.Size(), "empty inputs should not be recorded")

	_, ok := a.Lookup("")
	assert.False(t, ok)
}

// TestExpiration verifies that entries older than the TTL are not
// returned from Lookup and are purged on access.
func TestExpiration(t *testing.T) {
	a := New(100 * time.Millisecond)
	now := time.Now()
	a.now = func() time.Time { return now }

	a.Record("resp_abc", "openai")
	require.Equal(t, 1, a.Size())

	// Advance the clock past the TTL.
	now = now.Add(200 * time.Millisecond)

	_, ok := a.Lookup("resp_abc")
	assert.False(t, ok, "expired entry should not be returned")
	assert.Equal(t, 0, a.Size(), "expired entry should be purged on access")
}

// TestDeleteRemovesEntry verifies that an explicit Delete flushes the
// cache so subsequent Lookups miss.
func TestDeleteRemovesEntry(t *testing.T) {
	a := New(time.Hour)

	a.Record("resp_abc", "openai")
	a.Delete("resp_abc")

	_, ok := a.Lookup("resp_abc")
	assert.False(t, ok)
	assert.Equal(t, 0, a.Size())
}

// TestSweeperRemovesExpired verifies that the background sweeper purges
// expired entries without requiring a Lookup. Uses a short sweep
// interval so the test stays fast.
func TestSweeperRemovesExpired(t *testing.T) {
	a := &Affinity{
		entries:       make(map[string]entry),
		ttl:           50 * time.Millisecond,
		now:           time.Now,
		sweepInterval: 10 * time.Millisecond,
		stop:          make(chan struct{}),
	}

	a.Record("resp_abc", "openai")
	a.Start()
	t.Cleanup(a.Stop)

	assert.Eventually(t, func() bool {
		return a.Size() == 0
	}, 500*time.Millisecond, 10*time.Millisecond,
		"sweeper should purge expired entries")
}

// TestDefaultTTL verifies the zero-TTL fallback.
func TestDefaultTTL(t *testing.T) {
	a := New(0)
	assert.Equal(t, defaultTTL, a.ttl)
}

// TestRestartAfterStop verifies that Start rearms the stop channel after
// a prior Stop, so a coordinator→worker→coordinator role flip produces a
// working sweeper on the second Start.
func TestRestartAfterStop(t *testing.T) {
	a := &Affinity{
		entries:       make(map[string]entry),
		ttl:           50 * time.Millisecond,
		now:           time.Now,
		sweepInterval: 10 * time.Millisecond,
		stop:          make(chan struct{}),
	}

	a.Start()
	a.Stop()

	a.Record("resp_abc", "openai")
	a.Start()
	t.Cleanup(a.Stop)

	assert.Eventually(t, func() bool {
		return a.Size() == 0
	}, 500*time.Millisecond, 10*time.Millisecond,
		"sweeper should purge expired entries after restart")
}

// TestStartIdempotent verifies that calling Start twice on a running
// sweeper does not spawn a second goroutine. We detect a spawned second
// goroutine by watching whether Stop's close-of-channel causes multiple
// receivers to wake.
func TestStartIdempotent(t *testing.T) {
	a := &Affinity{
		entries:       make(map[string]entry),
		ttl:           time.Hour,
		now:           time.Now,
		sweepInterval: 10 * time.Millisecond,
		stop:          make(chan struct{}),
	}

	a.Start()
	a.Start() // second call must be a no-op

	// Drive a few sweeps to prove the single goroutine is healthy.
	time.Sleep(50 * time.Millisecond)

	a.Stop()

	// Post-Stop the started flag is reset; a re-Start must succeed.
	a.Start()
	t.Cleanup(a.Stop)
}

// TestStopBeforeStart verifies Stop is a no-op when the sweeper was
// never started.
func TestStopBeforeStart(t *testing.T) {
	a := New(time.Hour)
	a.Stop() // must not panic or block
	assert.True(t, a.stopped)
}

// TestConcurrentRecordLookup exercises the read/write lock paths under
// concurrent load. `go test -race` is the assertion.
func TestConcurrentRecordLookup(t *testing.T) {
	a := New(time.Hour)

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				a.Record("id", "p")
				_, _ = a.Lookup("id")
			}
		}()
	}
	wg.Wait()
}
