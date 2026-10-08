package client

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestClient(t *testing.T, handler http.Handler) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := NewClient(pkgConfig.ClientNodeConfig{
		Name:    "test",
		Address: srv.URL,
		APIKey:  "test-key",
	})
	return c, srv
}

// writeSSE writes a well-formed SSE frame.
func writeSSE(w http.ResponseWriter, flusher http.Flusher, name, data string) {
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data)
	flusher.Flush()
}

func TestSubscribeJob_ProgressThenDone(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/zzrouter/v1/jobs/job-1/stream", r.URL.Path)
		require.Equal(t, "test-key", r.Header.Get("X-API-Key"))
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f := w.(http.Flusher)
		writeSSE(w, f, "progress", `{"job_id":"job-1","phase":"running","percent":50,"step":"downloading"}`)
		writeSSE(w, f, "progress", `{"job_id":"job-1","phase":"running","percent":90}`)
		writeSSE(w, f, "done", `{"job_id":"job-1","phase":"done","percent":100}`)
	})

	c, _ := newTestClient(t, handler)

	var events []JobEvent
	terminal, err := c.SubscribeJob(context.Background(), "job-1", "", func(ev JobEvent) {
		events = append(events, ev)
	})
	require.NoError(t, err)
	require.NotNil(t, terminal)
	assert.Equal(t, "done", terminal.Phase)
	assert.Equal(t, 100, terminal.Percent)
	require.Len(t, events, 3)
	assert.Equal(t, JobEventProgress, events[0].Event)
	assert.Equal(t, "downloading", events[0].Step)
	assert.Equal(t, JobEventDone, events[2].Event)
}

func TestSubscribeJob_KeepAliveIgnored(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f := w.(http.Flusher)
		_, _ = fmt.Fprint(w, ": ping\n\n")
		f.Flush()
		writeSSE(w, f, "progress", `{"job_id":"j","phase":"running","percent":10}`)
		_, _ = fmt.Fprint(w, ": ping\n\n")
		f.Flush()
		writeSSE(w, f, "done", `{"job_id":"j","phase":"done"}`)
	})
	c, _ := newTestClient(t, handler)

	var count int
	terminal, err := c.SubscribeJob(context.Background(), "j", "", func(ev JobEvent) { count++ })
	require.NoError(t, err)
	require.NotNil(t, terminal)
	assert.Equal(t, 2, count, "keep-alive pings should not be surfaced to handler")
}

func TestSubscribeJob_NodeQueryForwarded(t *testing.T) {
	var got atomic.Value
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Store(r.URL.RawQuery)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f := w.(http.Flusher)
		writeSSE(w, f, "done", `{"job_id":"j","phase":"done"}`)
	})
	c, _ := newTestClient(t, handler)

	_, err := c.SubscribeJob(context.Background(), "j", "worker-1", nil)
	require.NoError(t, err)
	assert.Equal(t, "node=worker-1", got.Load())
}

func TestSubscribeJob_EpochMismatch(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = fmt.Fprint(w, `{"error":"epoch mismatch","code":"epoch_mismatch"}`)
	})
	c, _ := newTestClient(t, handler)
	_, err := c.SubscribeJob(context.Background(), "j", "", nil)
	require.ErrorIs(t, err, ErrJobEpochMismatch)
}

func TestSubscribeJob_NotFound(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	c, _ := newTestClient(t, handler)
	_, err := c.SubscribeJob(context.Background(), "j", "", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestSubscribeJob_ContextCancel(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer wg.Done()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f := w.(http.Flusher)
		writeSSE(w, f, "progress", `{"job_id":"j","phase":"running"}`)
		<-r.Context().Done()
	})
	c, _ := newTestClient(t, handler)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.SubscribeJob(ctx, "j", "", func(ev JobEvent) {
			cancel()
		})
		done <- err
	}()
	select {
	case err := <-done:
		// Either ctx.Err() or nil (if server shut cleanly) — both acceptable.
		_ = err
	case <-time.After(2 * time.Second):
		t.Fatal("SubscribeJob did not return after ctx cancel")
	}
	wg.Wait()
}

func TestCancelJob_Accepted(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodDelete, r.Method)
		assert.Equal(t, "/zzrouter/v1/jobs/job-9", r.URL.Path)
		w.WriteHeader(http.StatusAccepted)
		_, _ = fmt.Fprint(w, `{"accepted":true}`)
	})
	c, _ := newTestClient(t, handler)
	require.NoError(t, c.CancelJob(context.Background(), "job-9"))
}

func TestCancelJob_NotFoundIsNoOp(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	c, _ := newTestClient(t, handler)
	require.NoError(t, c.CancelJob(context.Background(), "job-9"))
}

func TestCancelJob_EmptyID(t *testing.T) {
	c := NewClient(pkgConfig.ClientNodeConfig{Address: "http://x"})
	err := c.CancelJob(context.Background(), "")
	require.Error(t, err)
	assert.Contains(t, strings.ToLower(err.Error()), "empty")
}
