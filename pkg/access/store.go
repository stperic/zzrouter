package access

import (
	"context"
	"errors"

	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/people"
	"github.com/stperic/zzrouter/pkg/access/teams"
	"github.com/stperic/zzrouter/pkg/audit"
)

// ErrClosed refuses every write after Close.
var ErrClosed = errors.New("access store is closed")

// Store is the one boundary for access records. Every write is a whole
// operation in one transaction: validate, persist, publish a new Snapshot,
// record ev; or change nothing and return the error. A zero ev records
// nothing, also for the records the store changes on its behalf. A state that
// breaks a record's Validate or Snapshot.Validate is refused with the error
// they return. The store assigns ID, UUID and timestamps; after Close every
// write and AuditPage return ErrClosed, and Snapshot keeps returning the last
// published state.
type Store interface {
	Snapshot() *Snapshot // lock-free
	// OnCommit registers fn before serving; fn runs after each commit,
	// outside the writer gate, before the operation returns. Commits can
	// notify out of order, so fn treats Store.Snapshot as current.
	OnCommit(fn func(Commit))

	// CreateKey with an empty TeamID also creates the key's personal team,
	// teams.PersonalID(id), with the key as its owner, recording team.created
	// before ev. A key joins only an existing shared team.
	CreateKey(ctx context.Context, id string, key keys.VirtualKey, ev audit.Event) (*keys.VirtualKey, string, error)
	// apply runs on a private copy under the gate; the store then restores ID,
	// UUID, credentials, team binding, CreatedAt, CreatedBy. apply must not call the Store.
	UpdateKey(ctx context.Context, id string, apply func(*keys.VirtualKey) error, ev audit.Event) (*keys.VirtualKey, error)
	RotateKey(ctx context.Context, id string, ev audit.Event) (*keys.VirtualKey, string, error)
	// DeleteKey also deletes the key's personal team, recording its
	// team.deleted, with cascade metadata, after ev.
	DeleteKey(ctx context.Context, id string, ev audit.Event) error
	CreateTeam(ctx context.Context, id string, team teams.Team, ev audit.Event) (*teams.Team, error)
	// apply follows UpdateKey's rules; the store restores ID, UUID, Kind,
	// CreatedAt, CreatedBy. A personal team is teams.ErrPersonalTeamImmutable.
	UpdateTeam(ctx context.Context, id string, apply func(*teams.Team) error, ev audit.Event) (*teams.Team, error)
	// DeleteTeam refuses a personal team with teams.ErrPersonalTeamImmutable
	// and a team that Snapshot.Membership says cannot go with
	// teams.ErrTeamHasMembers.
	DeleteTeam(ctx context.Context, id string, ev audit.Event) error
	// IssueSetup issues the owner's setup grant into the owner's workspace
	// team, creating that team in the same transaction when none is usable.
	IssueSetup(ctx context.Context, issuer people.Issuer) (*people.IssuedGrant, error)
	// Record appends ev for a change whose state lives outside the store, best effort.
	Record(ctx context.Context, ev audit.Event)
	// AuditPage returns up to limit (at most audit.MaxPage) recorded events f
	// selects after cursor, newest first by Time and ties in recording order,
	// never nil, with the opaque cursor of the next page, empty after the last.
	// A filter value matches literally. Each event's Time is the store's clock
	// when it recorded the event. A cursor the store did not issue for its
	// current generation and an equal f is audit.ErrCursor.
	AuditPage(ctx context.Context, f audit.Filter, cursor string, limit int) ([]audit.Event, string, error)

	People() People // nil when this node has no people store
	Close() error
}

// SnapshotOf returns st's published state. A node that holds no store passes
// nil and reads as empty.
func SnapshotOf(st interface{ Snapshot() *Snapshot }) *Snapshot {
	if st == nil {
		return nil
	}
	return st.Snapshot()
}

// People is today's identity operation set; *people.Store satisfies it unchanged.
type People interface {
	Authenticate(ctx context.Context, username, password string) (*people.Person, error)
	ChangePassword(ctx context.Context, id, revision, current, next string) (*people.Person, error)
	RevokeSessions(ctx context.Context, id, revision string) (*people.Person, error)
	Issue(ctx context.Context, req people.GrantRequest) (*people.IssuedGrant, error)
	Inspect(ctx context.Context, token string) (*people.Grant, error)
	Exchange(ctx context.Context, token, proof string) (*people.IssuedGrant, error)
	ConsumeSession(ctx context.Context, token, proof string) (people.Issuer, error)
	ClaimOwner(ctx context.Context, token, proof string, in people.CreatePerson) (*people.Person, error)
	AcceptInvitation(ctx context.Context, token, proof string, in people.CreatePerson) (*people.Person, error)
	Recover(ctx context.Context, token, proof, password string) (*people.Person, error)
	RevokeGrant(ctx context.Context, id string) error
	Update(ctx context.Context, id string, u people.UpdatePerson) (*people.Person, error)
	UpdateProgress(ctx context.Context, u people.ProgressUpdate) error
	MarkResponse(ctx context.Context, personID string) error
	MarkAppConnected(ctx context.Context) error
}

// Commit names the keys and teams one published change removed. Subscribers
// read Store.Snapshot: commits can notify out of order.
type Commit struct {
	RemovedKeys, RemovedTeams []string
}
