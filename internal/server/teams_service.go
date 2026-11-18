// package server — TeamsService: business logic for team CRUD.
//
// Team membership is owned by keys (VirtualKey.TeamID), not by teams —
// this service never mutates a member list because there isn't one. Listing
// the keys in a team is served via Snapshot.KeysByTeam.

package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/stperic/zzrouter/pkg/access"
	"github.com/stperic/zzrouter/pkg/access/people"
	"github.com/stperic/zzrouter/pkg/access/quota"
	"github.com/stperic/zzrouter/pkg/access/teams"
	"github.com/stperic/zzrouter/pkg/audit"
	"github.com/stperic/zzrouter/pkg/utils"
)

// personalTeamIDPrefix is reserved for auto-created personal teams. Shared
// teams created via the public API cannot squat on this prefix, since doing
// so would collide with a future key's auto-generated personal team ID.
const personalTeamIDPrefix = "personal-"

// ErrPersonalTeamImmutable is returned when a caller tries to mutate a
// personal team in a way the design forbids (adding keys, converting to
// shared, PATCHing limits after the fact, etc).
var ErrPersonalTeamImmutable = errors.New("personal team is immutable")

// TeamsService provides business logic for team management.
//
// mu serializes all mutation paths so the read-validate-write cycle is
// atomic against concurrent admin requests and against KeysService driving
// personal-team auto-create. KeysService holds its own lock AND acquires
// this service's lock via the helper methods below — lock order is always
// KeysService.mu → TeamsService.mu → the access store's writer gate, which
// is innermost. Don't reverse.
type TeamsService struct {
	mu     sync.Mutex
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

// TeamMembership reports dependencies and allowed lifecycle actions.
type TeamMembership struct {
	people.TeamDependencies
	AppKeys    int  `json:"app_keys"`
	CanDelete  bool `json:"can_delete"`
	CanSuspend bool `json:"can_suspend"`
}

// TeamResponse is the safe API representation of a team and its membership impact.
type TeamResponse struct {
	Membership          TeamMembership       `json:"membership"`
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
	if strings.HasPrefix(req.ID, personalTeamIDPrefix) {
		return nil, invalidInputf("team id %q: prefix %q is reserved for personal teams",
			req.ID, personalTeamIDPrefix)
	}
	if reservedTeamIDs[req.ID] {
		return nil, invalidInputf("%s", reservedNameMessage("team id", req.ID, "/zzrouter/v1/teams/", reservedTeamIDs))
	}

	s.mu.Lock()
	defer s.mu.Unlock()

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
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := s.store.Snapshot()
	team := snap.Team(id)
	if team == nil {
		return nil, fmt.Errorf("team %q: %w", id, teams.ErrTeamNotFound)
	}
	return s.response(snap, team), nil
}

// ListTeams returns all teams.
func (s *TeamsService) ListTeams() []*TeamResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := s.store.Snapshot()
	result := []*TeamResponse{}
	for _, team := range snap.Teams() {
		result = append(result, s.response(snap, team))
	}
	return result
}

// UpdateTeam applies a partial update to a team. Personal teams reject
// updates outright — their config is fixed at creation.
func (s *TeamsService) UpdateTeam(ctx context.Context, id string, req *UpdateTeamRequest, updatedBy string) (*TeamResponse, error) {
	if req.ResetPeriod != nil {
		if err := validateResetPeriod(*req.ResetPeriod); err != nil {
			return nil, err
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	snap := s.store.Snapshot()
	if req.Suspended != nil && *req.Suspended && snap.OwnerTeam(id) {
		return nil, fmt.Errorf("the owner's team cannot be suspended: %w", people.ErrOwnerProtected)
	}

	existing := snap.Team(id)
	if existing == nil {
		return nil, fmt.Errorf("team %q: %w", id, teams.ErrTeamNotFound)
	}
	if existing.Kind == teams.KindPersonal {
		return nil, fmt.Errorf("team %q: %w", id, ErrPersonalTeamImmutable)
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

// DeleteTeam removes a shared team. The team must have zero member keys —
// callers must delete or re-home member keys first. Personal teams cannot
// be deleted via this path; they cascade-delete with their key instead.
func (s *TeamsService) DeleteTeam(ctx context.Context, id, actor string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := s.store.Snapshot()
	existing := snap.Team(id)
	if existing == nil {
		return fmt.Errorf("team %q: %w", id, teams.ErrTeamNotFound)
	}
	if existing.Kind == teams.KindPersonal {
		return fmt.Errorf("team %q: %w", id, ErrPersonalTeamImmutable)
	}

	if impact := membership(snap, existing); !impact.CanDelete {
		return fmt.Errorf("team %q has %d app keys, %d people and %d pending enrollments; remove or reassign them first: %w",
			id, impact.AppKeys, impact.People, impact.Invitations+impact.EnrollmentGrants, teams.ErrTeamHasMembers)
	}

	err := s.store.DeleteTeam(ctx, id, audit.Event{
		Action:  audit.ActionTeamDeleted,
		Actor:   resolveActor(actor),
		Subject: id,
	})
	if err != nil {
		return err
	}

	if s.access != nil {
		s.access.OnTeamDeleted(id)
	}
	return nil
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
	s.store.Record(ctx, audit.Event{
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
// Personal-team lifecycle (KeysService-facing — not on the HTTP surface)
// ============================================================================

// CreatePersonalTeam creates a kind=personal team with the given ID and
// sensible defaults. Called by KeysService when a key is created without a
// caller-supplied team_id. Returns the created TeamResponse.
func (s *TeamsService) CreatePersonalTeam(ctx context.Context, id, keyName, createdBy string) (*TeamResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	team := teams.Team{
		Name:      "Personal: " + keyName,
		Kind:      teams.KindPersonal,
		CreatedBy: createdBy,
		UpdatedBy: createdBy,
	}
	created, err := s.store.CreateTeam(ctx, id, team, audit.Event{
		Action:  audit.ActionTeamCreated,
		Actor:   resolveActor(createdBy),
		Subject: id,
		Metadata: map[string]any{
			"kind":     string(teams.KindPersonal),
			"key_name": keyName,
		},
	})
	if err != nil {
		return nil, err
	}
	return s.response(s.store.Snapshot(), created), nil
}

// DeletePersonalTeam removes a personal team without the shared-team
// safeguards (no member check, no ErrPersonalTeamImmutable gate). Called by
// KeysService as part of key deletion cascade. Returns ErrTeamNotFound if
// the ID doesn't exist or references a non-personal team. actor is the
// cascading caller's identity (e.g. the admin deleting the owning key);
// passing "" attributes the op to the static admin sentinel.
func (s *TeamsService) DeletePersonalTeam(ctx context.Context, id, actor string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	existing := s.store.Snapshot().Team(id)
	if existing == nil || existing.Kind != teams.KindPersonal {
		return fmt.Errorf("team %q: %w", id, teams.ErrTeamNotFound)
	}

	err := s.store.DeleteTeam(ctx, id, audit.Event{
		Action:  audit.ActionTeamDeleted,
		Actor:   resolveActor(actor),
		Subject: id,
		Metadata: map[string]any{
			"kind":    string(teams.KindPersonal),
			"cascade": true,
		},
	})
	if err != nil {
		return err
	}
	if s.access != nil {
		s.access.OnTeamDeleted(id)
	}
	return nil
}

// ReconcilePersonalTeams sweeps orphan personal teams — any Kind=personal
// team with zero referencing keys gets deleted. Called by Server.Start after
// both stores are loaded so a crash mid-create-key doesn't leak an empty
// personal team into future sessions.
//
// Returns the number of teams reaped for logging.
func (s *TeamsService) ReconcilePersonalTeams() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	snap := s.store.Snapshot()
	reaped := 0
	for id, team := range snap.Teams() {
		if team.Kind != teams.KindPersonal || hasKeys(snap, id) {
			continue
		}
		err := s.store.DeleteTeam(context.Background(), id, audit.Event{
			Action:  audit.ActionTeamDeleted,
			Actor:   audit.ActorSystemReconciler,
			Subject: id,
			Metadata: map[string]any{
				"kind":    string(teams.KindPersonal),
				"orphan":  true,
				"cascade": false,
			},
		})
		if err != nil {
			slog.Warn("reconcile: failed to delete orphan personal team", "team", id, "error", err)
			continue
		}
		if s.access != nil {
			s.access.OnTeamDeleted(id)
		}
		reaped++
	}
	if reaped > 0 {
		slog.Info("reconcile: reaped orphan personal teams", "count", reaped)
	}
	return reaped
}

// ============================================================================
// Conversion helpers
// ============================================================================

// Caller holds the team mutation gate so policy and membership agree.
func (s *TeamsService) response(snap *access.Snapshot, team *teams.Team) *TeamResponse {
	response := toTeamResponse(team)
	if response != nil {
		response.Membership = membership(snap, team)
	}
	return response
}

func hasKeys(snap *access.Snapshot, teamID string) bool {
	for range snap.KeysByTeam(teamID) {
		return true
	}
	return false
}

func membership(snap *access.Snapshot, team *teams.Team) TeamMembership {
	result := TeamMembership{TeamDependencies: snap.TeamImpact(team.ID)}
	for range snap.KeysByTeam(team.ID) {
		result.AppKeys++
	}
	result.CanDelete = team.Kind == teams.KindShared && result.AppKeys == 0 && !result.HasMembers()
	result.CanSuspend = team.Kind == teams.KindShared && !result.OwnerProtected
	return result
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
