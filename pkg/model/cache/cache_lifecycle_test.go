package cache

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCache_Start_InstallsLifecycleCtx verifies the core lifecycle
// invariant: after Start, currentCtx returns the installed ctx (not
// the fallback Background).
func TestCache_Start_InstallsLifecycleCtx(t *testing.T) {
	c := newWorkerCache(t)

	assert.Equal(t, context.Background(), c.currentCtx(), "pre-Start: fallback is Background")

	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(parent)

	got := c.currentCtx()
	assert.NotEqual(t, context.Background(), got, "post-Start: lifecycle ctx installed")

	// Cancelling the parent must propagate to the installed ctx.
	cancel()
	select {
	case <-got.Done():
	case <-time.After(time.Second):
		t.Fatal("lifeCtx did not cancel when parent cancelled")
	}
}

// TestCache_Start_Idempotent verifies that a second Start with a
// different parent does not replace the already-installed ctx —
// guarding against accidental re-init during restart cycles.
func TestCache_Start_Idempotent(t *testing.T) {
	c := newWorkerCache(t)

	parent1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	c.Start(parent1)
	first := c.currentCtx()

	parent2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	c.Start(parent2)
	second := c.currentCtx()

	assert.Equal(t, first, second, "second Start must not replace the installed ctx")

	// Cancelling parent2 must NOT cancel the installed ctx — it's
	// derived from parent1.
	cancel2()
	select {
	case <-first.Done():
		t.Fatal("installed ctx cancelled from parent2; expected it to track parent1 only")
	case <-time.After(50 * time.Millisecond):
	}
}

// TestCache_Stop_WithoutStart_NoOp asserts Stop before Start is a safe
// no-op — required because Server.Stop runs unconditionally during
// graceful shutdown regardless of whether Start succeeded.
func TestCache_Stop_WithoutStart_NoOp(t *testing.T) {
	c := newWorkerCache(t)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	// Must return promptly (no hang waiting for a refresh that never ran).
	done := make(chan struct{})
	go func() {
		c.Stop(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Stop-without-Start hung")
	}
}

// TestCache_Stop_Twice_NoOp asserts Stop is safe to call repeatedly.
// Second Stop sees cancel==nil and returns immediately.
func TestCache_Stop_Twice_NoOp(t *testing.T) {
	c := newWorkerCache(t)
	c.Start(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c.Stop(ctx)
	c.Stop(ctx) // must not hang, panic, or error
}

// TestCache_Stop_CancelsLifecycleCtx — the captured lifeCtx from Start
// must be cancelled by Stop so in-flight consumers (fetchWorkerData,
// NotifyMasterCacheRefresh retry loop) unwind cleanly.
func TestCache_Stop_CancelsLifecycleCtx(t *testing.T) {
	c := newWorkerCache(t)
	c.Start(context.Background())

	// Snapshot the lifeCtx before Stop nils it.
	lifeCtx := c.currentCtx()
	require.NotEqual(t, context.Background(), lifeCtx, "pre-Stop: lifeCtx installed")

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c.Stop(ctx)

	select {
	case <-lifeCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("Stop did not cancel the lifecycle ctx")
	}

	// Post-Stop, currentCtx falls back to Background.
	assert.Equal(t, context.Background(), c.currentCtx(), "post-Stop: fallback to Background")
}

// TestCache_RestartCycle — the contract mTLS PR 4 depends on:
// Start → Stop → Start cycles cleanly. Each Start installs a fresh
// lifeCtx; the old one stays cancelled after the new Start lands.
func TestCache_RestartCycle(t *testing.T) {
	c := newWorkerCache(t)
	stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	// Cycle 1
	c.Start(context.Background())
	first := c.currentCtx()
	c.Stop(stopCtx)

	// Cycle 2
	c.Start(context.Background())
	second := c.currentCtx()
	defer c.Stop(stopCtx)

	assert.NotSame(t, first, second, "each Start must install a fresh ctx")

	// first must remain cancelled after the restart.
	select {
	case <-first.Done():
	default:
		t.Fatal("restart revived the old lifecycle ctx")
	}

	// second must be alive.
	select {
	case <-second.Done():
		t.Fatal("fresh lifecycle ctx already cancelled")
	default:
	}
}
