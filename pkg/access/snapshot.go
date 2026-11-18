package access

import (
	"iter"
	"os"

	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/people"
	"github.com/stperic/zzrouter/pkg/access/teams"
)

// Snapshot is one published state of keys, teams and people; a nil *Snapshot
// reads as empty. Yielded records are copies.
type Snapshot struct {
	keys       map[string]*keys.VirtualKey
	teams      map[string]*teams.Team
	keysByTeam map[string][]string
	gated      bool
	failed     bool
	people     *people.State
}

// FailedSnapshot is what a store publishes once it cannot vouch for its state:
// no key, team or person resolves, keyless callers meet a gated team, and the
// installation reads as claimed, so nothing is admitted and setup cannot start.
func FailedSnapshot() *Snapshot {
	return &Snapshot{keysByTeam: make(map[string][]string), gated: true, failed: true}
}

// NewSnapshot owns k and t: the caller never changes them or their records
// again. A nil p means no people.
func NewSnapshot(k map[string]*keys.VirtualKey, t map[string]*teams.Team, p *people.State) *Snapshot {
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

// PeopleState returns the published people state, nil for none. A store's
// people View reads it, so what people transitions read never runs ahead of
// the snapshot.
func (s *Snapshot) PeopleState() *people.State {
	if s == nil {
		return nil
	}
	return s.people
}

// Person returns one account, or nil.
func (s *Snapshot) Person(id string) *people.Person { return s.PeopleState().Person(id) }

// People lists accounts by username.
func (s *Snapshot) People() []people.Person { return s.PeopleState().People() }

// Owner returns the initial owner, or nil.
func (s *Snapshot) Owner() *people.Person { return s.PeopleState().Owner() }

// Initialized reports the permanent owner-claim marker.
func (s *Snapshot) Initialized() bool { return s != nil && s.failed || s.PeopleState().Initialized() }

// Progress returns the persisted onboarding state.
func (s *Snapshot) Progress() people.Progress { return s.PeopleState().Progress() }

// Invitations lists invitation and recovery grants, newest first.
func (s *Snapshot) Invitations() []people.Grant { return s.PeopleState().Invitations() }

// TeamImpact counts the people and pending enrollments that use teamID.
func (s *Snapshot) TeamImpact(teamID string) people.TeamDependencies {
	return s.PeopleState().TeamImpact(teamID)
}

// OwnerTeam reports whether teamID is the owner's team.
func (s *Snapshot) OwnerTeam(teamID string) bool { return s.PeopleState().OwnerTeam(teamID) }
