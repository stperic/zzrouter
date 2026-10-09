package wire

import (
	"context"
	"io"
	"sync"
	"sync/atomic"

	"github.com/stperic/zzrouter/pkg/observability/llm"
)

// MeteredBody meters a body a reverse proxy copies; Result is valid after Close.
type MeteredBody struct {
	ctx       context.Context
	r         io.ReadCloser
	meter     streamMeter
	mu        sync.Mutex
	closeOnce sync.Once
	closing   atomic.Bool
	closeErr  error
	closed    bool
}

// NewMeteredBody wraps r; a Close before the end is ClientGone when ctx is done, else Truncated.
func NewMeteredBody(ctx context.Context, r io.ReadCloser, recorder *llm.InferenceRecorder) *MeteredBody {
	return &MeteredBody{ctx: ctx, r: r, meter: streamMeter{recorder: recorder}}
}

func (m *MeteredBody) Read(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return 0, io.ErrClosedPipe
	}
	if m.meter.finished {
		return 0, io.EOF
	}
	n, err := m.r.Read(p)
	if n > 0 {
		m.meter.next(p[:n])
	}
	if err != nil {
		// A read failing because Close began is the proxy giving up on the client.
		m.meter.finish(err, m.ctx.Err() != nil || m.closing.Load())
	}
	return n, err
}

func (m *MeteredBody) Close() error {
	// Close the source before locking so a blocked Read can finish observing.
	m.closing.Store(true)
	m.closeOnce.Do(func() { m.closeErr = m.r.Close() })
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	m.meter.finish(nil, m.ctx.Err() != nil)
	return m.closeErr
}

// Result is stable after Close because finalization is idempotent.
func (m *MeteredBody) Result() Result {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.meter.result
}
