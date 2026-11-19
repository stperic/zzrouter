// Package audit defines the records of operator- and API-driven changes to
// access control state (keys, teams, people, quotas). The access store is
// their one writer and reader; this package holds only the event shape.
//
//   - Events are immutable once recorded.
//   - No PII beyond actor and subject identifiers. Never record secrets (key
//     material, plaintext credentials) in an event or its metadata.
package audit

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/stperic/zzrouter/pkg/utils"
)

// ErrCursor refuses a page cursor the history did not issue for its current
// generation: the reader starts again without one.
var ErrCursor = errors.New("audit history changed; refresh to start a new page")

// Action is the identifier for an audited mutation. Kept as a typed
// string so consumers can switch on known values without importing an
// enum; new actions are added by constant here.
type Action string

// Action constants, grouped by subject. Additive-only: a released action
// constant must not be renamed or repurposed.
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

	ActionPersonCreated   Action = "person.created"
	ActionPersonUpdated   Action = "person.updated"
	ActionPasswordChanged Action = "person.password_changed"
	ActionPersonRecovered Action = "person.recovered"
	ActionSessionsRevoked Action = "person.authority_revoked"
	ActionGrantIssued     Action = "enrollment.issued"
	ActionGrantRevoked    Action = "enrollment.revoked"
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
	// The store records an unresolved actor as "unknown" (Attribute).
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

// Validate enforces the event-shape contract. Recording an invalid
// event would pollute the audit log with records a compliance review
// cannot trust.
func (ev Event) Validate() error {
	if ev.Action == "" {
		return fmt.Errorf("audit: Action is required")
	}
	if ev.Actor == "" {
		return fmt.Errorf("audit: Actor is required")
	}
	if ev.Subject == "" {
		return fmt.Errorf("audit: Subject is required")
	}
	if _, err := json.Marshal(ev.Metadata); err != nil {
		return fmt.Errorf("audit: metadata does not encode as JSON: %w", err)
	}
	if !utils.ValidUTF8(ev.Metadata) {
		return fmt.Errorf("audit: metadata must be valid UTF-8")
	}
	return nil
}
