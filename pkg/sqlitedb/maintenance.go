package sqlitedb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

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
// not exist, owner-only and synced (its directory entry is not); a failure
// leaves no file. Nothing parses it, so it copies a file this release cannot
// read. The caller settles a journal first (Probe) and keeps writers out.
func CopyFile(path, dst string) error {
	return createFile(dst, func() error {
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer func() { _ = in.Close() }()
		out, err := os.OpenFile(dst, os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, in)
		return errors.Join(err, out.Sync(), out.Close())
	})
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

// Rename moves a closed database file and syncs the destination directory, so
// the move survives a power loss. Windows refuses to move an open file.
func Rename(from, to string) error {
	if err := os.Rename(from, to); err != nil {
		return err
	}
	return syncDir(filepath.Dir(to))
}

func syncFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	if err = errors.Join(f.Sync(), f.Close()); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func syncDir(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}
