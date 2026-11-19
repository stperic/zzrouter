package sqlitedb

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/stperic/zzrouter/pkg/security"
)

// CheckError reports corruption or a violation returned by a database check,
// or corruption a copy read.
type CheckError struct {
	// Check names the SQLite check that failed, or "copy".
	Check string
	// Detail is for logs only, never for programmatic decisions.
	Detail string
}

// Error describes the failed check for logging.
func (e *CheckError) Error() string { return e.Check + ": " + e.Detail }

// CheckName names the SQLite check Check runs before foreign_key_check.
func CheckName(full bool) string {
	if full {
		return "integrity_check"
	}
	return "quick_check"
}

// Check runs quick_check or integrity_check, then foreign_key_check; operational errors retain their causes.
func (d *DB) Check(ctx context.Context, full bool) error {
	for _, name := range []string{CheckName(full), "foreign_key_check"} {
		rows, err := d.db.QueryContext(ctx, "PRAGMA "+name)
		var detail string
		if err == nil {
			for rows.Next() {
				if name == "foreign_key_check" {
					var table, parent string
					var rowid sql.NullInt64
					var key int
					err = rows.Scan(&table, &rowid, &parent, &key)
					row := "NULL"
					if rowid.Valid {
						row = fmt.Sprint(rowid.Int64)
					}
					detail = fmt.Sprintf("table %s rowid %s references %s (foreign key %d)", table, row, parent, key)
					break
				}
				if err = rows.Scan(&detail); err != nil || detail != "ok" {
					break
				}
				detail = ""
			}
			err = errors.Join(err, rows.Err(), rows.Close())
		}
		if err != nil {
			if isCorrupt(err) {
				return &CheckError{name, err.Error()}
			}
			return fmt.Errorf("%s: %w", name, err)
		}
		if detail != "" {
			return &CheckError{name, detail}
		}
	}
	return nil
}

// RowQuerier is a database or one of its transactions.
type RowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// FreeBytes returns freelist_count times page_size, the space a compact copy
// can reclaim. Inside a write transaction it reads that transaction's state
// under the lock it holds.
func FreeBytes(ctx context.Context, q RowQuerier) (int64, error) {
	var count, size int64
	if err := q.QueryRowContext(ctx, "PRAGMA freelist_count").Scan(&count); err != nil {
		return 0, err
	}
	err := q.QueryRowContext(ctx, "PRAGMA page_size").Scan(&size)
	return count * size, err
}

// CopyTo writes a compact 0600 snapshot to dst, which must not exist; a failure
// leaves no file. VACUUM INTO syncs the copy at this connection's synchronous
// level; the directory entry is not synced, so a caller that keeps the copy
// syncs its directory. Reading a corrupt page is a *CheckError, as in Check.
func (d *DB) CopyTo(ctx context.Context, dst string) error {
	return createFile(dst, func() error {
		_, err := d.db.ExecContext(ctx, "VACUUM INTO ?", dst)
		if isCorrupt(err) {
			err = &CheckError{"copy", err.Error()}
		}
		return err
	})
}

// CopyFile copies the database file at path byte for byte to dst, which must
// not exist, owner-only and synced (its directory entry is not), and returns
// the SHA-256 of the bytes it wrote; a failure, cancellation included, leaves
// no file. Nothing parses it, so it copies a file this release cannot read.
// The caller settles a journal first (Probe) and keeps writers out.
func CopyFile(ctx context.Context, path, dst string) (string, error) {
	hash := sha256.New()
	err := createFile(dst, func() error {
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer func() { _ = in.Close() }()
		out, err := os.OpenFile(dst, os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		_, err = io.Copy(io.MultiWriter(out, hash), contextReader{ctx, in})
		return errors.Join(err, out.Sync(), out.Close())
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// contextReader stops reading once its context is done.
type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (c contextReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// createFile makes path, which must not exist, an empty owner-only file and
// runs fill; if either fails, it removes the file.
func createFile(path string, fill func() error) (err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, os.Remove(path))
		}
	}()
	if err = errors.Join(f.Close(), security.RestrictFilePermissions(path)); err != nil {
		return err
	}
	return fill()
}

// MakeDir creates the directory at path, with its parents, owner-only, and
// syncs its parent, so the new entry survives a power loss.
func MakeDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return errors.Join(security.RestrictDirectoryPermissions(path), SyncDir(filepath.Dir(path)))
}

// GiveOwner gives each of paths that exists the owner of the installation
// directory dir (security.GiveOwner), and reports them, relative to dir, to
// the WithStep observer as one step.
func GiveOwner(ctx context.Context, dir string, paths ...string) error {
	var given []string
	for _, path := range paths {
		if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err := security.GiveOwner(path, dir); err != nil {
			return fmt.Errorf("give %s the installation's owner: %w", path, err)
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		given = append(given, filepath.ToSlash(rel))
	}
	Step(ctx, "owned "+strings.Join(given, ", "))
	return nil
}

// Rename moves a closed database file and syncs the destination directory, so
// the move survives a power loss. Windows refuses to move an open file.
func Rename(from, to string) error {
	if err := os.Rename(from, to); err != nil {
		return err
	}
	return SyncDir(filepath.Dir(to))
}

func syncFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	if err = errors.Join(f.Sync(), f.Close()); err != nil {
		return err
	}
	return SyncDir(filepath.Dir(path))
}

// SyncDir syncs the directory at path, so the entries created, renamed or
// removed in it survive a power loss; Windows cannot sync a directory.
func SyncDir(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

type stepKey struct{}

// WithStep is a test seam: it returns ctx carrying observe, which an
// operation that must survive a crash calls after each durable step, so a test
// process can stop there and be killed. Without an observer, Step does nothing.
func WithStep(ctx context.Context, observe func(step string)) context.Context {
	return context.WithValue(ctx, stepKey{}, observe)
}

// Step reports a durable step to the observer ctx carries, if any.
func Step(ctx context.Context, step string) {
	if observe, ok := ctx.Value(stepKey{}).(func(string)); ok {
		observe(step)
	}
}
