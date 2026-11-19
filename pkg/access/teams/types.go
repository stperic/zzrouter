// Package teams defines teams and access policies without storage.
// Teams collect virtual keys under a shared identity — quotas and model access
// are enforced at the team level, keys point at their team via TeamID.
package teams

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/stperic/zzrouter/pkg/access/quota"
	"github.com/stperic/zzrouter/pkg/utils"
)

var (
	ErrTeamNotFound   = errors.New("team not found")
	ErrTeamExists     = errors.New("team already exists")
	ErrTeamHasMembers = errors.New("team has member keys")
	// ErrInvalid marks a team record that breaks a state invariant.
	ErrInvalid = errors.New("invalid team record")
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
	UUID string
	ID   string
	Name string

	Kind           TeamKind // "personal" | "shared" — immutable
	AllowedModels  []string
	RouteAccess    RouteAccess
	ProviderAccess ProviderAccess
	Suspended      bool
	SuspendedAt    *time.Time
	SuspendedBy    string

	quota.QuotaConfig

	CreatedAt time.Time
	UpdatedAt time.Time
	CreatedBy string
	UpdatedBy string
	Metadata  map[string]string
}

// Validate holds the invariants every stored team keeps.
func (t *Team) Validate() error {
	if t.Kind != KindPersonal && t.Kind != KindShared {
		if t.Kind == "" {
			return fmt.Errorf("%w: missing kind (expected %q or %q)", ErrInvalid, KindPersonal, KindShared)
		}
		return fmt.Errorf("%w: invalid kind %q (expected %q or %q)", ErrInvalid, t.Kind, KindPersonal, KindShared)
	}
	if err := t.RouteAccess.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if err := t.ProviderAccess.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if !utils.InRFC3339Range(t.SuspendedAt, &t.CreatedAt, &t.UpdatedAt) {
		return fmt.Errorf("%w: a time is outside the years 0001 to 9999", ErrInvalid)
	}
	if !t.ValidSpendLimit() {
		return fmt.Errorf("%w: spend_limit must be a finite number", ErrInvalid)
	}
	// Policies and metadata are stored as JSON.
	if !utils.ValidUTF8([]any{t.AllowedModels, t.RouteAccess.Routes, t.ProviderAccess.Providers, t.Metadata}) {
		return fmt.Errorf("%w: models, routes, providers and metadata must be valid UTF-8", ErrInvalid)
	}
	return nil
}

// Clone copies reference fields too, so a holder cannot edit another's record.
func (t *Team) Clone() *Team {
	if t == nil {
		return nil
	}
	c := *t
	c.AllowedModels = slices.Clone(t.AllowedModels)
	c.RouteAccess = t.RouteAccess.Clone()
	c.ProviderAccess = t.ProviderAccess.Clone()
	c.Metadata = maps.Clone(t.Metadata)
	if t.SuspendedAt != nil {
		suspended := *t.SuspendedAt
		c.SuspendedAt = &suspended
	}
	return &c
}
