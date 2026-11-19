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

// kept records which of an installation's store files existed.
type kept struct {
	Database bool `json:"database"`
	Journal  bool `json:"journal"` // what SQLite left of a journal: one it could not settle, or a cold one
	Marker   bool `json:"marker"`
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

// Replace publishes the image at src as dir's database and store marker,
// saving what it replaces in keep; want is the SHA-256 the caller checked src
// at, and foreign admits another store ID (AdmitStoreID). The caller holds the
// installation lock. Replace copies src beside the database, refusing a copy
// whose digest is not want before anything of the installation changes, and
// checks the copy; records in keep which store files exist, once SQLite settled
// a journal beside a readable database; copies them into keep and removes a
// journal it could not settle, which SQLite would otherwise roll back into the
// new file; renames the copy over the database, writes the marker, and gives
// every file it wrote the owner of dir. Each step is durable before the next,
// and a call after a crash at any point completes the replacement; Revert
// undoes it.
func Replace(ctx context.Context, dir, src, want, keep string, foreign bool) (Image, error) {
	live := databasePath(dir)
	staged := live + ".staged"
	if err := sqlitedb.MakeDir(filepath.Dir(live)); err != nil {
		return Image{}, err
	}
	if err := removeLeftovers(staged); err != nil {
		return Image{}, err
	}
	sum, err := sqlitedb.CopyFile(ctx, src, staged)
	if err == nil && sum != want {
		err = fmt.Errorf("the image is not the one checked: its SHA-256 is %s, not %s", sum, want)
	}
	var img Image
	if err == nil {
		sqlitedb.Step(ctx, "staged")
		img, err = CheckImage(ctx, staged)
	}
	if err == nil {
		_, err = AdmitStoreID(ctx, dir, img.StoreID, foreign)
	}
	if err != nil {
		return Image{}, errors.Join(err, removeLeftovers(staged))
	}
	if err := save(ctx, dir, keep); err != nil {
		return Image{}, err
	}
	if err := sqlitedb.Rename(staged, live); err != nil {
		return Image{}, err
	}
	sqlitedb.Step(ctx, "renamed")
	if err := ensureMarker(dir, img.StoreID); err != nil {
		return Image{}, err
	}
	sqlitedb.Step(ctx, "store marker")
	written := []string{filepath.Dir(live), live, filepath.Join(dir, MarkerName), keep, filepath.Join(keep, keptName)}
	for _, f := range (kept{}).files(dir, keep) {
		written = append(written, f.saved)
	}
	return img, sqlitedb.GiveOwner(ctx, dir, written...)
}

// save records in keep which of dir's store files exist, unless it did
// already, copies into keep each one that existed and keep lacks, and removes
// the journal beside the database.
func save(ctx context.Context, dir, keep string) error {
	if err := sqlitedb.MakeDir(keep); err != nil {
		return err
	}
	rec, err := readKept(keep)
	if errors.Is(err, fs.ErrNotExist) {
		rec, err = recordKept(ctx, dir, keep)
	}
	if err != nil {
		return err
	}
	for _, f := range rec.files(dir, keep) {
		if has, err := exists(f.saved); err != nil || has || !f.existed {
			if err != nil {
				return err
			}
			continue
		}
		if err := removeLeftovers(f.saved + ".tmp"); err != nil {
			return err
		}
		if _, err := sqlitedb.CopyFile(ctx, f.path, f.saved+".tmp"); err != nil {
			return err
		}
		if err := sqlitedb.Rename(f.saved+".tmp", f.saved); err != nil {
			return err
		}
		sqlitedb.Step(ctx, "kept "+filepath.Base(f.path))
	}
	journal := databasePath(dir) + "-journal"
	if err := removeLeftovers(journal); err != nil {
		return err
	}
	return sqlitedb.SyncDir(filepath.Dir(journal))
}

// recordKept lets SQLite settle a journal beside a readable database, then
// records in keep which of dir's store files exist.
func recordKept(ctx context.Context, dir, keep string) (kept, error) {
	_, err := sqlitedb.Probe(ctx, databasePath(dir))
	if err != nil && !errors.Is(err, sqlitedb.ErrUnreadable) && !errors.Is(err, sqlitedb.ErrNotExist) {
		return kept{}, err
	}
	var existed [3]bool
	for i, f := range (kept{}).files(dir, keep) {
		if existed[i], err = exists(f.path); err != nil {
			return kept{}, err
		}
	}
	rec := kept{Database: existed[0], Journal: existed[1], Marker: existed[2]}
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
// did not exist before it and its staged copy, and gives what it restored the
// owner of dir. generation is the one Replace published: while the database is
// at it, keep must hold every file Replace recorded, or Revert refuses with
// ErrKeptLost and changes nothing. Otherwise, before Replace recorded anything
// nothing of dir's store changed, and Revert removes only the staged copy. The
// caller holds the installation lock; a call after a crash at any point
// completes the revert.
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
		lost := recErr != nil
		for _, f := range files {
			has, err := exists(f.saved)
			if err != nil {
				return err
			}
			lost = lost || f.existed && !has
		}
		if lost {
			return ErrKeptLost
		}
	}
	if recErr != nil {
		return removeLeftovers(staged)
	}
	var restored []string
	for _, f := range files {
		has, err := exists(f.saved)
		switch {
		case err != nil:
		case has:
			err = sqlitedb.Rename(f.saved, f.path)
		case !f.existed:
			err = removeLeftovers(f.path)
		}
		// A file that existed and keep lacks was never replaced.
		if err != nil {
			return err
		}
		restored = append(restored, f.path)
		sqlitedb.Step(ctx, "restored "+filepath.Base(f.path))
	}
	if err := removeLeftovers(staged); err != nil {
		return err
	}
	if err := errors.Join(sqlitedb.SyncDir(filepath.Dir(staged)), sqlitedb.SyncDir(dir)); err != nil {
		return err
	}
	return sqlitedb.GiveOwner(ctx, dir, restored...)
}

func readKept(keep string) (kept, error) {
	var rec kept
	data, err := os.ReadFile(filepath.Join(keep, keptName))
	if err == nil {
		err = json.Unmarshal(data, &rec)
	}
	return rec, err
}

func exists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}
