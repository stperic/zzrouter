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
// operation: validate, persist, publish a new Snapshot, record ev; or change
// nothing and return the error. A zero ev records nothing. A record that breaks
// its Validate rule is refused with keys.ErrInvalid or teams.ErrInvalid. The
// store assigns ID, UUID and timestamps; after Close every write returns
// ErrClosed and Snapshot keeps returning the last published state.
type Store interface {
	Snapshot() *Snapshot // lock-free
	// OnCommit registers fn before serving; fn runs after each commit,
	// outside the writer gate, before the operation returns. Commits can
	// notify out of order, so fn treats Store.Snapshot as current.
	OnCommit(fn func(Commit))

	CreateKey(ctx context.Context, id string, key keys.VirtualKey, ev audit.Event) (*keys.VirtualKey, string, error)
	// apply runs on a private copy under the gate; the store then restores ID,
	// UUID, credentials, team binding, CreatedAt, CreatedBy. apply must not call the Store.
	UpdateKey(ctx context.Context, id string, apply func(*keys.VirtualKey) error, ev audit.Event) (*keys.VirtualKey, error)
	RotateKey(ctx context.Context, id string, ev audit.Event) (*keys.VirtualKey, string, error)
	DeleteKey(ctx context.Context, id string, ev audit.Event) error
	CreateTeam(ctx context.Context, id string, team teams.Team, ev audit.Event) (*teams.Team, error)
	// apply follows UpdateKey's rules; the store restores ID, UUID, Kind, CreatedAt, CreatedBy.
	UpdateTeam(ctx context.Context, id string, apply func(*teams.Team) error, ev audit.Event) (*teams.Team, error)
	DeleteTeam(ctx context.Context, id string, ev audit.Event) error
	// Record appends ev for a change whose state lives outside the store, best effort.
	Record(ctx context.Context, ev audit.Event)

	People() People          // nil when this node has no people store
	SetAuditSink(audit.Sink) // removed in step 12
	Close() error
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

// Commit describes one published change. Subscribers read Store.Snapshot:
// commits can notify out of order.
type Commit struct {
	KeysChanged               bool
	RemovedKeys, RemovedTeams []string
}
