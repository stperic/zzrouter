package teams

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"sync"
	"sync/atomic"

	"github.com/google/uuid"
	"github.com/stperic/zzrouter/pkg/utils"
	"gopkg.in/yaml.v3"
)

var (
	ErrTeamNotFound   = errors.New("team not found")
	ErrTeamExists     = errors.New("team already exists")
	ErrTeamHasMembers = errors.New("team has member keys")
)

// teamsFile is the on-disk format for teams.yaml.
type teamsFile struct {
	Version string           `yaml:"version"`
	Teams   map[string]*Team `yaml:"teams,omitempty"`
}

// FileTeamStore implements TeamStore backed by teams.yaml.
type FileTeamStore struct {
	mu    sync.RWMutex
	teams map[string]*Team
	path  string

	// hasGatedTeam is a lock-free snapshot of "any stored team has AllowedModels
	// set". Read on the anonymous compat hot path without touching the store
	// mutex — this is the gate that stops unauthenticated /v1/* and /api/*
	// traffic on a node where any team restricts models.
	hasGatedTeam atomic.Bool
}

// HasGatedTeam reports whether any stored team has AllowedModels set.
// Lock-free; safe to call on the request hot path.
func (s *FileTeamStore) HasGatedTeam() bool {
	return s.hasGatedTeam.Load()
}

// recomputeGated walks the current team map and refreshes hasGatedTeam.
// Must be called with the write lock held.
func (s *FileTeamStore) recomputeGated() {
	for _, t := range s.teams {
		if len(t.AllowedModels) > 0 {
			s.hasGatedTeam.Store(true)
			return
		}
	}
	s.hasGatedTeam.Store(false)
}

// NewFileTeamStore creates a team store that reads/writes to the given file path.
func NewFileTeamStore(path string) *FileTeamStore {
	return &FileTeamStore{
		teams: make(map[string]*Team),
		path:  path,
	}
}

// Load reads teams from teams.yaml.
func (s *FileTeamStore) Load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return fmt.Errorf("failed to read teams config: %w", err)
	}

	var cfg teamsFile
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("failed to parse teams config: %w", err)
	}

	loaded := make(map[string]*Team, len(cfg.Teams))
	for id, team := range cfg.Teams {
		team.ID = id
		if team.Kind == "" {
			return fmt.Errorf("team %q: missing kind (expected %q or %q)", id, KindPersonal, KindShared)
		}
		if team.Kind != KindPersonal && team.Kind != KindShared {
			return fmt.Errorf("team %q: invalid kind %q (expected %q or %q)", id, team.Kind, KindPersonal, KindShared)
		}
		loaded[id] = team
	}

	s.mu.Lock()
	s.teams = loaded
	s.recomputeGated()
	s.mu.Unlock()

	return nil
}

// Get returns a team by ID, or nil if not found.
func (s *FileTeamStore) Get(id string) *Team {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.teams[id]
}

// List returns all teams (shallow copies to prevent mutation of the map).
func (s *FileTeamStore) List() map[string]*Team {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make(map[string]*Team, len(s.teams))
	maps.Copy(result, s.teams)
	return result
}

// Create adds a new team.
func (s *FileTeamStore) Create(id string, team *Team) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.teams[id]; exists {
		return fmt.Errorf("team %q: %w", id, ErrTeamExists)
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
func (s *FileTeamStore) Update(id string, team *Team) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	existing, ok := s.teams[id]
	if !ok {
		return fmt.Errorf("team %q: %w", id, ErrTeamNotFound)
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
func (s *FileTeamStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.teams[id]; !ok {
		return fmt.Errorf("team %q: %w", id, ErrTeamNotFound)
	}

	delete(s.teams, id)
	s.recomputeGated()
	return nil
}

// Reload re-reads teams from disk.
func (s *FileTeamStore) Reload() error {
	return s.Load()
}

// Save persists the current state to disk.
func (s *FileTeamStore) Save() error {
	s.mu.RLock()
	cfg := teamsFile{
		Version: "1",
		Teams:   make(map[string]*Team, len(s.teams)),
	}
	maps.Copy(cfg.Teams, s.teams)
	s.mu.RUnlock()

	data, err := yaml.Marshal(&cfg)
	if err != nil {
		return fmt.Errorf("failed to marshal teams: %w", err)
	}

	return utils.AtomicWriteFile(s.path, data, 0o600)
}
