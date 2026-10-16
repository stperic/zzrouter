package download

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/stperic/zzrouter/pkg/utils"
)

// BodyStallTimeout bounds byte silence even when a large download takes days.
const BodyStallTimeout = 10 * time.Minute

type stallReader struct {
	io.ReadCloser
	mu        sync.Mutex
	timer     *time.Timer
	lastData  time.Time
	err       error
	closed    bool
	closeOnce sync.Once
	closeErr  error
}

// NewStallReader closes a stalled HTTP body to unblock Read. Call Close on exit.
func NewStallReader(body io.ReadCloser) io.ReadCloser {
	r := &stallReader{ReadCloser: body, lastData: utils.Now()}
	r.timer = time.AfterFunc(BodyStallTimeout, r.expire)
	return r
}

func (r *stallReader) expire() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	// A successful read may have reset the timer while this callback was queued.
	if remaining := BodyStallTimeout - time.Since(r.lastData); remaining > 0 {
		r.timer.Reset(remaining)
		r.mu.Unlock()
		return
	}
	r.err = fmt.Errorf("no data received for %s: %w", BodyStallTimeout, context.DeadlineExceeded)
	r.mu.Unlock()
	_ = r.Close()
}

func (r *stallReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return n, r.err
	}
	if n > 0 && !r.closed {
		r.lastData = utils.Now()
		r.timer.Reset(BodyStallTimeout)
	}
	return n, err
}

func (r *stallReader) Close() error {
	r.mu.Lock()
	r.closed = true
	r.timer.Stop()
	r.mu.Unlock()
	// Join a watchdog's Close so transfer cleanup cannot finish before the body does.
	r.closeOnce.Do(func() { r.closeErr = r.ReadCloser.Close() })
	return r.closeErr
}
