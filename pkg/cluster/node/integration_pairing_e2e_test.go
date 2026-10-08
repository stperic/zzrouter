package clusternode

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIntegration_PairingE2E_RealTLS is the end-to-end guard against
// the category of bug where worker-facing pairing endpoints go green
// in unit tests (which bypass the TLS config) but are unreachable in
// production because the coord's TLS listener rejects the clientless
// handshake. The whole arc runs against the real listener:
//
//  1. Coordinator: New + Start — binds a real TLS listener with real
//     buildTLSConfig, serves /cluster/pairing-request unauthenticated.
//  2. Unclaimed worker: New + Start (dormant, no listener) +
//     BeginPairing targeting the coord's bound Addr().
//  3. Admin goroutine: polls PendingPairings() until the worker's
//     entry shows up, then calls AcceptPairing.
//  4. Assertion: worker flips to Worker mode with a cert on disk
//     that chains to the coord's CA.
//  5. Stop both nodes; assert no goroutine leaks (with grace window).
//
// Failure modes this test catches:
//
//   - ClientAuth set too strictly on the coord (rejects the
//     clientless pairing handshake — the C1 bug).
//   - VerifyPeerCertificate rejecting the coord's chain.
//   - Missing mTLSOUCheck letting the pairing request leak into
//     mTLS-protected handlers (inverted-polarity version of C1).
//   - Pairing code reaching the store malformed, blocking accept.
func TestIntegration_PairingE2E_RealTLS(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: skipping in -short mode")
	}

	// Baseline goroutine count so the leak assertion at the end has a
	// reference point. Captured BEFORE we spawn anything.
	baseline := runtime.NumGoroutine()

	coord := bringUpCoordinator(t)
	coordURL := "https://" + coord.Addr().String()

	// Derive the coord's CA SPKI fingerprint for the worker's pin.
	caCert := coord.ca.Certificate()
	sum := sha256.Sum256(caCert.RawSubjectPublicKeyInfo)
	caFingerprint := hex.EncodeToString(sum[:])

	worker := bringUpUnclaimedWorker(t)

	// Start the admin-accept goroutine: poll for the worker's pending
	// entry, then accept it. Uses AcceptPairing (the same codepath the
	// admin HTTP handler drives).
	acceptDone := make(chan error, 1)
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			pending := coord.PendingPairings()
			if len(pending) > 0 {
				// Fetch the real code from the worker's active window.
				// The admin normally gets this off the worker display;
				// in the test harness we read it directly.
				info, ok := worker.PairingWindowInfoSnapshot()
				if !ok {
					acceptDone <- assertErr("worker has no active window when coord has a pending entry")
					return
				}
				_, err := coord.AcceptPairing(info.Code, coordURL)
				acceptDone <- err
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
		acceptDone <- assertErr("no pending pairing entry on coord after 5s")
	}()

	// Drive the worker's BeginPairing. This kicks off the polling
	// goroutine that'll hit coord's /cluster/pairing-request over real
	// TLS.
	_, err := worker.BeginPairing(BeginPairingOptions{
		CoordinatorURL: coordURL,
		CAFingerprint:  caFingerprint,
	})
	require.NoError(t, err)

	// Wait for the accept to land.
	select {
	case err := <-acceptDone:
		require.NoError(t, err, "admin accept failed")
	case <-time.After(6 * time.Second):
		t.Fatal("admin accept goroutine did not complete")
	}

	// Worker's loop should see "approved" on its next (or current)
	// poll and flip to Worker. With pollInterval=3s default and
	// long-poll serving, this converges within a few seconds.
	require.Eventually(t, func() bool { return worker.Mode() == Worker },
		8*time.Second, 50*time.Millisecond,
		"worker must flip to Worker mode after pairing completes")

	// Worker's installed cert must chain to coord's CA.
	caPath := filepath.Join(worker.cfg.ClusterDir, "ca.pem")
	caPEM, err := os.ReadFile(caPath)
	require.NoError(t, err, "worker must have persisted coord CA to cluster dir")
	assert.NotEmpty(t, caPEM)

	nodePath := filepath.Join(worker.cfg.IdentityDir, "node.pem")
	nodePEM, err := os.ReadFile(nodePath)
	require.NoError(t, err)

	caBlock, _ := pem.Decode(caPEM)
	require.NotNil(t, caBlock)
	parsedCA, err := x509.ParseCertificate(caBlock.Bytes)
	require.NoError(t, err)

	leafBlock, _ := pem.Decode(nodePEM)
	require.NotNil(t, leafBlock)
	leaf, err := x509.ParseCertificate(leafBlock.Bytes)
	require.NoError(t, err)

	roots := x509.NewCertPool()
	roots.AddCert(parsedCA)
	_, err = leaf.Verify(x509.VerifyOptions{
		Roots:     roots,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	})
	require.NoError(t, err,
		"worker's signed identity cert must chain to the coord CA it persisted")

	// Worker's coordinator URL should match the coord we paired against.
	urlData, err := os.ReadFile(filepath.Join(worker.cfg.ClusterDir, "coordinator_url"))
	require.NoError(t, err)
	assert.Contains(t, string(urlData), coord.Addr().String())

	// Clean shutdown. Both nodes must Stop without leaking goroutines.
	require.NoError(t, worker.Stop(context.Background()))
	require.NoError(t, coord.Stop(context.Background()))

	// Grace window for goroutines spawned by Start to unwind. GC
	// goroutines (pairing store, rate limiter) exit on Stop but
	// time.Ticker.C draining can take a moment.
	require.Eventually(t, func() bool {
		return runtime.NumGoroutine() <= baseline+2
	}, 2*time.Second, 50*time.Millisecond,
		"expected goroutine count to return to baseline %d, saw %d",
		baseline, runtime.NumGoroutine())
}

// bringUpCoordinator builds + starts a Coordinator node on an OS-
// assigned port with an inference handler pre-wired (not used by this
// test but required for worker-track modes; Coordinator doesn't
// actually need it but the constructor is mode-agnostic).
func bringUpCoordinator(t *testing.T) *Node {
	t.Helper()
	root := t.TempDir()
	cfg := Config{
		Mode:        Coordinator,
		Port:        0,
		BindHost:    "127.0.0.1",
		IdentityDir: filepath.Join(root, "coord-identity"),
		CADir:       filepath.Join(root, "coord-ca"),
		NodeName:    "coord",
	}
	n, err := New(cfg)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	t.Cleanup(func() { _ = n.Stop(context.Background()) })
	require.NoError(t, n.Start(ctx))
	require.NotNil(t, n.Addr(), "coord listener must be bound")
	return n
}

// bringUpUnclaimedWorker builds + starts an Unclaimed worker node
// (dormant on the cluster network, no listener). The inference
// handler is the 501-stub; BeginPairing doesn't need it to succeed
// but completePairing requires it be pre-wired for the post-pairing
// listener build.
func bringUpUnclaimedWorker(t *testing.T) *Node {
	t.Helper()
	root := t.TempDir()
	cfg := Config{
		Mode:              Unclaimed,
		Port:              0,
		BindHost:          "127.0.0.1",
		IdentityDir:       filepath.Join(root, "worker-identity"),
		ClusterDir:        filepath.Join(root, "worker-cluster"),
		PairingPath:       filepath.Join(root, "pairing.txt"),
		NodeName:          "worker-e2e",
		AdvertiseDNSNames: []string{"worker-e2e"},
	}
	n, err := New(cfg)
	require.NoError(t, err)
	stub := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotImplemented)
	})
	require.NoError(t, n.SetAdminAPIHandler(stub))
	require.NoError(t, n.SetWorkerCompatHandler(stub))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	t.Cleanup(func() { _ = n.Stop(context.Background()) })
	require.NoError(t, n.Start(ctx))
	require.Nil(t, n.Addr(), "unclaimed worker must be dormant (no bound listener)")
	return n
}

type testAssertionErr struct{ msg string }

func (e *testAssertionErr) Error() string { return e.msg }
func assertErr(msg string) error          { return &testAssertionErr{msg: msg} }
