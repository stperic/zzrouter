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

	"github.com/stperic/zzrouter/pkg/access/quota"
	"github.com/stperic/zzrouter/pkg/access/teams"
	"github.com/stperic/zzrouter/pkg/utils"
)

// UserRole represents the access level assigned to an API key.
type UserRole string

const (
	// RoleUser — read-only inference (OpenAI/Ollama compat surfaces).
	RoleUser UserRole = "user"
	// RoleAdmin — full management (teams, keys, spend, providers).
	RoleAdmin UserRole = "admin"
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

// KeyFingerprint returns a safe (first4...last4) fingerprint of an API
// key for audit logging without leaking the secret. Short keys (<=12
// chars) are returned unchanged — those are test fixtures, not real keys.
func KeyFingerprint(key string) string {
	if key == "" {
		return ""
	}
	if len(key) <= 12 {
		return key
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
	ID          string // "admin", "user", "cluster", or a virtual key ID.
	Name        string
	Fingerprint string // first4...last4 (safe to log).
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

// IsExpired returns true if the key has a set ExpiresAt in the past.
func (k *KeyPrincipal) IsExpired() bool {
	if k == nil || k.ExpiresAt == nil {
		return false
	}
	return utils.Now().After(*k.ExpiresAt)
}

// TeamPrincipal is the team-level identity when the key belongs to a team.
type TeamPrincipal struct {
	ID            string
	Name          string
	MemberRole    teams.MemberRole
	AllowedModels []string
	Quotas        quota.QuotaConfig

	Suspended   bool
	SuspendedAt *time.Time
	SuspendedBy string
}
