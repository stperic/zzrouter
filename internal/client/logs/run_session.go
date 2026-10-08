package logs

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	utilsclient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/apipath"
)

// sessionEventChanSize is the buffered capacity of the Events()
// channel. A generous buffer prevents the SSE reader from blocking the
// network layer if the consumer is temporarily slow (e.g., Bubbletea
// processing a redraw).
const sessionEventChanSize = 256

// reconnectBackoff is the delay between auto-reconnects after the
// server closes the SSE stream on its own (typically the cluster
// timeout). Kept small because the server end is cooperative.
const reconnectBackoff = 500 * time.Millisecond

// rotationSkipSlack is the number of extra events (beyond the tail's
// TotalLines) we will discard while searching for the dedup sentinel
// before deciding the log file rotated and emitting EventReset.
//
// The constant accounts for two windows: (1) the race between
// FetchTail returning and SSE opening, during which the provider may
// emit additional lines that pad the replayed stream beyond what the
// tail knew about, and (2) normal startup noise on chatty providers
// (vLLM, llama.cpp) that can produce dozens of lines per second.
//
// The effective slack is max(rotationSkipSlack, TotalLines/4) so busy
// logs get a proportional budget rather than a fixed one.
const rotationSkipSlack = 256

// RunSession drives the event stream for a single run. Construction
// picks one of two strategies based on the run's current Status:
//
//   - Running: fetch an initial tail, then open an SSE stream. The
//     server replays from byte 0, so we discard replayed events until
//     we find a sentinel matching the tail's last line. Any subsequent
//     events are appended. On SSE close (server timeout) we
//     auto-reconnect.
//
//   - Not running: fetch a single tail, emit entries, emit EventDone,
//     stop.
//
// The session owns one goroutine. Close() cancels its context and
// waits for the goroutine to exit, guaranteeing no leaks.
type RunSession struct {
	client *utilsclient.Client
	run    Run
	events chan Event

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// running is captured at construction time. The session does not
	// re-query the server on reconnect — if the run dies during a
	// stream, the SSE endpoint will close cleanly and we surface that
	// as EventDone.
	running bool

	// seq is the monotonic sequence counter emitted on Entry.Seq.
	// Updated only by the run goroutine.
	seq int64
}

// NewRunSession fetches run metadata, starts the background reader,
// and returns immediately. Events are consumed via Events(). Callers
// must call Close() when done.
//
// The caller's context is honoured for the lifetime of the session:
// cancelling ctx has the same effect as calling Close(), which is
// important for callers that already manage a request-scoped context.
//
// If the run does not exist, NewRunSession returns ErrRunNotFound
// without starting any goroutines.
func NewRunSession(ctx context.Context, c *utilsclient.Client, runID string) (*RunSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	run, err := GetRun(ctx, c, runID)
	if err != nil {
		return nil, err
	}

	// Derive the session context from the caller's ctx so parent
	// cancellation propagates. The returned cancel stops the session
	// explicitly when Close() is called.
	sessCtx, cancel := context.WithCancel(ctx)
	s := &RunSession{
		client:  c,
		run:     run,
		events:  make(chan Event, sessionEventChanSize),
		ctx:     sessCtx,
		cancel:  cancel,
		running: strings.EqualFold(run.Status, "running"),
	}

	s.wg.Add(1)
	go s.runLoop()
	return s, nil
}

// Run returns the Run metadata captured at construction time. The
// values do not auto-refresh.
func (s *RunSession) Run() Run { return s.run }

// Events returns the event channel. The channel is closed when the
// session's goroutine exits — either because Close() was called or
// because a non-running run finished its tail. Consumers can safely
// range over it.
func (s *RunSession) Events() <-chan Event { return s.events }

// Close cancels the session and waits for the background goroutine to
// exit. It is safe to call more than once.
func (s *RunSession) Close() {
	s.cancel()
	s.wg.Wait()
}

// runLoop is the single goroutine. It drives the whole session
// lifecycle and is the only writer on s.events. It always closes
// s.events on exit.
func (s *RunSession) runLoop() {
	defer s.wg.Done()
	defer close(s.events)

	// Step 1: initial tail fetch. Needed for both running and
	// non-running paths; for running runs it provides the dedup
	// sentinel.
	tail, err := FetchTail(s.ctx, s.client, s.run.ID, DefaultTailLines)
	tailFailed := err != nil
	if tailFailed {
		s.sendError(fmt.Errorf("fetch tail: %w", err))
		// For a stopped run, failing to fetch the tail is terminal.
		// For a running run, we still try to stream — the user will
		// see the error but get live logs.
		if !s.running {
			s.sendDone()
			return
		}
		// Running run: tell the consumer to clear any partial state,
		// because the upcoming SSE replay is now the authoritative
		// history — there's no sentinel to dedup against.
		if !s.sendReset() {
			return
		}
	}

	// Emit the tail entries immediately, whether or not we'll follow.
	for _, line := range tail.Lines {
		if !s.emitEntry(line) {
			return
		}
	}

	if !s.running {
		s.sendDone()
		return
	}

	// Step 2: follow via SSE. Loop handles auto-reconnect after the
	// server times out the stream. Dedup sentinel is the last tail
	// line; on reconnect after we've been catching up, it becomes the
	// last emitted line.
	sentinel := tail.LastLine
	if tailFailed {
		// We have no credible sentinel — accept everything the server
		// replays from byte 0. The EventReset above told the consumer
		// to start fresh.
		sentinel = ""
	}
	// Initial skip budget: total lines we expect to see replayed, plus
	// a proportional slack for new lines the provider emits between
	// FetchTail and SSE open. Busy startups can add hundreds of lines
	// in that window — a fixed 50-line slack was too small.
	slack := rotationSkipSlack
	if proportional := tail.TotalLines / 4; proportional > slack {
		slack = proportional
	}
	maxSkip := tail.TotalLines + slack

	for {
		nextSentinel, err := s.streamOnce(sentinel, maxSkip)
		if s.ctx.Err() != nil {
			return
		}
		if err != nil {
			s.sendError(err)
			// Brief pause before retrying, unless cancelled.
			select {
			case <-s.ctx.Done():
				return
			case <-time.After(reconnectBackoff):
			}
			continue
		}

		// Clean close (server EOF). Advance sentinel and reconnect.
		sentinel = nextSentinel
		// After the first successful sync, we no longer need the tail
		// slack — reconnects should catch up fast.
		maxSkip = rotationSkipSlack
		select {
		case <-s.ctx.Done():
			return
		case <-time.After(reconnectBackoff):
		}
	}
}

// streamOnce opens one SSE stream and processes frames until the
// connection closes or errors. Returns the new sentinel (the last
// line emitted during this connection, or the prior sentinel if we
// never caught up) and a non-nil error if the transport failed.
//
// A clean EOF is not an error: the caller's loop will reconnect.
func (s *RunSession) streamOnce(sentinel string, maxSkip int) (string, error) {
	path := apipath.RunLogs(s.run.ID) + "?follow=true"

	resp, err := s.client.DoStreamingRequest(s.ctx, "GET", path)
	if err != nil {
		return sentinel, fmt.Errorf("open sse: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return sentinel, fmt.Errorf("sse status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	// Run the parser in its own goroutine. When s.ctx is cancelled we
	// close the body to unblock it; ReadSSE returns nil on cancelled
	// reads. ReadSSE always closes `frames` on exit, so we detect
	// parser shutdown by the frames channel closing.
	frames := make(chan SSEFrame, 64)
	parserErr := make(chan error, 1)
	go func() {
		parserErr <- ReadSSE(s.ctx, resp.Body, frames)
	}()

	// Cleanup: close the body to unblock the parser if it's still
	// reading, then wait for it to finish so we don't leak goroutines.
	// The wait is safe because parserErr is buffered with capacity 1
	// and the parser writes to it exactly once before returning.
	// sync.Once guards against any future refactor that could invoke
	// waitParser concurrently; the body close is intentionally
	// idempotent under the stdlib.
	var (
		parserFinal error
		parserOnce  sync.Once
	)
	waitParser := func() {
		parserOnce.Do(func() {
			_ = resp.Body.Close()
			parserFinal = <-parserErr
		})
	}
	defer waitParser()

	caughtUp := sentinel == "" // no sentinel → nothing to skip
	skipped := 0
	lastEmitted := sentinel

	for {
		select {
		case <-s.ctx.Done():
			return lastEmitted, nil
		case f, ok := <-frames:
			if !ok {
				// Parser finished on its own (EOF or error). Collect
				// its result and return.
				waitParser()
				return lastEmitted, parserFinal
			}
			// Only "log" events carry data we care about. "done" and
			// "error" are terminal-ish; handle them explicitly.
			switch f.Event {
			case "", "log":
				line := f.Data
				if !caughtUp {
					if line == sentinel {
						caughtUp = true
						continue
					}
					skipped++
					if skipped > maxSkip {
						// Rotation fallback: sentinel not found in the
						// replayed stream. Tell the consumer to drop
						// its buffer and start accepting lines.
						if !s.sendReset() {
							return lastEmitted, nil
						}
						caughtUp = true
						// Fall through and emit this line — it's
						// genuinely new content under the new regime.
					} else {
						continue
					}
				}
				if !s.emitEntry(line) {
					return lastEmitted, nil
				}
				lastEmitted = line
			case "done":
				// Server closed the stream cleanly. Return and let
				// the outer loop decide whether to reconnect.
				return lastEmitted, nil
			case "error":
				// Non-fatal: surface to consumer, keep reading.
				s.sendError(fmt.Errorf("server: %s", f.Data))
			}
		}
	}
}

// emitEntry sends an EventEntry and returns false if the session was
// cancelled mid-send (caller should exit promptly).
func (s *RunSession) emitEntry(text string) bool {
	s.seq++
	select {
	case <-s.ctx.Done():
		return false
	case s.events <- EventEntry{Entry: Entry{Seq: s.seq, Text: text}}:
		return true
	}
}

// sendReset emits EventReset. Returns false on cancellation.
func (s *RunSession) sendReset() bool {
	// A reset invalidates prior Seq ordering semantics for consumers
	// that care, but we keep the counter monotonic across resets so
	// the channel stays totally ordered.
	select {
	case <-s.ctx.Done():
		return false
	case s.events <- EventReset{}:
		return true
	}
}

// sendError emits EventError. Never blocks on cancellation.
func (s *RunSession) sendError(err error) {
	select {
	case <-s.ctx.Done():
	case s.events <- EventError{Err: err}:
	}
}

// sendDone emits EventDone. Never blocks on cancellation.
func (s *RunSession) sendDone() {
	select {
	case <-s.ctx.Done():
	case s.events <- EventDone{}:
	}
}
