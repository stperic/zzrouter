package filestore

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"sync"
	"sync/atomic"

	"github.com/stperic/common/atomicfile"

	"github.com/google/uuid"
	"github.com/stperic/zzrouter/pkg/access/teams"
	"github.com/stperic/zzrouter/pkg/utils"
	"gopkg.in/yaml.v3"
)

// teamsFile is the on-disk format for teams.yaml.
type teamsFile struct {
	Version string                 `yaml:"version"`
	Teams   map[string]*teams.Team `yaml:"teams,omitempty"`
}

// Teams implements teams.TeamStore backed by teams.yaml.
type Teams struct {
	mu    sync.RWMutex
	teams map[string]*teams.Team
	path  string

	// hasGatedTeam is a lock-free snapshot of any model or provider restriction.
	// Read on the anonymous compat hot path without touching the store
	// mutex — this is the gate that stops unauthenticated /v1/* and /api/*
	// traffic on a node where any team restricts models or providers.
	hasGatedTeam atomic.Bool
}

// HasGatedTeam reports whether any stored team restricts models or providers.
// Lock-free; safe to call on the request hot path.
func (s *Teams) HasGatedTeam() bool {
	return s.hasGatedTeam.Load()
}

// recomputeGated walks the current team map and refreshes hasGatedTeam.
// Must be called with the write lock held.
func (s *Teams) recomputeGated() {
	for _, t := range s.teams {
		if len(t.AllowedModels) > 0 || t.ProviderAccess.Restricted() || t.RouteAccess.Restricted() {
			s.hasGatedTeam.Store(true)
			return
		}
	}
	s.hasGatedTeam.Store(false)
}

// NewTeams creates a team store that reads/writes to the given file path.
func NewTeams(path string) *Teams {
	return &Teams{
		teams: make(map[string]*teams.Team),
		path:  path,
	}
}

// Load reads teams from teams.yaml.
func (s *Teams) Load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return fmt.Errorf("failed to read teams config: %w", err)
	}

	loaded, err := DecodeTeams(data)
	if err != nil {
		return err
	}

	s.mu.Lock()
	s.teams = loaded
	s.recomputeGated()
	s.mu.Unlock()

	return nil
}

// DecodeTeams validates one persisted policy set without installing it.
func DecodeTeams(data []byte) (map[string]*teams.Team, error) {
	var cfg teamsFile
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("failed to parse teams config: %w", err)
	}

	// Decode effective mappings as well: YAML null (including aliases/merges)
	// skips a value unmarshaller, but must not mean an omitted legacy policy.
	var shape struct {
		Teams map[string]map[string]any `yaml:"teams"`
	}
	if err := yaml.Unmarshal(data, &shape); err != nil {
		return nil, err
	}
	for id, fields := range shape.Teams {
		for _, field := range []string{"provider_access", "route_access"} {
			if value, present := fields[field]; present && value == nil {
				return nil, fmt.Errorf("team %q: %s must be a policy object", id, field)
			}
		}
	}

	loaded := make(map[string]*teams.Team, len(cfg.Teams))
	for id, team := range cfg.Teams {
		if team == nil {
			return nil, fmt.Errorf("team %q must be an object", id)
		}
		if err := team.RouteAccess.Validate(); err != nil {
			return nil, err
		}
		if err := team.ProviderAccess.Validate(); err != nil {
			return nil, fmt.Errorf("team %q: %w", id, err)
		}
		team.ID = id
		if team.Kind == "" {
			return nil, fmt.Errorf("team %q: missing kind (expected %q or %q)", id, teams.KindPersonal, teams.KindShared)
		}
		if team.Kind != teams.KindPersonal && team.Kind != teams.KindShared {
			return nil, fmt.Errorf("team %q: invalid kind %q (expected %q or %q)", id, team.Kind, teams.KindPersonal, teams.KindShared)
		}
		loaded[id] = team
	}

	return loaded, nil
}

// Get returns a team by ID, or nil if not found.
func (s *Teams) Get(id string) *teams.Team {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.teams[id]
}

// List returns all teams (shallow copies to prevent mutation of the map).
func (s *Teams) List() map[string]*teams.Team {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make(map[string]*teams.Team, len(s.teams))
	maps.Copy(result, s.teams)
	return result
}

// Create adds a new team.
func (s *Teams) Create(id string, team *teams.Team) error {
	if err := team.RouteAccess.Validate(); err != nil {
		return err
	}
	if err := team.ProviderAccess.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.teams[id]; exists {
		return fmt.Errorf("team %q: %w", id, teams.ErrTeamExists)
	}
	if team.Kind == "" {
		return fmt.Errorf("team %q: kind is required", id)
	}

	team.ID = id
	if team.UUID == "" {
		team.UUID = uuid.New().String()
	}
	now := utils.NowUTC()
	team.CreatedAt = now
	team.UpdatedAt = now

	s.teams[id] = team
	s.recomputeGated()
	return nil
}

// Update replaces an existing team's mutable fields. Kind is preserved from
// the existing record — callers cannot convert a personal team to shared or
// vice versa.
func (s *Teams) Update(id string, team *teams.Team) error {
	if err := team.RouteAccess.Validate(); err != nil {
		return err
	}
	if err := team.ProviderAccess.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	existing, ok := s.teams[id]
	if !ok {
		return fmt.Errorf("team %q: %w", id, teams.ErrTeamNotFound)
	}

	// Preserve immutable fields
	team.ID = id
	team.UUID = existing.UUID
	team.Kind = existing.Kind
	team.CreatedAt = existing.CreatedAt
	team.CreatedBy = existing.CreatedBy
	team.UpdatedAt = utils.NowUTC()

	s.teams[id] = team
	s.recomputeGated()
	return nil
}

// Delete removes a team.
func (s *Teams) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.teams[id]; !ok {
		return fmt.Errorf("team %q: %w", id, teams.ErrTeamNotFound)
	}

	delete(s.teams, id)
	s.recomputeGated()
	return nil
}

// Reload re-reads teams from disk.
func (s *Teams) Reload() error {
	return s.Load()
}

// Save persists the current state to disk.
func (s *Teams) Save() error {
	return s.save(atomicfile.WriteFile)
}

func (s *Teams) save(write func(string, []byte, os.FileMode) error) error {
	s.mu.RLock()
	cfg := teamsFile{
		Version: "1",
		Teams:   make(map[string]*teams.Team, len(s.teams)),
	}
	maps.Copy(cfg.Teams, s.teams)
	s.mu.RUnlock()

	data, err := yaml.Marshal(&cfg)
	if err != nil {
		return fmt.Errorf("failed to marshal teams: %w", err)
	}

	err = write(s.path, data, 0o600)
	if errors.Is(err, atomicfile.ErrPostRenameDurability) {
		slog.Warn("Team state committed with uncertain durability", "error", err)
		return nil
	}
	return err
}
