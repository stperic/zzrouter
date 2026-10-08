package tui

import "testing"

func TestViewContextZeroValueIsSafe(t *testing.T) {
	var v ViewContext
	// Cancel on zero value is a no-op and must not panic.
	v.Cancel()
	// Context() on zero value lazy-inits.
	ctx := v.Context()
	if ctx == nil {
		t.Fatal("Context() returned nil")
	}
	if ctx.Err() != nil {
		t.Fatalf("fresh context should not be errored, got %v", ctx.Err())
	}
}

func TestViewContextCancelStopsContext(t *testing.T) {
	var v ViewContext
	ctx := v.Context()
	v.Cancel()
	if ctx.Err() == nil {
		t.Fatal("context should be cancelled after Cancel()")
	}
}

func TestViewContextCancelIdempotent(t *testing.T) {
	v := NewViewContext()
	v.Cancel()
	v.Cancel() // must not panic
}

func TestViewContextContextStableAcrossCalls(t *testing.T) {
	var v ViewContext
	c1 := v.Context()
	c2 := v.Context()
	if c1 != c2 {
		t.Fatal("Context() should return the same context on repeated calls")
	}
}
