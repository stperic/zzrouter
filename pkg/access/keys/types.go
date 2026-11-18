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

// VirtualKey represents a named API key with access controls and metadata.
//
// TeamID and TeamRole are required and immutable after creation. Every key
// belongs to exactly one team — when a caller creates a key without specifying
// a team, the service auto-creates a personal team and assigns the new key as
// its sole owner.
type VirtualKey struct {
	// Identity
	UUID        string `yaml:"uuid,omitempty"`
	ID          string `yaml:"-"` // from YAML map key
	Name        string `yaml:"name"`
	Description string `yaml:"description,omitempty"` // free-form annotation, e.g. "agent: research-bot"

	// Credentials
	HashedKey string `yaml:"hashed_key,omitempty"`
	KeyEnv    string `yaml:"key_env,omitempty"` // env var for raw key (dev convenience)

	// Team binding — immutable after creation.
	TeamID   string `yaml:"team_id"`
	TeamRole string `yaml:"team_role"` // "owner" | "member"

	// Access control
	Role        string     `yaml:"role,omitempty"` // "user" or "admin"
	Suspended   bool       `yaml:"suspended,omitempty"`
	SuspendedAt *time.Time `yaml:"suspended_at,omitempty"`
	SuspendedBy string     `yaml:"suspended_by,omitempty"`

	// Quotas (inline embed — provides RPMLimit, TPMLimit, MaxParallelRequests, SpendLimit, ResetPeriod, DefaultMaxTokens)
	quota.QuotaConfig `yaml:",inline"`

	// Lifecycle
	ExpiresAt  *time.Time `yaml:"expires_at,omitempty"`
	CreatedAt  time.Time  `yaml:"created_at,omitempty"`
	UpdatedAt  time.Time  `yaml:"updated_at,omitempty"`
	LastSeenAt time.Time  `yaml:"last_seen_at,omitempty"` // nothing sets or persists it yet
	CreatedBy  string     `yaml:"created_by,omitempty"`
	UpdatedBy  string     `yaml:"updated_by,omitempty"`

	// Metadata
	Metadata map[string]string `yaml:"metadata,omitempty"`
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
	case k.HashedKey == "" && k.KeyEnv == "":
		return fmt.Errorf("%w: must have hashed_key or key_env", ErrInvalid)
	case k.TeamID == "":
		return fmt.Errorf("%w: missing team_id", ErrInvalid)
	case k.TeamRole == "":
		return fmt.Errorf("%w: missing team_role", ErrInvalid)
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
