package wire

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cutReader yields data and then fails with io.ErrUnexpectedEOF, as an upstream that drops mid-body.
type cutReader struct{ r io.Reader }

func newCutReader(data string) *cutReader { return &cutReader{r: strings.NewReader(data)} }

func (c *cutReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if err == io.EOF {
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}

// stallReader yields data and then blocks until ctx ends, returning ctx.Err().
type stallReader struct {
	ctx context.Context
	r   io.Reader
}

func newStallReader(ctx context.Context, data string) *stallReader {
	return &stallReader{ctx: ctx, r: strings.NewReader(data)}
}

func (s *stallReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if err == io.EOF {
		<-s.ctx.Done()
		return n, s.ctx.Err()
	}
	return n, err
}

// newHangBeforeHeadersServer returns a server whose handler blocks until release is closed or the request ends, writing nothing.
func newHangBeforeHeadersServer(t *testing.T) (srv *httptest.Server, release func()) {
	t.Helper()
	done := make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(done) }) }
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-done:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() {
		release()
		srv.Close()
	})
	return srv, release
}

// newStallMidStreamServer returns a server that writes the given SSE frames with the given content type, flushes, and then blocks until release is closed or the request ends.
func newStallMidStreamServer(t *testing.T, contentType string, frames ...string) (srv *httptest.Server, release func()) {
	t.Helper()
	done := make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(done) }) }
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(http.StatusOK)
		for _, f := range frames {
			_, _ = io.WriteString(w, f)
		}
		w.(http.Flusher).Flush()
		select {
		case <-done:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() {
		release()
		srv.Close()
	})
	return srv, release
}

func TestFaultFixture_CutReader(t *testing.T) {
	const data = "partial body"
	got, err := io.ReadAll(newCutReader(data))
	assert.Equal(t, data, string(got))
	assert.ErrorIs(t, err, io.ErrUnexpectedEOF)
}

func TestFaultFixture_StallReader(t *testing.T) {
	const data = "partial body"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	timer := time.AfterFunc(50*time.Millisecond, cancel)
	defer timer.Stop()

	got, err := io.ReadAll(newStallReader(ctx, data))
	assert.Equal(t, data, string(got))
	assert.ErrorIs(t, err, context.Canceled)
}

func TestFaultFixture_HangBeforeHeaders(t *testing.T) {
	srv, _ := newHangBeforeHeadersServer(t)

	client := &http.Client{Timeout: 100 * time.Millisecond}
	resp, err := client.Get(srv.URL)
	if resp != nil {
		_ = resp.Body.Close()
	}
	require.Error(t, err)
	var ne net.Error
	timedOut := errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout())
	assert.True(t, timedOut, "expected a timeout, got %v", err)
}

func TestFaultFixture_StallMidStream(t *testing.T) {
	const frame = "data: {\"id\":\"1\"}\n\n"
	srv, _ := newStallMidStreamServer(t, "text/event-stream", frame)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	buf := make([]byte, len(frame))
	_, err = io.ReadFull(resp.Body, buf)
	require.NoError(t, err)
	assert.Equal(t, frame, string(buf))

	cancel()
	_, err = resp.Body.Read(make([]byte, 1))
	assert.Error(t, err)
}
