package sqlstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/stperic/common/atomicfile"
	"github.com/stperic/zzrouter/pkg/sqlitedb"
)

var (
	// ErrStoreIdentity refuses an image whose store ID an installation does not
	// admit: its store marker, or without one its database, admits only its own
	// store ID, and only an installation with neither admits another.
	ErrStoreIdentity = errors.New("the installation holds another access store")
	// ErrKeptLost refuses a revert while the database already is the image the
	// replacement published but keep no longer holds what it replaced: nothing
	// could restore it.
	ErrKeptLost = errors.New("what the replacement kept is gone, and the database already is the published image")
)

// keptName is Replace's record, in keep, of the store files that existed
// before it changed any.
const keptName = "kept.json"

// kept records which of an installation's store files existed, or, as Lost,
// that the database already was the published image when nothing was
// recorded, so what it replaced is gone.
type kept struct {
	Database bool `json:"database"`
	Journal  bool `json:"journal"` // what SQLite left of a journal: one it could not settle, or a cold one
	Marker   bool `json:"marker"`
	Lost     bool `json:"lost,omitempty"`
}

// storeFile is one file Replace saves and Revert restores: its live path, its
// path in keep and whether it existed.
type storeFile struct {
	path, saved string
	existed     bool
}

func (k kept) files(dir, keep string) []storeFile {
	files := []storeFile{{path: databasePath(dir), existed: k.Database}, {path: databasePath(dir) + "-journal", existed: k.Journal}, {path: filepath.Join(dir, MarkerName), existed: k.Marker}}
	for i := range files {
		files[i].saved = filepath.Join(keep, filepath.Base(files[i].path))
	}
	return files
}

// Installed describes the access store an installation holds: the store ID its
// marker names, or without one its database, "" with neither; and its
// database's generation, "" without a database it can read.
type Installed struct{ StoreID, Generation string }

// AdmitStoreID describes dir's installation and refuses with ErrStoreIdentity
// a storeID it does not admit, unless foreign, or an installation whose store
// ID it cannot read. Reading the database lets SQLite settle a journal beside
// it, the one change it may make.
func AdmitStoreID(ctx context.Context, dir, storeID string, foreign bool) (Installed, error) {
	inst, err := installed(ctx, dir)
	if err == nil && inst.StoreID != "" && inst.StoreID != storeID && !foreign {
		err = fmt.Errorf("%w (store ID %s, not the image's %s)", ErrStoreIdentity, inst.StoreID, storeID)
	}
	return inst, err
}

func installed(ctx context.Context, dir string) (Installed, error) {
	var inst Installed
	data, err := os.ReadFile(filepath.Join(dir, MarkerName))
	if err == nil {
		inst.StoreID, err = MarkerStoreID(data)
	} else if errors.Is(err, fs.ErrNotExist) {
		err = nil
	}
	if err != nil {
		return inst, err
	}
	db, _, err := sqlitedb.OpenAny(ctx, databasePath(dir), schema)
	if errors.Is(err, sqlitedb.ErrNotExist) {
		return inst, nil
	}
	var m Meta
	if err == nil {
		m, err = loadMeta(ctx, db.SQL())
		err = errors.Join(err, db.Close())
	}
	switch {
	case err == nil:
		inst.Generation = m.Generation
		if inst.StoreID == "" {
			inst.StoreID = m.StoreID
		}
	case !errors.As(fileRefusal(err), new(*Refusal)) && !errors.Is(err, errBroken):
		return inst, err // not a verdict on the file
	case inst.StoreID == "":
		return inst, fmt.Errorf("%w: its database has no store marker and its store ID cannot be read: %w", ErrStoreIdentity, err)
	}
	return inst, nil
}

// Replacement is what Replace published, and Lost when keep does not hold
// what it replaced: the database already was the published image when a call
// found nothing recorded in keep.
type Replacement struct {
	Image
	Lost bool
}

// Replace publishes the image at src as dir's database and store marker,
// saving what it replaces in keep; want is the SHA-256 the caller checked src
// at, and foreign admits another store ID (AdmitStoreID). The caller holds the
// installation lock. Replace copies src beside the database, refusing a copy
// whose digest is not want before anything of the installation changes, and
// checks the copy; records in keep which store files exist, once SQLite settled
// a journal beside a readable database; copies them into keep and removes a
// journal it could not settle, which SQLite would otherwise roll back into the
// new file; renames the copy over the database and writes the marker. Each
// step is durable before the next,
// and a call after a crash at any point completes the replacement; Revert
// undoes it.
func Replace(ctx context.Context, dir, src, want, keep string, foreign bool) (Replacement, error) {
	live := databasePath(dir)
	staged := live + ".staged"
	if err := sqlitedb.MakeDir(ctx, filepath.Dir(live)); err != nil {
		return Replacement{}, err
	}
	if err := sqlitedb.RemoveFile(staged); err != nil {
		return Replacement{}, err
	}
	sum, err := sqlitedb.CopyFile(ctx, src, staged)
	if err == nil && sum != want {
		err = fmt.Errorf("the image is not the one checked: its SHA-256 is %s, not %s", sum, want)
	}
	var rep Replacement
	var inst Installed
	if err == nil {
		sqlitedb.Step(ctx, "staged")
		rep.Image, err = CheckImage(ctx, staged)
	}
	if err == nil {
		inst, err = AdmitStoreID(ctx, dir, rep.StoreID, foreign)
	}
	if err != nil {
		return Replacement{}, errors.Join(err, sqlitedb.RemoveFile(staged))
	}
	if rep.Lost, err = save(ctx, dir, keep, inst.Generation == rep.Generation); err != nil {
		return Replacement{}, err
	}
	if err := sqlitedb.Rename(staged, live); err != nil {
		return Replacement{}, err
	}
	sqlitedb.Step(ctx, "renamed")
	if err := ensureMarker(dir, rep.StoreID); err != nil {
		return Replacement{}, err
	}
	sqlitedb.Step(ctx, "store marker")
	return rep, nil
}

// save records in keep which of dir's store files exist, unless it did
// already, copies into keep each one that existed and keep lacks, and removes
// the journal beside the database. With nothing recorded and the database
// already the published image, it records instead that what it replaced is
// lost, and reports that.
func save(ctx context.Context, dir, keep string, published bool) (bool, error) {
	if err := sqlitedb.MakeDir(ctx, keep); err != nil {
		return false, err
	}
	rec, err := readKept(keep)
	if errors.Is(err, fs.ErrNotExist) {
		rec, err = recordKept(ctx, dir, keep, published)
	}
	if err != nil {
		return false, err
	}
	for _, f := range rec.files(dir, keep) {
		if has, err := sqlitedb.Exists(f.saved); err != nil || has || !f.existed {
			if err != nil {
				return false, err
			}
			continue
		}
		if err := sqlitedb.RemoveFile(f.saved + ".tmp"); err != nil {
			return false, err
		}
		if _, err := sqlitedb.CopyFile(ctx, f.path, f.saved+".tmp"); err != nil {
			return false, err
		}
		if err := sqlitedb.Rename(f.saved+".tmp", f.saved); err != nil {
			return false, err
		}
		sqlitedb.Step(ctx, "kept "+filepath.Base(f.path))
	}
	return rec.Lost, sqlitedb.RemoveFile(databasePath(dir) + "-journal")
}

// recordKept lets SQLite settle a journal beside a readable database, then
// records in keep which of dir's store files exist, or, when the database
// already is the published image, that what it replaced is lost.
func recordKept(ctx context.Context, dir, keep string, published bool) (kept, error) {
	_, err := sqlitedb.Probe(ctx, databasePath(dir))
	if err != nil && !errors.Is(err, sqlitedb.ErrUnreadable) && !errors.Is(err, sqlitedb.ErrNotExist) {
		return kept{}, err
	}
	rec := kept{Lost: published}
	if !published {
		var existed [3]bool
		for i, f := range (kept{}).files(dir, keep) {
			if existed[i], err = sqlitedb.Exists(f.path); err != nil {
				return kept{}, err
			}
		}
		rec = kept{Database: existed[0], Journal: existed[1], Marker: existed[2]}
	}
	data, err := json.Marshal(rec)
	if err == nil {
		err = atomicfile.WriteFile(filepath.Join(keep, keptName), data, 0o600)
	}
	if err != nil {
		return kept{}, err
	}
	sqlitedb.Step(ctx, "kept record")
	return rec, nil
}

// Revert restores the store files Replace saved in keep, removes those that
// did not exist before it and its staged copy, removes the access directory if
// that left it empty. generation is the one Replace published: while the database is
// at it, keep must hold every file Replace recorded, or Revert refuses with
// ErrKeptLost and changes nothing. Before Replace recorded anything, nothing of
// dir's store changed but its staged copy and the access directory. The caller
// holds the installation lock; a call after a crash at any point completes the
// revert.
func Revert(ctx context.Context, dir, keep, generation string) error {
	staged := databasePath(dir) + ".staged"
	rec, recErr := readKept(keep)
	if recErr != nil && !errors.Is(recErr, fs.ErrNotExist) {
		return recErr
	}
	inst, err := installed(ctx, dir)
	if err != nil && !errors.Is(err, ErrStoreIdentity) {
		return err
	}
	files := rec.files(dir, keep)
	if generation != "" && inst.Generation == generation {
		lost := recErr != nil || rec.Lost
		for _, f := range files {
			has, err := sqlitedb.Exists(f.saved)
			if err != nil {
				return err
			}
			lost = lost || f.existed && !has
		}
		if lost {
			return ErrKeptLost
		}
	}
	for _, f := range files {
		if recErr != nil {
			break // nothing recorded, so nothing replaced
		}
		has, err := sqlitedb.Exists(f.saved)
		switch {
		case err != nil:
		case has:
			err = sqlitedb.Rename(f.saved, f.path)
		case !f.existed:
			err = sqlitedb.RemoveFile(f.path)
		}
		// A file that existed and keep lacks was never replaced.
		if err != nil {
			return err
		}
		sqlitedb.Step(ctx, "restored "+filepath.Base(f.path))
	}
	if err := sqlitedb.RemoveFile(staged); err != nil {
		return err
	}
	return errors.Join(sqlitedb.RemoveDirIfEmpty(filepath.Dir(staged)), sqlitedb.SyncDir(dir))
}

func readKept(keep string) (kept, error) {
	var rec kept
	data, err := os.ReadFile(filepath.Join(keep, keptName))
	if err == nil {
		err = json.Unmarshal(data, &rec)
	}
	return rec, err
}
