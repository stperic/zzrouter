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

// ErrStoreIdentity refuses an image whose store ID an installation does not
// admit: its store marker, or without one its database, admits only its own
// store ID, and only an installation with neither admits another.
var ErrStoreIdentity = errors.New("the installation holds another access store")

// keptName is Replace's record, in keep, of the store files that existed
// before it changed any.
const keptName = "kept.json"

// kept records which of an installation's store files existed.
type kept struct {
	Database bool `json:"database"`
	Journal  bool `json:"journal"` // what SQLite left of a journal: one it could not settle, or a cold one
	Marker   bool `json:"marker"`
}

// storeFile is one file Replace saves and Revert restores.
type storeFile struct {
	path    string
	existed bool
}

func (k kept) files(dir string) []storeFile {
	live := databasePath(dir)
	return []storeFile{{live, k.Database}, {live + "-journal", k.Journal}, {filepath.Join(dir, MarkerName), k.Marker}}
}

// AdmitStoreID returns the store ID dir's installation holds, "" when it has
// neither a store marker nor a database, and refuses with ErrStoreIdentity a
// storeID it does not admit, unless foreign, or an installation whose store ID
// it cannot read. The marker names the store; without one, the database does,
// read after SQLite settles a journal beside it.
func AdmitStoreID(ctx context.Context, dir, storeID string, foreign bool) (string, error) {
	installed, err := installedStoreID(ctx, dir)
	if err == nil && installed != "" && installed != storeID && !foreign {
		err = fmt.Errorf("%w (store ID %s, not the image's %s)", ErrStoreIdentity, installed, storeID)
	}
	return installed, err
}

func installedStoreID(ctx context.Context, dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, MarkerName))
	if err == nil {
		return MarkerStoreID(data)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	db, _, err := sqlitedb.OpenAny(ctx, databasePath(dir), schema)
	if errors.Is(err, sqlitedb.ErrNotExist) {
		return "", nil
	}
	var m Meta
	if err == nil {
		m, err = loadMeta(ctx, db.SQL())
		err = errors.Join(err, db.Close())
	}
	if err != nil {
		return "", fmt.Errorf("%w: its database has no store marker and its store ID cannot be read: %w", ErrStoreIdentity, err)
	}
	return m.StoreID, nil
}

// Replace publishes the checked image at src as dir's database and store
// marker, saving what it replaces in keep; foreign admits another store ID
// (AdmitStoreID). The caller holds the installation lock. Replace copies src
// beside the database and checks the copy; records in keep which store files
// exist, once SQLite settled a journal beside a readable database; copies them
// into keep and removes a journal it could not settle, which SQLite would
// otherwise roll back into the new file; renames the copy over the database and
// writes the marker. Each step is durable before the next, and a call after a
// crash at any point completes the replacement; Revert undoes it.
func Replace(ctx context.Context, dir, src, keep string, foreign bool) (Image, error) {
	live := databasePath(dir)
	staged := live + ".staged"
	if err := makePrivateDir(filepath.Dir(live)); err != nil {
		return Image{}, err
	}
	if err := removeLeftovers(staged); err != nil {
		return Image{}, err
	}
	if err := sqlitedb.CopyFile(ctx, src, staged); err != nil {
		return Image{}, err
	}
	img, err := CheckImage(ctx, staged)
	if err == nil {
		_, err = AdmitStoreID(ctx, dir, img.StoreID, foreign)
	}
	if err != nil {
		return Image{}, errors.Join(err, removeLeftovers(staged))
	}
	sqlitedb.Step(ctx, "staged")
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
	return img, nil
}

// save records in keep which of dir's store files exist, unless it did
// already, copies into keep each one that existed and keep lacks, and removes
// the journal beside the database.
func save(ctx context.Context, dir, keep string) error {
	if err := makePrivateDir(keep); err != nil {
		return err
	}
	if err := sqlitedb.SyncDir(filepath.Dir(keep)); err != nil {
		return err
	}
	rec, err := readKept(keep)
	if errors.Is(err, fs.ErrNotExist) {
		rec, err = recordKept(ctx, dir, keep)
	}
	if err != nil {
		return err
	}
	for _, f := range rec.files(dir) {
		saved := filepath.Join(keep, filepath.Base(f.path))
		if has, err := exists(saved); err != nil || has || !f.existed {
			if err != nil {
				return err
			}
			continue
		}
		if err := removeLeftovers(saved + ".tmp"); err != nil {
			return err
		}
		if err := sqlitedb.CopyFile(ctx, f.path, saved+".tmp"); err != nil {
			return err
		}
		if err := sqlitedb.Rename(saved+".tmp", saved); err != nil {
			return err
		}
		sqlitedb.Step(ctx, "kept "+filepath.Base(f.path))
	}
	return removeLeftovers(databasePath(dir) + "-journal")
}

// recordKept lets SQLite settle a journal beside a readable database, then
// records in keep which of dir's store files exist.
func recordKept(ctx context.Context, dir, keep string) (kept, error) {
	_, err := sqlitedb.Probe(ctx, databasePath(dir))
	if err != nil && !errors.Is(err, sqlitedb.ErrUnreadable) && !errors.Is(err, sqlitedb.ErrNotExist) {
		return kept{}, err
	}
	var rec kept
	live := databasePath(dir)
	for path, existed := range map[string]*bool{live: &rec.Database, live + "-journal": &rec.Journal, filepath.Join(dir, MarkerName): &rec.Marker} {
		if *existed, err = exists(path); err != nil {
			return kept{}, err
		}
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

// Revert restores the store files Replace saved in keep and removes those that
// did not exist before it, with its staged copy. Before Replace recorded
// anything, nothing of dir's store changed, and Revert removes only the staged
// copy. The caller holds the installation lock; a call after a crash at any
// point completes the revert.
func Revert(ctx context.Context, dir, keep string) error {
	staged := databasePath(dir) + ".staged"
	rec, err := readKept(keep)
	if errors.Is(err, fs.ErrNotExist) {
		return removeLeftovers(staged)
	}
	if err != nil {
		return err
	}
	for _, f := range rec.files(dir) {
		saved := filepath.Join(keep, filepath.Base(f.path))
		has, err := exists(saved)
		switch {
		case err != nil:
		case has:
			err = sqlitedb.Rename(saved, f.path)
		case !f.existed:
			err = removeLeftovers(f.path)
		}
		// A file that existed and keep lacks was never replaced.
		if err != nil {
			return err
		}
		sqlitedb.Step(ctx, "restored "+filepath.Base(f.path))
	}
	if err := removeLeftovers(staged); err != nil {
		return err
	}
	return errors.Join(sqlitedb.SyncDir(filepath.Dir(staged)), sqlitedb.SyncDir(dir))
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
