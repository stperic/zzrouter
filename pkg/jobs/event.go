package jobs

import (
	"errors"
	"time"
)

// Phase is the lifecycle state of a job.
type Phase string

const (
	PhasePending Phase = "pending"
	PhaseRunning Phase = "running"
	PhaseDone    Phase = "done"
	PhaseFailed  Phase = "failed"
)

// IsTerminal reports whether the phase is a final state.
func (p Phase) IsTerminal() bool {
	return p == PhaseDone || p == PhaseFailed
}

// Bytes reports data-transfer progress. Both zero means "don't render
// a bytes field."
type Bytes struct {
	Done  int64 `json:"done,omitempty"`
	Total int64 `json:"total,omitempty"`
}

// Meta is the kind-specific sidecar payload. Typed via the kind's godoc
// (e.g. download: {model, format}), not a union — keeps producers
// friendly at the cost of compile-time shape checking.
type Meta map[string]any

// Event is the wire envelope emitted for every progress tick, phase
// change, or stream-lifecycle signal. Serialized as JSON to SSE data
// frames.
type Event struct {
	// ExpiresAt is the terminal job's read deadline; later hand-outs can move it.
	ExpiresAt time.Time `json:"expires_at,omitzero"`
	JobID     string    `json:"job_id"`
	Node      string    `json:"node"`
	Kind      Kind      `json:"kind"`
	Epoch     string    `json:"epoch"`
	Seq       uint64    `json:"seq"`
	At        time.Time `json:"at"`
	Phase     Phase     `json:"phase"`
	Percent   int       `json:"percent,omitempty"`
	Step      string    `json:"step,omitempty"`
	Bytes     *Bytes    `json:"bytes,omitempty"`
	Meta      Meta      `json:"meta,omitempty"`
	Err       string    `json:"err,omitempty"`

	Warning string `json:"warning,omitempty"`

	// Dropped is set on the single events_dropped marker the ring emits
	// on Bounded overflow. Carries {since, current} so subscribers can
	// know the gap size. Normal events leave it nil.
	Dropped *DroppedMarker `json:"dropped,omitempty"`
}

// Validate checks the required wire identity and lifecycle phase; kind is extensible.
func (e Event) Validate() error {
	if e.JobID == "" || e.Node == "" || e.Kind == "" ||
		(e.Phase != PhasePending && e.Phase != PhaseRunning && !e.Phase.IsTerminal()) {
		return errors.New("invalid job event identity or phase")
	}
	return nil
}

// DroppedMarker is the events_dropped payload. Subscribers that see a
// non-nil Dropped should treat their historical view as incomplete
// between Since and Current and resnapshot if they need strict ordering.
type DroppedMarker struct {
	Since   uint64 `json:"since"`
	Current uint64 `json:"current"`
}
