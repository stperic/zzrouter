package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/quota"
	"github.com/stperic/zzrouter/pkg/access/teams"
	"github.com/stperic/zzrouter/pkg/audit"
	"github.com/stperic/zzrouter/pkg/utils"
)

// KeysService provides business logic for virtual key management.
// Mutation operations go through AccessControl to invalidate caches.
//
// Key creation is the driver for personal-team auto-creation: when a caller
// creates a key without specifying a team, this service asks TeamsService to
// mint a personal team and uses its ID. KeysService therefore holds a
// TeamsService pointer, and the two share a mutex on the combined path so
// two concurrent POST /keys calls can't both try to mint a personal team
// for the same key name.
type KeysService struct {
	mu     sync.Mutex
	store  *keys.FileKeyStore
	teams  *TeamsService
	access *AccessControl
	audit  audit.Sink
}

// NewKeysService creates a new keys service.
func NewKeysService(store *keys.FileKeyStore, teamsService *TeamsService, access *AccessControl) *KeysService {
	return &KeysService{store: store, teams: teamsService, access: access, audit: audit.Null{}}
}

// SetAuditSink replaces the audit sink. The zero-value sink is audit.Null,
// so tests that don't care about audit output can omit this call. Safe
// to call concurrently with mutation methods — acquires s.mu so an
// in-flight CreateKey that reads s.audit always sees a consistent value.
func (s *KeysService) SetAuditSink(sink audit.Sink) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sink == nil {
		s.audit = audit.Null{}
		return
	}
	s.audit = sink
}

// CreateKeyRequest is the input for creating a virtual key.
//
// TeamID is optional: omit it to get a personal team auto-created alongside
// the key (the new key becomes its sole owner). Pass an existing team_id to
// add the key to a shared team as TeamRole (defaults to "member").
type CreateKeyRequest struct {
	Name                string  `json:"name" binding:"required"`
	Description         string  `json:"description,omitempty"`
	Role                string  `json:"role"`
	TeamID              string  `json:"team_id,omitempty"`
	TeamRole            string  `json:"team_role,omitempty"`
	ExpiresAt           *string `json:"expires_at,omitempty"`
	MaxParallelRequests int     `json:"max_parallel_requests,omitempty"`
	RPMLimit            int     `json:"rpm_limit,omitempty"`
	TPMLimit            int     `json:"tpm_limit,omitempty"`
	SpendLimit          float64 `json:"spend_limit,omitempty"`
	ResetPeriod         string  `json:"reset_period,omitempty"`
	// DefaultMaxTokens controls the per-request budget reservation
	// estimate. Reservation cost = DefaultMaxTokens × 10 microUSD. The
	// fallback (1024) reserves $0.01024 per request, so any spend_limit
	// below that trips at first reservation regardless of actual cost.
	// Tight-budget keys MUST set this to match the expected output size.
	DefaultMaxTokens int               `json:"default_max_tokens,omitempty"`
	Metadata         map[string]string `json:"metadata,omitempty"`
}

// CreateKeyResponse is returned when a key is created.
type CreateKeyResponse struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Role     string `json:"role"`
	TeamID   string `json:"team_id"`
	TeamRole string `json:"team_role"`
	RawKey   string `json:"key"` // Shown once, never stored
}

// UpdateKeyRequest is the input for updating a virtual key. TeamID and
// TeamRole are not listed — team binding is immutable after creation.
type UpdateKeyRequest struct {
	Name                *string           `json:"name,omitempty"`
	Description         *string           `json:"description,omitempty"`
	Role                *string           `json:"role,omitempty"`
	Suspended           *bool             `json:"suspended,omitempty"`
	ExpiresAt           *string           `json:"expires_at,omitempty"`
	MaxParallelRequests *int              `json:"max_parallel_requests,omitempty"`
	RPMLimit            *int              `json:"rpm_limit,omitempty"`
	TPMLimit            *int              `json:"tpm_limit,omitempty"`
	SpendLimit          *float64          `json:"spend_limit,omitempty"`
	ResetPeriod         *string           `json:"reset_period,omitempty"`
	DefaultMaxTokens    *int              `json:"default_max_tokens,omitempty"`
	Metadata            map[string]string `json:"metadata,omitempty"`
}

// KeyResponse is the safe API representation of a key (no secrets).
type KeyResponse struct {
	ID          string `json:"id"`
	UUID        string `json:"uuid,omitempty"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Role        string `json:"role"`

	TeamID   string `json:"team_id"`
	TeamRole string `json:"team_role"`

	// Lifecycle / identity
	ExpiresAt  *string `json:"expires_at,omitempty"`
	CreatedAt  string  `json:"created_at,omitempty"`
	UpdatedAt  string  `json:"updated_at,omitempty"`
	LastSeenAt string  `json:"last_seen_at,omitempty"`
	CreatedBy  string  `json:"created_by,omitempty"`
	UpdatedBy  string  `json:"updated_by,omitempty"`

	// Suspension state
	Suspended   bool    `json:"suspended"`
	SuspendedAt *string `json:"suspended_at,omitempty"`
	SuspendedBy string  `json:"suspended_by,omitempty"`

	// Quota config
	MaxParallelRequests int     `json:"max_parallel_requests,omitempty"`
	RPMLimit            int     `json:"rpm_limit,omitempty"`
	TPMLimit            int     `json:"tpm_limit,omitempty"`
	SpendLimit          float64 `json:"spend_limit,omitempty"`
	ResetPeriod         string  `json:"reset_period,omitempty"`
	DefaultMaxTokens    int     `json:"default_max_tokens,omitempty"`

	Metadata  map[string]string `json:"metadata,omitempty"`
	IsExpired bool              `json:"is_expired"`

	// Usage carries the live spend ledger snapshot when the response
	// is built via a path that has access to the SpendTracker
	// (currently GET /keys/:id). Omitted from list responses to keep
	// the per-row payload small — agents wanting the full ledger view
	// should call /keys/:id or /spend/report.
	Usage *KeyUsageResponse `json:"usage,omitempty"`
}

// CreateKey creates a new virtual key and persists it. If req.TeamID is empty,
// a personal team is auto-created and the new key becomes its owner. If a
// team_id is provided, the key joins that team as TeamRole (defaulting to
// "member"). Personal teams can never be joined by a second key — attempting
// to pass a personal team's ID here is rejected by TeamsService.
//
// Write order: team first, then key. Crash window: personal team exists with
// no key → orphan, swept by ReconcilePersonalTeams on next startup.
func (s *KeysService) CreateKey(id string, req *CreateKeyRequest, createdBy string) (*CreateKeyResponse, error) {
	if req.Name == "" {
		return nil, invalidInputf("name is required")
	}
	if err := validateKeyRole(req.Role); err != nil {
		return nil, err
	}
	if err := validateResetPeriod(req.ResetPeriod); err != nil {
		return nil, err
	}
	if reservedKeyIDs[id] {
		return nil, invalidInputf("%s", reservedNameMessage("key id", id, "/zzrouter/v1/keys/", reservedKeyIDs))
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	role := req.Role
	if role == "" {
		role = "user"
	}

	// Resolve team binding.
	var (
		teamID          string
		teamRole        string
		createdPersonal bool
	)
	// Validate team_role before either branch. The personal-team branch
	// below assigns owner regardless, so a role sent with no team_id
	// used to be dropped without a word and answered 201 -- which tells
	// the caller the value they sent was applied.
	if req.TeamRole != "" && req.TeamRole != string(teams.RoleOwner) && req.TeamRole != string(teams.RoleMember) {
		return nil, invalidInputf("invalid team_role %q (expected %q or %q)",
			req.TeamRole, teams.RoleOwner, teams.RoleMember)
	}

	switch {
	case req.TeamID == "":
		// Auto-create a personal team; the new key is its sole owner.
		if s.teams == nil {
			return nil, errors.New("cannot auto-create personal team: teams service not wired")
		}
		personalID := personalTeamIDPrefix + id
		if _, err := s.teams.CreatePersonalTeam(personalID, req.Name, createdBy); err != nil {
			return nil, fmt.Errorf("auto-create personal team: %w", err)
		}
		teamID = personalID
		teamRole = string(teams.RoleOwner)
		createdPersonal = true
	default:
		// Joining an existing team — verify it exists and is shared.
		if s.teams == nil {
			return nil, errors.New("teams service not wired")
		}
		team := s.teams.store.Get(req.TeamID)
		if team == nil {
			return nil, invalidInputf("team %q: %w", req.TeamID, teams.ErrTeamNotFound)
		}
		if team.Kind == teams.KindPersonal {
			return nil, invalidInputf("team %q is personal and cannot accept additional keys", req.TeamID)
		}
		teamID = req.TeamID
		teamRole = req.TeamRole
		if teamRole == "" {
			teamRole = string(teams.RoleMember)
		}
	}

	vk := &keys.VirtualKey{
		Name:        req.Name,
		Description: req.Description,
		Role:        role,
		TeamID:      teamID,
		TeamRole:    teamRole,
		Metadata:    req.Metadata,
		CreatedBy:   createdBy,
		UpdatedBy:   createdBy,
	}
	vk.MaxParallelRequests = req.MaxParallelRequests
	vk.RPMLimit = req.RPMLimit
	vk.TPMLimit = req.TPMLimit
	vk.SpendLimit = req.SpendLimit
	vk.ResetPeriod = req.ResetPeriod
	vk.DefaultMaxTokens = req.DefaultMaxTokens

	if req.ExpiresAt != nil {
		t, err := time.Parse(time.RFC3339, *req.ExpiresAt)
		if err != nil {
			return nil, invalidInputf("invalid expires_at: %w", err)
		}
		vk.ExpiresAt = &t
	}

	rawKey, err := s.store.Create(id, vk)
	if err != nil {
		// Roll back the auto-created personal team so we don't leak an
		// orphan. A failure here is recoverable by the reconciliation
		// sweep on next startup, but eagerly rolling back keeps the
		// working directory clean for humans reading the yaml.
		if createdPersonal {
			if rollbackErr := s.teams.DeletePersonalTeam(teamID, createdBy); rollbackErr != nil {
				// Don't mask the original error, but make sure operators
				// see the orphan — it becomes the reconciliation sweep's
				// problem otherwise.
				slog.Warn("failed to roll back personal team after key create failure",
					"team", teamID, "error", rollbackErr)
			}
		}
		return nil, err
	}

	if err := s.store.Save(); err != nil {
		return nil, fmt.Errorf("key created but failed to persist: %w", err)
	}

	if s.access != nil {
		s.access.InvalidateAuthCache()
	}

	audit.Log(context.Background(), s.audit, audit.Event{
		Action:  audit.ActionKeyCreated,
		Actor:   resolveActor(createdBy),
		Subject: id,
		Metadata: map[string]any{
			"team_id":          teamID,
			"team_role":        teamRole,
			"created_personal": createdPersonal,
			"role":             role,
		},
	})

	return &CreateKeyResponse{
		ID:       id,
		Name:     vk.Name,
		Role:     vk.Role,
		TeamID:   vk.TeamID,
		TeamRole: vk.TeamRole,
		RawKey:   rawKey,
	}, nil
}

// GetKey returns a safe representation of a key plus its live usage
// snapshot inline (agents otherwise need a follow-up /keys/:id/usage
// call to get the full picture).
func (s *KeysService) GetKey(id string) (*KeyResponse, error) {
	vk := s.store.Get(id)
	if vk == nil {
		return nil, fmt.Errorf("key %q: %w", id, keys.ErrKeyNotFound)
	}
	resp := toKeyResponse(id, vk)
	if usage, err := s.GetKeyUsage(id); err == nil {
		resp.Usage = usage
	}
	return resp, nil
}

// ListKeys returns all keys in safe representation.
func (s *KeysService) ListKeys() []*KeyResponse {
	all := s.store.List()
	result := make([]*KeyResponse, 0, len(all))
	for id, vk := range all {
		result = append(result, toKeyResponse(id, vk))
	}
	return result
}

// UpdateKey updates mutable fields of a key. TeamID/TeamRole are immutable
// and not accepted here.
func (s *KeysService) UpdateKey(id string, req *UpdateKeyRequest, updatedBy string) (*KeyResponse, error) {
	if req.Role != nil {
		if err := validateKeyRole(*req.Role); err != nil {
			return nil, err
		}
	}
	if req.ResetPeriod != nil {
		if err := validateResetPeriod(*req.ResetPeriod); err != nil {
			return nil, err
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	existing := s.store.Get(id)
	if existing == nil {
		return nil, fmt.Errorf("key %q: %w", id, keys.ErrKeyNotFound)
	}

	updated := *existing
	if req.Name != nil {
		updated.Name = *req.Name
	}
	if req.Description != nil {
		updated.Description = *req.Description
	}
	if req.Role != nil {
		updated.Role = *req.Role
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
	if req.ExpiresAt != nil {
		t, err := time.Parse(time.RFC3339, *req.ExpiresAt)
		if err != nil {
			return nil, invalidInputf("invalid expires_at: %w", err)
		}
		updated.ExpiresAt = &t
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
		updated.Metadata = req.Metadata
	}
	updated.UpdatedBy = updatedBy

	if err := s.store.Update(id, &updated); err != nil {
		return nil, err
	}

	if err := s.store.Save(); err != nil {
		return nil, fmt.Errorf("key updated but failed to persist: %w", err)
	}

	if s.access != nil {
		s.access.InvalidateAuthCache()
	}

	audit.Log(context.Background(), s.audit, audit.Event{
		Action:   audit.ActionKeyUpdated,
		Actor:    resolveActor(updatedBy),
		Subject:  id,
		Metadata: updateKeyChangedFields(req),
	})

	return toKeyResponse(id, s.store.Get(id)), nil
}

// DeleteKey removes a key. If the key was the sole owner of a personal team,
// the personal team is cascade-deleted too. Shared teams are never auto-deleted.
//
// Delete order: key first, then team. Crash window: key deleted, personal
// team lingers → orphan, swept by ReconcilePersonalTeams on next startup.
func (s *KeysService) DeleteKey(id, actor string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	existing := s.store.Get(id)
	if existing == nil {
		return fmt.Errorf("key %q: %w", id, keys.ErrKeyNotFound)
	}
	teamID := existing.TeamID

	if err := s.store.Delete(id); err != nil {
		return err
	}

	if err := s.store.Save(); err != nil {
		return fmt.Errorf("key deleted but failed to persist: %w", err)
	}

	if s.access != nil {
		s.access.OnKeyDeleted(id)
	}

	audit.Log(context.Background(), s.audit, audit.Event{
		Action:   audit.ActionKeyDeleted,
		Actor:    resolveActor(actor),
		Subject:  id,
		Metadata: map[string]any{"team_id": teamID},
	})

	// Cascade-delete personal team if this key was its sole member.
	if teamID != "" && s.teams != nil {
		if err := s.teams.DeletePersonalTeam(teamID, actor); err != nil && !errors.Is(err, teams.ErrTeamNotFound) {
			// Not a personal team, or already gone — either is fine.
			_ = err
		}
	}
	return nil
}

// RotateKey generates a new raw key value and persists the change.
func (s *KeysService) RotateKey(id, actor string) (*CreateKeyResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	vk := s.store.Get(id)
	if vk == nil {
		return nil, fmt.Errorf("key %q: %w", id, keys.ErrKeyNotFound)
	}

	rawKey, err := s.store.RotateKey(id)
	if err != nil {
		return nil, err
	}

	if err := s.store.Save(); err != nil {
		return nil, fmt.Errorf("key rotated but failed to persist: %w", err)
	}

	if s.access != nil {
		s.access.InvalidateAuthCache()
	}

	audit.Log(context.Background(), s.audit, audit.Event{
		Action:  audit.ActionKeyRotated,
		Actor:   resolveActor(actor),
		Subject: id,
	})

	return &CreateKeyResponse{
		ID:       id,
		Name:     vk.Name,
		Role:     vk.Role,
		TeamID:   vk.TeamID,
		TeamRole: vk.TeamRole,
		RawKey:   rawKey,
	}, nil
}

// ReloadKeys re-reads keys from disk and invalidates the cache.
func (s *KeysService) ReloadKeys() error {
	if err := s.store.Reload(); err != nil {
		return err
	}
	if s.access != nil {
		s.access.InvalidateAuthCache()
	}
	return nil
}

// KeyUsageResponse is the response for GET /keys/:id/usage.
type KeyUsageResponse struct {
	KeyID        string  `json:"key_id"`
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

// SpendReportResponse is the response for GET /spend/report.
type SpendReportResponse struct {
	Keys       []KeyUsageResponse `json:"keys"`
	TotalSpend float64            `json:"total_spend_usd"`
}

// GetKeyUsage returns live usage/spend data for a key.
func (s *KeysService) GetKeyUsage(id string) (*KeyUsageResponse, error) {
	vk := s.store.Get(id)
	if vk == nil {
		return nil, fmt.Errorf("key %q: %w", id, keys.ErrKeyNotFound)
	}

	resp := &KeyUsageResponse{
		KeyID:      id,
		RPMLimit:   vk.RPMLimit,
		TPMLimit:   vk.TPMLimit,
		SpendLimit: vk.SpendLimit,
		Period:     vk.ResetPeriod,
	}

	if s.access != nil {
		if state := s.access.GetKeySpend(id); state != nil {
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

// ResetKeyUsage clears the spend/usage ledger for a key.
func (s *KeysService) ResetKeyUsage(id, actor string) error {
	if s.store.Get(id) == nil {
		return fmt.Errorf("key %q: %w", id, keys.ErrKeyNotFound)
	}
	if s.access != nil {
		s.access.ResetKeySpend(id)
	}
	audit.Log(context.Background(), s.audit, audit.Event{
		Action:  audit.ActionKeyUsageReset,
		Actor:   resolveActor(actor),
		Subject: id,
	})
	return nil
}

// GetSpendReport returns aggregated spend for every key with recorded traffic.
func (s *KeysService) GetSpendReport() *SpendReportResponse {
	resp := &SpendReportResponse{Keys: []KeyUsageResponse{}}
	if s.access == nil {
		return resp
	}

	states := s.access.GetAllSpendStates()
	for _, state := range states {
		if state == nil || state.Kind != quota.ScopeKey {
			continue
		}

		item := KeyUsageResponse{
			KeyID:        state.EntityID,
			SpendUSD:     state.SpendUSD(),
			ReservedUSD:  quota.MicroToUSD(state.ReservedMicro),
			TokensIn:     state.TokensIn,
			TokensOut:    state.TokensOut,
			RequestCount: state.RequestCount,
		}
		if !state.PeriodStart.IsZero() {
			item.PeriodStart = state.PeriodStart.Format(time.RFC3339)
		}
		if state.Period != "" {
			item.Period = string(state.Period)
		}
		if vk := s.store.Get(state.EntityID); vk != nil {
			item.SpendLimit = vk.SpendLimit
			item.RPMLimit = vk.RPMLimit
			item.TPMLimit = vk.TPMLimit
		}
		resp.Keys = append(resp.Keys, item)
		resp.TotalSpend += item.SpendUSD
	}
	return resp
}

func toKeyResponse(id string, vk *keys.VirtualKey) *KeyResponse {
	resp := &KeyResponse{
		ID:                  id,
		UUID:                vk.UUID,
		Name:                vk.Name,
		Description:         vk.Description,
		Role:                vk.Role,
		TeamID:              vk.TeamID,
		TeamRole:            vk.TeamRole,
		MaxParallelRequests: vk.MaxParallelRequests,
		RPMLimit:            vk.RPMLimit,
		TPMLimit:            vk.TPMLimit,
		SpendLimit:          vk.SpendLimit,
		ResetPeriod:         vk.ResetPeriod,
		DefaultMaxTokens:    vk.DefaultMaxTokens,
		Metadata:            vk.Metadata,
		Suspended:           vk.Suspended,
		SuspendedBy:         vk.SuspendedBy,
		CreatedBy:           vk.CreatedBy,
		UpdatedBy:           vk.UpdatedBy,
		IsExpired:           vk.IsExpired(),
	}
	if vk.ExpiresAt != nil {
		s := vk.ExpiresAt.Format(time.RFC3339)
		resp.ExpiresAt = &s
	}
	if vk.SuspendedAt != nil {
		s := vk.SuspendedAt.Format(time.RFC3339)
		resp.SuspendedAt = &s
	}
	if !vk.CreatedAt.IsZero() {
		resp.CreatedAt = vk.CreatedAt.Format(time.RFC3339)
	}
	if !vk.UpdatedAt.IsZero() {
		resp.UpdatedAt = vk.UpdatedAt.Format(time.RFC3339)
	}
	if !vk.LastSeenAt.IsZero() {
		resp.LastSeenAt = vk.LastSeenAt.Format(time.RFC3339)
	}
	return resp
}
