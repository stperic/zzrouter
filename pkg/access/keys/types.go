// Package keys defines virtual API keys and hashing without storage.
// Virtual keys extend the static 3-key model with named, scoped keys that
// belong to a team (the team governs model access and shared quotas).
package keys

import (
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/stperic/zzrouter/pkg/access/quota"
	"github.com/stperic/zzrouter/pkg/utils"
)

var (
	// ErrKeyNotFound is returned when a key ID does not exist.
	ErrKeyNotFound = errors.New("key not found")
	// ErrKeyExists is returned when creating a key with an ID that already exists.
	ErrKeyExists = errors.New("key already exists")
	// ErrInvalid marks a key record that breaks a state invariant.
	ErrInvalid = errors.New("invalid key record")
)

// The roles a key can hold; control.UserRole names the same values.
const (
	RoleAdmin = "admin"
	RoleUser  = "user"
)

// ValidRole reports whether role is one a key can hold.
func ValidRole(role string) bool { return role == RoleAdmin || role == RoleUser }

// VirtualKey represents a named API key with access controls and metadata.
//
// TeamID and TeamRole are required and immutable after creation. Every key
// belongs to exactly one team — when a caller creates a key without specifying
// a team, the service auto-creates a personal team and assigns the new key as
// its sole owner.
type VirtualKey struct {
	// Identity
	UUID        string
	ID          string
	Name        string
	Description string // free-form annotation, e.g. "agent: research-bot"

	// Credentials
	HashedKey string

	// Team binding — immutable after creation.
	TeamID   string
	TeamRole string // "owner" | "member"

	// Access control
	Role        string // RoleAdmin or RoleUser
	Suspended   bool
	SuspendedAt *time.Time
	SuspendedBy string

	// Quotas (inline embed — provides RPMLimit, TPMLimit, MaxParallelRequests, SpendLimit, ResetPeriod, DefaultMaxTokens)
	quota.QuotaConfig

	// Lifecycle
	ExpiresAt  *time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time
	LastSeenAt time.Time // nothing sets or persists it yet
	CreatedBy  string
	UpdatedBy  string

	// Metadata
	Metadata map[string]string
}

// IsExpired returns true if the key has passed its expiration time.
func (k *VirtualKey) IsExpired() bool {
	if k.ExpiresAt == nil {
		return false
	}
	return utils.Now().After(*k.ExpiresAt)
}

// Validate holds the invariants every stored key keeps.
func (k *VirtualKey) Validate() error {
	switch {
	case k.Name == "":
		return fmt.Errorf("%w: missing name", ErrInvalid)
	case k.HashedKey == "":
		return fmt.Errorf("%w: missing hashed_key", ErrInvalid)
	case k.TeamID == "":
		return fmt.Errorf("%w: missing team_id", ErrInvalid)
	case k.TeamRole == "":
		return fmt.Errorf("%w: missing team_role", ErrInvalid)
	case !ValidRole(k.Role):
		return fmt.Errorf("%w: invalid role %q (expected %q or %q)", ErrInvalid, k.Role, RoleAdmin, RoleUser)
	case !utils.InRFC3339Range(k.ExpiresAt, k.SuspendedAt, &k.CreatedAt, &k.UpdatedAt, &k.LastSeenAt):
		return fmt.Errorf("%w: a time is outside the years 0001 to 9999", ErrInvalid)
	case !k.ValidSpendLimit():
		return fmt.Errorf("%w: spend_limit must be a finite number", ErrInvalid)
	case !utils.ValidUTF8(k.Metadata): // stored as JSON
		return fmt.Errorf("%w: metadata must be valid UTF-8", ErrInvalid)
	}
	return nil
}

// Clone copies reference fields too, so a holder cannot edit another's record.
func (k *VirtualKey) Clone() *VirtualKey {
	if k == nil {
		return nil
	}
	c := *k
	c.Metadata = maps.Clone(k.Metadata)
	if k.ExpiresAt != nil {
		expires := *k.ExpiresAt
		c.ExpiresAt = &expires
	}
	if k.SuspendedAt != nil {
		suspended := *k.SuspendedAt
		c.SuspendedAt = &suspended
	}
	return &c
}
