// package server — TeamsService: business logic for team CRUD.
//
// Team membership is owned by keys (VirtualKey.TeamID), not by teams —
// this service never mutates a member list because there isn't one. Listing
// the keys in a team is served via Snapshot.KeysByTeam.

package server

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/stperic/zzrouter/pkg/access"
	"github.com/stperic/zzrouter/pkg/access/quota"
	"github.com/stperic/zzrouter/pkg/access/teams"
	"github.com/stperic/zzrouter/pkg/audit"
	"github.com/stperic/zzrouter/pkg/utils"
)

// TeamsService provides business logic for team management. The store keeps
// the rules that span records: personal teams, the owner's team, dependents.
type TeamsService struct {
	store  access.Store
	access *AccessControl
}

// NewTeamsService creates a new teams service.
func NewTeamsService(store access.Store, access *AccessControl) *TeamsService {
	return &TeamsService{store: store, access: access}
}

// ============================================================================
// Request / response DTOs
// ============================================================================

// CreateTeamRequest is the payload for POST /zzrouter/v1/teams.
// Only shared teams can be created via the API — personal teams are
// auto-created by KeysService and never accept a caller-supplied request.
type CreateTeamRequest struct {
	ID                  string               `json:"id" binding:"required"`
	Name                string               `json:"name" binding:"required"`
	AllowedModels       []string             `json:"allowed_models,omitempty"`
	RouteAccess         teams.RouteAccess    `json:"route_access,omitzero"`
	ProviderAccess      teams.ProviderAccess `json:"provider_access,omitzero"`
	MaxParallelRequests int                  `json:"max_parallel_requests,omitempty"`
	RPMLimit            int                  `json:"rpm_limit,omitempty"`
	TPMLimit            int                  `json:"tpm_limit,omitempty"`
	SpendLimit          float64              `json:"spend_limit,omitempty"`
	ResetPeriod         string               `json:"reset_period,omitempty"`
	DefaultMaxTokens    int                  `json:"default_max_tokens,omitempty"`
	Metadata            map[string]string    `json:"metadata,omitempty"`
}

// UpdateTeamRequest is the payload for PATCH /zzrouter/v1/teams/:id.
// Kind is not mutable; nil fields are left untouched.
type UpdateTeamRequest struct {
	Name                *string              `json:"name,omitempty"`
	AllowedModels       *[]string            `json:"allowed_models,omitempty"`
	RouteAccess         teams.RouteAccess    `json:"route_access,omitzero"`
	ProviderAccess      teams.ProviderAccess `json:"provider_access,omitzero"`
	Suspended           *bool                `json:"suspended,omitempty"`
	MaxParallelRequests *int                 `json:"max_parallel_requests,omitempty"`
	RPMLimit            *int                 `json:"rpm_limit,omitempty"`
	TPMLimit            *int                 `json:"tpm_limit,omitempty"`
	SpendLimit          *float64             `json:"spend_limit,omitempty"`
	ResetPeriod         *string              `json:"reset_period,omitempty"`
	DefaultMaxTokens    *int                 `json:"default_max_tokens,omitempty"`
	Metadata            map[string]string    `json:"metadata,omitempty"`
}

// TeamResponse is the safe API representation of a team and its membership impact.
type TeamResponse struct {
	Membership          access.Membership    `json:"membership"`
	ID                  string               `json:"id"`
	UUID                string               `json:"uuid"`
	Name                string               `json:"name"`
	Kind                string               `json:"kind"`
	AllowedModels       []string             `json:"allowed_models,omitempty"`
	RouteAccess         teams.RouteAccess    `json:"route_access"`
	ProviderAccess      teams.ProviderAccess `json:"provider_access"`
	Suspended           bool                 `json:"suspended"`
	SuspendedAt         *string              `json:"suspended_at,omitempty"`
	SuspendedBy         string               `json:"suspended_by,omitempty"`
	MaxParallelRequests int                  `json:"max_parallel_requests,omitempty"`
	RPMLimit            int                  `json:"rpm_limit,omitempty"`
	TPMLimit            int                  `json:"tpm_limit,omitempty"`
	SpendLimit          float64              `json:"spend_limit,omitempty"`
	ResetPeriod         string               `json:"reset_period,omitempty"`
	DefaultMaxTokens    int                  `json:"default_max_tokens,omitempty"`
	Metadata            map[string]string    `json:"metadata,omitempty"`
	CreatedAt           string               `json:"created_at,omitempty"`
	UpdatedAt           string               `json:"updated_at,omitempty"`
	CreatedBy           string               `json:"created_by,omitempty"`
	UpdatedBy           string               `json:"updated_by,omitempty"`
}

// TeamKeysResponse is the response for GET /zzrouter/v1/teams/:id/keys.
type TeamKeysResponse struct {
	TeamID string        `json:"team_id"`
	Keys   []TeamKeyItem `json:"keys"`
}

// TeamKeyItem is a single row in a TeamKeysResponse.
type TeamKeyItem struct {
	KeyID string `json:"key_id"`
	Name  string `json:"name,omitempty"`
	Role  string `json:"role"`
}

// TeamUsageResponse is the response for GET /zzrouter/v1/teams/:id/usage.
type TeamUsageResponse struct {
	TeamID string `json:"team_id"`
	quota.Usage
}

// ============================================================================
// CRUD — shared teams (admin surface)
// ============================================================================

// CreateTeam creates a shared team with the given config. Always Kind=shared —
// personal teams cannot be created via the public API.
func (s *TeamsService) CreateTeam(ctx context.Context, req *CreateTeamRequest, createdBy string) (*TeamResponse, error) {
	if req.ID == "" {
		return nil, invalidInputf("id is required")
	}
	if req.Name == "" {
		return nil, invalidInputf("name is required")
	}
	if err := s.validateRoutes(req.RouteAccess); err != nil {
		return nil, err
	}
	if err := req.ProviderAccess.Validate(); err != nil {
		return nil, invalidInputf("%w", err)
	}
	if err := validateResetPeriod(req.ResetPeriod); err != nil {
		return nil, err
	}
	if teams.IsPersonalID(req.ID) {
		return nil, invalidInputf("team id %q: the prefix %q is reserved for personal teams", req.ID, teams.PersonalPrefix)
	}
	if reservedTeamIDs[req.ID] {
		return nil, invalidInputf("%s", reservedNameMessage("team id", req.ID, "/zzrouter/v1/teams/", reservedTeamIDs))
	}

	team := teams.Team{
		Name:           req.Name,
		Kind:           teams.KindShared,
		AllowedModels:  slices.Clone(req.AllowedModels),
		ProviderAccess: req.ProviderAccess.Clone(),
		RouteAccess:    req.RouteAccess.Clone(),
		Metadata:       maps.Clone(req.Metadata),
		CreatedBy:      createdBy,
		UpdatedBy:      createdBy,
	}
	team.QuotaConfig = quota.QuotaConfig{
		MaxParallelRequests: req.MaxParallelRequests,
		RPMLimit:            req.RPMLimit,
		TPMLimit:            req.TPMLimit,
		SpendLimit:          req.SpendLimit,
		ResetPeriod:         req.ResetPeriod,
		DefaultMaxTokens:    req.DefaultMaxTokens,
	}

	created, err := s.store.CreateTeam(ctx, req.ID, team, audit.Event{
		Action:  audit.ActionTeamCreated,
		Actor:   resolveActor(createdBy),
		Subject: req.ID,
		Metadata: map[string]any{
			"name":            req.Name,
			"kind":            string(teams.KindShared),
			"provider_access": team.ProviderAccess.Clone(),
			"route_access":    team.RouteAccess.Clone(),
		},
	})
	if err != nil {
		return nil, err
	}
	return s.response(s.store.Snapshot(), created), nil
}

// GetTeam returns a team by ID.
func (s *TeamsService) GetTeam(id string) (*TeamResponse, error) {
	snap := s.store.Snapshot()
	team := snap.Team(id)
	if team == nil {
		return nil, fmt.Errorf("team %q: %w", id, teams.ErrTeamNotFound)
	}
	return s.response(snap, team), nil
}

// ListTeams returns all teams.
func (s *TeamsService) ListTeams() []*TeamResponse {
	snap := s.store.Snapshot()
	result := []*TeamResponse{}
	for _, team := range snap.Teams() {
		result = append(result, s.response(snap, team))
	}
	return result
}

// UpdateTeam applies a partial update to a team. The store refuses personal
// teams, whose config is fixed at creation, and suspending the owner's team.
func (s *TeamsService) UpdateTeam(ctx context.Context, id string, req *UpdateTeamRequest, updatedBy string) (*TeamResponse, error) {
	if req.ResetPeriod != nil {
		if err := validateResetPeriod(*req.ResetPeriod); err != nil {
			return nil, err
		}
	}
	if err := s.validateRoutes(req.RouteAccess); err != nil {
		return nil, err
	}
	if err := req.ProviderAccess.Validate(); err != nil {
		return nil, invalidInputf("%w", err)
	}
	updated, err := s.store.UpdateTeam(ctx, id, func(updated *teams.Team) error {
		if !req.RouteAccess.IsZero() {
			updated.RouteAccess = req.RouteAccess.Clone()
		}
		if req.ProviderAccess.Mode != "" {
			updated.ProviderAccess = req.ProviderAccess.Clone()
		}
		if req.Name != nil {
			updated.Name = *req.Name
		}
		if req.AllowedModels != nil {
			updated.AllowedModels = slices.Clone(*req.AllowedModels)
		}
		if req.Suspended != nil {
			updated.Suspended = *req.Suspended
			if *req.Suspended {
				now := utils.Now()
				updated.SuspendedAt = &now
				updated.SuspendedBy = updatedBy
			} else {
				updated.SuspendedAt = nil
				updated.SuspendedBy = ""
			}
		}
		if req.MaxParallelRequests != nil {
			updated.MaxParallelRequests = *req.MaxParallelRequests
		}
		if req.RPMLimit != nil {
			updated.RPMLimit = *req.RPMLimit
		}
		if req.TPMLimit != nil {
			updated.TPMLimit = *req.TPMLimit
		}
		if req.SpendLimit != nil {
			updated.SpendLimit = *req.SpendLimit
		}
		if req.ResetPeriod != nil {
			updated.ResetPeriod = *req.ResetPeriod
		}
		if req.DefaultMaxTokens != nil {
			updated.DefaultMaxTokens = *req.DefaultMaxTokens
		}
		if req.Metadata != nil {
			updated.Metadata = maps.Clone(req.Metadata)
		}
		updated.UpdatedBy = updatedBy
		return nil
	}, audit.Event{
		Action:   audit.ActionTeamUpdated,
		Actor:    resolveActor(updatedBy),
		Subject:  id,
		Metadata: updateTeamChangedFields(req),
	})
	if err != nil {
		return nil, err
	}
	return s.response(s.store.Snapshot(), updated), nil
}

// DeleteTeam removes a shared team. The store refuses one that keys, people
// or pending enrollments still use, and every personal team, which goes with
// its key instead.
func (s *TeamsService) DeleteTeam(ctx context.Context, id, actor string) error {
	return s.store.DeleteTeam(ctx, id, audit.Event{
		Action:  audit.ActionTeamDeleted,
		Actor:   resolveActor(actor),
		Subject: id,
	})
}

// GetTeamUsage returns live usage/spend for a team.
func (s *TeamsService) GetTeamUsage(id string) (*TeamUsageResponse, error) {
	team := s.store.Snapshot().Team(id)
	if team == nil {
		return nil, fmt.Errorf("team %q: %w", id, teams.ErrTeamNotFound)
	}

	var state *quota.SpendState
	if s.access != nil {
		state = s.access.GetTeamSpend(id)
	}
	resp := &TeamUsageResponse{TeamID: id, Usage: quota.CurrentUsage(team.QuotaConfig, state)}
	return resp, nil
}

// ResetTeamUsage clears the team's spend ledger.
func (s *TeamsService) ResetTeamUsage(ctx context.Context, id, actor string) error {
	if s.store.Snapshot().Team(id) == nil {
		return fmt.Errorf("team %q: %w", id, teams.ErrTeamNotFound)
	}
	if s.access != nil {
		s.access.ResetTeamSpend(id)
	}
	recordReset(ctx, s.store, audit.Event{
		Action:  audit.ActionTeamUsageReset,
		Actor:   resolveActor(actor),
		Subject: id,
	})
	return nil
}

// GetTeamKeys returns the keys that belong to a team by consulting the key
// store's reverse index — teams no longer carry their own member list.
func (s *TeamsService) GetTeamKeys(id string) (*TeamKeysResponse, error) {
	snap := s.store.Snapshot()
	if snap.Team(id) == nil {
		return nil, fmt.Errorf("team %q: %w", id, teams.ErrTeamNotFound)
	}

	resp := &TeamKeysResponse{TeamID: id, Keys: []TeamKeyItem{}}
	for keyID, vk := range snap.KeysByTeam(id) {
		resp.Keys = append(resp.Keys, TeamKeyItem{
			KeyID: keyID,
			Name:  vk.Name,
			Role:  vk.TeamRole,
		})
	}
	return resp, nil
}

// ============================================================================
// Conversion helpers
// ============================================================================

// response joins team with the membership snap projects; after a write, snap
// is read after its commit and may already hold a later one.
func (s *TeamsService) response(snap *access.Snapshot, team *teams.Team) *TeamResponse {
	response := toTeamResponse(team)
	if response != nil {
		response.Membership = snap.Membership(team.ID)
	}
	return response
}

func toTeamResponse(t *teams.Team) *TeamResponse {
	if t == nil {
		return nil
	}
	resp := &TeamResponse{
		ID:                  t.ID,
		UUID:                t.UUID,
		Name:                t.Name,
		Kind:                string(t.Kind),
		AllowedModels:       t.AllowedModels,
		ProviderAccess:      t.ProviderAccess.Clone(),
		RouteAccess:         t.RouteAccess.Clone(),
		Suspended:           t.Suspended,
		SuspendedBy:         t.SuspendedBy,
		MaxParallelRequests: t.MaxParallelRequests,
		RPMLimit:            t.RPMLimit,
		TPMLimit:            t.TPMLimit,
		SpendLimit:          t.SpendLimit,
		ResetPeriod:         t.ResetPeriod,
		DefaultMaxTokens:    t.DefaultMaxTokens,
		Metadata:            t.Metadata,
		CreatedBy:           t.CreatedBy,
		UpdatedBy:           t.UpdatedBy,
	}
	if t.SuspendedAt != nil {
		s := t.SuspendedAt.Format(time.RFC3339)
		resp.SuspendedAt = &s
	}
	if !t.CreatedAt.IsZero() {
		resp.CreatedAt = t.CreatedAt.Format(time.RFC3339)
	}
	if !t.UpdatedAt.IsZero() {
		resp.UpdatedAt = t.UpdatedAt.Format(time.RFC3339)
	}
	return resp
}

func (s *TeamsService) validateRoutes(policy teams.RouteAccess) error {
	if err := policy.Validate(); err != nil {
		return invalidInputf("%w", err)
	}
	if s.access != nil {
		if err := s.access.core.ValidateRoutes(policy); err != nil {
			return invalidInputf("%w", err)
		}
	} else if policy.Mode == "selected" {
		return invalidInputf("route catalog unavailable")
	}
	return nil
}
