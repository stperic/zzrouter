// Package sqlitedb holds one SQLite file through one connection without knowing its data.
package sqlitedb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"runtime"

	"github.com/stperic/zzrouter/pkg/security"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

var (
	// ErrNotExist means the database path does not exist.
	ErrNotExist = errors.New("SQLite file does not exist")
	// ErrForeign means the file belongs to another application.
	ErrForeign = errors.New("SQLite application ID differs")
	// ErrNewer means the file's schema version exceeds the supplied migrations.
	ErrNewer = errors.New("SQLite schema is newer")
	// ErrUnreadable means the file is not a usable SQLite database.
	ErrUnreadable = errors.New("the file is not a usable SQLite database")
)

// Header contains the application's identity and schema version from the file header.
type Header struct {
	// AppID identifies the application that owns the file.
	AppID int32
	// Version is the file's user_version.
	Version int
}

// DB holds one file through a single connection that is never expired automatically.
type DB struct {
	db     *sql.DB
	schema Schema
	// detached lets Close take the connection after the opener's context is done.
	detached context.Context
}

func isCorrupt(err error) bool {
	var e *sqlite.Error
	return errors.As(err, &e) && (e.Code()&0xff == sqlite3.SQLITE_CORRUPT || e.Code()&0xff == sqlite3.SQLITE_NOTADB)
}

func openError(path string, err error) error {
	if err == nil {
		return nil
	}
	if isCorrupt(err) {
		err = fmt.Errorf("%w: %w", ErrUnreadable, err)
	}
	return fmt.Errorf("open %s: %w", path, err)
}

func connect(ctx context.Context, path string, configured bool) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(abs), OmitHost: true}
	q := url.Values{"mode": {"rw"}, "_pragma": {"busy_timeout(5000)"}}
	if configured {
		q["_pragma"] = append(q["_pragma"], "foreign_keys(1)", "journal_mode(DELETE)", "synchronous(EXTRA)")
		q.Set("_txlock", "immediate")
	}
	if runtime.GOOS == "darwin" {
		q.Add("_pragma", "fullfsync(1)")
	}
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err == nil {
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
		// Ping runs SELECT, which parses schemas that Probe must not inspect.
		_, err = db.ExecContext(ctx, "PRAGMA user_version")
		if err != nil {
			err = errors.Join(err, db.Close())
		}
	}
	return db, err
}

// Probe reads the header without parsing the schema; journal recovery is its only possible write.
func Probe(ctx context.Context, path string) (h Header, err error) {
	defer func() { err = openError(path, err) }()
	if _, err = os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return h, ErrNotExist
		}
		return h, err
	}
	db, err := connect(ctx, path, false)
	if err != nil {
		return h, err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	if err = db.QueryRowContext(ctx, "PRAGMA application_id").Scan(&h.AppID); err == nil {
		err = db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&h.Version)
	}
	return h, err
}

// Open refuses absent, foreign, newer, or negatively versioned files and configures one connection.
func Open(ctx context.Context, path string, s Schema) (*DB, error) {
	h, err := Probe(ctx, path)
	if err != nil {
		return nil, err
	}
	if h.AppID != s.AppID {
		return nil, fmt.Errorf("%w: got %d, want %d", ErrForeign, h.AppID, s.AppID)
	}
	latest, err := s.Latest()
	if err != nil {
		return nil, err
	}
	if h.Version > latest {
		return nil, fmt.Errorf("%w: got %d, latest %d", ErrNewer, h.Version, latest)
	}
	if h.Version < 0 {
		return nil, fmt.Errorf("%w: invalid schema version: %d", ErrUnreadable, h.Version)
	}
	if err := security.RestrictFilePermissions(path); err != nil {
		return nil, openError(path, err)
	}
	db, err := connect(ctx, path, true)
	if err != nil {
		return nil, openError(path, err)
	}
	return &DB{db: db, schema: s, detached: context.WithoutCancel(ctx)}, nil
}

// Create makes and syncs an absent 0600 staged file, applying migrations and init atomically.
// Failure removes it; a crash can leave a file that Create refuses, so callers own staging cleanup.
func Create(ctx context.Context, path string, s Schema, init func(*sql.Tx) error) (d *DB, err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			if d != nil {
				err = errors.Join(err, d.Close())
			}
			err = errors.Join(err, os.Remove(path))
			d = nil
		}
	}()
	if err = errors.Join(f.Close(), security.RestrictFilePermissions(path)); err != nil {
		return nil, err
	}
	db, err := connect(ctx, path, true)
	if err != nil {
		return nil, err
	}
	d = &DB{db: db, schema: s, detached: context.WithoutCancel(ctx)}
	_, _, err = d.migrate(ctx, init, true)
	if err == nil {
		err = syncFile(path)
	}
	return d, err
}

// SQL exposes the one-connection pool; never share its raw connection across goroutines outside database/sql.
func (d *DB) SQL() *sql.DB { return d.db }

// Close blocks until in-flight work returns and releases the file before returning.
// It must never be called while the caller holds a *sql.Tx, *sql.Rows, or *sql.Conn
// obtained from SQL(), because it waits for the one connection.
func (d *DB) Close() error {
	conn, err := d.db.Conn(d.detached)
	closed := d.db.Close()
	if err != nil {
		return closed
	}
	return errors.Join(closed, conn.Close())
}

// Migrate applies all pending migrations and the version in one transaction; failure preserves both.
func (d *DB) Migrate(ctx context.Context) (from, to int, err error) {
	return d.migrate(ctx, nil, false)
}

func (d *DB) migrate(ctx context.Context, init func(*sql.Tx) error, creating bool) (from, to int, err error) {
	if err = d.schema.Verify(); err != nil {
		return
	}
	names, err := d.schema.migrations()
	if err != nil {
		return
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return
	}
	defer func() { _ = tx.Rollback() }()
	if err = tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&from); err != nil {
		return
	}
	to = from
	if from > len(names) {
		return from, to, ErrNewer
	}
	if from < 0 {
		return from, to, fmt.Errorf("invalid schema version: %d", from)
	}
	if from == len(names) && !creating {
		return
	}
	for _, name := range names[from:] {
		data, readErr := fs.ReadFile(d.schema.FS, name)
		if readErr != nil {
			return from, to, readErr
		}
		if _, err = tx.ExecContext(ctx, string(data)); err != nil {
			return from, to, fmt.Errorf("migration %s: %w", name, err)
		}
	}
	if init != nil {
		if err = init(tx); err != nil {
			return
		}
	}
	if _, err = tx.ExecContext(ctx, fmt.Sprintf("PRAGMA application_id=%d; PRAGMA user_version=%d", d.schema.AppID, len(names))); err == nil {
		err = tx.Commit()
	}
	if err == nil {
		to = len(names)
	}
	return
}
