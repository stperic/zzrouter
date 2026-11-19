package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"time"

	"github.com/stperic/zzrouter/pkg/sqlitedb"
)

// Integrity is the check a store passed when it opened; a failed one refuses the open.
type Integrity struct {
	Check     string // the SQLite check: quick_check for a live store
	CheckedAt time.Time
}

// Status is what a store reports about itself. A nil size is a measurement it
// could not take; file sizes may change while a write runs.
type Status struct {
	Meta
	Initialized bool // the permanent owner-claim marker
	// DatabaseBytes and JournalBytes are the files' lengths, 0 for an absent
	// journal; FreePageBytes is the reusable space the last commit left.
	DatabaseBytes, JournalBytes, FreePageBytes *int64
	Integrity                                  Integrity
}

// Readiness is what the owner-claim marker means to an operator.
type Readiness string

const (
	Ready         Readiness = "ready"          // an owner exists
	SetupRequired Readiness = "setup_required" // no owner has been claimed yet
)

func readiness(initialized bool) Readiness {
	if initialized {
		return Ready
	}
	return SetupRequired
}

// Readiness reports the owner-claim marker.
func (st Status) Readiness() Readiness { return readiness(st.Initialized) }

// Readiness reports the owner-claim marker; an image a newer release wrote
// reads as SetupRequired, since its records are not read.
func (img Image) Readiness() Readiness { return readiness(img.Initialized) }

// Status reports the published meta, the owner-claim marker, the file sizes,
// the free-page bytes the last commit left and the check the store passed when
// it opened. It runs no query, never waits for the writer gate and opens no
// file. A failed store returns ErrFailed.
func (s *Store) Status() (Status, error) {
	published := s.pub.Load()
	if published.snap.Failed() {
		return Status{}, ErrFailed
	}
	st := Status{Meta: published.meta, Initialized: published.snap.Initialized(), Integrity: s.integrity}
	if published.freeBytes.Valid {
		st.FreePageBytes = new(published.freeBytes.Int64)
	}
	st.DatabaseBytes, _ = fileBytes(s.path)
	journal, err := fileBytes(s.path + "-journal")
	if errors.Is(err, fs.ErrNotExist) {
		journal = new(int64)
	}
	st.JournalBytes = journal
	return st, nil
}

func fileBytes(path string) (*int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	return new(info.Size()), nil
}

// freeBytes reads the reusable space q sees, invalid when the read fails: a
// measurement never fails the open or commit it belongs to.
func freeBytes(ctx context.Context, q sqlitedb.RowQuerier) sql.NullInt64 {
	n, err := sqlitedb.FreeBytes(ctx, q)
	return sql.NullInt64{Int64: n, Valid: err == nil}
}
