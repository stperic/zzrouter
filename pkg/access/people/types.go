// Package people owns local human identities and their one-time enrollment grants.
// Team policy and browser sessions remain with their existing authorities.
package people

import (
	"context"
	"errors"
	"time"
)

// Domain refusals do not contain submitted passwords or bearer tokens.
var (
	ErrInvalidCredentials = errors.New("invalid username or password")
	ErrInvalidGrant       = errors.New("grant is invalid, expired or already used")
	ErrInvalidInput       = errors.New("invalid account input")
	ErrConflict           = errors.New("account already exists or changed")
	ErrOwnerProtected     = errors.New("the owner account is protected")
	ErrInitialized        = errors.New("owner setup is already complete")
	ErrNotFound           = errors.New("person not found")
	ErrLimited            = errors.New("too many account attempts; try again later")
	ErrClosed             = errors.New("people store is closed")
	ErrCapacity           = errors.New("people or invitation capacity reached")
)

// Person is an immutable public snapshot. Password hashes are never exposed.
type Person struct {
	ID        string    `json:"id"`
	Username  string    `json:"username"`
	Name      string    `json:"name"`
	Role      string    `json:"role"`
	TeamID    string    `json:"team_id"`
	Owner     bool      `json:"owner"`
	Suspended bool      `json:"suspended"`
	Revision  string    `json:"revision"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// CreatePerson contains credentials supplied during owner or invitation setup.
type CreatePerson struct {
	Username string `json:"username"`
	Name     string `json:"name"`
	Password string `json:"password"`
}

// UpdatePerson changes only mutable account properties.
type UpdatePerson struct {
	Name      *string `json:"name,omitempty"`
	Role      *string `json:"role,omitempty"`
	TeamID    *string `json:"team_id,omitempty"`
	Suspended *bool   `json:"suspended,omitempty"`
}

// Issuer binds a grant to live authority without retaining a reusable credential.
// Person references are checked inside the people transaction. Reference is an
// opaque, nonsecret reference for the consumer's static/virtual or local authority.
type Issuer struct {
	PersonID  string `json:"person_id,omitempty"`
	Revision  string `json:"revision,omitempty"`
	Reference string `json:"reference,omitempty"`
}

// Purpose prevents a grant from being redeemed at another access transition.
type Purpose string

const (
	// PurposeSetup authorizes the first owner account only.
	PurposeSetup Purpose = "setup"
	// PurposeSession transfers existing authority into a browser session.
	PurposeSession Purpose = "session"
	// PurposeInvitation authorizes one new person in a specified team and role.
	PurposeInvitation Purpose = "invitation"
	// PurposeRecovery authorizes one password replacement for a specified person.
	PurposeRecovery Purpose = "recovery"
)

// Grant is a safe description, without the bearer token or browser binding.
type Grant struct {
	ID        string     `json:"id"`
	Purpose   Purpose    `json:"purpose"`
	TeamID    string     `json:"team_id,omitempty"`
	Role      string     `json:"role,omitempty"`
	PersonID  string     `json:"person_id,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt time.Time  `json:"expires_at"`
	UsedAt    *time.Time `json:"used_at,omitempty"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
	Status    string     `json:"status"`
}

// GrantRequest specifies the limited authority to issue. The API authorizes issuance.
type GrantRequest struct {
	Purpose  Purpose
	Issuer   Issuer
	TeamID   string
	Role     string
	PersonID string
	TTL      time.Duration
}

// IssuedGrant reveals its token once; the store persists only a digest.
type IssuedGrant struct {
	Grant
	Token string `json:"token"`
}

// Progress holds persisted onboarding choices and server-observed milestones.
type Progress struct {
	Step             string    `json:"step"`
	Dismissed        bool      `json:"dismissed"`
	ResponseVerified bool      `json:"response_verified"`
	AppConnected     bool      `json:"app_connected"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// ProgressUpdate contains user choices, never inferred success evidence.
type ProgressUpdate struct {
	Step      *string `json:"step,omitempty"`
	Dismissed *bool   `json:"dismissed,omitempty"`
}

// Options joins the domain to existing credential and team authorities.
// Callbacks run with the people mutex held and must not re-enter people or OAuth.
// The external-issuer callback is never called for a person issuer. Servers must
// serialize team deletion with enrollment outside this store before reading teams.
type Options struct {
	ValidateExternalIssuer func(context.Context, string) error
	ValidateTeam           func(context.Context, string) error
}
