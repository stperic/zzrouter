package clusternode

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Stop must wait for the pairing-loop goroutine spawned by BeginPairing
// to exit before returning. Pre-fix Stop only signaled cancel and
// returned, leaving the goroutine running asynchronously — a leak past
// Stop that fooled goroutine-leak guards into thinking the node had
// shut down.
//
// The test forces a Started Unclaimed node to begin pairing against a
// non-routable coordinator URL, so the pairing loop is parked in HTTP
// retry/backoff. Stop should still return promptly because cancel
// closes the loop ctx; the window completion channel pins the join invariant.
func TestStop_JoinsPairingGoroutine(t *testing.T) {
	t.Parallel()
	n := startedUnclaimedNodeForJoinTest(t)

	// Pairing against a non-routable address — the loop will be parked
	// in HTTP retry until ctx is cancelled. We only need it to spawn,
	// not to succeed.
	_, err := n.BeginPairing(BeginPairingOptions{
		CoordinatorURL: "https://127.0.0.1:1/never",
		CAFingerprint:  validPin,
	})
	require.NoError(t, err)

	n.pairingMu.Lock()
	window := n.activePairingWindow
	n.pairingMu.Unlock()
	require.NotNil(t, window)
	done := window.done

	// Stop must return promptly and join this specific pairing attempt.
	stopReturned := make(chan error, 1)
	go func() {
		stopReturned <- n.Stop(context.Background())
	}()

	select {
	case err := <-stopReturned:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return within 5s — pairing goroutine likely not joined")
	}

	select {
	case <-done:
	default:
		t.Fatal("Stop returned before the pairing attempt exited")
	}
}

// startedUnclaimedNodeForJoinTest builds an Unclaimed node and calls
// Start so that Stop has the same prerequisites as production. The
// node is wired with a stub inference handler (Unclaimed mode requires
// one before Start per Node.Start's check).
func startedUnclaimedNodeForJoinTest(t *testing.T) *Node {
	t.Helper()
	dir := t.TempDir()
	cfg := Config{
		Mode:        Unclaimed,
		IdentityDir: filepath.Join(dir, "identity"),
		ClusterDir:  filepath.Join(dir, "cluster"),
		PairingPath: filepath.Join(dir, "pairing.txt"),
		NodeName:    "worker-stop-join",
		BindHost:    "127.0.0.1",
		Port:        0, // ephemeral
	}
	n, err := New(cfg)
	require.NoError(t, err)

	// Unclaimed Start requires both handlers — wire stubs that error
	// on every call (we never invoke them).
	n.adminAPI = &stubInfer{}
	n.workerCompat = &stubInfer{}

	require.NoError(t, n.Start(context.Background()))
	t.Cleanup(func() {
		// Best-effort double-stop; idempotent per Stop's contract.
		_ = n.Stop(context.Background())
	})
	return n
}

// stubInfer satisfies http.Handler so Unclaimed Start's inference-
// handler-required check passes. Body is never invoked in this test
// path — the pairing loop is parked in HTTP retry against an
// unroutable address, never reaching the inference surface.
type stubInfer struct{}

func (*stubInfer) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusInternalServerError)
}
