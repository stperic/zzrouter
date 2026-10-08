package clusternode

import (
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNotifyModeChange_NilCallback_IsNoOp pins the documented contract
// that OnModeChange left unset does not panic. Every code path that
// calls notifyModeChange relies on this — Config.validate does NOT
// enforce a callback.
func TestNotifyModeChange_NilCallback_IsNoOp(t *testing.T) {
	t.Parallel()
	cfg := Config{
		Mode:        Coordinator,
		BindHost:    "127.0.0.1",
		IdentityDir: filepath.Join(t.TempDir(), "id"),
		CADir:       filepath.Join(t.TempDir(), "ca"),
		NodeName:    "test-coord",
	}
	n, err := New(cfg)
	require.NoError(t, err)

	// Should not panic; no callback invoked.
	n.notifyModeChange(Worker, "test")
}

// TestNotifyModeChange_Callback_InvokedWithParams pins the firing
// contract — the callback sees the target mode and reason exactly as
// passed by the transition code path (completePairing,
// revertToUnclaimed).
func TestNotifyModeChange_Callback_InvokedWithParams(t *testing.T) {
	t.Parallel()
	var (
		mu     sync.Mutex
		gotTo  Mode
		gotWhy string
		calls  int
	)
	cfg := Config{
		Mode:        Coordinator,
		BindHost:    "127.0.0.1",
		IdentityDir: filepath.Join(t.TempDir(), "id"),
		CADir:       filepath.Join(t.TempDir(), "ca"),
		NodeName:    "test-coord",
		OnModeChange: func(to Mode, reason string) {
			mu.Lock()
			defer mu.Unlock()
			gotTo = to
			gotWhy = reason
			calls++
		},
	}
	n, err := New(cfg)
	require.NoError(t, err)

	n.notifyModeChange(Worker, "pairing succeeded")

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 1, calls)
	assert.Equal(t, Worker, gotTo)
	assert.Equal(t, "pairing succeeded", gotWhy)
}

// Integration coverage for the two real firing sites (completePairing,
// revertToUnclaimed) is deferred — those paths require a multi-node
// harness tracked as Tier 2 of handoff item #3. The unit tests above
// plus a grep for notifyModeChange call sites are enough coverage for
// the single-node landing.
