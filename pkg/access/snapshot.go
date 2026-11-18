package access

import (
	"iter"
	"os"

	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/people"
	"github.com/stperic/zzrouter/pkg/access/teams"
)

// PeopleReader is the people state a Snapshot reads; *people.Store satisfies it.
type PeopleReader interface {
	Get(id string) *people.Person
	List() []people.Person
	Owner() *people.Person
	Initialized() bool
	Progress() people.Progress
	Invitations() []people.Grant
	TeamImpact(teamID string) people.TeamDependencies
	OwnerTeam(teamID string) bool
}

// Snapshot is one published state; a nil *Snapshot reads as empty.
// Yielded records are copies. Until step 6 the people methods read the live store.
type Snapshot struct {
	keys       map[string]*keys.VirtualKey
	teams      map[string]*teams.Team
	keysByTeam map[string][]string
	gated      bool
	people     PeopleReader
}

// NewSnapshot owns k and t: the caller never changes them or their records
// again. A nil p means no people.
func NewSnapshot(k map[string]*keys.VirtualKey, t map[string]*teams.Team, p PeopleReader) *Snapshot {
	s := &Snapshot{keys: k, teams: t, keysByTeam: make(map[string][]string), people: p}
	for id, key := range k {
		s.keysByTeam[key.TeamID] = append(s.keysByTeam[key.TeamID], id)
	}
	for _, team := range t {
		s.gated = s.gated || len(team.AllowedModels) > 0 || team.ProviderAccess.Restricted() || team.RouteAccess.Restricted()
	}
	return s
}

// Key returns a copy of one key, or nil.
func (s *Snapshot) Key(id string) *keys.VirtualKey {
	if s == nil {
		return nil
	}
	return s.keys[id].Clone()
}

// Keys yields a copy of every key, in no particular order.
func (s *Snapshot) Keys() iter.Seq2[string, *keys.VirtualKey] {
	return func(yield func(string, *keys.VirtualKey) bool) {
		if s == nil {
			return
		}
		for id, key := range s.keys {
			if !yield(id, key.Clone()) {
				return
			}
		}
	}
}

// KeysByTeam yields a copy of every key bound to teamID, in no particular order.
func (s *Snapshot) KeysByTeam(teamID string) iter.Seq2[string, *keys.VirtualKey] {
	return func(yield func(string, *keys.VirtualKey) bool) {
		if s == nil || teamID == "" {
			return
		}
		for _, id := range s.keysByTeam[teamID] {
			if !yield(id, s.keys[id].Clone()) {
				return
			}
		}
	}
}

// ValidateRawKey compares env-var keys directly and hashed keys with Argon2id.
func (s *Snapshot) ValidateRawKey(raw string) (string, *keys.VirtualKey) {
	if s == nil {
		return "", nil
	}
	for id, key := range s.keys {
		if key.KeyEnv != "" {
			if value := os.Getenv(key.KeyEnv); value != "" && value == raw {
				return id, key.Clone()
			}
			continue
		}
		if key.HashedKey != "" && keys.VerifyKey(raw, key.HashedKey) {
			return id, key.Clone()
		}
	}
	return "", nil
}

// Team returns a copy of one team, or nil.
func (s *Snapshot) Team(id string) *teams.Team {
	if s == nil {
		return nil
	}
	return s.teams[id].Clone()
}

// Teams yields a copy of every team, in no particular order.
func (s *Snapshot) Teams() iter.Seq2[string, *teams.Team] {
	return func(yield func(string, *teams.Team) bool) {
		if s == nil {
			return
		}
		for id, team := range s.teams {
			if !yield(id, team.Clone()) {
				return
			}
		}
	}
}

// HasGatedTeam reports whether any team restricts models, providers or routes.
// The anonymous compat gate reads it on every keyless request.
func (s *Snapshot) HasGatedTeam() bool { return s != nil && s.gated }

func (s *Snapshot) livePeople() PeopleReader {
	if s == nil {
		return nil
	}
	return s.people
}

// Person returns one account, or nil.
func (s *Snapshot) Person(id string) *people.Person {
	if p := s.livePeople(); p != nil {
		return p.Get(id)
	}
	return nil
}

// People lists accounts by username.
func (s *Snapshot) People() []people.Person {
	if p := s.livePeople(); p != nil {
		return p.List()
	}
	return []people.Person{}
}

// Owner returns the initial owner, or nil.
func (s *Snapshot) Owner() *people.Person {
	if p := s.livePeople(); p != nil {
		return p.Owner()
	}
	return nil
}

// Initialized reports the permanent owner-claim marker.
func (s *Snapshot) Initialized() bool {
	p := s.livePeople()
	return p != nil && p.Initialized()
}

// Progress returns the persisted onboarding state.
func (s *Snapshot) Progress() people.Progress {
	if p := s.livePeople(); p != nil {
		return p.Progress()
	}
	return people.Progress{}
}

// Invitations lists invitation and recovery grants, newest first.
func (s *Snapshot) Invitations() []people.Grant {
	if p := s.livePeople(); p != nil {
		return p.Invitations()
	}
	return []people.Grant{}
}

// TeamImpact counts the people and pending enrollments that use teamID.
func (s *Snapshot) TeamImpact(teamID string) people.TeamDependencies {
	if p := s.livePeople(); p != nil {
		return p.TeamImpact(teamID)
	}
	return people.TeamDependencies{}
}

// OwnerTeam reports whether teamID is the owner's team.
func (s *Snapshot) OwnerTeam(teamID string) bool {
	p := s.livePeople()
	return p != nil && p.OwnerTeam(teamID)
}
