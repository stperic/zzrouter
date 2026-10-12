package prov_apps

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type deletionOwner interface {
	WithModelDeletion(context.Context, func(context.Context) error) error
}

func deletionOwnership(t *testing.T, m *ProviderAppManager) deletionOwner {
	t.Helper()
	owner, ok := any(m).(deletionOwner)
	require.True(t, ok, "physical deletion must participate in run-core ownership")
	return owner
}

func TestModelDeletionOwnershipContract(t *testing.T) {
	t.Run("writer precedes cold start", func(t *testing.T) {
		var present atomic.Bool
		present.Store(true)
		r := newContractRig(t, false, WithLocalCatalog(func(context.Context, string) (bool, error) { return present.Load(), nil }))
		owner := deletionOwnership(t, r.m)
		barrier := newBarrier()
		done := make(chan error, 1)
		go func() {
			done <- owner.WithModelDeletion(t.Context(), func(context.Context) error { barrier.hold(); present.Store(false); return nil })
		}()
		barrier.reached(t)
		out := r.ensure(t.Context(), EnsureRequest{Provider: "generic", Model: "gone"})
		waitParkedOn(t, ").decideAndExecuteStart(", 1, contractWait)
		require.Empty(t, r.m.instances.List())
		barrier.open()
		require.NoError(t, <-done)
		result := collect(t, []<-chan outcome{out})[0]
		require.ErrorIs(t, result.err, ErrModelNotLocal)
		require.Empty(t, r.m.instances.List())
	})
	t.Run("start registration precedes writer", func(t *testing.T) {
		barrier := newBarrier()
		r := newContractRig(t, false, WithModelSource(func(context.Context, string) string { barrier.hold(); return "" }))
		owner := deletionOwnership(t, r.m)
		out := r.ensure(t.Context(), EnsureRequest{Provider: "generic", Model: "held"})
		barrier.reached(t)
		done := make(chan error, 1)
		go func() {
			done <- owner.WithModelDeletion(t.Context(), func(context.Context) error {
				if len(r.m.instances.List()) == 0 {
					return errors.New("writer crossed unregistered physical use")
				}
				return nil
			})
		}()
		waitParkedOn(t, ").WithModelDeletion(", 1, contractWait)
		barrier.open()
		result := collect(t, []<-chan outcome{out})[0]
		require.NoError(t, result.err)
		require.NoError(t, <-done)
	})
	t.Run("waiting writer cancellation releases admission", func(t *testing.T) {
		barrier := newBarrier()
		r := newContractRig(t, false, WithModelSource(func(context.Context, string) string { barrier.hold(); return "" }))
		owner := deletionOwnership(t, r.m)
		out := r.ensure(t.Context(), EnsureRequest{Provider: "generic", Model: "held"})
		barrier.reached(t)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var called atomic.Bool
		done := make(chan error, 1)
		go func() {
			done <- owner.WithModelDeletion(ctx, func(context.Context) error { called.Store(true); return nil })
		}()
		waitParkedOn(t, ").WithModelDeletion(", 1, contractWait)
		cancel()
		require.ErrorIs(t, <-done, context.Canceled)
		require.False(t, called.Load())
		barrier.open()
		require.NoError(t, collect(t, []<-chan outcome{out})[0].err)
		require.NoError(t, owner.WithModelDeletion(t.Context(), func(context.Context) error { return nil }))
	})
	t.Run("Stop cancels and drains active writer", func(t *testing.T) {
		r := newContractRig(t, false)
		owner := deletionOwnership(t, r.m)
		entered := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- owner.WithModelDeletion(t.Context(), func(ctx context.Context) error { close(entered); <-ctx.Done(); return ctx.Err() })
		}()
		<-entered
		ctx, cancel := context.WithTimeout(t.Context(), contractWait)
		defer cancel()
		require.NoError(t, r.m.Stop(ctx))
		require.ErrorIs(t, <-done, context.Canceled)
		require.ErrorIs(t, owner.WithModelDeletion(t.Context(), func(context.Context) error { t.Error("writer admitted after Stop"); return nil }), ErrShutdown)
	})
	t.Run("observation cannot cross writer", func(t *testing.T) {
		r := newServedRig(t)
		owner := deletionOwnership(t, r.m)
		barrier := newBarrier()
		r.daemon.set(func(d *fakeDaemon) { d.psHook = barrier.hold })
		done := make(chan struct{})
		go func() { r.m.reconcileServed(t.Context(), "ollama"); close(done) }()
		barrier.reached(t)
		written := make(chan error, 1)
		go func() { written <- owner.WithModelDeletion(t.Context(), func(context.Context) error { return nil }) }()
		waitParkedOn(t, ").WithModelDeletion(", 1, contractWait)
		barrier.open()
		select {
		case <-done:
		case <-time.After(contractWait):
			t.Fatal("observation did not release")
		}
		require.NoError(t, <-written)
	})
}

func TestModelDeletionPermitCapacityContract(t *testing.T) {
	r := newContractRig(t, false)
	require.NoError(t, r.m.WithModelDeletion(t.Context(), func(context.Context) error {
		reader := r.m.catalog.TryAcquire(1)
		if reader {
			r.m.catalog.Release(1)
		}
		require.False(t, reader, "exclusive deletion must occupy every reader slot")
		return nil
	}))
	require.True(t, r.m.catalog.TryAcquire(1), "completion must release admission")
	r.m.catalog.Release(1)
}
