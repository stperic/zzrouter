package utils

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// GoSafe must run the function — establishing the basic happy path.
func TestGoSafe_RunsFunction(t *testing.T) {
	t.Parallel()
	done := make(chan struct{})
	GoSafe("test-runs", func() {
		close(done)
	})
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("GoSafe did not invoke fn within 2s")
	}
}

// GoSafe must catch a panic in fn so the test process survives. If
// the panic propagated, this test would crash the test binary —
// PASS is the assertion.
func TestGoSafe_RecoversFromPanic(t *testing.T) {
	t.Parallel()
	var wg sync.WaitGroup
	wg.Add(1)
	GoSafe("test-panic", func() {
		defer wg.Done() // must run even after panic — recover unblocks
		panic("intentional test panic — recovery should swallow")
	})
	// 2s bound: if recover fails the wg.Done never fires (defer order
	// is recover-then-Done since recover is defined LATER in the body)
	// and the test will time out instead of crashing.
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("wg.Done did not fire — recover may have failed")
	}
}

// RecoverAndLog (exported deferred handler) must compose with the
// caller's own defers without breaking the wg.Done-after-recover
// ordering documented in safego.go.
func TestRecoverAndLog_ComposesWithWaitGroup(t *testing.T) {
	t.Parallel()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()                 // runs SECOND (LIFO)
		defer RecoverAndLog("composes") // runs FIRST
		panic("composed panic")
	}()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("wg.Done did not fire — recover ordering may be wrong")
	}
}

// Many concurrent panics must all be recovered cleanly. Catches a
// regression where someone accidentally captures shared state in the
// recover handler.
func TestGoSafe_RecoversMultipleConcurrentPanics(t *testing.T) {
	t.Parallel()
	const N = 32
	var wg sync.WaitGroup
	wg.Add(N)
	var fired atomic.Int64
	for i := 0; i < N; i++ {
		i := i
		GoSafe("concurrent", func() {
			defer wg.Done()
			defer fired.Add(1)
			if i%2 == 0 {
				panic(i)
			}
		})
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("not all goroutines reached wg.Done — recover failed somewhere")
	}
	require.Equal(t, int64(N), fired.Load())
	assert.Equal(t, N, int(fired.Load()))
}
