package modelregistry

import (
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stretchr/testify/require"
)

// TestDownloadTracker_OnCompleteFiresWithoutDebounce is a regression guard
// for the cache-unification arc's closing of issue #6.
//
// Before the arc, SetOnCompleteCallback fired through a 2s time.AfterFunc
// debounce. That 2-second window was the observable race: a user issuing
// /v1/chat/completions immediately after a successful pull hit the
// coordinator's model cache before it had been invalidated and got back
// "model not found in registry." The debounce was intended to coalesce
// bursts of completions, but only ever had one completion per download
// to coalesce — the fix was to drop it entirely and fire the callback
// on the completed-transition edge.
//
// If this test ever times out, someone reintroduced a timer in the
// completion path. Don't — the invariant is "one completion event,
// one callback fire, no delay."
func TestDownloadTracker_OnCompleteFiresWithoutDebounce(t *testing.T) {
	dt := NewDownloadTrackerWithPersistence(filepath.Join(t.TempDir(), "downloads.json"))
	// Stop before TempDir's RemoveAll (cleanups run LIFO): a terminal update
	// persists from a detached goroutine, which otherwise races the removal.
	t.Cleanup(dt.Stop)

	fired := make(chan struct{})
	dt.SetOnCompleteCallback(func() { close(fired) })

	const key = "test-host/huggingface/test-org/test-repo"
	gen := dt.StartDownload(key, "test-org/test-repo")
	dt.UpdateDownloadFullGen(key, gen, string(constants.StatusCompleted), "", 0, 0, 100, 0, 0, 0)

	// Tight budget: the callback fires via `go cb()` from inside
	// updateDownloadFullInternal, so 500ms is generous for scheduling.
	// The old 2s debounce would miss this window by 1.5s.
	select {
	case <-fired:
		// ok — no debounce regression.
	case <-time.After(500 * time.Millisecond):
		t.Fatal("OnComplete callback did not fire within 500ms; debounce regression? See issue #6 and commit 94882b2.")
	}
}

// TestDownloadTracker_OnCompleteFiresOncePerTransition ensures a single
// completed-edge produces exactly one callback invocation. Second updates
// at status=completed are no-ops (wasCompleted=true guard) — prevents a
// redundant cache-invalidation storm on repeated status writes.
func TestDownloadTracker_OnCompleteFiresOncePerTransition(t *testing.T) {
	dt := NewDownloadTrackerWithPersistence(filepath.Join(t.TempDir(), "downloads.json"))
	// Stop before TempDir's RemoveAll (cleanups run LIFO): a terminal update
	// persists from a detached goroutine, which otherwise races the removal.
	t.Cleanup(dt.Stop)

	var fires atomic.Int32
	dt.SetOnCompleteCallback(func() { fires.Add(1) })

	const key = "test-host/huggingface/test-org/test-repo"
	gen := dt.StartDownload(key, "test-org/test-repo")
	dt.UpdateDownloadFullGen(key, gen, string(constants.StatusCompleted), "", 0, 0, 100, 0, 0, 0)
	dt.UpdateDownloadFullGen(key, gen, string(constants.StatusCompleted), "", 0, 0, 100, 0, 0, 0)

	// Let the goroutine-spawned callback run.
	require.Eventually(t, func() bool { return fires.Load() >= 1 }, time.Second, 10*time.Millisecond)

	// Give any stray goroutines a grace window to fire.
	time.Sleep(100 * time.Millisecond)

	require.EqualValues(t, 1, fires.Load(), "OnComplete must fire once per completed-transition edge, not per write")
}
