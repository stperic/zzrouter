package genai

import (
	"sync"
	"testing"
)

// TestModelAllowlist_NilCheckerPassthrough pins the default behavior:
// no checker registered → caller-supplied model name passes through
// verbatim. Backwards-compat default — operators opt in to gating.
func TestModelAllowlist_NilCheckerPassthrough(t *testing.T) {
	defer SwapModelAllowlist(nil)()

	if got := GateModelLabel("anything-the-caller-typed"); got != "anything-the-caller-typed" {
		t.Errorf("nil checker = %q, want passthrough", got)
	}
	if got := GateModelLabel(""); got != "" {
		t.Errorf("empty input should stay empty, got %q", got)
	}
}

// TestModelAllowlist_KnownModelPassthrough pins that the checker
// returning true preserves the raw caller-supplied value — operators
// see the exact model dimension, bounded to known values.
func TestModelAllowlist_KnownModelPassthrough(t *testing.T) {
	defer SwapModelAllowlist(func(name string) bool {
		return name == "gpt-4-turbo" || name == "llama3:8b"
	})()

	if got := GateModelLabel("gpt-4-turbo"); got != "gpt-4-turbo" {
		t.Errorf("known model = %q, want passthrough", got)
	}
	if got := GateModelLabel("llama3:8b"); got != "llama3:8b" {
		t.Errorf("known model = %q, want passthrough", got)
	}
}

// TestModelAllowlist_UnknownModelSentinel pins that a checker
// returning false collapses the label to UnknownModelLabel.
func TestModelAllowlist_UnknownModelSentinel(t *testing.T) {
	defer SwapModelAllowlist(func(name string) bool {
		return name == "gpt-4-turbo"
	})()

	if got := GateModelLabel("not-on-the-list"); got != UnknownModelLabel {
		t.Errorf("unknown model = %q, want %q sentinel", got, UnknownModelLabel)
	}
	// Empty input stays empty even with checker — the gate doesn't
	// invent a non-empty sentinel for absent labels.
	if got := GateModelLabel(""); got != "" {
		t.Errorf("empty input should stay empty under checker, got %q", got)
	}
}

// TestModelAllowlist_SwapRestoresPrevious pins the cleanup hazard the
// inline SetModelAllowlist(nil) approach didn't handle: if a peer test
// or production startup hook had set a real allowlist, t.Cleanup
// SetModelAllowlist(nil) would clobber it. SwapModelAllowlist returns
// a restore closure that puts the previous value back.
func TestModelAllowlist_SwapRestoresPrevious(t *testing.T) {
	originalChecker := func(name string) bool { return name == "original-baseline" }
	SetModelAllowlist(originalChecker)
	defer SetModelAllowlist(nil)

	// Inner swap simulates a sub-test or peer that wires its own
	// allowlist and then hands control back.
	restore := SwapModelAllowlist(func(name string) bool { return name == "inner" })
	if got := GateModelLabel("inner"); got != "inner" {
		t.Errorf("inner allowlist = %q, want passthrough", got)
	}
	if got := GateModelLabel("original-baseline"); got != UnknownModelLabel {
		t.Errorf("inner overrode outer; original-baseline = %q, want sentinel", got)
	}

	restore()

	// Outer allowlist is back: inner's exclusive value is now unknown,
	// the outer value passes through.
	if got := GateModelLabel("original-baseline"); got != "original-baseline" {
		t.Errorf("after restore, original-baseline = %q, want passthrough", got)
	}
	if got := GateModelLabel("inner"); got != UnknownModelLabel {
		t.Errorf("after restore, inner = %q, want sentinel", got)
	}
}

// TestModelAllowlist_ConcurrentSwap exercises the atomic-pointer path:
// SetModelAllowlist + GateModelLabel race-clean across goroutines.
// Mirrors the rebind pattern operators would use to refresh the
// allowlist when the model registry changes.
func TestModelAllowlist_ConcurrentSwap(t *testing.T) {
	defer SwapModelAllowlist(nil)()
	const goroutines = 20
	const iterations = 200

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(id int) {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				if id%2 == 0 {
					SetModelAllowlist(func(name string) bool { return name == "ok" })
				} else {
					_ = GateModelLabel("ok")
					_ = GateModelLabel("not-ok")
				}
			}
		}(i)
	}
	wg.Wait()
}
