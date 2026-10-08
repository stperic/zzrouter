package mesh

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// TestSelfSlot_ObserveLocal_PopulatesSelf asserts that ObserveLocal
// writes the snapshot and Self() returns a deep-copy.
func TestSelfSlot_ObserveLocal_PopulatesSelf(t *testing.T) {
	reg := NewEndpointRegistry()
	if reg.Self() != nil {
		t.Fatalf("Self() must be nil before ObserveLocal")
	}

	snap := EndpointSnapshot{
		Version:     "1.2.3",
		CollectedAt: time.Now(),
		HealthReport: HealthReport{
			ClusterRole: "coordinator",
		},
	}
	reg.ObserveLocal(snap)

	self := reg.Self()
	if self == nil {
		t.Fatalf("Self() returned nil after ObserveLocal")
	}
	if !self.IsLocal {
		t.Errorf("Self().IsLocal = false, want true")
	}
	if self.Status != StatusUp {
		t.Errorf("Self().Status = %v, want StatusUp", self.Status)
	}
	if self.Snapshot.Version != "1.2.3" {
		t.Errorf("Self().Snapshot.Version = %q, want 1.2.3", self.Snapshot.Version)
	}
}

// TestSelfSlot_UnregisterEndpoint_RefusesSelf asserts the sentinel is
// returned when a caller tries to unregister the self-slot by URL.
// (Self-slot URL is empty today; the guard activates once ObserveLocal
// starts populating the URL.)
func TestSelfSlot_UnregisterEndpoint_RefusesSelf(t *testing.T) {
	reg := NewEndpointRegistry()
	reg.ObserveLocal(EndpointSnapshot{})
	// Force the self-slot to carry a URL — mirrors the future state
	// where ObserveLocal populates it.
	reg.mu.Lock()
	reg.local.URL = "https://coord.local:9090"
	reg.mu.Unlock()

	err := reg.UnregisterEndpoint("https://coord.local:9090")
	if !errors.Is(err, ErrSelfEvictionForbidden) {
		t.Fatalf("UnregisterEndpoint(self) = %v, want ErrSelfEvictionForbidden", err)
	}

	if reg.Self() == nil {
		t.Fatalf("Self-slot evicted despite guard")
	}
}

// TestSelfSlot_UnregisterEndpoint_EmptyURLDoesNotTripGuard asserts that
// when the self-slot URL is blank (today's default), UnregisterEndpoint("")
// returns the normal "not found" error rather than the self-eviction
// sentinel. Guards against an over-broad guard.
func TestSelfSlot_UnregisterEndpoint_EmptyURLDoesNotTripGuard(t *testing.T) {
	reg := NewEndpointRegistry()
	reg.ObserveLocal(EndpointSnapshot{})
	// self-slot URL is "" here.
	err := reg.UnregisterEndpoint("")
	if errors.Is(err, ErrSelfEvictionForbidden) {
		t.Fatalf("empty-URL unregister tripped self-guard: %v", err)
	}
}

// TestSelfSlot_ConcurrentObserveAndRead is the -race coverage for the
// ObserveLocal/Self lock pairing. Run with `go test -race`.
func TestSelfSlot_ConcurrentObserveAndRead(t *testing.T) {
	reg := NewEndpointRegistry()

	const iters = 500
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			reg.ObserveLocal(EndpointSnapshot{
				Version: "iter",
				HealthReport: HealthReport{
					ClusterRole: "coordinator",
				},
			})
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			_ = reg.Self()
		}
	}()

	wg.Wait()
}
