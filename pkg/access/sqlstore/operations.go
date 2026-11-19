package sqlstore

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/stperic/zzrouter/pkg/access"
	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/people"
	"github.com/stperic/zzrouter/pkg/access/teams"
	"github.com/stperic/zzrouter/pkg/audit"
	"github.com/stperic/zzrouter/pkg/utils"
)

// ownerWorkspace marks the shared team a setup grant enrolls the owner into.
const ownerWorkspace = "owner_workspace"

func credential() (string, string, error) {
	raw, err := keys.GenerateRawKey()
	if err != nil {
		return "", "", err
	}
	hash, err := keys.HashKey(raw)
	return raw, hash, err
}

// newTeam is team as the store creates it, with its ID, UUID and timestamps.
func newTeam(id string, team teams.Team, now time.Time) *teams.Team {
	created := team.Clone()
	created.ID, created.UUID = id, uuid.NewString()
	created.CreatedAt, created.UpdatedAt = now, now
	return created
}

// CreateKey stores a new hashed key, with its personal team when key names
// none, and returns it with its one-time raw key.
func (s *Store) CreateKey(ctx context.Context, id string, key keys.VirtualKey, ev audit.Event) (*keys.VirtualKey, string, error) {
	raw, hash, err := credential()
	if err != nil {
		return nil, "", err
	}
	created := key.Clone()
	err = s.mutate(ctx, keyRecords|teamRecords, func(t *tx) error {
		if _, exists := t.keys[id]; exists {
			return fmt.Errorf("key %q: %w", id, keys.ErrKeyExists)
		}
		created.ID, created.UUID, created.HashedKey = id, uuid.NewString(), hash
		created.CreatedAt = utils.NowUTC()
		created.UpdatedAt = created.CreatedAt
		personal := created.TeamID == ""
		if personal {
			created.TeamID, created.TeamRole = teams.PersonalID(id), string(teams.RoleOwner)
			team := teams.Team{Name: "Personal: " + key.Name, Kind: teams.KindPersonal, CreatedBy: key.CreatedBy, UpdatedBy: key.CreatedBy}
			t.teams[created.TeamID] = newTeam(created.TeamID, team, created.CreatedAt)
			t.record(alongside(ev, audit.ActionTeamCreated, created.TeamID, map[string]any{"kind": string(teams.KindPersonal), "key_name": key.Name}))
		}
		t.keys[id] = created
		t.record(stamped(ev, map[string]any{"team_id": created.TeamID, "team_role": created.TeamRole, "created_personal": personal}))
		return nil
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
		next.ID, next.UUID, next.HashedKey = id, existing.UUID, existing.HashedKey
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
		next.HashedKey = hash
		return next, nil
	})
	if err != nil {
		return nil, "", err
	}
	return rotated, raw, nil
}

func (s *Store) replaceKey(ctx context.Context, id string, ev audit.Event, replace func(*keys.VirtualKey) (*keys.VirtualKey, error)) (*keys.VirtualKey, error) {
	var next *keys.VirtualKey
	err := s.mutate(ctx, keyRecords, func(t *tx) error {
		existing, ok := t.keys[id]
		if !ok {
			return fmt.Errorf("key %q: %w", id, keys.ErrKeyNotFound)
		}
		var err error
		if next, err = replace(existing); err != nil {
			return err
		}
		next.UpdatedAt = utils.NowUTC()
		t.keys[id] = next
		t.record(ev)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return next.Clone(), nil
}

// DeleteKey removes a key and its personal team.
func (s *Store) DeleteKey(ctx context.Context, id string, ev audit.Event) error {
	return s.mutate(ctx, keyRecords|teamRecords, func(t *tx) error {
		key, ok := t.keys[id]
		if !ok {
			return fmt.Errorf("key %q: %w", id, keys.ErrKeyNotFound)
		}
		delete(t.keys, id)
		t.record(stamped(ev, map[string]any{"team_id": key.TeamID}))
		if team := t.teams[key.TeamID]; team != nil && team.Kind == teams.KindPersonal {
			delete(t.teams, key.TeamID)
			t.record(alongside(ev, audit.ActionTeamDeleted, key.TeamID, map[string]any{"kind": string(teams.KindPersonal), "cascade": true}))
		}
		return nil
	})
}

// CreateTeam stores a new team.
func (s *Store) CreateTeam(ctx context.Context, id string, team teams.Team, ev audit.Event) (*teams.Team, error) {
	var created *teams.Team
	err := s.mutate(ctx, teamRecords, func(t *tx) error {
		if _, exists := t.teams[id]; exists {
			return fmt.Errorf("team %q: %w", id, teams.ErrTeamExists)
		}
		created = newTeam(id, team, utils.NowUTC())
		t.teams[id] = created
		t.record(ev)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return created.Clone(), nil
}

// UpdateTeam applies apply to a copy of a shared team, then restores the
// store-owned fields.
func (s *Store) UpdateTeam(ctx context.Context, id string, apply func(*teams.Team) error, ev audit.Event) (*teams.Team, error) {
	var next *teams.Team
	err := s.mutate(ctx, teamRecords, func(t *tx) error {
		existing, ok := t.teams[id]
		if !ok {
			return fmt.Errorf("team %q: %w", id, teams.ErrTeamNotFound)
		}
		if err := existing.CheckMutable(); err != nil {
			return err
		}
		next = existing.Clone()
		if err := apply(next); err != nil {
			return err
		}
		next = next.Clone()
		next.ID, next.UUID, next.Kind = id, existing.UUID, existing.Kind
		next.CreatedAt, next.CreatedBy = existing.CreatedAt, existing.CreatedBy
		next.UpdatedAt = utils.NowUTC()
		t.teams[id] = next
		t.record(ev)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return next.Clone(), nil
}

// DeleteTeam removes a shared team that nothing depends on.
func (s *Store) DeleteTeam(ctx context.Context, id string, ev audit.Event) error {
	return s.mutate(ctx, teamRecords, func(t *tx) error {
		team, ok := t.teams[id]
		if !ok {
			return fmt.Errorf("team %q: %w", id, teams.ErrTeamNotFound)
		}
		if err := access.DeletionRefusal(team, access.MembershipOf(team, t.keys, t.people)); err != nil {
			return err
		}
		delete(t.teams, id)
		t.record(ev)
		return nil
	})
}

// IssueSetup issues the owner's setup grant into the owner's workspace team,
// creating that team in the same commit when no usable one exists.
func (s *Store) IssueSetup(ctx context.Context, issuer people.Issuer) (*people.IssuedGrant, error) {
	if s.people == nil {
		return nil, access.ErrClosed // a store opened without people offers no people operation
	}
	var token string
	var committed *tx
	err := s.mutate(ctx, teamRecords|peopleRecords, func(t *tx) error {
		committed = t
		id := ""
		for teamID, team := range t.teams {
			if team.Kind == teams.KindShared && !team.Suspended && team.Metadata[ownerWorkspace] == "true" {
				id = teamID
			}
		}
		if id == "" {
			id = "workspace-" + uuid.NewString()
			workspace := teams.Team{Name: "Personal workspace", Kind: teams.KindShared, CreatedBy: "installation", Metadata: map[string]string{ownerWorkspace: "true"}}
			t.teams[id] = newTeam(id, workspace, utils.NowUTC())
		}
		var ev audit.Event
		var err error
		token, ev, err = s.people.IssueOn(ctx, t.people, teamCheck(t.teams), people.GrantRequest{Purpose: people.PurposeSetup, TeamID: id, Role: "admin", Issuer: issuer})
		t.record(ev)
		return err
	})
	if err != nil {
		return nil, err
	}
	return committed.people.Issued(token), nil
}
