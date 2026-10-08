package clusternode

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// unclaimedNodeForLifecycle builds an Unclaimed Node with all the
// on-disk state initUnclaimedState expects (identity keypair, etc.)
// so BeginPairing can build a CSR. Does NOT call Start — we exercise
// BeginPairing / CancelPairing in isolation, without a live listener.
func unclaimedNodeForLifecycle(t *testing.T) *Node {
	t.Helper()
	dir := t.TempDir()
	cfg := Config{
		Mode:        Unclaimed,
		IdentityDir: filepath.Join(dir, "identity"),
		ClusterDir:  filepath.Join(dir, "cluster"),
		PairingPath: filepath.Join(dir, "pairing.txt"),
		NodeName:    "worker-test",
	}
	// Unclaimed requires inference handler before Start; we don't
	// call Start so skip that.
	n, err := New(cfg)
	require.NoError(t, err)
	t.Cleanup(func() {
		n.pairingMu.Lock()
		var done <-chan struct{}
		if n.activePairingWindow != nil {
			done = n.activePairingWindow.done
			n.cancelWindowLocked()
		}
		n.pairingMu.Unlock()
		if done != nil {
			<-done
		}
	})
	return n
}

// validPin is a well-formed but arbitrary CA fingerprint hex string.
// Tests that don't exercise the actual transport handshake can use it
// to satisfy BeginPairing's validation.
const validPin = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestBeginPairing_NewWindow(t *testing.T) {
	// Hold the real pairing request until cleanup, so network timing cannot close the window.
	ca := newTestCA(t, "lifecycle")
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	coordinator := testTLSServer(t, ca, func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(entered) })
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	t.Cleanup(func() { close(release) })
	n := unclaimedNodeForLifecycle(t)

	info, err := n.BeginPairing(BeginPairingOptions{
		CoordinatorURL: coordinator.URL,
		CAFingerprint:  ca.spkiHex,
	})
	require.NoError(t, err)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("pairing request did not reach the coordinator")
	}
	assert.Len(t, info.Code, 16)
	assert.False(t, info.Deadline.IsZero())

	// pairing.txt present, 0600.
	data, err := os.ReadFile(n.cfg.PairingPath)
	require.NoError(t, err)
	assert.Contains(t, string(data), info.Code)
	st, err := os.Stat(n.cfg.PairingPath)
	require.NoError(t, err)
	if runtime.GOOS != "windows" {
		assert.Equal(t, os.FileMode(0o600), st.Mode().Perm())
	} else {
		// Windows mode bits do not express owner-only access; this test does not inspect ACLs.
		assert.True(t, st.Mode().IsRegular())
	}

	// Snapshot accessor reports the same window.
	got, ok := n.PairingWindowInfoSnapshot()
	require.True(t, ok)
	assert.Equal(t, info.Code, got.Code)
}

func TestBeginPairing_Idempotent(t *testing.T) {
	n := unclaimedNodeForLifecycle(t)

	first, err := n.BeginPairing(BeginPairingOptions{
		CoordinatorURL: "https://coord:9091",
		CAFingerprint:  validPin,
	})
	require.NoError(t, err)

	// Second call without Regenerate returns the same code.
	second, err := n.BeginPairing(BeginPairingOptions{
		CoordinatorURL: "https://coord:9091",
		CAFingerprint:  validPin,
	})
	require.NoError(t, err)
	assert.Equal(t, first.Code, second.Code)
	assert.True(t, first.Deadline.Equal(second.Deadline))
}

func TestBeginPairing_Regenerate(t *testing.T) {
	n := unclaimedNodeForLifecycle(t)

	first, err := n.BeginPairing(BeginPairingOptions{
		CoordinatorURL: "https://coord:9091",
		CAFingerprint:  validPin,
	})
	require.NoError(t, err)
	n.pairingMu.Lock()
	firstWindow := n.activePairingWindow
	n.pairingMu.Unlock()
	// Regeneration removes this window from the node, so cleanup must retain it.
	require.NotNil(t, firstWindow)
	t.Cleanup(func() {
		firstWindow.timer.Stop()
		firstWindow.cancel()
		<-firstWindow.done
	})

	second, err := n.BeginPairing(BeginPairingOptions{
		CoordinatorURL: "https://coord:9091",
		CAFingerprint:  validPin,
		Regenerate:     true,
	})
	require.NoError(t, err)
	assert.NotEqual(t, first.Code, second.Code, "regenerate must produce a fresh code")
}

func TestBeginPairing_RejectsMissingCoordinatorURL(t *testing.T) {
	n := unclaimedNodeForLifecycle(t)
	_, err := n.BeginPairing(BeginPairingOptions{
		CAFingerprint: validPin,
	})
	assert.ErrorIs(t, err, ErrPairingURLMissing)
}

func TestBeginPairing_RejectsMalformedCAFingerprint(t *testing.T) {
	n := unclaimedNodeForLifecycle(t)
	_, err := n.BeginPairing(BeginPairingOptions{
		CoordinatorURL: "https://coord:9091",
		CAFingerprint:  "not-hex",
	})
	assert.ErrorIs(t, err, ErrPairingCAFingerprintMalformed)
}

func TestBeginPairing_RejectsWrongMode(t *testing.T) {
	// Disabled node — no identity required, no BeginPairing valid.
	n, err := New(Config{Mode: Disabled})
	require.NoError(t, err)

	_, err = n.BeginPairing(BeginPairingOptions{
		CoordinatorURL: "https://coord:9091",
		CAFingerprint:  validPin,
	})
	assert.ErrorIs(t, err, ErrPairingNotUnclaimed)
}

func TestCancelPairing_NoActiveWindow(t *testing.T) {
	n := unclaimedNodeForLifecycle(t)
	err := n.CancelPairing()
	assert.ErrorIs(t, err, ErrPairingNoActiveWindow)
}

func TestCancelPairing_ClearsWindow(t *testing.T) {
	n := unclaimedNodeForLifecycle(t)

	_, err := n.BeginPairing(BeginPairingOptions{
		CoordinatorURL: "https://coord:9091",
		CAFingerprint:  validPin,
	})
	require.NoError(t, err)

	// pairing.txt exists pre-cancel.
	_, err = os.Stat(n.cfg.PairingPath)
	require.NoError(t, err)
	n.pairingMu.Lock()
	window := n.activePairingWindow
	n.pairingMu.Unlock()
	// CancelPairing clears the active pointer before its attempt has exited.
	require.NotNil(t, window)
	t.Cleanup(func() {
		window.timer.Stop()
		window.cancel()
		<-window.done
	})

	require.NoError(t, n.CancelPairing())

	// pairing.txt removed post-cancel.
	_, err = os.Stat(n.cfg.PairingPath)
	assert.True(t, errors.Is(err, os.ErrNotExist), "pairing.txt must be removed on cancel")

	// Snapshot accessor reports no window.
	_, ok := n.PairingWindowInfoSnapshot()
	assert.False(t, ok)
}

func TestPairingWindowInfoSnapshot_NoWindow(t *testing.T) {
	n := unclaimedNodeForLifecycle(t)
	_, ok := n.PairingWindowInfoSnapshot()
	assert.False(t, ok)
}

func TestBeginPairing_AcceptsSHA256Prefix(t *testing.T) {
	// normalizeCAFingerprint handles "sha256:<hex>" — BeginPairing
	// should accept that form as well as bare hex.
	n := unclaimedNodeForLifecycle(t)
	_, err := n.BeginPairing(BeginPairingOptions{
		CoordinatorURL: "https://coord:9091",
		CAFingerprint:  "sha256:" + validPin,
	})
	assert.NoError(t, err)
}

// TestIsValidPairingCode pins the wire-format validator used by both
// the coord-side pairing-request handler and the admin-side accept
// handler. Single source of truth for format; any drift between
// callers would silently admit malformed codes.
func TestIsValidPairingCode(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"ABCDEFGHIJKLMNOP", true},   // 16 uppercase letters
		{"234567234567ABCD", true},   // base32 digits
		{"abcdefghijklmnop", false},  // lowercase
		{"ABCDEFGHIJKLMN", false},    // too short
		{"ABCDEFGHIJKLMNOPQ", false}, // too long
		{"ABCDEFGH01234567", false},  // '0' and '1' not in base32 alphabet
		{"ABCDEFGH!@#$%^&*", false},  // symbols
		{"", false},                  // empty
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, IsValidPairingCode(tc.in), "input %q", tc.in)
	}
}
