package instance

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/stperic/zzrouter/pkg/prov_apps/keepalive"
)

// Lease is one request's hold on an instance: its concurrency slot, its place in the
// active-request count, and its activity stamps. It is the only way to hold a slot.
type Lease struct {
	instance *Instance
	once     sync.Once
}

// Acquire waits for a slot within the queue timeout and stamps activity with hints.
func (i *Instance) Acquire(ctx context.Context, hints *keepalive.Override) (*Lease, error) {
	if err := i.acquireSlot(ctx); err != nil {
		return nil, err
	}
	atomic.AddInt64(&i.activeRequests, 1)
	i.StampActivityWithHints(hints)
	return &Lease{instance: i}, nil
}

// AdmissionRetryAfter is the delay a refused caller is told to wait: slots free as soon as an
// in-flight request finishes.
const AdmissionRetryAfter = 5 * time.Second

// Reasons an instance refuses a request; both are AdmissionError reasons.
var (
	ErrQueueFull        = errors.New("instance request queue is full")
	ErrQueueWaitExpired = errors.New("instance request queue wait expired")
)

// AdmissionError is an instance refusing a request before it was sent: busy, not broken.
type AdmissionError struct {
	Reason     error
	RetryAfter time.Duration
}

func (e *AdmissionError) Error() string { return e.Reason.Error() }
func (e *AdmissionError) Unwrap() error { return e.Reason }

func admissionError(reason error) *AdmissionError {
	return &AdmissionError{Reason: reason, RetryAfter: AdmissionRetryAfter}
}

func (i *Instance) acquireSlot(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if i.concurrencySem == nil {
		return nil
	}
	i.queueMu.Lock()
	select {
	case i.concurrencySem <- struct{}{}:
		i.queueMu.Unlock()
		return nil
	default:
	}
	if i.queueTimeout == 0 || i.waiters >= i.maxQueuedRequests {
		i.queueMu.Unlock()
		return admissionError(ErrQueueFull)
	}
	i.waiters++
	i.queueMu.Unlock()
	defer func() {
		i.queueMu.Lock()
		i.waiters--
		i.queueMu.Unlock()
	}()

	var expired <-chan time.Time
	if i.queueTimeout > 0 {
		timer := time.NewTimer(i.queueTimeout)
		defer timer.Stop()
		expired = timer.C
	}
	select {
	case i.concurrencySem <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-i.concurrencySem
			return err
		}
		return nil
	case <-expired:
		if err := ctx.Err(); err != nil {
			return err
		}
		return admissionError(ErrQueueWaitExpired)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Release ends the hold once, however often it is called, and re-stamps activity so a
// request longer than keep_alive still re-arms the unload timer. It keeps the current
// keep_alive: a later request's hint must not be overwritten by an earlier one finishing.
func (l *Lease) Release() {
	l.once.Do(func() {
		i := l.instance
		// Re-arm expiry before the idle reaper can observe zero active requests.
		i.StampActivityWithHints(nil)
		atomic.AddInt64(&i.activeRequests, -1)
		if i.concurrencySem != nil {
			<-i.concurrencySem
		}
	})
}

// Instance returns the leased instance.
func (l *Lease) Instance() *Instance {
	return l.instance
}
