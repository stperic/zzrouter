// Package teams provides team management with shared quotas and model access.
// Teams collect virtual keys under a shared identity — quotas and model access
// are enforced at the team level, keys point at their team via TeamID.
package teams

import (
	"time"

	"github.com/stperic/zzrouter/pkg/access/quota"
)

// MemberRole is the role a key plays within its team.
// Stored on VirtualKey.TeamRole as a plain string; constants here are the
// canonical source of truth for service-layer validation.
type MemberRole string

const (
	RoleOwner  MemberRole = "owner"
	RoleMember MemberRole = "member"
)

// TeamKind distinguishes auto-created personal teams (one key, immutable,
// cascade-deleted with their key) from shared teams (explicitly created,
// multi-member, manually managed).
//
// Kind is set at team creation and never mutates. A personal team can never
// gain a second member or be converted to shared — the service layer enforces
// this so the cascade-delete rule stays trivially correct.
type TeamKind string

const (
	KindPersonal TeamKind = "personal"
	KindShared   TeamKind = "shared"
)

// Team represents a collection of virtual keys with shared quotas and model access.
type Team struct {
	UUID string `yaml:"uuid"`
	ID   string `yaml:"-"` // from YAML map key
	Name string `yaml:"name"`

	Kind          TeamKind   `yaml:"kind"` // "personal" | "shared" — immutable
	AllowedModels []string   `yaml:"allowed_models,omitempty"`
	Suspended     bool       `yaml:"suspended,omitempty"`
	SuspendedAt   *time.Time `yaml:"suspended_at,omitempty"`
	SuspendedBy   string     `yaml:"suspended_by,omitempty"`

	quota.QuotaConfig `yaml:",inline"`

	CreatedAt time.Time         `yaml:"created_at,omitempty"`
	UpdatedAt time.Time         `yaml:"updated_at,omitempty"`
	CreatedBy string            `yaml:"created_by,omitempty"`
	UpdatedBy string            `yaml:"updated_by,omitempty"`
	Metadata  map[string]string `yaml:"metadata,omitempty"`
}

// TeamStore provides CRUD operations for teams.
type TeamStore interface {
	Get(id string) *Team
	List() map[string]*Team
	Create(id string, team *Team) error
	Update(id string, team *Team) error
	Delete(id string) error
	Reload() error
	Save() error

	// HasGatedTeam reports whether any team has AllowedModels set.
	// Read on the anonymous compat hot path — must be lock-free.
	HasGatedTeam() bool
}
