package sqlstore

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/stperic/zzrouter/pkg/access/people"
	"github.com/stperic/zzrouter/pkg/audit"
	"github.com/stperic/zzrouter/pkg/sqlitedb"
)

// ErrNoStore means a directory holds neither an access database nor a store
// marker: a worker, or a node that never started as a coordinator.
var ErrNoStore = errors.New("no access store: neither a database nor a store marker")

// Image describes an image: a consistent copy of an access database in a file
// of its own.
type Image struct {
	Meta
	// Initialized reports that an owner exists. It is false for an image a
	// newer release wrote, whose records this release cannot read.
	Initialized bool
}

// errLiveImage refuses an image operation on the live database's file, which
// an open store would overwrite with what it publishes, and on anything but a
// regular file, since a link can name that file under another name.
var errLiveImage = errors.New("an image must be a regular file other than the live access database")

// admitImage refuses, before anything opens it, a path errLiveImage names and
// an image in WAL mode, as Unreadable: opening it read-write rewrites its header.
func admitImage(path string) error {
	if strings.EqualFold(filepath.Base(path), fileName) {
		return errLiveImage
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errLiveImage
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	// The file format versions, header bytes 18 and 19, are 2 in WAL mode; a
	// shorter file is SQLite's to judge.
	var header [20]byte
	if _, err := io.ReadFull(f, header[:]); err == nil && (header[18] == 2 || header[19] == 2) {
		return refuse(Unreadable, errors.New("the image is in WAL mode"))
	}
	return nil
}

// releaseObjects lists the schema objects this release's migrations create.
var releaseObjects = sync.OnceValues(func() ([]sqlitedb.Object, error) {
	return schema.Objects(context.Background())
})

// sameObjects refuses a database whose schema objects are not exactly the ones
// this release creates: a trigger, view or altered table, carried by an archive
// or added by another tool, would run inside the store's transactions, and the
// node must not serve a store it would refuse to back up.
func sameObjects(ctx context.Context, db *sqlitedb.DB) error {
	want, err := releaseObjects()
	if err != nil {
		return err
	}
	got, err := db.Objects(ctx)
	if err == nil && !slices.Equal(got, want) {
		err = &Refusal{Reason: IntegrityFailed, detail: "its schema objects differ from this release's"}
	}
	return err
}

// ReadsSchema reports whether this release checks and prepares an image at
// schema version v: images are never migrated, so only the latest version.
func ReadsSchema(v int) bool {
	latest, err := schema.Latest()
	return err == nil && v == latest
}

// MarkerStoreID returns the store ID a store marker's content names.
func MarkerStoreID(data []byte) (string, error) {
	var m marker
	if err := json.Unmarshal(data, &m); err != nil || m.Version != 1 || m.StoreID == "" {
		return "", errors.New("not a store marker")
	}
	return m.StoreID, nil
}

func (s *Store) image() Image {
	published := s.pub.Load()
	return Image{Meta: published.meta, Initialized: published.snap.Initialized()}
}

// Capture copies the live store into dst, which must not exist, at one
// committed sequence and returns the image's meta. It holds the writer gate for
// the copy only. A closed store refuses with access.ErrClosed, a failed one
// with ErrFailed and one another program holds with ErrBusy. A failure leaves
// no file at dst; an existing dst is refused and left as it was.
func (s *Store) Capture(ctx context.Context, dst string) (Meta, error) {
	s.gate.Lock()
	defer s.gate.Unlock()
	if s.refusal != nil {
		return Meta{}, s.refusal
	}
	if err := s.db.CopyTo(ctx, dst); err != nil {
		return Meta{}, busy(err)
	}
	return s.pub.Load().meta, nil
}

// CaptureStopped copies the store of the stopped installation in dir into dst,
// which must not exist, and checks the image as CheckImage does; the caller
// holds the installation lock. Recovering a journal a crash left is the only
// change it makes to the store. A database a newer release wrote is copied
// byte for byte and unchecked, so it can be backed up whether or not this
// release parses its schema; its Image holds the header's schema version and,
// when readable, the meta table. Neither a database nor a store marker is
// ErrNoStore; a marker alone is Missing, and a store that cannot open is its
// *Refusal. A failure leaves no file at dst.
func CaptureStopped(ctx context.Context, dir, dst string) (Image, error) {
	none, err := noStore(dir)
	if none {
		err = ErrNoStore
	}
	if err != nil {
		return Image{}, err
	}
	path := databasePath(dir)
	db, h, err := sqlitedb.OpenAny(ctx, path, schema)
	if err != nil {
		return Image{}, busy(fileRefusal(err))
	}
	defer func() { _ = db.Close() }() // read-only, opened after the journal settled
	if err := schema.Admit(h); errors.Is(err, sqlitedb.ErrNewer) {
		img := Image{Meta: Meta{SchemaVersion: h.Version}}
		// The meta table keeps its layout in every schema version.
		if m, err := loadMeta(ctx, db.SQL()); err == nil {
			img.Meta = m
		}
		if err := sqlitedb.CopyFile(ctx, path, dst); err != nil {
			return Image{}, err
		}
		return img, nil
	} else if err != nil {
		return Image{}, fileRefusal(err)
	}
	if err := db.CopyTo(ctx, dst); err != nil {
		return Image{}, busy(fileRefusal(err))
	}
	img, err := CheckImage(ctx, dst)
	if err != nil {
		if removeErr := os.Remove(dst); removeErr != nil {
			err = errors.Join(err, removeErr)
		}
	}
	return img, err
}

// CheckImage runs a start's checks on the image at path, with a full integrity
// check and its schema objects compared with this release's, and returns its
// meta. A failure is a *Refusal with a start's reason:
// an image a newer release wrote is SchemaNewer, one at an older schema version
// Unreadable, since images are never migrated. It never changes the image's
// content, size or modification time, but restricts the file to its owner as
// every open does; where that fails, on a read-only medium, it returns a plain
// error, not a Refusal. The live database's file, a path that is not a
// regular file and an image in WAL mode are refused before anything opens them.
func CheckImage(ctx context.Context, path string) (Image, error) {
	s := &Store{}
	if err := s.attach(ctx, path, true); err != nil {
		return Image{}, err
	}
	return s.image(), s.Close()
}

// PrepareImage readies the image at path to replace a store's content, in one
// transaction: a new random generation with the store ID kept, a new credential
// revision for every person, every pending sign-in and enrollment grant
// expired, and an audit row recording the restore. Person and app-key
// references issued before stop resolving; password and key hashes and used
// grants are kept. It refuses what CheckImage refuses.
func PrepareImage(ctx context.Context, path string) (Image, error) {
	s := &Store{}
	if err := s.attach(ctx, path, true); err != nil {
		return Image{}, err
	}
	restored := s.pub.Load().meta
	err := s.mutate(ctx, peopleRecords, func(t *tx) error {
		t.generation = uuid.NewString()
		t.record(audit.Event{Action: audit.ActionRestorePrepared, Actor: audit.ActorLocalOperator, Subject: restored.StoreID,
			Metadata: map[string]any{"generation": t.generation, "restored_generation": restored.Generation}})
		var err error
		t.people, err = people.NewState(people.Restaged(t.people.Records()))
		return err
	})
	prepared := s.image()
	if err = errors.Join(err, s.Close()); err != nil {
		return Image{}, err
	}
	return prepared, nil
}
