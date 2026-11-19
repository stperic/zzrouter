// Package control owns the pure-domain access-control logic: API-key
// authentication (static + virtual with Argon2id cache), team-scoped
// model authorization, quota enforcement chain, pending-reservation
// stash + reaper, and post-response spend settlement.
//
// All code here is gin-free. HTTP adaptation (header extraction,
// response rendering, gin-context read/write) lives in the server
// package's AccessControl type, which wraps *Core.
package control

import (
	"time"

	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/people"
	"github.com/stperic/zzrouter/pkg/access/quota"
	"github.com/stperic/zzrouter/pkg/access/teams"
	"github.com/stperic/zzrouter/pkg/utils"
)

// UserRole represents the access level assigned to an API key.
type UserRole string

// IdentityKind distinguishes human accounts from application and installation keys.
type IdentityKind string

const (
	// KindPerson denotes a local human account.
	KindPerson IdentityKind = people.Kind
	// KindKey denotes a managed application key.
	KindKey IdentityKind = "key"
	// KindStatic denotes an installation credential.
	KindStatic IdentityKind = "static"
)

const (
	// RoleUser — read-only inference (OpenAI/Ollama compat surfaces).
	RoleUser UserRole = keys.RoleUser
	// RoleAdmin — full management (teams, keys, spend, providers).
	RoleAdmin UserRole = keys.RoleAdmin
)

// RoleAllowed reports whether a key's role satisfies the required role.
//
// Roles are strict, non-hierarchical pools: admin does NOT satisfy
// "user required" and vice versa. Callers that want to accept multiple
// roles use the AuthenticateMultiRole path. Letting a user key through
// an admin gate would silently grant management access to read-only
// inference credentials.
func RoleAllowed(keyRole, requiredRole UserRole) bool {
	return keyRole == requiredRole
}

// KeyFingerprint returns a safe fingerprint for audit logging. Short keys are
// fully redacted; longer keys expose only first4...last4.
func KeyFingerprint(key string) string {
	if key == "" {
		return ""
	}
	if len(key) <= 12 {
		return "[redacted]"
	}
	return key[:4] + "..." + key[len(key)-4:]
}

// StaticKeySet holds the two static keys read from node.yaml by the
// server-side config layer. Empty values mean "never matches" in the
// constant-time comparison.
type StaticKeySet struct {
	Admin string
	User  string
}

// AccessContext is the authenticated identity resolved from an API key.
// Populated by Core.Authenticate and carried from the gin layer into
// Core.Enforce.
type AccessContext struct {
	Key  *KeyPrincipal
	Team *TeamPrincipal // nil if key has no team
}

// KeyPrincipal is the key-level identity resolved from an API key.
type KeyPrincipal struct {
	Kind        IdentityKind
	ID          string // "admin", "user", "cluster", or a virtual key ID.
	Name        string
	Fingerprint string // Redacted or first4...last4 (safe to log).
	Role        UserRole
	IsVirtual   bool
	TeamID      string // "" for static keys, set for virtual keys.
	TeamRole    string // "owner" | "member", empty for static keys.
	Metadata    map[string]string
	Quotas      quota.QuotaConfig

	// Suspension state (virtual keys only).
	Suspended   bool
	SuspendedAt *time.Time
	SuspendedBy string

	// Expiration (virtual keys only).
	ExpiresAt *time.Time
}

// TeamBound shares policy and quota enforcement between people and managed keys.
func (k *KeyPrincipal) TeamBound() bool { return k != nil && (k.Kind == KindPerson || k.IsVirtual) }

// IdentityKind returns the explicit human kind or the legacy key classification.
func (k *KeyPrincipal) IdentityKind() IdentityKind {
	if k.Kind == KindPerson {
		return KindPerson
	}
	if k.IsVirtual {
		return KindKey
	}
	return KindStatic
}

// Subject captures immutable request-time attribution for post-response settlement.
type Subject struct {
	Kind   IdentityKind
	ID     string
	TeamID string
}

// Subject returns the caller and team captured at admission time.
func (k *KeyPrincipal) Subject() Subject {
	return Subject{Kind: k.IdentityKind(), ID: k.ID, TeamID: k.TeamID}
}

// ScopeKind separates person and key counters even if their identifiers coincide.
func (s Subject) ScopeKind() quota.ScopeKind {
	if s.Kind == KindPerson {
		return quota.ScopePerson
	}
	return quota.ScopeKey
}

// ReservationKey keeps human reservations separate from legacy key identifiers.
func (s Subject) ReservationKey() string {
	if s.Kind == KindPerson {
		return "person:" + s.ID
	}
	return s.ID
}

// ReservationKey returns the exact identity used when stashing budget reservations.
func (k *KeyPrincipal) ReservationKey() string { return k.Subject().ReservationKey() }

// IsExpired returns true if the key has a set ExpiresAt in the past.
func (k *KeyPrincipal) IsExpired() bool {
	if k == nil || k.ExpiresAt == nil {
		return false
	}
	return utils.Now().After(*k.ExpiresAt)
}

// TeamPrincipal is the team-level identity when the key belongs to a team.
type TeamPrincipal struct {
	ID             string
	Name           string
	MemberRole     teams.MemberRole
	AllowedModels  []string
	RouteAccess    teams.RouteAccess
	ProviderAccess teams.ProviderAccess
	Quotas         quota.QuotaConfig

	Suspended   bool
	SuspendedAt *time.Time
	SuspendedBy string
}
