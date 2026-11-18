package sqlitedb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/stperic/zzrouter/pkg/security"
)

// CheckError reports corruption or a violation returned by a database check.
type CheckError struct {
	// Check names the SQLite check that failed.
	Check string
	// Detail is for logs only, never for programmatic decisions.
	Detail string
}

// Error describes the failed check for logging.
func (e *CheckError) Error() string { return e.Check + ": " + e.Detail }

// Check runs quick_check or integrity_check, then foreign_key_check; operational errors retain their causes.
func (d *DB) Check(ctx context.Context, full bool) error {
	check := "quick_check"
	if full {
		check = "integrity_check"
	}
	for _, name := range []string{check, "foreign_key_check"} {
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

// FreeBytes returns freelist_count times page_size, the space a compact copy can reclaim.
func (d *DB) FreeBytes(ctx context.Context) (int64, error) {
	var count, size int64
	if err := d.db.QueryRowContext(ctx, "PRAGMA freelist_count").Scan(&count); err != nil {
		return 0, err
	}
	err := d.db.QueryRowContext(ctx, "PRAGMA page_size").Scan(&size)
	return count * size, err
}

// CopyTo writes a compact 0600 snapshot to an absent or empty destination and syncs it and its directory.
func (d *DB) CopyTo(ctx context.Context, dst string) (err error) {
	f, err := os.OpenFile(dst, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	created := err == nil
	if errors.Is(err, os.ErrExist) {
		f, err = os.OpenFile(dst, os.O_RDWR, 0)
	}
	if err != nil {
		return err
	}
	defer func() {
		if err != nil && created {
			err = errors.Join(err, os.Remove(dst))
		}
	}()
	info, err := f.Stat()
	if err == nil && (!info.Mode().IsRegular() || info.Size() != 0) {
		err = fmt.Errorf("copy destination is not an empty file: %s", dst)
	}
	if err = errors.Join(err, f.Close()); err != nil {
		return err
	}
	if err := security.RestrictFilePermissions(dst); err != nil {
		return err
	}
	if _, err := d.db.ExecContext(ctx, "VACUUM INTO ?", dst); err != nil {
		return err
	}
	return syncFile(dst)
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
