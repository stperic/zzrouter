// Package jobstream is the TUI primitive for subscribing to one or
// more /zzrouter/v1/jobs/:id/stream SSE feeds concurrently. Each active
// subscription is a Row owned by the parent view, keyed by JobID, and
// cancelled on view exit via the tui.Canceller contract.
//
// Design note (see docs/plan_tui_jobstream_phase2.md for the full spec,
// including the pre-code Bubbletea-expert review findings): each Row
// owns a producer goroutine + buffered channel; the view's Update loop
// pulls one FrameMsg per Next cmd and re-schedules. Bubbletea v2 runs
// each Cmd on its own goroutine, so N rows => N parked goroutines on
// N channels, serialized only at the Program's message pump.
package jobstream

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	tea "charm.land/bubbletea/v2"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
)

// chanCap matches the chat-stream precedent (chat_update.go:373). SSE
// peaks ~10 events/sec per job; 64 is ~6s of buffer.
const chanCap = 64

// FrameMsg is delivered to the parent view's Update loop for every SSE
// frame. Views route by JobID to their owning Row. A final FrameMsg
// with Done=true (possibly carrying Err) closes the row.
type FrameMsg struct {
	JobID string
	Event pkgClient.JobEvent
	Err   error
	Done  bool
}

// Row is the per-job state a view owns. Create one via Subscribe and
// store keyed by JobID. Call Cancel on view exit or row removal. Safe
// to call Cancel multiple times and safe on an uninitialised (zero)
// Row (no-op).
type Row struct {
	JobID  string
	Node   string
	Latest pkgClient.JobEvent // last non-terminal frame (empty before first)
	Phase  string             // mirrors JobEvent.Phase; "" before first frame
	Err    error              // set on failure / epoch mismatch / transport error
	Done   bool               // true once a terminal FrameMsg has been consumed

	// Dropped counts events_dropped markers the server emitted. Not a
	// data event — the view MAY render it as "N events missed" if desired.
	// Atomic because the producer goroutine writes it from onEvent;
	// views reading from Update must use Load for a race-free snapshot.
	Dropped atomic.Uint64

	ch     chan tea.Msg
	cancel context.CancelFunc
}

// Subscribe starts a new subscription and returns the Row + initial
// tea.Cmd. The caller MUST:
//  1. store the Row keyed by JobID so Update can look it up on FrameMsg,
//  2. return the returned tea.Cmd from the Update that called Subscribe,
//  3. call Row.Cancel() when removing a row or when the owning view exits.
//
// The Row's context is a child of parent — when parent is cancelled
// (e.g. tui.ViewContext.Cancel on view pop) every Row's producer
// goroutine unblocks on ctx.Done and exits.
func Subscribe(parent context.Context, client *pkgClient.Client, jobID, node string) (*Row, tea.Cmd) {
	ctx, cancel := context.WithCancel(parent)
	r := &Row{
		JobID:  jobID,
		Node:   node,
		ch:     make(chan tea.Msg, chanCap),
		cancel: cancel,
	}

	go func(ch chan<- tea.Msg) {
		defer close(r.ch)
		terminal, err := client.SubscribeJob(ctx, jobID, node, func(ev pkgClient.JobEvent) {
			// Suppress events_dropped markers — counted on the row; views
			// may render the counter if they want.
			if ev.Event == pkgClient.JobEventEventsDropped {
				r.Dropped.Add(1)
				return
			}
			// Terminal events are emitted after SubscribeJob returns
			// (see the final select below); don't double-emit here.
			// Phase wins over event name: if the server misnames a
			// terminal as event:progress, the phase check catches it.
			if ev.Event == pkgClient.JobEventDone || ev.IsTerminal() {
				return
			}
			select {
			case ch <- FrameMsg{JobID: jobID, Event: ev}:
			case <-ctx.Done():
			}
		})

		// Emit exactly one terminal FrameMsg so the view can tidy up.
		// On ctx cancel, skip the emit — the close(ch) defer handles
		// the parked Next cmd, which synthesises the terminal msg.
		if ctx.Err() != nil {
			return
		}
		final := FrameMsg{JobID: jobID, Done: true}
		switch {
		case err != nil:
			final.Err = err
		case terminal != nil:
			final.Event = *terminal
			// A job that failed on the node arrives as an ordinary terminal
			// frame with no transport error, so Err would stay nil and every
			// consumer would read the failure as a success. Carry it.
			if e := terminalError(jobID, *terminal); e != nil {
				final.Err = e
			}
		}
		select {
		case ch <- final:
		case <-ctx.Done():
		}
	}(r.ch)

	return r, Next(r)
}

// terminalError converts a failed terminal event into an error, or returns
// nil when the job finished cleanly.
func terminalError(jobID string, ev pkgClient.JobEvent) error {
	if ev.Err != "" {
		return errors.New(ev.Err)
	}
	if ev.Phase == pkgClient.JobPhaseFailed {
		// Failed with no message is still a failure; saying so beats
		// reporting success because the node was terse.
		return fmt.Errorf("job %s failed", jobID)
	}
	return nil
}

// Next returns the cmd that pulls the next FrameMsg for this row.
// Returns nil once the row is Done so the view can stop re-scheduling.
func Next(r *Row) tea.Cmd {
	if r == nil || r.Done || r.ch == nil {
		return nil
	}
	ch := r.ch
	jobID := r.JobID
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			// Channel closed via Cancel or producer exit; synthesise a
			// terminal so the view can drop the row.
			return FrameMsg{JobID: jobID, Done: true}
		}
		return msg
	}
}

// Cancel tears down the subscription. Safe to call multiple times and
// safe on a zero-value Row. The producer goroutine exits on ctx.Done;
// we spawn a background drainer to unblock any pending channel send
// from the callback so the producer can always terminate.
func (r *Row) Cancel() {
	if r == nil {
		return
	}
	if r.cancel != nil {
		r.cancel()
		r.cancel = nil
	}
	if r.ch != nil {
		ch := r.ch
		// Drain in a goroutine — producer will close after ctx.Done
		// propagates, at which point range exits cleanly.
		go func() {
			for range ch {
			}
		}()
	}
}
