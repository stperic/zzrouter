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
	"strings"
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

// Options configures Open. With People set, the issuer validator is required.
type Options struct {
	People                 bool // offer people transitions; the snapshot holds stored people either way
	ValidateExternalIssuer func(context.Context, string) error
	// Fatal, if set, is called once, outside the writer gate, with an error
	// wrapping ErrFailed when the store fails closed.
	Fatal func(error)
}

// Meta are the values any release can read from any schema version: the
// immutable StoreID, the Generation that a restore replaces, the SchemaVersion
// from the file header, the Revision that record changes advance and the
// Sequence that every commit advances.
type Meta struct {
	StoreID, Generation string
	SchemaVersion       int
	Revision, Sequence  uint64
}

// part is the set of records a commit may change; none records events only.
type part uint8

const (
	keyRecords part = 1 << iota
	teamRecords
	peopleRecords
	auditOnly part = 0
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
	meta Meta
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
// holds the installation lock. A damaged or newer store is a *Refusal; one
// another program held past the busy timeout is ErrBusy, since the file is
// intact; a cancelled open returns the context's error and a file-system
// failure its own.
func Open(ctx context.Context, dir string, opts Options) (*Store, error) {
	s, err := open(ctx, dir, opts)
	if err != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return s, busy(err)
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
	if err := s.attach(ctx, path, false); err != nil {
		return nil, err
	}
	if err := ensureMarker(dir, s.pub.Load().meta.StoreID); err != nil {
		_ = s.db.Close()
		return nil, err
	}
	return s, nil
}

// errLiveImage refuses an image operation on the live database's file, which
// an open store would overwrite with what it publishes.
var errLiveImage = errors.New("an image cannot be the live access database file")

// attach opens the database at path into s, refusing it for a start's reasons.
func (s *Store) attach(ctx context.Context, path string, image bool) error {
	if image && strings.EqualFold(filepath.Base(path), fileName) {
		return errLiveImage
	}
	db, err := sqlitedb.Open(ctx, path, schema)
	if err != nil {
		return fileRefusal(err)
	}
	st, err := check(ctx, db, image)
	if err != nil {
		_ = db.Close()
		return err
	}
	s.db = db
	s.pub.Store(st)
	return nil
}

func (s *Store) openPeople(opts Options) error {
	var err error
	s.people, err = people.New(people.Options{
		ValidateExternalIssuer: opts.ValidateExternalIssuer,
		Mutate: func(ctx context.Context, change func(*people.State, people.TeamCheck) (audit.Event, error)) (*people.State, error) {
			var committed *tx // diff leaves the published people state here
			err := s.mutate(ctx, peopleRecords, func(t *tx) error {
				committed = t
				ev, err := change(t.people, teamCheck(t.teams))
				t.record(ev)
				return err
			})
			if err != nil {
				return nil, err
			}
			return committed.people, nil
		},
		View: func() (*people.State, people.TeamCheck) {
			published := s.pub.Load()
			return published.people, teamCheck(published.teams)
		},
	})
	return err
}

// teamCheck admits people into teams of one state's records: the published
// ones for reads, the ones being committed inside a transaction.
func teamCheck(records map[string]*teams.Team) people.TeamCheck {
	return func(id string) error { return access.AdmitPeople(records[id]) }
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
	path := databasePath(dir)
	staged := path + ".staged"
	if err := removeLeftovers(staged); err != nil {
		return "", err
	}
	if none, err := noStore(dir); !none {
		return path, err
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

func databasePath(dir string) string { return filepath.Join(dir, "access", fileName) }

// noStore reports whether dir holds neither a database nor a store marker. A
// marker without its database is Missing: an empty store there would let the
// next visitor claim the owner account.
func noStore(dir string) (bool, error) {
	if _, err := os.Lstat(databasePath(dir)); !errors.Is(err, fs.ErrNotExist) {
		return false, nil // any other failure is the opener's to report
	}
	if _, err := os.Lstat(filepath.Join(dir, markerName)); !errors.Is(err, fs.ErrNotExist) {
		if err == nil {
			err = refuse(Missing, nil)
		}
		return false, err
	}
	return true, nil
}

// Settle finishes SQLite's recovery of a journal a crash left beside dir's
// database, so a copy of the file holds exactly the committed transactions.
// The caller holds the installation lock. No database is nothing to settle;
// an unreadable or foreign file is the Refusal Open would give.
func Settle(ctx context.Context, dir string) error {
	h, err := sqlitedb.Probe(ctx, databasePath(dir))
	if errors.Is(err, sqlitedb.ErrNotExist) {
		return nil
	}
	if err == nil {
		// A journal settles whatever the schema version, so a newer release's
		// database can still be backed up before it is restored over.
		if err = schema.Admit(h); errors.Is(err, sqlitedb.ErrNewer) {
			return nil
		}
	}
	return fileRefusal(err)
}

// fileRefusal is the store's reason for a file sqlitedb would not open or
// found damaged.
func fileRefusal(err error) error {
	var failed *sqlitedb.CheckError
	switch {
	case errors.As(err, &failed):
		return refuse(IntegrityFailed, err)
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
// refuse. A live store gets quick_check and migrates before loading, because
// the loader reads the latest schema; an image gets integrity_check and, never
// being migrated, must already be at the latest schema.
func check(ctx context.Context, db *sqlitedb.DB, image bool) (*state, error) {
	if err := db.Check(ctx, image); err != nil {
		return nil, fileRefusal(err)
	}
	if image {
		if err := atLatest(ctx, db); err != nil {
			return nil, err
		}
	} else if _, _, err := db.Migrate(ctx); err != nil {
		return nil, fmt.Errorf("migrate access store: %w", err)
	}
	st, err := load(ctx, db.SQL(), 1)
	if errors.Is(err, errBroken) {
		return nil, refuse(IntegrityFailed, err)
	}
	return st, err
}

// atLatest refuses an image whose header names a schema version other than the
// latest: Open already refused a newer one, so a lower one is a damaged header.
func atLatest(ctx context.Context, db *sqlitedb.DB) error {
	var version int
	err := db.SQL().QueryRowContext(ctx, "PRAGMA user_version").Scan(&version)
	latest, latestErr := schema.Latest()
	if err = errors.Join(err, latestErr); err == nil && version != latest {
		err = refuse(Unreadable, fmt.Errorf("image schema version %d, latest %d", version, latest))
	}
	return err
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

// load reads and validates every record: the state a reopen publishes, under
// a keys version above any the process published before.
func load(ctx context.Context, db querier, keysVersion uint64) (*state, error) {
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
	snap := access.NewSnapshot(m.Generation, keysVersion, r.keys, r.teams, r.people)
	if err := snap.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", errBroken, err)
	}
	return &state{records: r, snap: snap, meta: m}, nil
}

func loadMeta(ctx context.Context, db querier) (Meta, error) {
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return Meta{}, err
	}
	values := make(map[string]string)
	pair := func(kv *[2]string) []col { return []col{{"key", &kv[0]}, {"value", &kv[1]}} }
	if err := scanAll(ctx, db, pair, func(kv *[2]string) { values[kv[0]] = kv[1] }, "SELECT key, value FROM meta"); err != nil {
		return Meta{}, err
	}
	m := Meta{StoreID: values["store_id"], Generation: values["generation"], SchemaVersion: version}
	revision, err := strconv.ParseUint(values["revision"], 10, 64)
	sequence, err2 := strconv.ParseUint(values["sequence"], 10, 64)
	m.Revision, m.Sequence = revision, sequence
	if err = errors.Join(err, err2); err == nil && (m.StoreID == "" || m.Generation == "") {
		err = errors.New("store ID or generation is missing")
	}
	if err != nil {
		return Meta{}, fmt.Errorf("%w: meta: %w", errBroken, err)
	}
	return m, nil
}

// Snapshot returns the last published state without locking.
func (s *Store) Snapshot() *access.Snapshot { return s.pub.Load().snap }

// Meta reports the published meta without a query.
func (s *Store) Meta() Meta { return s.pub.Load().meta }

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
	if err := s.mutate(ctx, auditOnly, func(t *tx) error { t.record(ev); return nil }); err != nil {
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
