package server

import (
	"sync"
	"sync/atomic"

	"github.com/stperic/zzrouter/pkg/access/quota"
)

// spendEventBufferSize bounds memory per subscriber. 64 events ≈ 8 KB at
// the typical event size — plenty of headroom for an agent that drains
// at network speed, small enough that a stuck subscriber drops events
// instead of pinning the whole bus.
const spendEventBufferSize = 64

// SpendEventBus implements quota.BreachObserver by fanning every breach
// out to live SSE subscribers. Drop-on-overflow per subscriber so a
// single slow client can't backpressure the enforcement hot path.
type SpendEventBus struct {
	mu          sync.Mutex
	subscribers map[*spendSubscriber]struct{}
}

type spendSubscriber struct {
	ch      chan quota.BreachEvent
	dropped atomic.Int64
}

// NewSpendEventBus returns a ready-to-use bus. Cheap to construct.
func NewSpendEventBus() *SpendEventBus {
	return &SpendEventBus{subscribers: map[*spendSubscriber]struct{}{}}
}

// OnBreach broadcasts to every live subscriber. Non-blocking — full
// channels increment the per-subscriber dropped counter and return.
//
// Lock invariant: OnBreach and the unsub closure both take b.mu, so a
// channel can't be closed mid-send. If you ever refactor to release
// the lock during the per-subscriber send, you MUST switch unsub to
// delete-then-drain to avoid send-on-closed-chan panics.
func (b *SpendEventBus) OnBreach(event quota.BreachEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for sub := range b.subscribers {
		select {
		case sub.ch <- event:
		default:
			sub.dropped.Add(1)
		}
	}
}

// Subscribe returns a buffered channel of events plus a pointer to the
// per-subscriber dropped counter and an unsub function the caller MUST
// invoke (typically via defer) to release the subscriber slot.
func (b *SpendEventBus) Subscribe() (events <-chan quota.BreachEvent, dropped *atomic.Int64, unsub func()) {
	sub := &spendSubscriber{ch: make(chan quota.BreachEvent, spendEventBufferSize)}
	b.mu.Lock()
	b.subscribers[sub] = struct{}{}
	b.mu.Unlock()
	return sub.ch, &sub.dropped, func() {
		b.mu.Lock()
		delete(b.subscribers, sub)
		b.mu.Unlock()
		close(sub.ch)
	}
}

// SubscriberCount is exposed for tests pinning the cleanup contract.
func (b *SpendEventBus) SubscriberCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subscribers)
}
