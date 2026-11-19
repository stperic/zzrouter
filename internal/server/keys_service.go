package server

import (
	"cmp"
	"context"
	"fmt"
	"time"

	"github.com/stperic/zzrouter/pkg/access"
	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/quota"
	"github.com/stperic/zzrouter/pkg/access/teams"
	"github.com/stperic/zzrouter/pkg/audit"
	"github.com/stperic/zzrouter/pkg/utils"
)

// KeysService provides business logic for virtual key management. A key
// created without a team gets its personal team from the store, in the same
// transaction.
type KeysService struct {
	store  access.Store
	access *AccessControl
}

// NewKeysService creates a new keys service.
func NewKeysService(store access.Store, access *AccessControl) *KeysService {
	return &KeysService{store: store, access: access}
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
// the key gets its personal team and becomes its owner. If a team_id is
// provided, the key joins that shared team as TeamRole (defaulting to
// "member"); the store refuses a missing or personal team. A malformed
// request is refused before any record is consulted.
func (s *KeysService) CreateKey(ctx context.Context, id string, req *CreateKeyRequest, createdBy string) (*CreateKeyResponse, error) {
	if req.Name == "" {
		return nil, invalidInputf("name is required")
	}
	if reservedKeyIDs[id] {
		return nil, invalidInputf("%s", reservedNameMessage("key id", id, "/zzrouter/v1/keys/", reservedKeyIDs))
	}
	role := cmp.Or(req.Role, keys.RoleUser)
	if err := validateKeyRole(role); err != nil {
		return nil, err
	}
	// A team_role sent without a team_id is ignored, but it is malformed all the same.
	if req.TeamRole != "" && !teams.MemberRole(req.TeamRole).Valid() {
		return nil, invalidInputf("invalid team_role %q (expected %q or %q)", req.TeamRole, teams.RoleOwner, teams.RoleMember)
	}
	if err := validateResetPeriod(req.ResetPeriod); err != nil {
		return nil, err
	}
	expiresAt, err := parseExpiry(req.ExpiresAt)
	if err != nil {
		return nil, err
	}

	vk := keys.VirtualKey{
		Name:        req.Name,
		Description: req.Description,
		Role:        role,
		TeamID:      req.TeamID,
		Metadata:    req.Metadata,
		CreatedBy:   createdBy,
		UpdatedBy:   createdBy,
	}
	if req.TeamID != "" {
		vk.TeamRole = cmp.Or(req.TeamRole, string(teams.RoleMember))
	}
	vk.MaxParallelRequests = req.MaxParallelRequests
	vk.RPMLimit = req.RPMLimit
	vk.TPMLimit = req.TPMLimit
	vk.SpendLimit = req.SpendLimit
	vk.ResetPeriod = req.ResetPeriod
	vk.DefaultMaxTokens = req.DefaultMaxTokens
	vk.ExpiresAt = expiresAt

	// The store adds the team facts it decides to the event.
	created, rawKey, err := s.store.CreateKey(ctx, id, vk, audit.Event{
		Action:   audit.ActionKeyCreated,
		Actor:    resolveActor(createdBy),
		Subject:  id,
		Metadata: map[string]any{"role": vk.Role},
	})
	if err != nil {
		return nil, err
	}
	return createKeyResponse(created, rawKey), nil
}

// GetKey returns a safe representation of a key plus its live usage
// snapshot inline (agents otherwise need a follow-up /keys/:id/usage
// call to get the full picture).
func (s *KeysService) GetKey(id string) (*KeyResponse, error) {
	vk := s.store.Snapshot().Key(id)
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
	result := []*KeyResponse{}
	for id, vk := range s.store.Snapshot().Keys() {
		result = append(result, toKeyResponse(id, vk))
	}
	return result
}

// UpdateKey updates mutable fields of a key. TeamID/TeamRole are immutable
// and not accepted here.
func (s *KeysService) UpdateKey(ctx context.Context, id string, req *UpdateKeyRequest, updatedBy string) (*KeyResponse, error) {
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
	expiresAt, err := parseExpiry(req.ExpiresAt)
	if err != nil {
		return nil, err
	}

	updated, err := s.store.UpdateKey(ctx, id, func(updated *keys.VirtualKey) error {
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
		if expiresAt != nil {
			updated.ExpiresAt = expiresAt
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
		return nil
	}, audit.Event{
		Action:   audit.ActionKeyUpdated,
		Actor:    resolveActor(updatedBy),
		Subject:  id,
		Metadata: updateKeyChangedFields(req),
	})
	if err != nil {
		return nil, err
	}
	return toKeyResponse(id, updated), nil
}

// DeleteKey removes a key; the store deletes its personal team with it and
// records the team the deleted key belonged to. Shared teams are never auto-deleted.
func (s *KeysService) DeleteKey(ctx context.Context, id, actor string) error {
	return s.store.DeleteKey(ctx, id, audit.Event{
		Action:  audit.ActionKeyDeleted,
		Actor:   resolveActor(actor),
		Subject: id,
	})
}

// parseExpiry reads an RFC 3339 expires_at; none is nil.
func parseExpiry(value *string) (*time.Time, error) {
	if value == nil {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, *value)
	if err != nil {
		return nil, invalidInputf("invalid expires_at: %w", err)
	}
	return &t, nil
}

// RotateKey generates a new raw key value and persists the change.
func (s *KeysService) RotateKey(ctx context.Context, id, actor string) (*CreateKeyResponse, error) {
	rotated, rawKey, err := s.store.RotateKey(ctx, id, audit.Event{
		Action:  audit.ActionKeyRotated,
		Actor:   resolveActor(actor),
		Subject: id,
	})
	if err != nil {
		return nil, err
	}
	return createKeyResponse(rotated, rawKey), nil
}

func createKeyResponse(vk *keys.VirtualKey, rawKey string) *CreateKeyResponse {
	return &CreateKeyResponse{ID: vk.ID, Name: vk.Name, Role: vk.Role, TeamID: vk.TeamID, TeamRole: vk.TeamRole, RawKey: rawKey}
}

// KeyUsageResponse is the response for GET /keys/:id/usage.
type KeyUsageResponse struct {
	KeyID string `json:"key_id"`
	quota.Usage
}

// SpendReportResponse is the response for GET /spend/report.
type SpendReportResponse struct {
	Keys       []KeyUsageResponse `json:"keys"`
	TotalSpend float64            `json:"total_spend_usd"`
}

// GetKeyUsage returns live usage/spend data for a key.
func (s *KeysService) GetKeyUsage(id string) (*KeyUsageResponse, error) {
	vk := s.store.Snapshot().Key(id)
	if vk == nil {
		return nil, fmt.Errorf("key %q: %w", id, keys.ErrKeyNotFound)
	}

	var state *quota.SpendState
	if s.access != nil {
		state = s.access.GetKeySpend(id)
	}
	resp := &KeyUsageResponse{KeyID: id, Usage: quota.CurrentUsage(vk.QuotaConfig, state)}
	return resp, nil
}

// ResetKeyUsage clears the spend/usage ledger for a key.
func (s *KeysService) ResetKeyUsage(ctx context.Context, id, actor string) error {
	if s.store.Snapshot().Key(id) == nil {
		return fmt.Errorf("key %q: %w", id, keys.ErrKeyNotFound)
	}
	if s.access != nil {
		s.access.ResetKeySpend(id)
	}
	recordReset(ctx, s.store, audit.Event{
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

	snap := s.store.Snapshot()
	states := s.access.GetAllSpendStates()
	for _, state := range states {
		if state == nil || state.Kind != quota.ScopeKey {
			continue
		}

		var config quota.QuotaConfig
		if vk := snap.Key(state.EntityID); vk != nil {
			config = vk.QuotaConfig
		}
		item := KeyUsageResponse{KeyID: state.EntityID, Usage: quota.CurrentUsage(config, state)}
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
