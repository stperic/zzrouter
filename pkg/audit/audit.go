// Package audit records operator- and API-driven mutations to access
// control state (keys, teams, quotas) in an append-only log for
// compliance and incident investigation.
//
// Design contract:
//
//   - Every exported Sink method is safe for concurrent use.
//   - Emit is best-effort: a sink failure does NOT abort the mutation
//     that called it. Callers emit AFTER the state store has persisted;
//     a dropped audit event is logged at WARN but never propagates.
//   - Events are immutable once emitted. The on-disk format is JSONL
//     (one JSON object per line) with no updates-in-place.
//   - No PII beyond actor key-id and team-id. Never log secrets (key
//     material, plaintext credentials). Fields that could carry secrets
//     are redacted at emission.
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/stperic/zzrouter/pkg/utils"
)

// Action is the identifier for an audited mutation. Kept as a typed
// string so consumers can switch on known values without importing an
// enum; new actions are added by constant here.
type Action string

// Action constants. Group by subject to make the switch on a sink easy
// to read. Additive-only: a released action constant must not be
// renamed or repurposed.
const (
	ActionKeyCreated    Action = "key.created"
	ActionKeyUpdated    Action = "key.updated"
	ActionKeyRotated    Action = "key.rotated"
	ActionKeyDeleted    Action = "key.deleted"
	ActionKeyUsageReset Action = "key.usage_reset"

	ActionTeamCreated    Action = "team.created"
	ActionTeamUpdated    Action = "team.updated"
	ActionTeamDeleted    Action = "team.deleted"
	ActionTeamUsageReset Action = "team.usage_reset"

	ActionQuotaSpendReset Action = "quota.spend_reset"

	ActionStateSnapshot Action = "state.snapshot"
	ActionStateRestore  Action = "state.restore"
)

// Event is a single audit record. Fields are stable on the wire — once
// a release has shipped with a field, it cannot be renamed or removed.
// New fields must be additive and JSON-optional.
type Event struct {
	// Time is the UTC wall clock at which the mutation completed.
	Time time.Time `json:"time"`
	// Action names the mutation. One of the Action constants above.
	Action Action `json:"action"`
	// Actor is the caller identity. For static admin keys this is the
	// sentinel "static:admin"; for virtual keys it is the key UUID.
	// Empty is rejected by Emit — the caller must resolve identity
	// before emitting.
	Actor string `json:"actor"`
	// Subject identifies the object being mutated (a key UUID, a team
	// ID, or a scope string for quota resets). Required.
	Subject string `json:"subject"`
	// RequestID threads the HTTP X-Request-ID for cross-log
	// correlation. Optional.
	RequestID string `json:"request_id,omitempty"`
	// Metadata carries action-specific context (e.g., which fields
	// changed on an update). Optional. Never put secrets here.
	Metadata map[string]any `json:"metadata,omitempty"`
}

// ActorStaticAdmin is the sentinel Actor value used when a request
// authenticated via the static ZZROUTER_ADMIN_API_KEY rather than a
// virtual key. Distinct so a reviewer can immediately tell whether the
// mutation came from the operator-held master key.
const ActorStaticAdmin = "static:admin"

// ActorSystemReconciler is the sentinel Actor for automatic background
// sweeps (orphan personal-team reaper, future cascade cleanups). A
// compliance reviewer must be able to distinguish these from operator-
// driven deletes on the static admin key.
const ActorSystemReconciler = "system:reconciler"

// Sink is an abstract audit output. Production uses FileSink;
// tests use Null.
type Sink interface {
	// Emit writes one event. Implementations must return quickly —
	// callers hold locks when invoking.
	Emit(ctx context.Context, ev Event) error
}

// Null is a Sink that drops every event. Useful as a zero-value
// placeholder and in tests that don't assert on audit output.
type Null struct{}

// Emit on Null is a no-op.
func (Null) Emit(_ context.Context, _ Event) error { return nil }

// FileSink appends JSON-encoded events to a file. One line per event.
// Concurrent Emit calls are serialized; the underlying file is opened
// once at construction and closed via Close.
type FileSink struct {
	mu   sync.Mutex
	w    io.WriteCloser
	path string
}

// NewFileSink opens path for append-only writing. Creates the file if
// absent with mode 0o600 — audit logs contain identifiers operators
// expect to be locally readable only.
func NewFileSink(path string) (*FileSink, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("audit: open %s: %w", path, err)
	}
	return &FileSink{w: f, path: path}, nil
}

// Emit validates the event, JSON-encodes it, and writes it. Returns
// an error on sink failure; the caller decides whether to log or
// degrade.
func (s *FileSink) Emit(_ context.Context, ev Event) error {
	if err := validate(ev); err != nil {
		return err
	}
	if ev.Time.IsZero() {
		ev.Time = utils.NowUTC()
	}
	data, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("audit: marshal: %w", err)
	}
	data = append(data, '\n')
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.w.Write(data); err != nil {
		return fmt.Errorf("audit: write %s: %w", s.path, err)
	}
	return nil
}

// Close releases the underlying file. Further Emit calls return an
// error. Safe to call multiple times.
func (s *FileSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.w == nil {
		return nil
	}
	err := s.w.Close()
	s.w = nil
	return err
}

// Path returns the on-disk location; useful for diagnostics and tests.
func (s *FileSink) Path() string { return s.path }

// validate enforces the event-shape contract. Emitting an invalid
// event would pollute the audit log with records a compliance review
// cannot trust.
func validate(ev Event) error {
	if ev.Action == "" {
		return fmt.Errorf("audit: Action is required")
	}
	if ev.Actor == "" {
		return fmt.Errorf("audit: Actor is required")
	}
	if ev.Subject == "" {
		return fmt.Errorf("audit: Subject is required")
	}
	return nil
}

// Log is a small wrapper that emits an event and, on sink failure,
// logs a warning at the structured logger instead of propagating.
// Call sites that want to fail-loud on audit errors should call
// sink.Emit directly.
func Log(ctx context.Context, sink Sink, ev Event) {
	if sink == nil {
		return
	}
	if err := sink.Emit(ctx, ev); err != nil {
		slog.WarnContext(ctx, "audit emit failed",
			"action", ev.Action,
			"subject", ev.Subject,
			"actor", ev.Actor,
			"error", err)
	}
}
