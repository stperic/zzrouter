// Package sqlstore implements access.Store over one SQLite file,
// <dir>/access/access.sqlite. A change and its audit event commit in one
// transaction; reads come from the snapshot published after it, never from SQL.
package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/google/uuid"
	"github.com/stperic/common/atomicfile"
	"github.com/stperic/zzrouter/pkg/access"
	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/people"
	"github.com/stperic/zzrouter/pkg/access/teams"
	"github.com/stperic/zzrouter/pkg/audit"
	"github.com/stperic/zzrouter/pkg/security"
	"github.com/stperic/zzrouter/pkg/sqlitedb"
)

const (
	fileName   = "access.sqlite"
	markerName = "access-store.json" // beside node.yaml: tells a lost database from a fresh install
)

// Reason is the closed cause of a Refusal.
type Reason string

const (
	// Missing means the store marker says a database existed, and it is gone.
	Missing Reason = "missing"
	// SchemaNewer means a newer release wrote the database.
	SchemaNewer Reason = "schema_newer"
	// IntegrityFailed means the database opens but fails its checks.
	IntegrityFailed Reason = "integrity_failed"
	// Unreadable means the database cannot be opened.
	Unreadable Reason = "unreadable"
)

var nextActions = map[Reason]string{
	Missing:         "Stop the service, then activate a checked backup, or remove the store marker (access-store.json) to start over at owner setup.",
	SchemaNewer:     "Stop the service, then start the release that wrote the database, or activate a backup the installed release can read.",
	IntegrityFailed: "Stop the service, then activate a checked backup, or remove the access directory and the store marker to start over at owner setup.",
	Unreadable:      "Stop the service, then activate a checked backup, or remove the access directory and the store marker to start over at owner setup.",
}

// Refusal is a store that cannot open: the node exits with Reason and
// NextAction. Its message carries no path or database text; the cause stays in
// the error chain for logs.
type Refusal struct {
	Reason Reason
	cause  error
}

func refuse(reason Reason, cause error) *Refusal { return &Refusal{Reason: reason, cause: cause} }

// Error names the reason and the next action.
func (r *Refusal) Error() string {
	return "access store cannot open: " + string(r.Reason) + ". " + r.NextAction()
}

// Unwrap returns the cause, for logs.
func (r *Refusal) Unwrap() error { return r.cause }

// NextAction is the operator's next step; it starts with stopping the service.
func (r *Refusal) NextAction() string { return nextActions[r.Reason] }

// Options configures Open. With People set, both validators are required.
type Options struct {
	People                 bool // offer people transitions; the snapshot holds stored people either way
	ValidateExternalIssuer func(context.Context, string) error
	ValidateTeam           func(*access.Snapshot, string) error
	// Fatal, if set, is called once, outside the writer gate, with an error
	// wrapping ErrFailed when the store fails closed.
	Fatal func(error)
}

// meta are the values any release can read: see the meta table.
type meta struct {
	storeID, generation string
	revision, sequence  uint64
}

// part names the records a commit may change.
type part int

const (
	auditOnly part = iota
	keyRecords
	teamRecords
	peopleRecords
)

// records are one state; a commit replaces a map or the people state, never edits it.
type records struct {
	keys   map[string]*keys.VirtualKey
	teams  map[string]*teams.Team
	people *people.State
}

// state is one published commit.
type state struct {
	records
	snap *access.Snapshot
	meta meta
}

// Store is access.Store over access.sqlite.
type Store struct {
	db     *sqlitedb.DB
	people *people.Store
	fatal  func(error)
	pub    atomic.Pointer[state]

	gate         sync.Mutex // serializes commits and guards the fields below
	subs         []func(access.Commit)
	refusal      error               // access.ErrClosed or ErrFailed: refuses writes and audit reads
	beforeCommit func(*sql.Tx) error // test hook, run just before COMMIT
}

var _ access.Store = (*Store)(nil)

// Open opens dir's store, creating an empty one on first start. The caller
// holds the installation lock. A damaged or newer store is a *Refusal; a
// cancelled open returns the context's error and a file-system failure its own.
func Open(ctx context.Context, dir string, opts Options) (*Store, error) {
	s, err := open(ctx, dir, opts)
	if err != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return s, err
}

func open(ctx context.Context, dir string, opts Options) (*Store, error) {
	s := &Store{fatal: opts.Fatal}
	if opts.People {
		if err := s.openPeople(opts); err != nil {
			return nil, fmt.Errorf("initialize people: %w", err)
		}
	}
	path, err := ensureFile(ctx, dir)
	if err != nil {
		return nil, err
	}
	db, err := sqlitedb.Open(ctx, path, schema)
	if err != nil {
		return nil, fileRefusal(err)
	}
	st, err := check(ctx, db)
	if err == nil {
		err = ensureMarker(dir, st.meta.storeID)
	}
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	s.db = db
	s.pub.Store(st)
	return s, nil
}

func (s *Store) openPeople(opts Options) error {
	var validateTeam func(context.Context, string) error
	if opts.ValidateTeam != nil {
		validateTeam = func(_ context.Context, id string) error { return opts.ValidateTeam(s.Snapshot(), id) }
	}
	var err error
	s.people, err = people.New(people.Options{
		ValidateExternalIssuer: opts.ValidateExternalIssuer, ValidateTeam: validateTeam,
		Mutate: func(ctx context.Context, change func(*people.State) (audit.Event, error)) (*people.State, error) {
			var committed *records // diff leaves the published people state here
			err := s.mutate(ctx, peopleRecords, func(r *records) (audit.Event, error) {
				committed = r
				return change(r.people)
			})
			if err != nil {
				return nil, err
			}
			return committed.people, nil
		},
		View: func() *people.State { return s.Snapshot().PeopleState() },
	})
	return err
}

// ensureFile restricts the access directory and returns the database path,
// publishing an empty database on first start.
func ensureFile(ctx context.Context, dir string) (string, error) {
	accessDir := filepath.Join(dir, "access")
	if err := os.MkdirAll(accessDir, 0o700); err != nil {
		return "", err
	}
	if err := security.RestrictDirectoryPermissions(accessDir); err != nil {
		return "", err
	}
	path := filepath.Join(accessDir, fileName)
	staged := path + ".staged"
	if err := removeLeftovers(staged); err != nil {
		return "", err
	}
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		return path, nil // any other failure is Open's to report
	}
	// An empty store here would let the next visitor claim the owner account.
	if _, err := os.Lstat(filepath.Join(dir, markerName)); !errors.Is(err, fs.ErrNotExist) {
		if err != nil {
			return "", err
		}
		return "", refuse(Missing, nil)
	}
	if err := removeLeftovers(path + "-journal"); err != nil {
		return "", err
	}
	storeID := uuid.NewString()
	db, err := sqlitedb.Create(ctx, staged, schema, func(tx *sql.Tx) error { return initialize(ctx, tx, storeID) })
	if err != nil {
		return "", err
	}
	if err := db.Close(); err != nil {
		return "", err
	}
	if err := sqlitedb.Rename(staged, path); err != nil {
		return "", err
	}
	return path, ensureMarker(dir, storeID)
}

// Settle finishes SQLite's recovery of a journal a crash left beside dir's
// database, so a copy of the file holds exactly the committed transactions.
// The caller holds the installation lock. No database is nothing to settle;
// a file this release would refuse to open is that Refusal.
func Settle(ctx context.Context, dir string) error {
	h, err := sqlitedb.Probe(ctx, filepath.Join(dir, "access", fileName))
	if errors.Is(err, sqlitedb.ErrNotExist) {
		return nil
	}
	if err == nil {
		err = schema.Admit(h)
	}
	return fileRefusal(err)
}

// fileRefusal is the store's reason for a file sqlitedb would not open.
func fileRefusal(err error) error {
	switch {
	case errors.Is(err, sqlitedb.ErrNewer):
		return refuse(SchemaNewer, err)
	case errors.Is(err, sqlitedb.ErrUnreadable), errors.Is(err, sqlitedb.ErrForeign):
		return refuse(Unreadable, err)
	}
	return err
}

// Excluded lists, slash-separated and relative to dir, the files beside the
// database that a copy of dir must not carry: a journal belongs to the moment
// it was left, and a staged file was never published.
func Excluded() []string {
	name := "access/" + fileName
	return []string{name + "-journal", name + ".staged"}
}

// removeLeftovers removes files an earlier attempt left. SQLite drops a journal
// beside an empty file, but would replay one beside a published database.
func removeLeftovers(paths ...string) error {
	for _, path := range paths {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

func initialize(ctx context.Context, tx *sql.Tx, storeID string) error {
	_, err := tx.ExecContext(ctx, "INSERT INTO meta (key, value) VALUES ('store_id', ?), ('generation', ?), ('revision', '0'), ('sequence', '0')", storeID, storeID)
	if err == nil {
		_, err = tx.ExecContext(ctx, installationTable.insert, ptrs(installationCols(&people.Records{}))...)
	}
	return err
}

// check refuses a database that fails its checks or holds a record its rules
// refuse; it migrates before loading because the loader reads the latest schema.
func check(ctx context.Context, db *sqlitedb.DB) (*state, error) {
	if err := db.Check(ctx, false); err != nil {
		var failed *sqlitedb.CheckError
		if errors.As(err, &failed) {
			return nil, refuse(IntegrityFailed, err)
		}
		return nil, err
	}
	if _, _, err := db.Migrate(ctx); err != nil {
		return nil, fmt.Errorf("migrate access store: %w", err)
	}
	st, err := load(ctx, db.SQL())
	if errors.Is(err, errBroken) {
		return nil, refuse(IntegrityFailed, err)
	}
	return st, err
}

// errBroken marks a stored record that breaks its rules or cannot be decoded,
// as opposed to a failure to read it.
var errBroken = errors.New("a stored record breaks its rules")

type marker struct {
	Version int    `json:"version"`
	StoreID string `json:"store_id"`
}

// ensureMarker writes the marker unless it already names storeID.
func ensureMarker(dir, storeID string) error {
	path := filepath.Join(dir, markerName)
	want := marker{1, storeID}
	data, err := os.ReadFile(path)
	var current marker
	if err == nil && json.Unmarshal(data, &current) == nil && current == want {
		return nil
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if data, err = json.Marshal(want); err != nil {
		return err
	}
	return atomicfile.WriteFile(path, data, 0o600)
}

// load reads and validates every record: the state a reopen publishes.
func load(ctx context.Context, db querier) (*state, error) {
	m, err := loadMeta(ctx, db)
	if err != nil {
		return nil, err
	}
	r := records{keys: make(map[string]*keys.VirtualKey), teams: make(map[string]*teams.Team)}
	p := people.Records{People: make(map[string]people.PersonRecord), Grants: make(map[string]people.GrantRecord)}
	installations := 0
	err = scanAll(ctx, db, keyCols, func(k *keys.VirtualKey) { r.keys[k.ID] = k }, keyTable.selectAll)
	if err == nil {
		err = scanAll(ctx, db, teamCols, func(t *teams.Team) { r.teams[t.ID] = t }, teamTable.selectAll)
	}
	if err == nil {
		err = scanAll(ctx, db, personCols, func(rec *people.PersonRecord) { p.People[rec.ID] = *rec }, personTable.selectAll)
	}
	if err == nil {
		err = scanAll(ctx, db, grantCols, func(g *grantRow) { p.Grants[g.hash] = g.GrantRecord }, grantTable.selectAll)
	}
	if err == nil {
		err = scanAll(ctx, db, installationCols, func(i *people.Records) {
			p.OwnerID, p.Progress = i.OwnerID, i.Progress
			installations++
		}, installationTable.selectAll)
	}
	if err != nil {
		return nil, err
	}
	if installations != 1 {
		return nil, fmt.Errorf("%w: installation has %d rows", errBroken, installations)
	}
	for id, k := range r.keys {
		if err := k.Validate(); err != nil {
			return nil, fmt.Errorf("%w: key %q: %w", errBroken, id, err)
		}
	}
	for id, t := range r.teams {
		if err := t.Validate(); err != nil {
			return nil, fmt.Errorf("%w: team %q: %w", errBroken, id, err)
		}
	}
	p.Initialized = p.OwnerID != ""
	if r.people, err = people.NewState(p); err != nil {
		return nil, fmt.Errorf("%w: %w", errBroken, err)
	}
	return &state{records: r, snap: access.NewSnapshot(m.generation, r.keys, r.teams, r.people), meta: m}, nil
}

func loadMeta(ctx context.Context, db querier) (meta, error) {
	values := make(map[string]string)
	pair := func(kv *[2]string) []col { return []col{{"key", &kv[0]}, {"value", &kv[1]}} }
	if err := scanAll(ctx, db, pair, func(kv *[2]string) { values[kv[0]] = kv[1] }, "SELECT key, value FROM meta"); err != nil {
		return meta{}, err
	}
	m := meta{storeID: values["store_id"], generation: values["generation"]}
	revision, err := strconv.ParseUint(values["revision"], 10, 64)
	sequence, err2 := strconv.ParseUint(values["sequence"], 10, 64)
	m.revision, m.sequence = revision, sequence
	if err = errors.Join(err, err2); err == nil && (m.storeID == "" || m.generation == "") {
		err = errors.New("store ID or generation is missing")
	}
	if err != nil {
		return meta{}, fmt.Errorf("%w: meta: %w", errBroken, err)
	}
	return m, nil
}

// Snapshot returns the last published state without locking.
func (s *Store) Snapshot() *access.Snapshot { return s.pub.Load().snap }

// Meta reports the published store ID, generation, revision and sequence
// without a query.
func (s *Store) Meta() (storeID, generation string, revision, sequence uint64) {
	m := s.pub.Load().meta
	return m.storeID, m.generation, m.revision, m.sequence
}

// OnCommit registers fn to run after each commit that changes records.
func (s *Store) OnCommit(fn func(access.Commit)) {
	s.gate.Lock()
	defer s.gate.Unlock()
	s.subs = append(s.subs, fn)
}

// People returns the people transitions, or nil when Options.People was off.
func (s *Store) People() access.People {
	if s.people == nil {
		return nil
	}
	return s.people
}

// Record commits ev alone, for a change whose state lives outside the store:
// it advances the sequence, not the revision. A failure is logged.
func (s *Store) Record(ctx context.Context, ev audit.Event) {
	ctx = context.WithoutCancel(ctx)
	if err := s.mutate(ctx, auditOnly, func(*records) (audit.Event, error) { return ev, nil }); err != nil {
		slog.WarnContext(ctx, "Audit record failed", "action", ev.Action, "subject", ev.Subject, "error", err)
	}
}

// Close waits for in-flight commits and closes the database; closing twice is
// harmless. Every later write and audit read returns access.ErrClosed (or
// ErrFailed after a failure), and Snapshot keeps the last published state.
func (s *Store) Close() error {
	s.gate.Lock()
	defer s.gate.Unlock()
	if s.refusal == nil {
		s.refusal = access.ErrClosed
	}
	return s.db.Close()
}
