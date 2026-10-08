package keys

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestFileKeyStore_OnChangeFiresOnMutation is a regression guard for the
// auth-cache security fix in commit 2ec802b. The keyAuthenticator's
// principal cache subscribes to KeyStore.OnChange so that revoking or
// rotating a key takes effect on the next request. Before the fix, a
// revoked key kept authenticating for up to authCacheTTL because no
// invalidation wire existed.
//
// If this test ever regresses (mutation stops firing listeners, or
// listeners no longer fire on the right set of mutations), the auth
// cache silently retains stale principals and key revocation becomes
// delayed by the cache TTL.
func TestFileKeyStore_OnChangeFiresOnMutation(t *testing.T) {
	t.Parallel()
	store := NewFileKeyStore("")

	var fires atomic.Int32
	store.OnChange(func() { fires.Add(1) })

	// Create → 1 fire
	_, err := store.Create("k1", &VirtualKey{Name: "K1", Role: "user", TeamID: "t", TeamRole: "owner"})
	require.NoError(t, err)
	require.EqualValues(t, 1, fires.Load(), "Create must fire OnChange")

	// Update → 2 fires
	err = store.Update("k1", &VirtualKey{Name: "K1-renamed", Role: "user", TeamID: "t", TeamRole: "owner"})
	require.NoError(t, err)
	require.EqualValues(t, 2, fires.Load(), "Update must fire OnChange")

	// RotateKey → 3 fires
	_, err = store.RotateKey("k1")
	require.NoError(t, err)
	require.EqualValues(t, 3, fires.Load(), "RotateKey must fire OnChange")

	// Delete → 4 fires
	err = store.Delete("k1")
	require.NoError(t, err)
	require.EqualValues(t, 4, fires.Load(), "Delete must fire OnChange")

	// Errors don't fire: create-duplicate after delete errors cleanly.
	err = store.Delete("k1")
	require.Error(t, err)
	require.EqualValues(t, 4, fires.Load(), "failed Delete must not fire OnChange")
}

// TestFileKeyStore_OnChangeSupportsMultipleListeners ensures more than
// one subscriber receives the event — forward-compat for future uses
// (audit log, cache invalidation, etc.) that might stack on the auth
// cache's existing subscription.
func TestFileKeyStore_OnChangeSupportsMultipleListeners(t *testing.T) {
	t.Parallel()
	store := NewFileKeyStore("")

	var a, b atomic.Int32
	store.OnChange(func() { a.Add(1) })
	store.OnChange(func() { b.Add(1) })

	_, err := store.Create("k", &VirtualKey{Name: "K", Role: "user", TeamID: "t", TeamRole: "owner"})
	require.NoError(t, err)

	require.EqualValues(t, 1, a.Load())
	require.EqualValues(t, 1, b.Load())
}

// TestFileKeyStore_OnChangeRunsOutsideLock verifies that a listener
// invoking store methods does not deadlock. notifyListeners must be
// called after the store lock is released; otherwise a subscribing
// cache that wants to list keys on invalidation would deadlock.
func TestFileKeyStore_OnChangeRunsOutsideLock(t *testing.T) {
	t.Parallel()
	store := NewFileKeyStore("")

	done := make(chan struct{})
	store.OnChange(func() {
		// Call back into the store under the listener. If
		// notifyListeners ran under the write lock, this would
		// deadlock the write-lock re-entry (sync.RWMutex is not
		// reentrant).
		_ = store.List()
		close(done)
	})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = store.Create("k", &VirtualKey{Name: "K", Role: "user", TeamID: "t", TeamRole: "owner"})
	}()
	wg.Wait()
	<-done
}
