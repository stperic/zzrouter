package role

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewManager_InvalidInitialRoleFails(t *testing.T) {
	t.Parallel()
	_, err := NewManager(Role("bogus"))
	require.Error(t, err)
}

func TestNewManager_ValidInitialRole(t *testing.T) {
	t.Parallel()
	for _, r := range []Role{RoleDisabled, RoleCoordinator, RoleUnclaimed, RoleWorker} {
		m, err := NewManager(r)
		require.NoError(t, err, "role %s", r)
		assert.Equal(t, r, m.Current())
	}
}

func TestManager_SetToSameRole_IsNoOp(t *testing.T) {
	t.Parallel()
	m, _ := NewManager(RoleCoordinator)

	var calls int32
	m.Subscribe(func(Transition) { atomic.AddInt32(&calls, 1) })

	require.NoError(t, m.Set(context.Background(), RoleCoordinator, "idempotent"))
	assert.Equal(t, int32(0), atomic.LoadInt32(&calls),
		"subscriber must not fire when role is unchanged")
}

func TestManager_SetDifferentRole_FansOut(t *testing.T) {
	t.Parallel()
	m, _ := NewManager(RoleUnclaimed)

	var got Transition
	m.Subscribe(func(tr Transition) { got = tr })

	require.NoError(t, m.Set(context.Background(), RoleWorker, "pairing succeeded"))
	assert.Equal(t, RoleUnclaimed, got.From)
	assert.Equal(t, RoleWorker, got.To)
	assert.Equal(t, "pairing succeeded", got.Reason)
	assert.False(t, got.At.IsZero())
}

func TestManager_InvalidTargetRole_Errors(t *testing.T) {
	t.Parallel()
	m, _ := NewManager(RoleDisabled)
	err := m.Set(context.Background(), Role("bogus"), "reason")
	require.Error(t, err)
	assert.Equal(t, RoleDisabled, m.Current(), "state must not mutate on invalid target")
}

func TestManager_CanceledContext_Errors(t *testing.T) {
	t.Parallel()
	m, _ := NewManager(RoleDisabled)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := m.Set(ctx, RoleCoordinator, "reason")
	require.Error(t, err)
	assert.Equal(t, RoleDisabled, m.Current())
}

func TestManager_Subscribers_FireInOrder(t *testing.T) {
	t.Parallel()
	m, _ := NewManager(RoleDisabled)

	var order []int
	var mu sync.Mutex
	for i := 0; i < 3; i++ {
		m.Subscribe(func(Transition) {
			mu.Lock()
			order = append(order, i)
			mu.Unlock()
		})
	}

	require.NoError(t, m.Set(context.Background(), RoleCoordinator, ""))
	assert.Equal(t, []int{0, 1, 2}, order)
}

func TestManager_Unsubscribe_Removes(t *testing.T) {
	t.Parallel()
	m, _ := NewManager(RoleDisabled)

	var calls int32
	unsubscribe := m.Subscribe(func(Transition) { atomic.AddInt32(&calls, 1) })

	require.NoError(t, m.Set(context.Background(), RoleCoordinator, ""))
	require.Equal(t, int32(1), atomic.LoadInt32(&calls))

	unsubscribe()
	unsubscribe() // must be safe on second call

	require.NoError(t, m.Set(context.Background(), RoleUnclaimed, ""))
	assert.Equal(t, int32(1), atomic.LoadInt32(&calls),
		"unsubscribed callback must not fire")
}

func TestManager_PanickingSubscriber_DoesNotBreakChain(t *testing.T) {
	t.Parallel()
	m, _ := NewManager(RoleDisabled)

	var ranAfter bool
	m.Subscribe(func(Transition) { panic("boom") })
	m.Subscribe(func(Transition) { ranAfter = true })

	require.NoError(t, m.Set(context.Background(), RoleCoordinator, ""))
	assert.True(t, ranAfter, "subsequent subscriber must run despite earlier panic")
}

func TestManager_ConcurrentSet_Serializes(t *testing.T) {
	t.Parallel()
	m, _ := NewManager(RoleDisabled)

	var observed []Transition
	var mu sync.Mutex
	m.Subscribe(func(tr Transition) {
		mu.Lock()
		observed = append(observed, tr)
		mu.Unlock()
	})

	roles := []Role{RoleCoordinator, RoleUnclaimed, RoleWorker, RoleDisabled, RoleCoordinator}
	var wg sync.WaitGroup
	for _, r := range roles {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = m.Set(context.Background(), r, "concurrent")
		}()
	}
	wg.Wait()

	// Every recorded transition's From must equal the previous To.
	// This proves the subscriber sees a coherent chain, not a
	// transposed pair from a racy read.
	mu.Lock()
	defer mu.Unlock()
	for i := 1; i < len(observed); i++ {
		assert.Equal(t, observed[i-1].To, observed[i].From,
			"transition chain broken at index %d: %+v → %+v", i, observed[i-1], observed[i])
	}
}

func TestManager_StartStop_NoOp(t *testing.T) {
	t.Parallel()
	m, _ := NewManager(RoleDisabled)
	ctx := context.Background()
	require.NoError(t, m.Start(ctx))
	require.NoError(t, m.Stop(ctx))
	// Second Stop must be safe too.
	require.NoError(t, m.Stop(ctx))
}

func TestManager_NilReceiver_IsSafe(t *testing.T) {
	t.Parallel()
	var m *Manager
	assert.Equal(t, RoleDisabled, m.Current())
	assert.Error(t, m.Set(context.Background(), RoleCoordinator, ""))
	// Subscribe on nil returns a no-op unsubscribe without panicking.
	unsub := m.Subscribe(func(Transition) {})
	unsub()
}

func TestManager_InjectedClock_UsedForTransitionTime(t *testing.T) {
	t.Parallel()
	m, _ := NewManager(RoleDisabled)
	fixed := time.Date(2026, 4, 17, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return fixed }

	var at time.Time
	m.Subscribe(func(tr Transition) { at = tr.At })
	require.NoError(t, m.Set(context.Background(), RoleCoordinator, ""))
	assert.Equal(t, fixed, at)
}

func TestManager_CurrentAvailableWhileSubscriberDrains(t *testing.T) {
	m, err := NewManager(RoleCoordinator)
	require.NoError(t, err)
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	m.Subscribe(func(tr Transition) {
		close(entered)
		roleRead := make(chan Role, 1)
		go func() { roleRead <- m.Current() }()
		select {
		case got := <-roleRead:
			assert.Equal(t, tr.To, got)
		case <-time.After(time.Second):
			t.Error("authorization cannot read role while teardown waits for it")
		}
		<-release
	})
	go func() { defer close(done); _ = m.Set(t.Context(), RoleWorker, "demote") }()
	<-entered
	read := make(chan bool, 1)
	go func() { read <- m.Transitioning() }()
	select {
	case active := <-read:
		require.True(t, active)
	case <-time.After(time.Second):
		t.Fatal("readiness blocked behind subsystem teardown")
	}
	releaseOnce.Do(func() { close(release) })
	<-done
	require.False(t, m.Transitioning())
	require.Equal(t, RoleWorker, m.Current())
}

func TestManager_TransitionWaitsForSubscribersAndRechecksCancellation(t *testing.T) {
	m, err := NewManager(RoleWorker)
	require.NoError(t, err)
	entered, release, first := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	m.Subscribe(func(Transition) { close(entered); <-release })
	go func() { defer close(first); _ = m.Set(t.Context(), RoleCoordinator, "promote") }()
	<-entered
	select {
	case <-first:
		t.Fatal("Set returned before subscribers finished")
	default:
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	second := make(chan error, 1)
	go func() { second <- m.Set(ctx, RoleDisabled, "cancelled transition") }()
	require.ErrorIs(t, <-second, context.Canceled)
	releaseOnce.Do(func() { close(release) })
	<-first
	require.Equal(t, RoleCoordinator, m.Current())
}

func TestManager_SubscriberCanUnsubscribeAndReadCurrent(t *testing.T) {
	m, err := NewManager(RoleWorker)
	require.NoError(t, err)
	var unsubscribe func()
	calls := 0
	unsubscribe = m.Subscribe(func(tr Transition) { require.Equal(t, tr.To, m.Current()); calls++; unsubscribe() })
	require.NoError(t, m.Set(t.Context(), RoleCoordinator, "promote"))
	require.NoError(t, m.Set(t.Context(), RoleWorker, "demote"))
	require.Equal(t, 1, calls)
}
