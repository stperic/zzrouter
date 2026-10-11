package instance

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/dispatch/wire"
	"github.com/stperic/zzrouter/pkg/protocol/openai"
)

const testFirstByteTimeout = 100 * time.Millisecond
const testStreamIdleTimeout = 20 * time.Millisecond
const testStreamFrame = "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"

type leaseTransport func(*http.Request) (*http.Response, error)

func (f leaseTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func streamTestLease(t *testing.T) (*Instance, *Lease) {
	t.Helper()
	inst := NewInstance("stream", "engine", "m", 0, 0, 0)
	inst.InitConcurrencyLimit(ConcurrencyConfig{MaxConcurrentRequests: 1})
	return inst, acquireTestLease(t, inst)
}

func assertStreamSlotFree(t *testing.T, inst *Instance) {
	t.Helper()
	require.Zero(t, inst.GetActiveRequests())
	next, err := inst.Acquire(context.Background(), nil)
	require.NoError(t, err)
	next.Release()
}

func TestFirstByteDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		inst, lease := streamTestLease(t)
		parent, cancel := context.WithTimeout(context.Background(), 10*testFirstByteTimeout)
		defer cancel()
		ctx, _ := lease.watchStream(parent, testFirstByteTimeout, testStreamIdleTimeout)
		client := &http.Client{Transport: leaseTransport(func(req *http.Request) (*http.Response, error) {
			<-req.Context().Done()
			return nil, context.Cause(req.Context())
		})}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://engine.test", nil)
		require.NoError(t, err)
		_, err = client.Do(req) //nolint:bodyclose // the send times out: no body
		require.ErrorIs(t, err, ErrFirstByteTimeout)
		require.ErrorIs(t, context.Cause(ctx), ErrFirstByteTimeout)
		require.NoError(t, parent.Err())
		lease.Release()
		assertStreamSlotFree(t, inst)
	})
}

type silentStreamBody struct {
	ctx context.Context
	io.Reader
}

func (b silentStreamBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	if err == io.EOF {
		<-b.ctx.Done()
		return n, b.ctx.Err()
	}
	return n, err
}
func (silentStreamBody) Close() error { return nil }

func TestStreamIdleDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		inst, lease := streamTestLease(t)
		parent, cancel := context.WithTimeout(context.Background(), 10*testFirstByteTimeout)
		defer cancel()
		ctx, wrap := lease.watchStream(parent, testFirstByteTimeout, testStreamIdleTimeout)
		body := wrap(silentStreamBody{ctx, strings.NewReader(testStreamFrame)})
		w := httptest.NewRecorder()
		start := time.Now()
		result := wire.CopyStream(parent, w, body, nil, nil)
		require.ErrorIs(t, result.Err, ErrStreamIdle)
		assert.Less(t, time.Since(start), testFirstByteTimeout, "a stall is cut one idle window after the last byte, not at the first-byte deadline")
		require.Equal(t, wire.Truncated, result.Outcome)
		failure, failed := result.Failure()
		require.True(t, failed)
		openai.New().EndStream(w, failure)
		assert.Contains(t, w.Body.String(), testStreamFrame)
		assert.Contains(t, w.Body.String(), `"code":"upstream_stream_interrupted"`)
		assert.NotContains(t, w.Body.String(), "[DONE]")
		require.NoError(t, body.Close())
		lease.Release()
		assertStreamSlotFree(t, inst)
	})
}

type steadyStreamBody struct {
	ctx    context.Context
	frames int
}

func (b *steadyStreamBody) Read(p []byte) (int, error) {
	if b.frames == 0 {
		return 0, io.EOF
	}
	select {
	case <-time.After(testStreamIdleTimeout / 2):
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	}
	b.frames--
	frame := testStreamFrame
	if b.frames == 0 {
		frame = "data: [DONE]\n\n"
	}
	return copy(p, frame), nil
}
func (*steadyStreamBody) Close() error { return nil }

func TestSlowSteadyStreamSurvives(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		inst, lease := streamTestLease(t)
		ctx, wrap := lease.watchStream(context.Background(), testFirstByteTimeout, testStreamIdleTimeout)
		body := wrap(&steadyStreamBody{ctx: ctx, frames: 12})
		result := wire.CopyStream(context.Background(), httptest.NewRecorder(), body, nil, nil)
		require.Equal(t, wire.Completed, result.Outcome)
		require.NoError(t, result.Err)
		require.NoError(t, body.Close())
		lease.Release()
		assertStreamSlotFree(t, inst)
	})
}

func TestStreamWatchStopsOnCloseOrRelease(t *testing.T) {
	for _, closeBody := range []bool{false, true} {
		t.Run(map[bool]string{false: "release", true: "close"}[closeBody], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				_, lease := streamTestLease(t)
				ctx, wrap := lease.watchStream(context.Background(), testFirstByteTimeout, testStreamIdleTimeout)
				body := wrap(io.NopCloser(strings.NewReader(testStreamFrame)))
				if closeBody {
					require.NoError(t, body.Close())
				} else {
					lease.Release()
				}
				time.Sleep(2 * testFirstByteTimeout)
				assert.ErrorIs(t, context.Cause(ctx), context.Canceled)
				lease.watch.mu.Lock()
				assert.True(t, lease.watch.stopped)
				lease.watch.mu.Unlock()
			})
		})
	}
}

func TestStreamWatchZeroByteReadKeepsFirstByteTimer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, lease := streamTestLease(t)
		ctx, wrap := lease.watchStream(context.Background(), testFirstByteTimeout, testStreamIdleTimeout)
		body := wrap(io.NopCloser(zeroByteReader{}))
		_, err := body.Read(make([]byte, 1))
		require.NoError(t, err)
		time.Sleep(2 * testStreamIdleTimeout)
		require.NoError(t, ctx.Err())
		time.Sleep(testFirstByteTimeout)
		assert.ErrorIs(t, context.Cause(ctx), ErrFirstByteTimeout)
		require.NoError(t, body.Close())
	})
}

type zeroByteReader struct{}

func (zeroByteReader) Read([]byte) (int, error) { return 0, nil }

func TestStreamWatchConcurrentReleaseAndClose(t *testing.T) {
	_, lease := streamTestLease(t)
	_, wrap := lease.watchStream(context.Background(), time.Second, time.Second)
	body := wrap(io.NopCloser(strings.NewReader(testStreamFrame)))
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(lease.Release)
		wg.Go(func() { _ = body.Close() })
	}
	wg.Wait()
}

func TestFirstByteDeadlineHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) { <-req.Context().Done() }))
	defer srv.Close()
	inst, lease := streamTestLease(t)
	parent, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ctx, _ := lease.watchStream(parent, testFirstByteTimeout, testStreamIdleTimeout)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	require.NoError(t, err)
	_, err = srv.Client().Do(req) //nolint:bodyclose // the send times out: no body
	require.ErrorIs(t, err, ErrFirstByteTimeout)
	lease.Release()
	assertStreamSlotFree(t, inst)
}

func TestStreamIdleDeadlineHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, testStreamFrame)
		w.(http.Flusher).Flush()
		<-req.Context().Done()
	}))
	defer srv.Close()
	inst, lease := streamTestLease(t)
	parent, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ctx, wrap := lease.watchStream(parent, testFirstByteTimeout, testStreamIdleTimeout)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	require.NoError(t, err)
	resp, err := srv.Client().Do(req) //nolint:bodyclose // closed below through its watch wrapper
	require.NoError(t, err)
	body := wrap(resp.Body)
	w := httptest.NewRecorder()
	result := wire.CopyStream(parent, w, body, nil, nil)
	require.ErrorIs(t, result.Err, ErrStreamIdle)
	failure, failed := result.Failure()
	require.True(t, failed)
	openai.New().EndStream(w, failure)
	assert.Contains(t, w.Body.String(), `"code":"upstream_stream_interrupted"`)
	require.NoError(t, body.Close())
	lease.Release()
	assertStreamSlotFree(t, inst)
}
