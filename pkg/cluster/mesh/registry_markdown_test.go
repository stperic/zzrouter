package mesh

import (
	"errors"
	"sync"
	"testing"
)

// TestRegistry_MarkDown_UnknownURL asserts MarkDown on an unregistered
// URL is a no-op — no panic, no state side effects, zero Transition.
// Locks the goodbye-handler contract: a goodbye for an unknown peer is
// idempotent.
func TestRegistry_MarkDown_UnknownURL(t *testing.T) {
	t.Parallel()
	reg := NewEndpointRegistry()
	tr := reg.MarkDown("http://ghost:9090", errors.New("goodbye"))
	if !tr.IsZero() {
		t.Errorf("MarkDown(unknown) = %s->%s, want zero Transition", tr.From, tr.To)
	}
	if reg.Count() != 0 {
		t.Errorf("Count = %d after MarkDown(unknown), want 0", reg.Count())
	}
}

// TestRegistry_MarkDown_TransitionsToDown asserts a known endpoint
// goes to StatusDown with the reason surfaced in LastError.
func TestRegistry_MarkDown_TransitionsToDown(t *testing.T) {
	t.Parallel()
	reg := NewEndpointRegistry()
	url := "http://w1:9090"
	if err := reg.RegisterEndpoint(&Endpoint{URL: url, Status: StatusUp}); err != nil {
		t.Fatal(err)
	}
	// Seed liveness to UP so the transition path exercises UP→DOWN.
	reg.Observe(url, ProbeResult{OK: true, Quality: QualityFull, Snapshot: &Connection{}})

	reason := errors.New("peer goodbye")
	tr := reg.MarkDown(url, reason)
	if tr.IsZero() {
		t.Fatalf("MarkDown(known) returned zero Transition")
	}
	if tr.To != StatusDown {
		t.Errorf("Transition.To = %s, want StatusDown", tr.To)
	}
	ep, err := reg.GetEndpointByURL(url)
	if err != nil {
		t.Fatal(err)
	}
	if ep.Status != StatusDown {
		t.Errorf("endpoint Status = %s, want StatusDown", ep.Status)
	}
	if ep.Snapshot.LastError != reason.Error() {
		t.Errorf("Snapshot.LastError = %q, want %q", ep.Snapshot.LastError, reason.Error())
	}
}

// TestRegistry_MarkDown_ConcurrentWithObserve is -race coverage for the
// MarkDown/Observe lock pairing. No assertions beyond the race detector.
func TestRegistry_MarkDown_ConcurrentWithObserve(t *testing.T) {
	t.Parallel()
	reg := NewEndpointRegistry()
	url := "http://w1:9090"
	if err := reg.RegisterEndpoint(&Endpoint{URL: url}); err != nil {
		t.Fatal(err)
	}

	const iters = 200
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range iters {
			reg.Observe(url, ProbeResult{OK: true, Quality: QualityFull, Snapshot: &Connection{}})
		}
	}()
	go func() {
		defer wg.Done()
		for range iters {
			reg.MarkDown(url, errors.New("bye"))
		}
	}()
	wg.Wait()
}
