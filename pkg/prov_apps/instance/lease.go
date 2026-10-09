package instance

import (
	"context"
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

func (i *Instance) acquireSlot(ctx context.Context) error {
	if i.concurrencySem == nil {
		return nil
	}
	select {
	case i.concurrencySem <- struct{}{}:
		return nil
	default:
	}
	// Zero refuses at the door; negative waits as long as the caller does.
	if i.queueTimeout == 0 {
		return ErrAtCapacity
	}
	if i.queueTimeout < 0 {
		select {
		case i.concurrencySem <- struct{}{}:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	timer := time.NewTimer(i.queueTimeout)
	defer timer.Stop()
	select {
	case i.concurrencySem <- struct{}{}:
		return nil
	case <-timer.C:
		return ErrAtCapacity
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
