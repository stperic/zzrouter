package jobstream

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
)

func newTestClient(t *testing.T, handler http.Handler) *pkgClient.Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return pkgClient.NewClient(pkgConfig.ClientNodeConfig{
		Name:    "test",
		Address: srv.URL,
		APIKey:  "test-key",
	})
}

func writeSSE(w http.ResponseWriter, f http.Flusher, name, data string) {
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data)
	f.Flush()
}

// pump exhausts one Cmd and returns the resulting FrameMsg. Fatal on
// nil cmd or non-FrameMsg result.
func pump(t *testing.T, row *Row, next func() any) FrameMsg {
	t.Helper()
	m := next()
	if m == nil {
		t.Fatalf("expected FrameMsg, got nil")
	}
	fm, ok := m.(FrameMsg)
	if !ok {
		t.Fatalf("expected FrameMsg, got %T", m)
	}
	if fm.Err == nil {
		row.Latest = fm.Event
		row.Phase = fm.Event.Phase
	} else {
		row.Err = fm.Err
	}
	if fm.Done {
		row.Done = true
	}
	return fm
}

func TestSubscribe_ProgressThenTerminal(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f := w.(http.Flusher)
		writeSSE(w, f, "progress", `{"job_id":"j","phase":"running","percent":50,"step":"downloading"}`)
		writeSSE(w, f, "progress", `{"job_id":"j","phase":"running","percent":90}`)
		writeSSE(w, f, "done", `{"job_id":"j","phase":"done","percent":100}`)
	}))

	row, cmd := Subscribe(context.Background(), client, "j", "")
	require.NotNil(t, cmd)

	// First frame
	fm1 := pump(t, row, func() any { return cmd() })
	assert.Equal(t, "j", fm1.JobID)
	assert.Equal(t, 50, fm1.Event.Percent)
	assert.False(t, fm1.Done)

	// Second frame via Next
	fm2 := pump(t, row, func() any { return Next(row)() })
	assert.Equal(t, 90, fm2.Event.Percent)

	// Terminal
	fm3 := pump(t, row, func() any { return Next(row)() })
	assert.True(t, fm3.Done)
	assert.Equal(t, "done", fm3.Event.Phase)

	// Next after Done returns nil
	assert.Nil(t, Next(row))
}

func TestSubscribe_ErrorTerminal(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	row, cmd := Subscribe(context.Background(), client, "bad", "")
	fm := pump(t, row, func() any { return cmd() })
	assert.True(t, fm.Done)
	require.Error(t, fm.Err)
}

func TestSubscribe_EventsDroppedSuppressed(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f := w.(http.Flusher)
		writeSSE(w, f, "events_dropped", `{"job_id":"j","phase":"running","dropped":{"since":1,"current":5}}`)
		writeSSE(w, f, "progress", `{"job_id":"j","phase":"running","percent":30}`)
		writeSSE(w, f, "done", `{"job_id":"j","phase":"done"}`)
	}))

	row, cmd := Subscribe(context.Background(), client, "j", "")

	// First user-visible frame should be the progress (dropped was suppressed).
	fm1 := pump(t, row, func() any { return cmd() })
	assert.Equal(t, 30, fm1.Event.Percent, "events_dropped should not surface")

	// Terminal
	fm2 := pump(t, row, func() any { return Next(row)() })
	assert.True(t, fm2.Done)

	// Dropped counter was incremented
	assert.Equal(t, uint64(1), row.Dropped.Load())
}

func TestCancel_IsIdempotentAndZeroSafe(t *testing.T) {
	var zero Row
	zero.Cancel() // must not panic

	var nilRow *Row
	nilRow.Cancel() // must not panic

	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f := w.(http.Flusher)
		writeSSE(w, f, "progress", `{"job_id":"j","phase":"running"}`)
		<-r.Context().Done()
	}))
	row, _ := Subscribe(context.Background(), client, "j", "")
	row.Cancel()
	row.Cancel() // second call is a no-op
}

func TestCancel_UnblocksPendingNext(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(1)
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer wg.Done()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f := w.(http.Flusher)
		writeSSE(w, f, "progress", `{"job_id":"j","phase":"running"}`)
		<-r.Context().Done()
	}))

	row, cmd := Subscribe(context.Background(), client, "j", "")
	// First frame
	pump(t, row, func() any { return cmd() })

	// Park a Next then cancel — Next must return a synthesised terminal.
	done := make(chan FrameMsg, 1)
	go func() {
		m := Next(row)()
		done <- m.(FrameMsg)
	}()

	time.Sleep(20 * time.Millisecond)
	row.Cancel()

	select {
	case fm := <-done:
		assert.True(t, fm.Done, "parked Next must unblock with terminal after Cancel")
	case <-time.After(2 * time.Second):
		t.Fatal("parked Next did not unblock after Cancel")
	}
	wg.Wait()
}

func TestParentCtxCancel_ExitsCleanly(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(1)
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer wg.Done()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f := w.(http.Flusher)
		writeSSE(w, f, "progress", `{"job_id":"j","phase":"running"}`)
		<-r.Context().Done()
	}))

	parent, cancelParent := context.WithCancel(context.Background())
	row, cmd := Subscribe(parent, client, "j", "")
	pump(t, row, func() any { return cmd() })

	done := make(chan FrameMsg, 1)
	go func() {
		m := Next(row)()
		done <- m.(FrameMsg)
	}()

	time.Sleep(20 * time.Millisecond)
	cancelParent()

	select {
	case fm := <-done:
		assert.True(t, fm.Done, "parent ctx cancel must tear down Next")
	case <-time.After(2 * time.Second):
		t.Fatal("Next did not unblock after parent ctx cancel")
	}
	wg.Wait()
}

func TestConcurrentRows_DoNotCross(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// JobID is the last path segment between /jobs/ and /stream
		jobID := r.URL.Path
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f := w.(http.Flusher)
		writeSSE(w, f, "progress", fmt.Sprintf(`{"job_id":"%s","phase":"running","percent":10}`, jobIDFromPath(jobID)))
		writeSSE(w, f, "done", fmt.Sprintf(`{"job_id":"%s","phase":"done"}`, jobIDFromPath(jobID)))
	}))

	rowA, cmdA := Subscribe(context.Background(), client, "A", "")
	rowB, cmdB := Subscribe(context.Background(), client, "B", "")

	fmA1 := pump(t, rowA, func() any { return cmdA() })
	fmB1 := pump(t, rowB, func() any { return cmdB() })
	assert.Equal(t, "A", fmA1.JobID)
	assert.Equal(t, "B", fmB1.JobID)

	fmA2 := pump(t, rowA, func() any { return Next(rowA)() })
	fmB2 := pump(t, rowB, func() any { return Next(rowB)() })
	assert.True(t, fmA2.Done)
	assert.True(t, fmB2.Done)
}

func jobIDFromPath(p string) string {
	// p = /zzrouter/v1/jobs/<id>/stream
	const prefix = "/zzrouter/v1/jobs/"
	s := p[len(prefix):]
	// strip /stream suffix
	if i := len(s) - len("/stream"); i >= 0 {
		s = s[:i]
	}
	return s
}

// A job that fails on the node arrives as an ordinary terminal frame with no
// transport error. Without carrying it onto FrameMsg.Err, every consumer
// reads the failure as a success and tells the operator the work is done.
func TestSubscribe_FailedJobCarriesTheError(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f, ok := w.(http.Flusher)
		require.True(t, ok)
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(w, f, "error",
			`{"job_id":"inst_fail","phase":"failed","err":"download returned status 404"}`)
	}))

	row, next := Subscribe(context.Background(), client, "inst_fail", "")
	defer row.Cancel()

	frame := pump(t, row, func() any { return next() })
	assert.True(t, frame.Done)
	require.Error(t, frame.Err, "a failed job must not read as a successful one")
	assert.Contains(t, frame.Err.Error(), "404")
}

// A terse node that reports the phase without a message still failed.
func TestSubscribe_FailedJobWithoutMessage(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f, ok := w.(http.Flusher)
		require.True(t, ok)
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(w, f, "error", `{"job_id":"inst_terse","phase":"failed"}`)
	}))

	row, next := Subscribe(context.Background(), client, "inst_terse", "")
	defer row.Cancel()

	frame := pump(t, row, func() any { return next() })
	require.Error(t, frame.Err)
	assert.Contains(t, frame.Err.Error(), "failed")
}

// The clean path must stay clean: a done job carries no error.
func TestSubscribe_DoneJobCarriesNoError(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f, ok := w.(http.Flusher)
		require.True(t, ok)
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(w, f, "done", `{"job_id":"inst_ok","phase":"done","percent":100}`)
	}))

	row, next := Subscribe(context.Background(), client, "inst_ok", "")
	defer row.Cancel()

	frame := pump(t, row, func() any { return next() })
	assert.True(t, frame.Done)
	assert.NoError(t, frame.Err)
}
