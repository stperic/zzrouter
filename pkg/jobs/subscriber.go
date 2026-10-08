package jobs

import "sync"

// Subscriber is a live stream of events from a single job. Open with
// Registry.Subscribe; the returned channel closes when the producer
// terminates AND all replay+backlog have drained, when the caller
// invokes Unsubscribe, or when the registry is stopped.
//
// Backpressure: each subscriber has a fixed channel buffer. Slow
// readers get drop-oldest semantics — the producer never blocks.
// Dropped counts are exposed via Dropped() for observability.
type Subscriber struct {
	ch       chan Event
	detach   func()
	detachMu sync.Once

	// mu serializes deliver against close so the non-blocking drain
	// loop in deliver cannot race with a concurrent close(s.ch),
	// which would panic on send. All mutations of closed/dropped
	// live under this lock.
	mu      sync.Mutex
	dropped uint64
	closed  bool
}

// newSubscriber constructs a Subscriber with the given channel buffer
// and detach hook. detach is invoked exactly once from Unsubscribe.
func newSubscriber(buf int, detach func()) *Subscriber {
	if buf < 1 {
		buf = 1
	}
	return &Subscriber{
		ch:     make(chan Event, buf),
		detach: detach,
	}
}

// Events returns the receive channel. Closed when the stream ends.
func (s *Subscriber) Events() <-chan Event { return s.ch }

// Unsubscribe detaches the subscriber from the job. Idempotent. Safe
// to call from any goroutine. The registered detach hook runs exactly
// once; the subscriber channel closes from the detach path so readers
// observe end-of-stream.
func (s *Subscriber) Unsubscribe() {
	s.detachMu.Do(func() {
		if s.detach != nil {
			s.detach()
		}
	})
}

// Dropped returns the cumulative drop count since creation. Grows when
// the subscriber's channel is full at deliver time. Safe to call after
// the stream has ended — counters remain readable.
func (s *Subscriber) Dropped() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropped
}

// deliver is the producer-side fan-out path. Non-blocking: if the
// channel is full, the oldest buffered event is drained to make room.
// Holds s.mu across the send so a concurrent close cannot race the
// drop-oldest drain and produce a send-on-closed-channel panic.
// Delivery against a closed subscriber still bumps Dropped so the
// counter accurately reflects lost events.
func (s *Subscriber) deliver(ev Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		s.dropped++
		return
	}
	select {
	case s.ch <- ev:
		return
	default:
	}
	// Channel full: drain oldest, then send. With s.mu held nobody else
	// is delivering concurrently, so the slot freed by the drain is
	// guaranteed free at the subsequent send.
	select {
	case <-s.ch:
		s.dropped++
	default:
		// Reader drained concurrently between our full-send and the
		// drain attempt; nothing to discard. Fall through to send.
	}
	s.ch <- ev
}

// close shuts the subscriber's channel. Idempotent. Called by the
// registry when the job terminates or the subscriber unsubscribes.
func (s *Subscriber) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	close(s.ch)
}
