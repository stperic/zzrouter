package signal

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSignaler_Notify_CoordinatorNoOp — only workers should notify
// their master. On a coordinator (IsWorker=false) the call is a
// silent no-op, no goroutine spawned.
func TestSignaler_Notify_CoordinatorNoOp(t *testing.T) {
	s := New(Config{
		IsWorker:        func() bool { return false },
		MTLSClient:      func() *http.Client { return &http.Client{} },
		CoordinatorURL:  func() string { return "https://coord:9091" },
		WorkerPublicURL: func() string { return "" },
	})

	s.Start(context.Background())
	defer s.Stop(context.Background())

	require.NoError(t, s.NotifyCacheRefresh(t.Context()))

	// Stop must return promptly because notifyWG is empty.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	s.Stop(ctx)
}

// TestSignaler_Notify_NoOpWhenNotStarted — the lifecycle gate: no
// active lifeCtx means the notify goroutine is never kicked off.
// Prevents post-Stop or pre-Start notifications from firing stale
// HTTP requests.
func TestSignaler_Notify_NoOpWhenNotStarted(t *testing.T) {
	s := New(Config{
		IsWorker:        func() bool { return true },
		MTLSClient:      func() *http.Client { return &http.Client{} },
		CoordinatorURL:  func() string { return "https://coord:9091" },
		WorkerPublicURL: func() string { return "https://worker:9090" },
	})

	// No Start called — should bail via lifeCtx==nil check.
	require.NoError(t, s.NotifyCacheRefresh(t.Context()))

	// A Stop with short ctx would hang if a goroutine had been spawned.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	s.Stop(ctx)
}

// TestSignaler_Notify_NoOpAfterStop — once Stop has niled lifeCtx,
// subsequent notify calls bail. Regression guard for the Add-under-
// lock gate.
func TestSignaler_Notify_NoOpAfterStop(t *testing.T) {
	s := New(Config{
		IsWorker:        func() bool { return true },
		MTLSClient:      func() *http.Client { return &http.Client{} },
		CoordinatorURL:  func() string { return "https://coord:9091" },
		WorkerPublicURL: func() string { return "https://worker:9090" },
	})

	s.Start(context.Background())
	s.Stop(context.Background())

	// Post-Stop notify must be a no-op.
	require.NoError(t, s.NotifyCacheRefresh(t.Context()))

	// A Stop with near-zero deadline would return immediately either
	// way, but the lifecycle-check contract guarantees no goroutine
	// was spawned.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	s.Stop(ctx)
}

// TestSignaler_StartStopStart — Start after a prior Stop re-installs
// the lifecycle. Required for coordinator promote/demote cycles.
func TestSignaler_StartStopStart(t *testing.T) {
	s := New(Config{
		IsWorker:        func() bool { return true },
		MTLSClient:      func() *http.Client { return nil },
		CoordinatorURL:  func() string { return "" },
		WorkerPublicURL: func() string { return "" },
	})

	for cycle := 0; cycle < 3; cycle++ {
		s.Start(context.Background())
		// NotifyCacheRefresh short-circuits on errCoordNotWired; the
		// point is that it doesn't panic and Stop drains cleanly.
		require.ErrorIs(t, s.NotifyCacheRefresh(t.Context()), errCoordNotWired)
		stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		s.Stop(stopCtx)
		cancel()
	}
}

// TestSignaler_Stop_WaitsForInFlight — Stop blocks until an in-flight
// notify goroutine exits. Uses an httptest server that blocks until
// the test cancels the lifecycle ctx via Stop.
func TestSignaler_Stop_WaitsForInFlight(t *testing.T) {
	release := make(chan struct{})
	requests := atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		select {
		case <-release:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	defer close(release)

	s := New(Config{
		IsWorker:        func() bool { return true },
		MTLSClient:      func() *http.Client { return srv.Client() },
		CoordinatorURL:  func() string { return srv.URL },
		WorkerPublicURL: func() string { return "http://worker:9090" },
	})

	s.Start(context.Background())
	go func() { _ = s.NotifyCacheRefresh(t.Context()) }()

	require.Eventually(t, func() bool {
		return requests.Load() >= 1
	}, time.Second, 5*time.Millisecond, "notify goroutine should have hit the server")

	stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		s.Stop(stopCtx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop hung waiting for notify goroutine")
	}

	assert.NoError(t, stopCtx.Err(), "Stop returned before its own deadline")
}

// TestSignaler_Goodbye_SkipWhenNotWorker — coordinator calls are silent
// no-ops. Validates the early IsWorker gate before attempting any POST.
func TestSignaler_Goodbye_SkipWhenNotWorker(t *testing.T) {
	called := atomic.Int32{}
	s := New(Config{
		IsWorker: func() bool { return false },
		MTLSClient: func() *http.Client {
			called.Add(1)
			return &http.Client{}
		},
		CoordinatorURL:  func() string { return "https://coord:9091" },
		WorkerPublicURL: func() string { return "https://worker:9090" },
	})

	s.SendGoodbye(context.Background())
	if called.Load() != 0 {
		t.Errorf("MTLSClient was called %d times on non-worker; want 0", called.Load())
	}
}

// TestSignaler_Goodbye_SkipWhenWorkerURLEmpty — WorkerPublicURL=""
// short-circuits before the POST. The coord can't resolve an unknown
// peer, so skipping is better than sending a blank node_url.
func TestSignaler_Goodbye_SkipWhenWorkerURLEmpty(t *testing.T) {
	hit := atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := New(Config{
		IsWorker:        func() bool { return true },
		MTLSClient:      func() *http.Client { return srv.Client() },
		CoordinatorURL:  func() string { return srv.URL },
		WorkerPublicURL: func() string { return "" },
	})

	s.SendGoodbye(context.Background())
	if hit.Load() != 0 {
		t.Errorf("coord received %d POSTs with empty workerURL; want 0", hit.Load())
	}
}

// The coordinator identifies refresh notices by certificate; only goodbye needs the worker URL.
func TestSignaler_Notify_OmitsWorkerURL(t *testing.T) {
	const workerURL = "http://192.0.2.10:9090"

	bodies := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 256)
		n, _ := r.Body.Read(buf)
		bodies <- string(buf[:n])
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := New(Config{
		IsWorker:        func() bool { return true },
		MTLSClient:      func() *http.Client { return srv.Client() },
		CoordinatorURL:  func() string { return srv.URL },
		WorkerPublicURL: func() string { return workerURL },
	})

	s.Start(context.Background())
	require.NoError(t, s.NotifyCacheRefresh(t.Context()))

	select {
	case got := <-bodies:
		want := `{}`
		if got != want {
			t.Errorf("notify body = %q, want %q", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("notify goroutine did not hit the server within 1s")
	}

	stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	s.Stop(stopCtx)
}

// TestSignaler_Goodbye_PostsNodeURL — happy path: the coord receives
// a POST with the worker's public URL in the body.
func TestSignaler_Goodbye_PostsNodeURL(t *testing.T) {
	const workerURL = "http://192.0.2.10:9090"

	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		buf := make([]byte, 256)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := New(Config{
		IsWorker:        func() bool { return true },
		MTLSClient:      func() *http.Client { return srv.Client() },
		CoordinatorURL:  func() string { return srv.URL },
		WorkerPublicURL: func() string { return workerURL },
	})

	s.SendGoodbye(context.Background())

	if gotPath != "/zzrouter/v1/internal/goodbye" {
		t.Errorf("POST path = %q, want /zzrouter/v1/internal/goodbye", gotPath)
	}
	if gotBody != `{"node_url":"`+workerURL+`"}` {
		t.Errorf("POST body = %q, want node_url=%q", gotBody, workerURL)
	}
}

type catalogRefreshTransport func(*http.Request) (*http.Response, error)

func (f catalogRefreshTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSignaler_NotifyWaitsForCatalogAcknowledgement(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	returned := make(chan error, 1)
	client := &http.Client{Transport: catalogRefreshTransport(func(r *http.Request) (*http.Response, error) {
		close(entered)
		<-release
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	})}
	s := New(Config{IsWorker: func() bool { return true }, MTLSClient: func() *http.Client { return client }, CoordinatorURL: func() string { return "https://coordinator.test" }})
	s.Start(t.Context())
	t.Cleanup(func() { s.Stop(context.Background()) })
	go func() { returned <- s.NotifyCacheRefresh(t.Context()) }()
	<-entered
	early := false
	select {
	case <-returned:
		early = true
	default:
	}
	close(release)
	if !early {
		require.NoError(t, <-returned)
	}
	require.False(t, early, "notification must wait for publication acknowledgement")
}

func TestSignaler_NotifyRetryBound(t *testing.T) {
	var attempts atomic.Int32
	client := &http.Client{Transport: catalogRefreshTransport(func(r *http.Request) (*http.Response, error) {
		attempts.Add(1)
		deadline, ok := r.Context().Deadline()
		require.True(t, ok)
		require.WithinDuration(t, time.Now().Add(10*time.Second), deadline, time.Second)
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	})}
	s := New(Config{IsWorker: func() bool { return true }, MTLSClient: func() *http.Client { return client }, CoordinatorURL: func() string { return "https://coordinator.test" }})
	s.Start(t.Context())
	t.Cleanup(func() { s.Stop(context.Background()) })
	require.Error(t, s.NotifyCacheRefresh(t.Context()))
	require.EqualValues(t, 3, attempts.Load())
}

func TestSignaler_NotifyCancellation(t *testing.T) {
	for _, lifecycle := range []bool{false, true} {
		t.Run(map[bool]string{false: "caller", true: "lifecycle"}[lifecycle], func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			lifeCtx, stop := context.WithCancel(t.Context())
			defer stop()
			var attempts atomic.Int32
			client := &http.Client{Transport: catalogRefreshTransport(func(r *http.Request) (*http.Response, error) {
				attempts.Add(1)
				if lifecycle {
					stop()
				} else {
					cancel()
				}
				return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(strings.NewReader("{}"))}, nil
			})}
			s := New(Config{IsWorker: func() bool { return true }, MTLSClient: func() *http.Client { return client }, CoordinatorURL: func() string { return "https://coordinator.test" }})
			s.Start(lifeCtx)
			t.Cleanup(func() { s.Stop(context.Background()) })
			require.ErrorIs(t, s.NotifyCacheRefresh(ctx), context.Canceled)
			require.EqualValues(t, 1, attempts.Load())
		})
	}
}

func TestSignalerAsyncAdmissionRacingStop(t *testing.T) {
	checked, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	s := New(Config{IsWorker: func() bool { close(checked); <-release; return true }, MTLSClient: func() *http.Client { calls.Add(1); return nil }})
	s.Start(t.Context())
	go func() { s.NotifyCacheRefreshAsync(t.Context()); close(returned) }()
	<-checked
	stopped := make(chan struct{})
	go func() { s.Stop(t.Context()); close(stopped) }()
	select {
	case <-stopped:
		t.Fatal("shutdown passed admission between its check and Add")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	<-returned
	<-stopped
	before := calls.Load()
	s.NotifyCacheRefreshAsync(t.Context())
	require.Equal(t, before, calls.Load(), "admission after Stop must not add work")
}

func TestSignalerAsyncStalledNotificationHasBoundedStop(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	s := New(Config{IsWorker: func() bool { return true }, MTLSClient: func() *http.Client { close(entered); <-release; return nil }})
	s.Start(t.Context())
	s.NotifyCacheRefreshAsync(t.Context())
	<-entered
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	returned := make(chan struct{})
	go func() { s.Stop(ctx); close(returned) }()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("stalled notification hung Stop")
	}
	close(release)
	s.notifyWG.Wait()
}
