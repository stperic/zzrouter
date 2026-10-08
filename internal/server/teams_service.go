// package server — TeamsService: business logic for team CRUD.
//
// Team membership is owned by keys (VirtualKey.TeamID), not by teams —
// this service never mutates a member list because there isn't one. Listing
// the keys in a team is served via keyStore.ListByTeam.

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

	"github.com/stperic/zzrouter/pkg/access/keys"
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
// KeysService.mu → TeamsService.mu. Don't reverse.
type TeamsService struct {
	mu       sync.Mutex
	store    *teams.FileTeamStore
	keyStore *keys.FileKeyStore
	access   *AccessControl
	audit    audit.Sink
}

// NewTeamsService creates a new teams service.
func NewTeamsService(store *teams.FileTeamStore, keyStore *keys.FileKeyStore, access *AccessControl) *TeamsService {
	return &TeamsService{store: store, keyStore: keyStore, access: access, audit: audit.Null{}}
}

// SetAuditSink replaces the audit sink. The zero-value sink is audit.Null,
// so tests that don't care about audit output can omit this call. Safe
// to call concurrently with mutation methods — acquires s.mu so an
// in-flight CreateTeam that reads s.audit always sees a consistent value.
func (s *TeamsService) SetAuditSink(sink audit.Sink) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sink == nil {
		s.audit = audit.Null{}
		return
	}
	s.audit = sink
}

// ============================================================================
// Request / response DTOs
// ============================================================================

// CreateTeamRequest is the payload for POST /zzrouter/v1/teams.
// Only shared teams can be created via the API — personal teams are
// auto-created by KeysService and never accept a caller-supplied request.
type CreateTeamRequest struct {
	ID                  string            `json:"id" binding:"required"`
	Name                string            `json:"name" binding:"required"`
	AllowedModels       []string          `json:"allowed_models,omitempty"`
	MaxParallelRequests int               `json:"max_parallel_requests,omitempty"`
	RPMLimit            int               `json:"rpm_limit,omitempty"`
	TPMLimit            int               `json:"tpm_limit,omitempty"`
	SpendLimit          float64           `json:"spend_limit,omitempty"`
	ResetPeriod         string            `json:"reset_period,omitempty"`
	DefaultMaxTokens    int               `json:"default_max_tokens,omitempty"`
	Metadata            map[string]string `json:"metadata,omitempty"`
}

// UpdateTeamRequest is the payload for PATCH /zzrouter/v1/teams/:id.
// Kind is not mutable; nil fields are left untouched.
type UpdateTeamRequest struct {
	Name                *string           `json:"name,omitempty"`
	AllowedModels       *[]string         `json:"allowed_models,omitempty"`
	Suspended           *bool             `json:"suspended,omitempty"`
	MaxParallelRequests *int              `json:"max_parallel_requests,omitempty"`
	RPMLimit            *int              `json:"rpm_limit,omitempty"`
	TPMLimit            *int              `json:"tpm_limit,omitempty"`
	SpendLimit          *float64          `json:"spend_limit,omitempty"`
	ResetPeriod         *string           `json:"reset_period,omitempty"`
	DefaultMaxTokens    *int              `json:"default_max_tokens,omitempty"`
	Metadata            map[string]string `json:"metadata,omitempty"`
}

// TeamResponse is the safe API representation of a team. Members are no
// longer embedded — clients hit GET /teams/:id/keys for the member list.
type TeamResponse struct {
	ID                  string            `json:"id"`
	UUID                string            `json:"uuid"`
	Name                string            `json:"name"`
	Kind                string            `json:"kind"`
	AllowedModels       []string          `json:"allowed_models,omitempty"`
	Suspended           bool              `json:"suspended"`
	SuspendedAt         *string           `json:"suspended_at,omitempty"`
	SuspendedBy         string            `json:"suspended_by,omitempty"`
	MaxParallelRequests int               `json:"max_parallel_requests,omitempty"`
	RPMLimit            int               `json:"rpm_limit,omitempty"`
	TPMLimit            int               `json:"tpm_limit,omitempty"`
	SpendLimit          float64           `json:"spend_limit,omitempty"`
	ResetPeriod         string            `json:"reset_period,omitempty"`
	DefaultMaxTokens    int               `json:"default_max_tokens,omitempty"`
	Metadata            map[string]string `json:"metadata,omitempty"`
	CreatedAt           string            `json:"created_at,omitempty"`
	UpdatedAt           string            `json:"updated_at,omitempty"`
	CreatedBy           string            `json:"created_by,omitempty"`
	UpdatedBy           string            `json:"updated_by,omitempty"`
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
	TeamID       string  `json:"team_id"`
	SpendUSD     float64 `json:"spend_usd"`
	ReservedUSD  float64 `json:"reserved_usd,omitempty"`
	TokensIn     int64   `json:"tokens_in"`
	TokensOut    int64   `json:"tokens_out"`
	RequestCount int64   `json:"request_count"`
	PeriodStart  string  `json:"period_start,omitempty"`
	Period       string  `json:"period,omitempty"`
	SpendLimit   float64 `json:"spend_limit,omitempty"`
	RPMLimit     int     `json:"rpm_limit,omitempty"`
	TPMLimit     int     `json:"tpm_limit,omitempty"`
}

// ============================================================================
// CRUD — shared teams (admin surface)
// ============================================================================

// CreateTeam creates a shared team with the given config. Always Kind=shared —
// personal teams cannot be created via the public API.
func (s *TeamsService) CreateTeam(req *CreateTeamRequest, createdBy string) (*TeamResponse, error) {
	if req.ID == "" {
		return nil, invalidInputf("id is required")
	}
	if req.Name == "" {
		return nil, invalidInputf("name is required")
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

	team := &teams.Team{
		Name:          req.Name,
		Kind:          teams.KindShared,
		AllowedModels: slices.Clone(req.AllowedModels),
		Metadata:      maps.Clone(req.Metadata),
		CreatedBy:     createdBy,
		UpdatedBy:     createdBy,
	}
	team.QuotaConfig = quota.QuotaConfig{
		MaxParallelRequests: req.MaxParallelRequests,
		RPMLimit:            req.RPMLimit,
		TPMLimit:            req.TPMLimit,
		SpendLimit:          req.SpendLimit,
		ResetPeriod:         req.ResetPeriod,
		DefaultMaxTokens:    req.DefaultMaxTokens,
	}

	if err := s.store.Create(req.ID, team); err != nil {
		return nil, err
	}
	if err := s.store.Save(); err != nil {
		return nil, fmt.Errorf("team created but failed to persist: %w", err)
	}

	audit.Log(context.Background(), s.audit, audit.Event{
		Action:  audit.ActionTeamCreated,
		Actor:   resolveActor(createdBy),
		Subject: req.ID,
		Metadata: map[string]any{
			"name": req.Name,
			"kind": string(teams.KindShared),
		},
	})

	return toTeamResponse(s.store.Get(req.ID)), nil
}

// GetTeam returns a team by ID.
func (s *TeamsService) GetTeam(id string) (*TeamResponse, error) {
	team := s.store.Get(id)
	if team == nil {
		return nil, fmt.Errorf("team %q: %w", id, teams.ErrTeamNotFound)
	}
	return toTeamResponse(team), nil
}

// ListTeams returns all teams.
func (s *TeamsService) ListTeams() []*TeamResponse {
	all := s.store.List()
	result := make([]*TeamResponse, 0, len(all))
	for _, team := range all {
		result = append(result, toTeamResponse(team))
	}
	return result
}

// UpdateTeam applies a partial update to a team. Personal teams reject
// updates outright — their config is fixed at creation.
func (s *TeamsService) UpdateTeam(id string, req *UpdateTeamRequest, updatedBy string) (*TeamResponse, error) {
	if req.ResetPeriod != nil {
		if err := validateResetPeriod(*req.ResetPeriod); err != nil {
			return nil, err
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	existing := s.store.Get(id)
	if existing == nil {
		return nil, fmt.Errorf("team %q: %w", id, teams.ErrTeamNotFound)
	}
	if existing.Kind == teams.KindPersonal {
		return nil, fmt.Errorf("team %q: %w", id, ErrPersonalTeamImmutable)
	}

	updated := *existing
	updated.AllowedModels = slices.Clone(existing.AllowedModels)
	updated.Metadata = maps.Clone(existing.Metadata)

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

	if err := s.store.Update(id, &updated); err != nil {
		return nil, err
	}
	if err := s.store.Save(); err != nil {
		return nil, fmt.Errorf("team updated but failed to persist: %w", err)
	}

	audit.Log(context.Background(), s.audit, audit.Event{
		Action:   audit.ActionTeamUpdated,
		Actor:    resolveActor(updatedBy),
		Subject:  id,
		Metadata: updateTeamChangedFields(req),
	})

	return toTeamResponse(s.store.Get(id)), nil
}

// DeleteTeam removes a shared team. The team must have zero member keys —
// callers must delete or re-home member keys first. Personal teams cannot
// be deleted via this path; they cascade-delete with their key instead.
func (s *TeamsService) DeleteTeam(id, actor string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	existing := s.store.Get(id)
	if existing == nil {
		return fmt.Errorf("team %q: %w", id, teams.ErrTeamNotFound)
	}
	if existing.Kind == teams.KindPersonal {
		return fmt.Errorf("team %q: %w", id, ErrPersonalTeamImmutable)
	}

	if s.keyStore != nil {
		members := s.keyStore.ListByTeam(id)
		if len(members) > 0 {
			return fmt.Errorf("team %q has %d member keys; delete or re-home them first: %w",
				id, len(members), teams.ErrTeamHasMembers)
		}
	}

	if err := s.store.Delete(id); err != nil {
		return err
	}
	if err := s.store.Save(); err != nil {
		return fmt.Errorf("team deleted but failed to persist: %w", err)
	}

	if s.access != nil {
		s.access.OnTeamDeleted(id)
	}

	audit.Log(context.Background(), s.audit, audit.Event{
		Action:  audit.ActionTeamDeleted,
		Actor:   resolveActor(actor),
		Subject: id,
	})
	return nil
}

// GetTeamUsage returns live usage/spend for a team.
func (s *TeamsService) GetTeamUsage(id string) (*TeamUsageResponse, error) {
	team := s.store.Get(id)
	if team == nil {
		return nil, fmt.Errorf("team %q: %w", id, teams.ErrTeamNotFound)
	}

	resp := &TeamUsageResponse{
		TeamID:     id,
		RPMLimit:   team.RPMLimit,
		TPMLimit:   team.TPMLimit,
		SpendLimit: team.SpendLimit,
		Period:     team.ResetPeriod,
	}

	if s.access != nil {
		if state := s.access.GetTeamSpend(id); state != nil {
			resp.SpendUSD = state.SpendUSD()
			resp.ReservedUSD = quota.MicroToUSD(state.ReservedMicro)
			resp.TokensIn = state.TokensIn
			resp.TokensOut = state.TokensOut
			resp.RequestCount = state.RequestCount
			if !state.PeriodStart.IsZero() {
				resp.PeriodStart = state.PeriodStart.Format(time.RFC3339)
			}
			if state.Period != "" {
				resp.Period = string(state.Period)
			}
		}
	}
	return resp, nil
}

// ResetTeamUsage clears the team's spend ledger.
func (s *TeamsService) ResetTeamUsage(id, actor string) error {
	if s.store.Get(id) == nil {
		return fmt.Errorf("team %q: %w", id, teams.ErrTeamNotFound)
	}
	if s.access != nil {
		s.access.ResetTeamSpend(id)
	}
	audit.Log(context.Background(), s.audit, audit.Event{
		Action:  audit.ActionTeamUsageReset,
		Actor:   resolveActor(actor),
		Subject: id,
	})
	return nil
}

// GetTeamKeys returns the keys that belong to a team by consulting the key
// store's reverse index — teams no longer carry their own member list.
func (s *TeamsService) GetTeamKeys(id string) (*TeamKeysResponse, error) {
	team := s.store.Get(id)
	if team == nil {
		return nil, fmt.Errorf("team %q: %w", id, teams.ErrTeamNotFound)
	}

	resp := &TeamKeysResponse{TeamID: id, Keys: []TeamKeyItem{}}
	if s.keyStore == nil {
		return resp, nil
	}
	for keyID, vk := range s.keyStore.ListByTeam(id) {
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
func (s *TeamsService) CreatePersonalTeam(id, keyName, createdBy string) (*TeamResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	team := &teams.Team{
		Name:      "Personal: " + keyName,
		Kind:      teams.KindPersonal,
		CreatedBy: createdBy,
		UpdatedBy: createdBy,
	}
	if err := s.store.Create(id, team); err != nil {
		return nil, err
	}
	if err := s.store.Save(); err != nil {
		return nil, fmt.Errorf("personal team created but failed to persist: %w", err)
	}
	audit.Log(context.Background(), s.audit, audit.Event{
		Action:  audit.ActionTeamCreated,
		Actor:   resolveActor(createdBy),
		Subject: id,
		Metadata: map[string]any{
			"kind":     string(teams.KindPersonal),
			"key_name": keyName,
		},
	})
	return toTeamResponse(s.store.Get(id)), nil
}

// DeletePersonalTeam removes a personal team without the shared-team
// safeguards (no member check, no ErrPersonalTeamImmutable gate). Called by
// KeysService as part of key deletion cascade. Returns ErrTeamNotFound if
// the ID doesn't exist or references a non-personal team. actor is the
// cascading caller's identity (e.g. the admin deleting the owning key);
// passing "" attributes the op to the static admin sentinel.
func (s *TeamsService) DeletePersonalTeam(id, actor string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	existing := s.store.Get(id)
	if existing == nil {
		return fmt.Errorf("team %q: %w", id, teams.ErrTeamNotFound)
	}
	if existing.Kind != teams.KindPersonal {
		return fmt.Errorf("team %q: %w", id, teams.ErrTeamNotFound)
	}

	if err := s.store.Delete(id); err != nil {
		return err
	}
	if err := s.store.Save(); err != nil {
		return fmt.Errorf("personal team deleted but failed to persist: %w", err)
	}
	if s.access != nil {
		s.access.OnTeamDeleted(id)
	}
	audit.Log(context.Background(), s.audit, audit.Event{
		Action:  audit.ActionTeamDeleted,
		Actor:   resolveActor(actor),
		Subject: id,
		Metadata: map[string]any{
			"kind":    string(teams.KindPersonal),
			"cascade": true,
		},
	})
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

	if s.keyStore == nil {
		return 0
	}

	reaped := 0
	for id, team := range s.store.List() {
		if team.Kind != teams.KindPersonal {
			continue
		}
		if len(s.keyStore.ListByTeam(id)) > 0 {
			continue
		}
		if err := s.store.Delete(id); err != nil {
			slog.Warn("reconcile: failed to delete orphan personal team", "team", id, "error", err)
			continue
		}
		if s.access != nil {
			s.access.OnTeamDeleted(id)
		}
		audit.Log(context.Background(), s.audit, audit.Event{
			Action:  audit.ActionTeamDeleted,
			Actor:   audit.ActorSystemReconciler,
			Subject: id,
			Metadata: map[string]any{
				"kind":    string(teams.KindPersonal),
				"orphan":  true,
				"cascade": false,
			},
		})
		reaped++
	}
	if reaped > 0 {
		if err := s.store.Save(); err != nil {
			slog.Warn("reconcile: failed to persist after reaping orphan personal teams", "error", err)
		}
		slog.Info("reconcile: reaped orphan personal teams", "count", reaped)
	}
	return reaped
}

// ============================================================================
// Conversion helpers
// ============================================================================

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
