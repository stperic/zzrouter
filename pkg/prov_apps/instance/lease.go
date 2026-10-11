package instance

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/observability/proxy"
	"github.com/stperic/zzrouter/pkg/prov_apps/keepalive"
	"github.com/stperic/zzrouter/pkg/utils"
)

// Lease is one request's hold on an instance: its concurrency slot, its place in the
// active-request count, and its activity stamps. It is the only way to hold a slot.
type Lease struct {
	instance *Instance
	once     sync.Once
	mu       sync.Mutex
	watch    *streamWatch
	released bool
}

// Acquire waits for a slot within the queue timeout and stamps activity with hints.
func (i *Instance) Acquire(ctx context.Context, hints *keepalive.Override) (_ *Lease, err error) {
	start := utils.Now()
	defer func() {
		proxy.RecordAdmissionWait(ctx, i.Provider, i.Model, admissionOutcome(err), utils.Now().Sub(start))
	}()
	err = i.acquireSlot(ctx)
	if err != nil {
		return nil, err
	}
	i.mu.Lock()
	if i.idleStopClaimed || i.Status == StatusStopping || i.Status.IsTerminal() {
		i.mu.Unlock()
		if i.concurrencySem != nil {
			<-i.concurrencySem
		}
		return nil, admissionError(ErrStopping)
	}
	atomic.AddInt64(&i.activeRequests, 1)
	i.mu.Unlock()
	i.StampActivityWithHints(hints)
	return &Lease{instance: i}, nil
}

// AdmissionRetryAfter is the delay a refused caller is told to wait: slots free as soon as an
// in-flight request finishes.
const AdmissionRetryAfter = 5 * time.Second

// Reasons an instance refuses a request; each is an AdmissionError reason.
var (
	ErrQueueFull        = errors.New("instance request queue is full")
	ErrQueueWaitExpired = errors.New("instance request queue wait expired")
	ErrStopping         = errors.New("instance is stopping")
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

// admissionOutcome names how a slot request ended, for the admission wait histogram. Anything
// that is not an admission refusal or success is the caller giving up (a context error).
func admissionOutcome(err error) string {
	switch {
	case err == nil:
		return proxy.AdmissionAdmitted
	case errors.Is(err, ErrQueueFull):
		return proxy.AdmissionQueueFull
	case errors.Is(err, ErrQueueWaitExpired):
		return proxy.AdmissionWaitExpired
	case errors.Is(err, ErrStopping):
		return proxy.AdmissionStopping
	default:
		return proxy.AdmissionCancelled
	}
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
		l.mu.Lock()
		l.released = true
		if l.watch != nil {
			l.watch.stop()
		}
		l.mu.Unlock()
		i := l.instance
		i.released()
		if i.concurrencySem != nil {
			<-i.concurrencySem
		}
	})
}

// Instance returns the leased instance.
func (l *Lease) Instance() *Instance {
	return l.instance
}

// ErrFirstByteTimeout means the engine stayed silent through its prefill budget.
var ErrFirstByteTimeout = streamTimeoutError("instance first body byte timeout")

// ErrStreamIdle means an opened engine stream stopped producing bytes.
var ErrStreamIdle = streamTimeoutError("instance stream idle timeout")

type streamTimeoutError string

func (e streamTimeoutError) Error() string { return string(e) }
func (streamTimeoutError) Timeout() bool   { return true }
func (streamTimeoutError) Temporary() bool { return false }

// WatchStream bounds engine silence while allowing a continuously read stream to run indefinitely.
// A lease carries one request, so it has one watch: later calls return the first one.
func (l *Lease) WatchStream(ctx context.Context) (context.Context, func(io.ReadCloser) io.ReadCloser) {
	return l.watchStream(ctx, constants.DefaultFirstByteTimeout, constants.DefaultStreamIdleTimeout)
}

func (l *Lease) watchStream(ctx context.Context, firstByte, idle time.Duration) (context.Context, func(io.ReadCloser) io.ReadCloser) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.watch == nil {
		ctx, cancel := context.WithCancelCause(ctx)
		w := &streamWatch{ctx: ctx, cancel: cancel, idle: idle, cause: ErrFirstByteTimeout, deadline: utils.Now().Add(firstByte)}
		w.mu.Lock()
		w.timer = time.AfterFunc(firstByte, w.expire)
		w.mu.Unlock()
		l.watch = w
		if l.released {
			w.stop()
		}
	}
	w := l.watch
	return w.ctx, func(body io.ReadCloser) io.ReadCloser { return &watchedBody{ReadCloser: body, watch: w} }
}

type streamWatch struct {
	mu       sync.Mutex
	ctx      context.Context
	cancel   context.CancelCauseFunc
	timer    *time.Timer
	deadline time.Time
	idle     time.Duration
	cause    error
	stopped  bool
}

func (w *streamWatch) expire() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped || w.ctx.Err() != nil {
		return
	}
	// A dispatched callback may outlive Reset; only the current silence window can expire.
	if remaining := w.deadline.Sub(utils.Now()); remaining > 0 {
		w.timer.Reset(remaining)
		return
	}
	w.stopped = true
	w.cancel(w.cause)
}

func (w *streamWatch) stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stopLocked()
}

func (w *streamWatch) stopLocked() {
	w.stopped = true
	w.timer.Stop()
	w.cancel(nil)
}

type watchedBody struct {
	io.ReadCloser
	watch *streamWatch
}

func (b *watchedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	w := b.watch
	w.mu.Lock()
	defer w.mu.Unlock()
	if cause := context.Cause(w.ctx); errors.Is(cause, ErrFirstByteTimeout) || errors.Is(cause, ErrStreamIdle) {
		err = cause
	}
	if err != nil {
		w.stopLocked()
	} else if n > 0 && !w.stopped {
		w.cause = ErrStreamIdle
		w.deadline = utils.Now().Add(w.idle)
		w.timer.Reset(w.idle)
	}
	return n, err
}

func (b *watchedBody) Close() error {
	b.watch.stop()
	return b.ReadCloser.Close()
}
