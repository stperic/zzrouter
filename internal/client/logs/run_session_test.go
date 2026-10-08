package logs

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	utilsclient "github.com/stperic/zzrouter/internal/client/utils"
)

// sessionTestServer is a shared test harness. It serves:
//
//   - GET /zzrouter/v1/runs        → single-run list
//   - GET /zzrouter/v1/runs/:id/logs (lines=N)   → tail JSON
//   - GET /zzrouter/v1/runs/:id/logs (follow=true) → SSE stream
//
// The SSE stream is driven by the `stream` channel so tests can push
// frames on their own schedule and end the stream with close(stream).
type sessionTestServer struct {
	run utilsclient.Instance

	mu         sync.Mutex
	tailLines  []string // static tail response
	totalLines int

	// tailFails causes the tail handler to return 500 instead of a
	// normal JSON response. Used to exercise the "running run, tail
	// fetch failed → EventReset then SSE authoritative" branch.
	tailFails bool

	// streamChans is a slice because streamOnce reconnects; each
	// reconnect pops the next channel off the slice.
	streamChans [][]streamFrame
	streamIdx   int

	// openCount counts how many times the SSE endpoint was hit.
	// Tests use this to verify reconnect behavior.
	openCount int
}

type streamFrame struct {
	event string
	data  string
}

func (s *sessionTestServer) setTail(lines []string, total int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tailLines = append([]string(nil), lines...)
	s.totalLines = total
}

func (s *sessionTestServer) addStream(frames []streamFrame) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.streamChans = append(s.streamChans, frames)
}

func (s *sessionTestServer) nextStream() ([]streamFrame, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.streamIdx >= len(s.streamChans) {
		return nil, false
	}
	f := s.streamChans[s.streamIdx]
	s.streamIdx++
	s.openCount++
	return f, true
}

func (s *sessionTestServer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/zzrouter/v1/runs":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []utilsclient.Instance{s.run},
			})
		case strings.HasSuffix(r.URL.Path, "/logs"):
			if r.URL.Query().Get("follow") == "true" {
				s.serveSSE(w, r)
			} else {
				s.serveTail(w, r)
			}
		default:
			http.NotFound(w, r)
		}
	}
}

func (s *sessionTestServer) serveTail(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	fails := s.tailFails
	lines := append([]string(nil), s.tailLines...)
	total := s.totalLines
	s.mu.Unlock()
	if fails {
		http.Error(w, "simulated tail failure", http.StatusInternalServerError)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"success": true,
		"data": map[string]any{
			"lines":       lines,
			"total_lines": total,
			"count":       len(lines),
		},
	})
}

// currentOpenCount returns the number of SSE connections made so far.
// Used by tests that verify reconnect behavior.
func (s *sessionTestServer) currentOpenCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.openCount
}

func (s *sessionTestServer) serveSSE(w http.ResponseWriter, r *http.Request) {
	frames, ok := s.nextStream()
	if !ok {
		// No more scripted streams; hold the connection open until
		// the client disconnects, to simulate a quiet live stream.
		<-r.Context().Done()
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, _ := w.(http.Flusher)
	for _, f := range frames {
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", f.event, f.data)
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// drainEntries collects events until it has gathered `want` entries or
// the context expires. Non-entry events are ignored unless they are
// EventDone, which terminates the drain early.
func drainEntries(t *testing.T, s *RunSession, want int, timeout time.Duration) []Entry {
	t.Helper()
	var got []Entry
	deadline := time.After(timeout)
	for len(got) < want {
		select {
		case ev, ok := <-s.Events():
			if !ok {
				return got
			}
			switch e := ev.(type) {
			case EventEntry:
				got = append(got, e.Entry)
			case EventDone:
				return got
			case EventError:
				t.Logf("EventError during drain: %v", e.Err)
			}
		case <-deadline:
			return got
		}
	}
	return got
}

func TestRunSession_StoppedRunEmitsTailThenDone(t *testing.T) {
	srv := &sessionTestServer{
		run: utilsclient.Instance{ID: "r_1", App: "mlx", Status: "stopped"},
	}
	srv.setTail([]string{"a", "b", "c"}, 3)
	c, _ := newTestClient(t, srv.handler())

	sess, err := NewRunSession(context.Background(), c, "r_1")
	if err != nil {
		t.Fatalf("NewRunSession: %v", err)
	}
	defer sess.Close()

	got := drainEntries(t, sess, 3, 2*time.Second)
	if len(got) != 3 {
		t.Fatalf("want 3 entries, got %d", len(got))
	}
	for i, want := range []string{"a", "b", "c"} {
		if got[i].Text != want {
			t.Errorf("entry %d: want %q got %q", i, want, got[i].Text)
		}
		if got[i].Seq != int64(i+1) {
			t.Errorf("entry %d: seq want %d got %d", i, i+1, got[i].Seq)
		}
	}

	// Channel must close after EventDone — consume remaining events.
	for ev := range sess.Events() {
		_ = ev
	}
}

func TestRunSession_RunningRunSkipsReplayedTailLines(t *testing.T) {
	srv := &sessionTestServer{
		run: utilsclient.Instance{ID: "r_1", App: "mlx", Status: "running"},
	}
	// Tail says total=3, last line "c". SSE replays "a", "b", "c",
	// then delivers the genuinely new line "d". Session should skip
	// the first three (they match / precede the sentinel) and emit
	// "a","b","c" from the tail fetch, then "d" from the stream.
	srv.setTail([]string{"a", "b", "c"}, 3)
	srv.addStream([]streamFrame{
		{event: "log", data: "a"},
		{event: "log", data: "b"},
		{event: "log", data: "c"},
		{event: "log", data: "d"},
	})
	c, _ := newTestClient(t, srv.handler())

	sess, err := NewRunSession(context.Background(), c, "r_1")
	if err != nil {
		t.Fatalf("NewRunSession: %v", err)
	}
	defer sess.Close()

	got := drainEntries(t, sess, 4, 2*time.Second)
	if len(got) != 4 {
		t.Fatalf("want 4 entries, got %d (%+v)", len(got), got)
	}
	wantTexts := []string{"a", "b", "c", "d"}
	for i, w := range wantTexts {
		if got[i].Text != w {
			t.Errorf("entry %d: want %q got %q", i, w, got[i].Text)
		}
	}
}

func TestRunSession_RotationFallbackEmitsReset(t *testing.T) {
	srv := &sessionTestServer{
		run: utilsclient.Instance{ID: "r_1", App: "mlx", Status: "running"},
	}
	// Tail's last line is "c". Stream does NOT contain "c" — simulate
	// log rotation. After the skip budget is exhausted the session
	// should emit EventReset and start accepting new content.
	//
	// Skip budget = totalLines + max(rotationSkipSlack, totalLines/4).
	// With totalLines=3 that is 3 + 256 = 259. We push 260 mismatched
	// events so event 260 triggers the reset and becomes the first
	// entry under the new regime.
	srv.setTail([]string{"a", "b", "c"}, 3)
	const mismatchedCount = 260
	var frames []streamFrame
	for i := 0; i < mismatchedCount; i++ {
		frames = append(frames, streamFrame{event: "log", data: fmt.Sprintf("new-%d", i)})
	}
	srv.addStream(frames)
	c, _ := newTestClient(t, srv.handler())

	sess, err := NewRunSession(context.Background(), c, "r_1")
	if err != nil {
		t.Fatalf("NewRunSession: %v", err)
	}
	defer sess.Close()

	var sawReset bool
	var postResetEntries int
	var tailEntries int
	deadline := time.After(2 * time.Second)
loop:
	for {
		select {
		case ev, ok := <-sess.Events():
			if !ok {
				break loop
			}
			switch ev.(type) {
			case EventEntry:
				if sawReset {
					postResetEntries++
					if postResetEntries >= 1 {
						break loop
					}
				} else {
					tailEntries++
				}
			case EventReset:
				sawReset = true
			}
		case <-deadline:
			break loop
		}
	}

	if tailEntries != 3 {
		t.Errorf("want 3 pre-reset tail entries, got %d", tailEntries)
	}
	if !sawReset {
		t.Errorf("want EventReset, got none")
	}
	if postResetEntries < 1 {
		t.Errorf("want at least 1 post-reset entry, got %d", postResetEntries)
	}
}

func TestRunSession_CloseTerminatesPromptlyAndClosesChannel(t *testing.T) {
	srv := &sessionTestServer{
		run: utilsclient.Instance{ID: "r_1", App: "mlx", Status: "running"},
	}
	srv.setTail([]string{"a"}, 1)
	// No scripted streams; the SSE handler blocks on r.Context().Done()
	// so we exercise the cancel-during-stream path.
	c, _ := newTestClient(t, srv.handler())

	sess, err := NewRunSession(context.Background(), c, "r_1")
	if err != nil {
		t.Fatalf("NewRunSession: %v", err)
	}

	// Let the session pull the tail and connect.
	time.Sleep(100 * time.Millisecond)

	// Close must return promptly (waits for the run goroutine to
	// exit). If the goroutine were leaked, Close would block forever.
	done := make(chan struct{})
	go func() {
		sess.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close() did not return within 2s — goroutine leak")
	}

	// Events channel must be closed — range must terminate quickly.
	drained := make(chan struct{})
	go func() {
		for range sess.Events() {
		}
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Events() channel did not close after Close()")
	}
}

func TestRunSession_RunNotFound(t *testing.T) {
	c, _ := newTestClient(t, fakeRuns([]utilsclient.Instance{
		{ID: "r_other", App: "mlx"},
	}))

	_, err := NewRunSession(context.Background(), c, "r_missing")
	if err == nil {
		t.Fatal("want error for missing run")
	}
}

// TestRunSession_AutoReconnectAfterDone drives two scripted streams.
// The first ends with event "done", which causes streamOnce to return
// cleanly; the outer loop must wait reconnectBackoff, open a second
// connection, and resume. On the second connection the replay must
// dedup against the new sentinel (the last line emitted), which lets
// genuinely new lines land in the ring.
func TestRunSession_AutoReconnectAfterDone(t *testing.T) {
	srv := &sessionTestServer{
		run: utilsclient.Instance{ID: "r_1", App: "mlx", Status: "running"},
	}
	srv.setTail([]string{"a"}, 1)
	// Stream 1: replays "a" (matches sentinel, skipped) then done.
	srv.addStream([]streamFrame{
		{event: "log", data: "a"},
		{event: "done", data: ""},
	})
	// Stream 2: replays "a" (matches new sentinel), then the new "b".
	srv.addStream([]streamFrame{
		{event: "log", data: "a"},
		{event: "log", data: "b"},
	})
	c, _ := newTestClient(t, srv.handler())

	sess, err := NewRunSession(context.Background(), c, "r_1")
	if err != nil {
		t.Fatalf("NewRunSession: %v", err)
	}
	defer sess.Close()

	// Expect: tail "a" (pre-stream), then reconnect, then "b".
	got := drainEntries(t, sess, 2, 3*time.Second)
	if len(got) != 2 {
		t.Fatalf("want 2 entries, got %d (%+v)", len(got), got)
	}
	if got[0].Text != "a" || got[1].Text != "b" {
		t.Errorf("entries: want [a b], got [%q %q]", got[0].Text, got[1].Text)
	}
	if n := srv.currentOpenCount(); n < 2 {
		t.Errorf("want at least 2 SSE connections, got %d", n)
	}
}

// TestRunSession_TailFailureOnRunningRunEmitsResetThenStream covers
// the "running run, tail fetch failed" branch: the session surfaces
// the error, emits EventReset (so the consumer discards partial
// state), then accepts every replayed SSE frame as authoritative
// (sentinel is empty, so caughtUp starts true).
func TestRunSession_TailFailureOnRunningRunEmitsResetThenStream(t *testing.T) {
	srv := &sessionTestServer{
		run: utilsclient.Instance{ID: "r_1", App: "mlx", Status: "running"},
	}
	srv.tailFails = true
	srv.addStream([]streamFrame{
		{event: "log", data: "x"},
		{event: "log", data: "y"},
	})
	c, _ := newTestClient(t, srv.handler())

	sess, err := NewRunSession(context.Background(), c, "r_1")
	if err != nil {
		t.Fatalf("NewRunSession: %v", err)
	}
	defer sess.Close()

	var sawError, sawReset bool
	var entries []string
	deadline := time.After(2 * time.Second)
loop:
	for {
		select {
		case ev, ok := <-sess.Events():
			if !ok {
				break loop
			}
			switch e := ev.(type) {
			case EventError:
				sawError = true
			case EventReset:
				sawReset = true
			case EventEntry:
				entries = append(entries, e.Entry.Text)
				if len(entries) >= 2 {
					break loop
				}
			}
		case <-deadline:
			break loop
		}
	}

	if !sawError {
		t.Errorf("want EventError for tail failure, got none")
	}
	if !sawReset {
		t.Errorf("want EventReset after tail failure, got none")
	}
	if len(entries) != 2 || entries[0] != "x" || entries[1] != "y" {
		t.Errorf("want entries [x y], got %v", entries)
	}
}

// TestRunSession_PreCancelledContext verifies that a ctx already in
// the cancelled state short-circuits NewRunSession before any HTTP
// round-trip or goroutine spawn.
func TestRunSession_PreCancelledContext(t *testing.T) {
	srv := &sessionTestServer{
		run: utilsclient.Instance{ID: "r_1", App: "mlx", Status: "running"},
	}
	srv.setTail([]string{"a"}, 1)
	c, _ := newTestClient(t, srv.handler())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	sess, err := NewRunSession(ctx, c, "r_1")
	if sess != nil {
		t.Errorf("want nil session on cancelled ctx, got %+v", sess)
		sess.Close()
	}
	if err != context.Canceled {
		t.Errorf("want context.Canceled, got %v", err)
	}
}

// TestRunSession_ParentContextCancelPropagates verifies that
// cancelling the caller's context — without calling sess.Close() —
// causes the session goroutine to exit and the events channel to
// close. This is the "request-scoped ctx" contract.
func TestRunSession_ParentContextCancelPropagates(t *testing.T) {
	srv := &sessionTestServer{
		run: utilsclient.Instance{ID: "r_1", App: "mlx", Status: "running"},
	}
	srv.setTail([]string{"a"}, 1)
	// No scripted streams — SSE handler blocks until client goes away.
	c, _ := newTestClient(t, srv.handler())

	ctx, cancel := context.WithCancel(context.Background())
	sess, err := NewRunSession(ctx, c, "r_1")
	if err != nil {
		t.Fatalf("NewRunSession: %v", err)
	}
	// Consume the initial tail entry so we know the goroutine is live.
	select {
	case ev := <-sess.Events():
		if _, ok := ev.(EventEntry); !ok {
			t.Fatalf("first event: want EventEntry, got %T", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("never received initial tail entry")
	}

	// Cancelling the parent must drain the channel without sess.Close().
	cancel()
	drained := make(chan struct{})
	go func() {
		for range sess.Events() {
		}
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(2 * time.Second):
		t.Fatal("Events() channel did not close after parent ctx cancel")
	}
	// Still safe to call Close() after — should be a no-op.
	sess.Close()
}
