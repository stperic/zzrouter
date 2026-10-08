// Package keys provides virtual API key management for the gateway API.
// Virtual keys extend the static 3-key model with named, scoped keys that
// belong to a team (the team governs model access and shared quotas).
package keys

import (
	"errors"
	"time"

	"github.com/stperic/zzrouter/pkg/access/quota"
	"github.com/stperic/zzrouter/pkg/utils"
)

var (
	// ErrKeyNotFound is returned when a key ID does not exist.
	ErrKeyNotFound = errors.New("key not found")
	// ErrKeyExists is returned when creating a key with an ID that already exists.
	ErrKeyExists = errors.New("key already exists")
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
	LastSeenAt time.Time  `yaml:"last_seen_at,omitempty"` // hot in-memory, persisted on save cycle
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

// KeyStore provides CRUD operations for virtual keys.
type KeyStore interface {
	Get(id string) *VirtualKey
	List() map[string]*VirtualKey
	ListByTeam(teamID string) map[string]*VirtualKey
	ValidateRawKey(rawKey string) (string, *VirtualKey)
	Create(id string, key *VirtualKey) (rawKey string, err error)
	Update(id string, key *VirtualKey) error
	Delete(id string) error
	RotateKey(id string) (rawKey string, err error)
	Reload() error
	Save() error

	// OnChange registers a listener that fires after any successful
	// mutation (Create / Update / Delete / RotateKey / Reload). No
	// arguments — the listener re-queries the store as needed. The
	// auth cache subscribes here so revoked or rotated keys become
	// un-authenticatable immediately, rather than lingering until
	// their TTL window expires.
	OnChange(fn func())
}
