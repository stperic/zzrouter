package access

import (
	"fmt"

	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/people"
	"github.com/stperic/zzrouter/pkg/access/teams"
)

// Validate holds the rules between records that every commit and every load
// keeps; each record's own rules are its Validate. A key refusal wraps
// keys.ErrInvalid, an orphan personal team teams.ErrInvalid, a person
// people.ErrInvalidGrant, a suspension SuspensionRefusal's error.
func (s *Snapshot) Validate() error {
	for id, key := range s.keys {
		team := s.teams[key.TeamID]
		switch {
		case team == nil:
			return fmt.Errorf("key %q: %w: team %q: %w", id, keys.ErrInvalid, key.TeamID, teams.ErrTeamNotFound)
		case !teams.MemberRole(key.TeamRole).Valid():
			return fmt.Errorf("key %q: %w: invalid team_role %q (expected %q or %q)", id, keys.ErrInvalid, key.TeamRole, teams.RoleOwner, teams.RoleMember)
		case team.Kind == teams.KindPersonal && (key.TeamID != teams.PersonalID(id) || key.TeamRole != string(teams.RoleOwner)):
			return fmt.Errorf("key %q: %w: team %q is personal and takes no second key: %w", id, keys.ErrInvalid, key.TeamID, teams.ErrPersonalTeamImmutable)
		}
	}
	owner := s.Owner()
	for id, team := range s.teams {
		if team.Kind == teams.KindPersonal && len(s.keysByTeam[id]) == 0 {
			return fmt.Errorf("team %q: %w: a personal team has no key", id, teams.ErrInvalid)
		}
		if team.Suspended {
			if err := SuspensionRefusal(team, owner != nil && owner.TeamID == id); err != nil {
				return err
			}
		}
	}
	for person := range s.PeopleState().All() {
		if !shared(s.teams[person.TeamID]) {
			return fmt.Errorf("person %q: %w: team %q is missing or personal", person.ID, people.ErrInvalidGrant, person.TeamID)
		}
	}
	return nil
}

// shared reports whether team exists and is shared, the only kind people join.
func shared(team *teams.Team) bool { return team != nil && team.Kind == teams.KindShared }

// AdmitPeople admits a person or a grant into team, nil when absent: an
// existing shared team that is not suspended. A refusal is people.ErrInvalidGrant.
func AdmitPeople(team *teams.Team) error {
	if !shared(team) || team.Suspended {
		return people.ErrInvalidGrant
	}
	return nil
}

// DeletionRefusal says why team cannot be deleted: it is personal and goes
// with its key, or keys, people or pending enrollments in m still use it.
func DeletionRefusal(team *teams.Team, m Membership) error {
	if err := team.CheckMutable(); err != nil {
		return err
	}
	if m.AppKeys > 0 || m.HasMembers() {
		return fmt.Errorf("team %q has %d app keys, %d people and %d pending enrollments; remove or reassign them first: %w",
			team.ID, m.AppKeys, m.People, m.Invitations+m.EnrollmentGrants, teams.ErrTeamHasMembers)
	}
	return nil
}

// SuspensionRefusal says why team cannot be suspended: it is personal, or it
// is the owner's team, which would lock the owner out.
func SuspensionRefusal(team *teams.Team, ownerTeam bool) error {
	if err := team.CheckMutable(); err != nil {
		return err
	}
	if ownerTeam {
		return fmt.Errorf("the owner's team cannot be suspended: %w", people.ErrOwnerProtected)
	}
	return nil
}

// Membership is what depends on a team and the lifecycle actions it allows.
type Membership struct {
	people.TeamDependencies
	AppKeys    int  `json:"app_keys"`
	CanDelete  bool `json:"can_delete"`
	CanSuspend bool `json:"can_suspend"`
}

// MembershipOf projects what depends on team among the records of one state.
func MembershipOf(team *teams.Team, k map[string]*keys.VirtualKey, p *people.State) Membership {
	appKeys := 0
	for _, key := range k {
		if key.TeamID == team.ID {
			appKeys++
		}
	}
	return membership(team, appKeys, p)
}

// membership asks the refusals themselves what team's lifecycle allows.
func membership(team *teams.Team, appKeys int, p *people.State) Membership {
	m := Membership{TeamDependencies: p.TeamImpact(team.ID), AppKeys: appKeys}
	m.CanDelete = DeletionRefusal(team, m) == nil
	m.CanSuspend = SuspensionRefusal(team, m.OwnerProtected) == nil
	return m
}
