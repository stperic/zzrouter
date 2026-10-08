package routeevents

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBus_PublishFansOutToAllSubscribers(t *testing.T) {
	b := NewBus()
	ch1, _, unsub1 := b.Subscribe(nil)
	defer unsub1()
	ch2, _, unsub2 := b.Subscribe(nil)
	defer unsub2()

	b.Publish(Event{Type: EventRouteCreated, Route: "fast-chat"})

	for _, ch := range []<-chan Event{ch1, ch2} {
		select {
		case ev := <-ch:
			assert.Equal(t, EventRouteCreated, ev.Type)
			assert.Equal(t, "fast-chat", ev.Route)
			assert.False(t, ev.Timestamp.IsZero(), "publish must stamp timestamp")
		case <-time.After(100 * time.Millisecond):
			t.Fatal("subscriber didn't receive event")
		}
	}
}

func TestBus_FilterRunsBeforeSend(t *testing.T) {
	b := NewBus()
	onlyBreaker := func(ev Event) bool {
		return ev.Type == EventBreakerOpened || ev.Type == EventBreakerClosed
	}
	ch, _, unsub := b.Subscribe(onlyBreaker)
	defer unsub()

	b.Publish(Event{Type: EventRouteCreated, Route: "fast-chat"})
	b.Publish(Event{Type: EventBreakerOpened, Replica: "r1"})

	select {
	case ev := <-ch:
		assert.Equal(t, EventBreakerOpened, ev.Type,
			"filter must drop non-matching events; first delivered must be the breaker one")
	case <-time.After(100 * time.Millisecond):
		t.Fatal("filtered subscriber didn't receive matching event")
	}
}

func TestBus_DropOldestOnSlowSubscriber(t *testing.T) {
	b := NewBus()
	_, dropped, unsub := b.Subscribe(nil)
	defer unsub()

	// Buffer is 64; publish 200 events without draining.
	for i := 0; i < 200; i++ {
		b.Publish(Event{Type: EventCooldownStarted, Replica: "r"})
	}
	// 200 published, 64 buffered → 136 dropped.
	got := dropped.Load()
	assert.GreaterOrEqual(t, got, int64(100),
		"slow subscriber must accumulate drops, got %d", got)
}

func TestBus_UnsubReleasesSlot(t *testing.T) {
	b := NewBus()
	_, _, unsub1 := b.Subscribe(nil)
	_, _, unsub2 := b.Subscribe(nil)
	assert.Equal(t, 2, b.SubscriberCount())
	unsub1()
	assert.Equal(t, 1, b.SubscriberCount())
	unsub2()
	assert.Equal(t, 0, b.SubscriberCount())
}

func TestBus_NilReceiverIsSafe(t *testing.T) {
	var b *Bus
	b.Publish(Event{Type: EventRouteCreated}) // must not panic
	ch, _, unsub := b.Subscribe(nil)
	defer unsub()
	// closed channel returns the zero value immediately.
	_, ok := <-ch
	assert.False(t, ok, "nil bus must hand back a closed channel")
	assert.Equal(t, 0, b.SubscriberCount())
}

func TestBus_PublishUnderConcurrentSubscribe(t *testing.T) {
	// Race-detector run; the lock invariant is that Publish and unsub
	// serialize on b.mu so a send can't race a close.
	b := NewBus()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, unsub := b.Subscribe(nil)
			defer unsub()
			time.Sleep(time.Millisecond)
		}()
	}
	for i := 0; i < 100; i++ {
		b.Publish(Event{Type: EventRouteUpdated, Route: "x"})
	}
	wg.Wait()
}

func TestBus_PublishDoesNotBlockSubscribeOrUnsub(t *testing.T) {
	// Regression: Publish previously held b.mu across the whole fan-out,
	// so concurrent Subscribe/unsub queued behind every publish.
	b := NewBus()

	// Slow subscriber: full buffer, never drains.
	_, _, slowUnsub := b.Subscribe(nil)
	defer slowUnsub()
	for i := 0; i < eventBufferSize+8; i++ {
		b.Publish(Event{Type: EventCooldownStarted, Replica: "slow"})
	}

	// While the slow sub stays full, Subscribe + unsub must still make
	// progress. Race detector covers the channel side; the timing here
	// pins "no deadlock if a publisher is mid-fan-out."
	done := make(chan struct{})
	go func() {
		for i := 0; i < 50; i++ {
			_, _, u := b.Subscribe(nil)
			u()
		}
		close(done)
	}()
	go func() {
		for i := 0; i < 50; i++ {
			b.Publish(Event{Type: EventRouteUpdated, Route: "x"})
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("subscribe/unsub blocked by publish — bus lock held across fan-out")
	}
}

func TestEventTypes_ClosedEnumStable(t *testing.T) {
	// String-form of every constant must match the on-the-wire tag
	// dashboards filter on. A typo here is a silent drift.
	want := map[EventType]string{
		EventRouteCreated:       "route_created",
		EventRouteUpdated:       "route_updated",
		EventRouteDeleted:       "route_deleted",
		EventRouteExpired:       "route_expired",
		EventReplicaAdded:       "replica_added",
		EventReplicaRemoved:     "replica_removed",
		EventBreakerOpened:      "breaker_opened",
		EventBreakerClosed:      "breaker_closed",
		EventCooldownStarted:    "cooldown_started",
		EventCooldownEnded:      "cooldown_ended",
		EventHealthChanged:      "health_changed",
		EventAutoRouteGenerated: "auto_route_generated",
		EventAutoRouteRevoked:   "auto_route_revoked",
	}
	for et, expected := range want {
		assert.Equal(t, expected, string(et))
	}
	require.Len(t, AllEventTypes(), len(want),
		"AllEventTypes must include every constant")
}
