package logs

import (
	utilsclient "github.com/stperic/zzrouter/internal/client/utils"
)

// Run is the local view of a provider run used throughout this
// package and by presentation layers. It is deliberately a thin struct
// (not an alias for utilsclient.Instance) so the transport-layer type
// can evolve without leaking fields into the public logs API.
//
// Field names use the domain vocabulary — Provider rather than App —
// even though the wire type still uses the historical names.
type Run struct {
	ID        string
	Provider  string // inference provider (mlx, vllm, ollama, ...)
	Model     string
	Node      string
	Status    string // "running" | "stopped" | ...
	StartedAt string // RFC3339
}

// runFromInstance narrows a transport Instance to the fields the logs
// feature actually needs. Defined here rather than as a method on
// Instance so the utils package has no knowledge of Run.
func runFromInstance(i utilsclient.Instance) Run {
	return Run{
		ID:        i.ID,
		Provider:  i.App,
		Model:     i.Model,
		Node:      i.Node,
		Status:    i.Status,
		StartedAt: i.StartedAt,
	}
}

// Entry is one log line emitted by a RunSession.
type Entry struct {
	// Seq is a monotonic sequence number within a single session. It
	// starts at 1, increments by 1 per entry, and survives reconnects
	// (the session re-syncs via content-match dedup, so the consumer's
	// Seq counter keeps advancing). Useful for stable sorting and
	// "jump to line N" operations in the viewer.
	Seq int64

	// Text is the raw log line as emitted by the provider process,
	// with the trailing newline stripped.
	Text string
}

// Event is emitted on RunSession.Events(). Exactly one of the concrete
// variants below will be observed per send.
type Event interface {
	isEvent()
}

// EventEntry carries a log line.
type EventEntry struct {
	Entry Entry
}

// EventReset signals that the consumer should clear its buffer. Emitted
// when the session detects log rotation (the sentinel used for dedup
// could not be found in the replayed stream, so history from the new
// stream supersedes what we had).
type EventReset struct{}

// EventDone signals that the stream has ended cleanly. Emitted for
// stopped runs after the initial tail, and when a running run
// transitions to stopped during viewing.
type EventDone struct{}

// EventError carries a non-fatal transport error. The session stays
// alive until Close() is called; the consumer decides whether to
// surface this (TUI toast, CLI stderr) or continue silently.
type EventError struct {
	Err error
}

func (EventEntry) isEvent() {}
func (EventReset) isEvent() {}
func (EventDone) isEvent()  {}
func (EventError) isEvent() {}
