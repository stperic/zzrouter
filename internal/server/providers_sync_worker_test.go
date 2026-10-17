package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	"github.com/stretchr/testify/require"
)

// gate blocks a push handler until opened; opening twice is harmless, so
// cleanup can open whatever a failed test left shut.
func gate() (chan struct{}, func()) {
	ch := make(chan struct{})
	return ch, sync.OnceFunc(func() { close(ch) })
}

// A write that lands while a round is in flight rides the next round. The
// waiter for one worker must not answer from the round in flight, must
// follow the write to the next round when the one in flight ends, and
// must answer once that worker has it while another holds the round open.
func TestAwaitWorkerFollowsTheRoundCarryingTheWrite(t *testing.T) {
	store := newFanOutTestStore(t)
	firstFast, openFirstFast := gate()
	firstSlow, openFirstSlow := gate()
	secondFast, openSecondFast := gate()
	secondSlow, openSecondSlow := gate()
	var fastPushes, slowPushes atomic.Int32
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fastPushes.Add(1) == 1 {
			<-firstFast
		} else {
			<-secondFast
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(fast.Close)
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if slowPushes.Add(1) == 1 {
			<-firstSlow
		} else {
			<-secondSlow // the round carrying the write never finishes here
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(slow.Close)
	// Registered after Close, so they run first: Close waits for the handlers.
	t.Cleanup(func() { openFirstFast(); openFirstSlow(); openSecondFast(); openSecondSlow() })
	f := newProviderSyncFanOut(store, func() bool { return true },
		func() []*mesh.Endpoint {
			return []*mesh.Endpoint{{URL: fast.URL, NodeName: "fast"}, {URL: slow.URL, NodeName: "slow"}}
		},
		func() *http.Client { return http.DefaultClient },
		compatibleSyncClusterClient,
	)

	f.fire(store.Config())
	require.Eventually(t, func() bool { return fastPushes.Load() == 1 && slowPushes.Load() == 1 }, time.Second, time.Millisecond)
	f.fire(store.Config()) // the write, queued behind the round in flight

	reached := make(chan bool, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { reached <- f.AwaitWorker(ctx, "fast") }()
	openFirstFast()
	select {
	case <-reached:
		t.Fatal("answered from the round in flight, which does not carry the write")
	case <-time.After(200 * time.Millisecond):
	}
	openFirstSlow() // the round in flight ends; the waiter must move on
	openSecondFast()
	select {
	case ok := <-reached:
		require.True(t, ok)
	case <-ctx.Done():
		t.Fatal("the waiter stayed on the finished round")
	}
}

// A round that ends wakes whoever waits on it, whether a round follows or
// not: a waiter parked after its last answer has nothing else to wake it.
func TestEndedRoundWakesItsWaiters(t *testing.T) {
	for _, follows := range []bool{false, true} {
		store := newFanOutTestStore(t)
		release, open := gate()
		var pushes atomic.Int32
		worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if pushes.Add(1) == 1 {
				<-release
			}
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(worker.Close)
		t.Cleanup(open)
		f := newProviderSyncFanOut(store, func() bool { return true },
			func() []*mesh.Endpoint { return []*mesh.Endpoint{{URL: worker.URL, NodeName: "w1"}} },
			func() *http.Client { return http.DefaultClient },
			compatibleSyncClusterClient,
		)
		f.fire(store.Config())
		require.Eventually(t, func() bool { return pushes.Load() == 1 }, time.Second, time.Millisecond)
		f.mu.Lock()
		first := f.current
		f.mu.Unlock()
		if follows {
			f.fire(store.Config())
		}
		open()
		require.Eventually(t, func() bool { return len(first.landed()) == 1 }, time.Second, time.Millisecond)
		select {
		case <-first.next():
		case <-time.After(2 * time.Second):
			t.Fatalf("a parked waiter was not woken when the round ended (round follows: %v)", follows)
		}
	}
}
