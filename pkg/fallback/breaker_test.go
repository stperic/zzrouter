package fallback

import (
	"errors"
	"testing"
	"time"
)

// These tests originated in pkg/connectivity/connectivity_test.go when the
// circuit breaker lived there. They moved here with the implementation so
// they retain access to unexported fields for white-box assertions.

func TestNewBreakerManager(t *testing.T) {
	m := NewBreakerManager()

	if m == nil {
		t.Fatal("NewBreakerManager() returned nil")
	}
	if m.breakers == nil {
		t.Error("breakers map should not be nil")
	}
}

func TestBreakerManager_GetBreaker_New(t *testing.T) {
	m := NewBreakerManager()
	b := m.GetBreaker("http://localhost:11434")

	if b == nil {
		t.Fatal("GetBreaker() returned nil")
	}
	if b.key != "http://localhost:11434" {
		t.Errorf("key = %q, want %q", b.key, "http://localhost:11434")
	}
	if b.state != BreakerClosed {
		t.Errorf("Initial state = %v, want BreakerClosed", b.state)
	}
	if b.cfg.MaxFailures != 3 {
		t.Errorf("MaxFailures = %d, want 3", b.cfg.MaxFailures)
	}
	if b.cfg.SuccessThreshold != 2 {
		t.Errorf("SuccessThreshold = %d, want 2", b.cfg.SuccessThreshold)
	}
}

func TestBreakerManager_GetBreaker_Existing(t *testing.T) {
	m := NewBreakerManager()
	url := "http://localhost:11434"

	b1 := m.GetBreaker(url)
	b2 := m.GetBreaker(url)

	if b1 != b2 {
		t.Error("GetBreaker() should return same instance for same URL")
	}
}

func TestBreakerManager_GetBreaker_Concurrent(t *testing.T) {
	m := NewBreakerManager()
	url := "http://localhost:11434"

	done := make(chan *Breaker, 10)
	for range 10 {
		go func() {
			done <- m.GetBreaker(url)
		}()
	}

	var breakers []*Breaker
	for range 10 {
		breakers = append(breakers, <-done)
	}
	for i := 1; i < len(breakers); i++ {
		if breakers[i] != breakers[0] {
			t.Error("Concurrent GetBreaker() should return same instance")
		}
	}
}

func TestBreaker_Execute_Success(t *testing.T) {
	cb := NewBreakerManager().GetBreaker("http://test:1234")

	err := cb.Execute(func() error { return nil })

	if err != nil {
		t.Errorf("Execute() error = %v, want nil", err)
	}
	if cb.State() != BreakerClosed {
		t.Errorf("State = %v, want BreakerClosed", cb.State())
	}
	if cb.failureCount != 0 {
		t.Errorf("failureCount = %d, want 0", cb.failureCount)
	}
}

func TestBreaker_Execute_Failure(t *testing.T) {
	cb := NewBreakerManager().GetBreaker("http://test:1234")

	err := cb.Execute(func() error { return errors.New("test error") })

	if err == nil {
		t.Error("Execute() should return error")
	}
	if cb.failureCount != 1 {
		t.Errorf("failureCount = %d, want 1", cb.failureCount)
	}
}

func TestBreaker_StateTransition_ClosedToOpen(t *testing.T) {
	cb := NewBreakerManager().GetBreaker("http://test:1234")

	for range 3 {
		_ = cb.Execute(func() error { return errors.New("failure") })
	}

	if cb.State() != BreakerOpen {
		t.Errorf("State = %v, want BreakerOpen after %d failures", cb.State(), cb.cfg.MaxFailures)
	}
}

func TestBreaker_Execute_WhenOpen(t *testing.T) {
	cb := NewBreakerManager().GetBreaker("http://test:1234")
	cb.cfg.Timeout = 1 * time.Hour // Long timeout so it stays open

	for range 3 {
		_ = cb.Execute(func() error { return errors.New("failure") })
	}

	callCount := 0
	err := cb.Execute(func() error {
		callCount++
		return nil
	})

	if err == nil {
		t.Error("Execute() should return error when breaker is open")
	}
	if callCount != 0 {
		t.Errorf("Function should not be called when breaker is open, callCount = %d", callCount)
	}
}

func TestBreaker_StateTransition_OpenToHalfOpen(t *testing.T) {
	cb := NewBreakerManager().GetBreaker("http://test:1234")
	cb.cfg.Timeout = 1 * time.Millisecond

	for range 3 {
		_ = cb.Execute(func() error { return errors.New("failure") })
	}
	if cb.State() != BreakerOpen {
		t.Fatal("Breaker should be open")
	}

	time.Sleep(10 * time.Millisecond)

	_ = cb.Execute(func() error { return nil })

	state := cb.State()
	if state != BreakerHalfOpen && state != BreakerClosed {
		t.Errorf("State = %v, want BreakerHalfOpen or BreakerClosed", state)
	}
}

func TestBreaker_StateTransition_HalfOpenToClosed(t *testing.T) {
	cb := NewBreakerManager().GetBreaker("http://test:1234")
	cb.cfg.Timeout = 1 * time.Millisecond
	cb.cfg.SuccessThreshold = 2

	for range 3 {
		_ = cb.Execute(func() error { return errors.New("failure") })
	}

	time.Sleep(10 * time.Millisecond)

	_ = cb.Execute(func() error { return nil })
	_ = cb.Execute(func() error { return nil })

	if cb.State() != BreakerClosed {
		t.Errorf("State = %v, want BreakerClosed after %d successes", cb.State(), cb.cfg.SuccessThreshold)
	}
}

func TestBreaker_StateTransition_HalfOpenToOpen(t *testing.T) {
	cb := NewBreakerManager().GetBreaker("http://test:1234")
	cb.cfg.Timeout = 1 * time.Millisecond
	cb.cfg.SuccessThreshold = 2

	for range 3 {
		_ = cb.Execute(func() error { return errors.New("failure") })
	}

	time.Sleep(10 * time.Millisecond)

	_ = cb.Execute(func() error { return nil })
	_ = cb.Execute(func() error { return errors.New("failure in half-open") })

	if cb.State() != BreakerOpen {
		t.Errorf("State = %v, want BreakerOpen after failure in half-open", cb.State())
	}
}

func TestBreaker_GetState(t *testing.T) {
	cb := NewBreakerManager().GetBreaker("http://test:1234")

	if cb.GetState() != BreakerClosed {
		t.Errorf("GetState() = %v, want BreakerClosed", cb.GetState())
	}
}

func TestBreaker_IsOpen(t *testing.T) {
	cb := NewBreakerManager().GetBreaker("http://test:1234")
	cb.cfg.Timeout = 1 * time.Hour

	if cb.IsOpen() {
		t.Error("IsOpen() should be false initially")
	}

	for range 3 {
		_ = cb.Execute(func() error { return errors.New("failure") })
	}

	if !cb.IsOpen() {
		t.Error("IsOpen() should be true after failures")
	}
}

// TestBreaker_PanicReleasesProbeSlot is the regression guard for the latent
// bug the senior review caught: prior implementations leaked the half-open
// probe slot when fn() panicked, wedging the breaker permanently.
func TestBreaker_PanicReleasesProbeSlot(t *testing.T) {
	cb := NewBreakerManager().GetBreaker("http://test:1234")
	cb.cfg.Timeout = 1 * time.Millisecond

	// Open the breaker.
	for range 3 {
		_ = cb.Execute(func() error { return errors.New("failure") })
	}
	time.Sleep(5 * time.Millisecond)

	// Probe panics — defer in ExecuteAny must still release the slot.
	func() {
		defer func() { _ = recover() }()
		_ = cb.Execute(func() error { panic("boom") })
	}()

	// Slot must be free; another probe must be admittable. If the slot
	// leaked, this Execute would be rejected with "circuit breaker open".
	called := false
	_ = cb.Execute(func() error {
		called = true
		return nil
	})
	if !called {
		t.Fatal("probe after panic was rejected — half-open slot leaked")
	}
}

func TestBreaker_SuccessResetsFailureCount(t *testing.T) {
	cb := NewBreakerManager().GetBreaker("http://test:1234")

	_ = cb.Execute(func() error { return errors.New("fail1") })
	_ = cb.Execute(func() error { return errors.New("fail2") })

	if cb.failureCount != 2 {
		t.Fatalf("failureCount should be 2, got %d", cb.failureCount)
	}

	_ = cb.Execute(func() error { return nil })

	if cb.failureCount != 0 {
		t.Errorf("failureCount = %d, want 0 after success", cb.failureCount)
	}
}
