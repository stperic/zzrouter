package sqlstore

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/teams"
	"github.com/stperic/zzrouter/pkg/audit"
	"github.com/stperic/zzrouter/pkg/utils"
)

// The operations below are the file adapter's, which the server switch deletes;
// mutate validates every record they change.

func credential() (string, string, error) {
	raw, err := keys.GenerateRawKey()
	if err != nil {
		return "", "", err
	}
	hash, err := keys.HashKey(raw)
	return raw, hash, err
}

// CreateKey stores a new hashed key and returns it with its one-time raw key.
func (s *Store) CreateKey(ctx context.Context, id string, key keys.VirtualKey, ev audit.Event) (*keys.VirtualKey, string, error) {
	raw, hash, err := credential()
	if err != nil {
		return nil, "", err
	}
	created := key.Clone()
	err = s.mutate(ctx, keyRecords, func(r *records) (audit.Event, error) {
		if _, exists := r.keys[id]; exists {
			return audit.Event{}, fmt.Errorf("key %q: %w", id, keys.ErrKeyExists)
		}
		created.ID, created.UUID, created.HashedKey, created.KeyEnv = id, uuid.NewString(), hash, ""
		created.CreatedAt = utils.NowUTC()
		created.UpdatedAt = created.CreatedAt
		r.keys[id] = created
		return ev, nil
	})
	if err != nil {
		return nil, "", err
	}
	return created.Clone(), raw, nil
}

// UpdateKey applies apply to a copy, then restores the store-owned fields.
func (s *Store) UpdateKey(ctx context.Context, id string, apply func(*keys.VirtualKey) error, ev audit.Event) (*keys.VirtualKey, error) {
	return s.replaceKey(ctx, id, ev, func(existing *keys.VirtualKey) (*keys.VirtualKey, error) {
		next := existing.Clone()
		if err := apply(next); err != nil {
			return nil, err
		}
		next = next.Clone()
		next.ID, next.UUID, next.HashedKey, next.KeyEnv = id, existing.UUID, existing.HashedKey, existing.KeyEnv
		next.TeamID, next.TeamRole = existing.TeamID, existing.TeamRole
		next.CreatedAt, next.CreatedBy = existing.CreatedAt, existing.CreatedBy
		return next, nil
	})
}

// RotateKey replaces the credential and returns the new one-time raw key.
func (s *Store) RotateKey(ctx context.Context, id string, ev audit.Event) (*keys.VirtualKey, string, error) {
	raw, hash, err := credential()
	if err != nil {
		return nil, "", err
	}
	rotated, err := s.replaceKey(ctx, id, ev, func(existing *keys.VirtualKey) (*keys.VirtualKey, error) {
		next := existing.Clone()
		next.HashedKey, next.KeyEnv = hash, ""
		return next, nil
	})
	if err != nil {
		return nil, "", err
	}
	return rotated, raw, nil
}

func (s *Store) replaceKey(ctx context.Context, id string, ev audit.Event, replace func(*keys.VirtualKey) (*keys.VirtualKey, error)) (*keys.VirtualKey, error) {
	var next *keys.VirtualKey
	err := s.mutate(ctx, keyRecords, func(r *records) (audit.Event, error) {
		existing, ok := r.keys[id]
		if !ok {
			return audit.Event{}, fmt.Errorf("key %q: %w", id, keys.ErrKeyNotFound)
		}
		var err error
		if next, err = replace(existing); err != nil {
			return audit.Event{}, err
		}
		next.UpdatedAt = utils.NowUTC()
		r.keys[id] = next
		return ev, nil
	})
	if err != nil {
		return nil, err
	}
	return next.Clone(), nil
}

// DeleteKey removes a key.
func (s *Store) DeleteKey(ctx context.Context, id string, ev audit.Event) error {
	return s.mutate(ctx, keyRecords, func(r *records) (audit.Event, error) {
		if _, ok := r.keys[id]; !ok {
			return audit.Event{}, fmt.Errorf("key %q: %w", id, keys.ErrKeyNotFound)
		}
		delete(r.keys, id)
		return ev, nil
	})
}

// CreateTeam stores a new team.
func (s *Store) CreateTeam(ctx context.Context, id string, team teams.Team, ev audit.Event) (*teams.Team, error) {
	created := team.Clone()
	err := s.mutate(ctx, teamRecords, func(r *records) (audit.Event, error) {
		if _, exists := r.teams[id]; exists {
			return audit.Event{}, fmt.Errorf("team %q: %w", id, teams.ErrTeamExists)
		}
		created.ID, created.UUID = id, uuid.NewString()
		created.CreatedAt = utils.NowUTC()
		created.UpdatedAt = created.CreatedAt
		r.teams[id] = created
		return ev, nil
	})
	if err != nil {
		return nil, err
	}
	return created.Clone(), nil
}

// UpdateTeam applies apply to a copy, then restores the store-owned fields.
func (s *Store) UpdateTeam(ctx context.Context, id string, apply func(*teams.Team) error, ev audit.Event) (*teams.Team, error) {
	var next *teams.Team
	err := s.mutate(ctx, teamRecords, func(r *records) (audit.Event, error) {
		existing, ok := r.teams[id]
		if !ok {
			return audit.Event{}, fmt.Errorf("team %q: %w", id, teams.ErrTeamNotFound)
		}
		next = existing.Clone()
		if err := apply(next); err != nil {
			return audit.Event{}, err
		}
		next = next.Clone()
		next.ID, next.UUID, next.Kind = id, existing.UUID, existing.Kind
		next.CreatedAt, next.CreatedBy = existing.CreatedAt, existing.CreatedBy
		next.UpdatedAt = utils.NowUTC()
		r.teams[id] = next
		return ev, nil
	})
	if err != nil {
		return nil, err
	}
	return next.Clone(), nil
}

// DeleteTeam removes a team.
func (s *Store) DeleteTeam(ctx context.Context, id string, ev audit.Event) error {
	return s.mutate(ctx, teamRecords, func(r *records) (audit.Event, error) {
		if _, ok := r.teams[id]; !ok {
			return audit.Event{}, fmt.Errorf("team %q: %w", id, teams.ErrTeamNotFound)
		}
		delete(r.teams, id)
		return ev, nil
	})
}
