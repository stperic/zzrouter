package group

import (
	"context"
	"sync"
	"testing"
	"time"

	route_events "github.com/stperic/zzrouter/pkg/observability/route_events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReaper_ExpiresAndPublishesRouteExpired(t *testing.T) {
	bus := route_events.NewBus()
	ch, _, unsub := bus.Subscribe(func(ev route_events.Event) bool {
		return ev.Type == route_events.EventRouteExpired
	})
	defer unsub()

	s := NewGroupStore()
	s.SetEventBus(bus)
	require.NoError(t, s.Set("ephemeral", ModelGroup{
		Strategy:  StrategyPriority,
		Replicas:  []Replica{{Name: "r1", Model: "m", App: "ollama"}},
		ExpiresAt: time.Now().Add(-time.Second), // already expired
	}))
	require.NoError(t, s.Set("perpetual", ModelGroup{
		Strategy: StrategyPriority,
		Replicas: []Replica{{Name: "r1", Model: "m", App: "ollama"}},
	}))

	s.reapOnce()

	assert.Nil(t, s.Get("ephemeral"), "expired route must be gone")
	assert.NotNil(t, s.Get("perpetual"), "perpetual route stays")

	select {
	case ev := <-ch:
		assert.Equal(t, route_events.EventRouteExpired, ev.Type)
		assert.Equal(t, "ephemeral", ev.Route)
	case <-time.After(200 * time.Millisecond):
		t.Fatal("missing route_expired event")
	}
}

func TestReaper_SkipsPerpetualAndUnexpired(t *testing.T) {
	s := NewGroupStore()
	require.NoError(t, s.Set("perpetual", ModelGroup{
		Strategy: StrategyPriority,
		Replicas: []Replica{{Name: "r1", Model: "m", App: "ollama"}},
	}))
	require.NoError(t, s.Set("future", ModelGroup{
		Strategy:  StrategyPriority,
		Replicas:  []Replica{{Name: "r1", Model: "m", App: "ollama"}},
		ExpiresAt: time.Now().Add(time.Hour),
	}))

	s.reapOnce()

	assert.NotNil(t, s.Get("perpetual"))
	assert.NotNil(t, s.Get("future"))
}

func TestReaper_GoroutineStopsOnContextCancel(t *testing.T) {
	s := NewGroupStore()
	ctx, cancel := context.WithCancel(context.Background())
	done := s.StartReaper(ctx, 10*time.Millisecond)

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reaper did not stop on ctx cancel")
	}
}

func TestReaper_TicksProcessExpiry(t *testing.T) {
	// Drive the reaper goroutine with a tight tick so the test
	// doesn't depend on the 1-minute default. Pins the contract:
	// expired routes vanish after a tick, perpetual stays.
	s := NewGroupStore()
	require.NoError(t, s.Set("ephemeral", ModelGroup{
		Strategy:  StrategyPriority,
		Replicas:  []Replica{{Name: "r1", Model: "m", App: "ollama"}},
		ExpiresAt: time.Now().Add(20 * time.Millisecond),
	}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := s.StartReaper(ctx, 10*time.Millisecond)

	// Wait for the expiry to fire — bounded poll, not a sleep.
	deadline := time.Now().Add(2 * time.Second)
	for s.Get("ephemeral") != nil {
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("ephemeral route not reaped within 2s")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
}

func TestReaper_ConcurrentMutationDoesNotPanic(t *testing.T) {
	// Race-detector run. Pins the "TTL is best-effort; concurrent
	// extension may or may not win" contract — the goal is no
	// crash, not strict ordering.
	s := NewGroupStore()
	require.NoError(t, s.Set("hot", ModelGroup{
		Strategy:  StrategyPriority,
		Replicas:  []Replica{{Name: "r1", Model: "m", App: "ollama"}},
		ExpiresAt: time.Now().Add(-time.Second),
	}))

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				s.reapOnce()
				_ = s.Get("hot")
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = s.Set("hot", ModelGroup{
					Strategy:  StrategyPriority,
					Replicas:  []Replica{{Name: "r1", Model: "m", App: "ollama"}},
					ExpiresAt: time.Now().Add(time.Hour),
				})
			}
		}()
	}
	wg.Wait()
}
