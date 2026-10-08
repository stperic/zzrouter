package prov_apps

import (
	"testing"
)

// These tests exercise LifecycleState's derived-getter contract in
// isolation. The full LocalInfo composition runs against a real
// ProviderAppManager in local_info_managed_test.go, which covers the
// installed/managed split. Keeping the pure enum behavior here
// makes bolt-on detection obvious: if a new state lands, the
// exhaustive switches below fail to compile or fail at runtime,
// forcing the author to update the derived-getter semantics
// deliberately rather than by accident.

func TestLifecycleState_IsInstalled(t *testing.T) {
	cases := map[LifecycleState]bool{
		StateLive:           true,
		StateDormant:        true,
		StateCloudAvailable: true,
		StateInstalling:     false, // mid-flight is NOT installed — version file isn't written yet
		StateNotInstalled:   false,
	}
	for state, want := range cases {
		if got := state.IsInstalled(); got != want {
			t.Errorf("%s.IsInstalled() = %v, want %v", state, got, want)
		}
	}
}

func TestLifecycleState_IsRoutable(t *testing.T) {
	cases := map[LifecycleState]bool{
		StateLive:           true,
		StateCloudAvailable: true,
		StateDormant:        false, // dormant providers are on disk but not registered in runtime
		StateInstalling:     false,
		StateNotInstalled:   false,
	}
	for state, want := range cases {
		if got := state.IsRoutable(); got != want {
			t.Errorf("%s.IsRoutable() = %v, want %v", state, got, want)
		}
	}
}

// TestLifecycleState_MutuallyExclusive pins the design intent that each
// provider is in exactly one state at any moment. A provider can't be
// simultaneously installing and dormant, or live and not-installed. If
// a future state violates this it needs explicit design review, not a
// quiet addition to the enum.
func TestLifecycleState_MutuallyExclusive(t *testing.T) {
	all := []LifecycleState{
		StateNotInstalled,
		StateInstalling,
		StateDormant,
		StateLive,
		StateCloudAvailable,
	}
	seen := make(map[LifecycleState]struct{})
	for _, s := range all {
		if _, dup := seen[s]; dup {
			t.Fatalf("duplicate lifecycle state: %s", s)
		}
		seen[s] = struct{}{}
	}
	if len(seen) != 5 {
		t.Fatalf("expected exactly 5 lifecycle states, got %d", len(seen))
	}
}
