package recovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"time"

	"github.com/stperic/common/atomicfile"
	"github.com/stperic/zzrouter/pkg/access/sqlstore"
	"github.com/stperic/zzrouter/pkg/security"
	"github.com/stperic/zzrouter/pkg/sqlitedb"
	"github.com/stperic/zzrouter/pkg/utils"
)

const (
	// activationFile is the activation marker beside node.yaml: while it
	// exists, an activation is incomplete and the node does not start.
	activationFile = "recovery-activation.json"
	// rollbackDir holds a directory per activation, named after the generation
	// it published: rollbackFile, and in store/ what the store replaced.
	rollbackDir  = "rollback"
	rollbackFile = "rollback.json"
)

// Reason is the closed cause of a Refusal.
type Reason string

const (
	// ActivationIncomplete means an activation did not finish: the node does
	// not start until it is finished or rolled back.
	ActivationIncomplete Reason = "activation_incomplete"
	// InUse means a running node or another recovery command holds the
	// installation lock.
	InUse Reason = "in_use"
	// StagingInvalid means the directory is not a complete staged database
	// bundle, or changed since restore staged it.
	StagingInvalid Reason = "staging_invalid"
	// ForeignStore means the installation holds another access store.
	ForeignStore Reason = "foreign_store"
	// AlreadyActivated means the staging directory's activation finished.
	AlreadyActivated Reason = "already_activated"
	// NoActivation means a rollback found no incomplete activation.
	NoActivation Reason = "no_activation"
)

// Refusal is an activation refused before it changed anything, or a start
// refused while one is incomplete.
type Refusal struct {
	Reason     Reason
	Detail     string // what was found
	NextAction string // the operator's next step
}

func (r *Refusal) Error() string { return string(r.Reason) + ": " + r.Detail + ". " + r.NextAction }

// ActivateOptions selects what Activate does: Rollback undoes an incomplete
// activation instead; DryRun runs every check and changes nothing.
type ActivateOptions struct{ Rollback, DryRun bool }

// Activation describes an activation: the staged image it publishes, its
// rollback directory's ID, which is the image's generation, and the targets
// whose previous content that directory keeps.
type Activation struct {
	Profile    Profile
	Store      sqlstore.Image
	RollbackID string
	Replaced   []string
}

// activation is an activation's record: its marker while it runs, and
// rollbackFile in its rollback directory.
type activation struct {
	Version    int       `json:"version"`
	Profile    Profile   `json:"profile"`
	Staging    string    `json:"staging"`
	Generation string    `json:"generation"`
	Created    time.Time `json:"created"`
	Replaced   []string  `json:"replaced"`
}

// Activate publishes the staged database bundle in staging as the access store
// of the stopped installation in dir, holding its installation lock. Its checks
// run first, in order and read-only: no INCOMPLETE mark, a staging manifest of
// a staged database bundle, exactly its entries, the image's digest, check and
// generation as staged, and the installation's store identity
// (sqlstore.AdmitStoreID). A refusal changes nothing. It then writes the
// activation marker and the rollback directory, replaces the store
// (sqlstore.Replace), and removes the marker and the staging directory. A crash
// in between leaves the marker, which stops the node (PendingActivation): a
// rerun completes the activation, and Rollback instead restores what the
// rollback directory keeps. DryRun stops after the checks.
func Activate(ctx context.Context, dir, staging string, o ActivateOptions) (Activation, error) {
	lock, err := lockInstallation(dir)
	switch {
	case errors.Is(err, atomicfile.ErrLocked):
		return Activation{}, &Refusal{InUse, "the installation lock is held", "Stop the service, its supervisor's restarts and any other recovery command, then run this again."}
	case errors.Is(err, fs.ErrNotExist):
		return Activation{}, fmt.Errorf("missing installation lock; start this installation with the current release first: %w", err)
	case err != nil:
		return Activation{}, err
	}
	defer func() { _ = lock.Close() }()
	if staging, err = filepath.Abs(staging); err != nil {
		return Activation{}, err
	}
	pending, err := readActivation(dir)
	if err != nil {
		return Activation{}, err
	}
	if pending != nil && pending.Staging != staging {
		return Activation{}, incomplete(dir, pending)
	}
	if o.Rollback {
		return rollBack(ctx, dir, pending, o.DryRun)
	}
	a, err := checkStaging(ctx, dir, staging, pending)
	if err != nil || o.DryRun {
		return a, err
	}
	record := activation{Version: 1, Profile: a.Profile, Staging: staging, Generation: a.RollbackID, Created: utils.NowUTC(), Replaced: a.Replaced}
	if pending != nil {
		record = *pending
	} else if err := writeRecord(ctx, filepath.Join(dir, activationFile), record, "activation marker"); err != nil {
		return a, err
	}
	if err := replaceStore(ctx, dir, staging, record); err != nil {
		return a, errors.Join(err, incomplete(dir, &record))
	}
	if err := os.RemoveAll(staging); err != nil {
		return a, fmt.Errorf("the activation finished, but its staging directory, which holds credential hashes, could not be removed; delete it: %w", err)
	}
	return a, nil
}

// replaceStore runs the steps between the activation marker and its removal.
func replaceStore(ctx context.Context, dir, staging string, record activation) error {
	rollback := filepath.Join(dir, rollbackDir, record.Generation)
	for _, path := range []string{filepath.Dir(rollback), rollback} {
		if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		if err := errors.Join(security.RestrictDirectoryPermissions(path), sqlitedb.SyncDir(filepath.Dir(path))); err != nil {
			return err
		}
	}
	if err := writeRecord(ctx, filepath.Join(rollback, rollbackFile), record, "rollback record"); err != nil {
		return err
	}
	if _, err := sqlstore.Replace(ctx, dir, stagedImage(staging), filepath.Join(rollback, "store"), false); err != nil {
		return err
	}
	return removeMarker(ctx, dir)
}

// checkStaging runs Activate's checks, in order and read-only.
func checkStaging(ctx context.Context, dir, staging string, pending *activation) (Activation, error) {
	invalid := func(detail string) error {
		return &Refusal{StagingInvalid, detail, "Stage the database bundle again with zzrouter-node recovery restore into a new directory, and activate that one."}
	}
	if _, err := os.Lstat(filepath.Join(staging, incompleteName)); !errors.Is(err, fs.ErrNotExist) {
		if err == nil {
			err = invalid("the staging directory is marked " + incompleteName)
		}
		return Activation{}, err
	}
	data, err := readBounded(filepath.Join(staging, "manifest.json"), maxManifest)
	var m Manifest
	if err == nil {
		m, err = decodeManifest(data)
	}
	if err != nil {
		return Activation{}, invalid("no staging manifest: " + err.Error())
	}
	if m.Profile != ProfileDatabase || m.Staged == nil || m.Store == nil {
		return Activation{}, invalid(fmt.Sprintf("it is not a staged database bundle (profile %q)", m.Profile))
	}
	var entries []string
	err = filepath.WalkDir(staging, func(path string, _ fs.DirEntry, err error) error {
		if err == nil && path != staging {
			var rel string
			rel, err = filepath.Rel(staging, path)
			entries = append(entries, filepath.ToSlash(rel))
		}
		return err
	})
	if err != nil {
		return Activation{}, err
	}
	if !slices.Equal(entries, []string{"manifest.json", path.Dir(imageName), imageName}) {
		return Activation{}, invalid("it holds entries other than manifest.json and " + imageName)
	}
	_, sum, err := hashFile(ctx, staging, filepath.FromSlash(imageName))
	if err != nil {
		return Activation{}, err
	}
	if sum != m.Staged.ImageSHA256 {
		return Activation{}, invalid("the image's SHA-256 is not staged.image_sha256")
	}
	img, err := sqlstore.CheckImage(ctx, stagedImage(staging))
	if err != nil {
		return Activation{}, archiveImage(err)
	}
	if img.Generation != m.Staged.Generation {
		return Activation{}, invalid("the image's generation is not staged.generation")
	}
	installed, err := sqlstore.AdmitStoreID(ctx, dir, img.StoreID, false)
	if errors.Is(err, sqlstore.ErrStoreIdentity) {
		err = &Refusal{ForeignStore, err.Error(), "Activate a backup of this installation's own access store; to replace it with another installation's, move this installation's access directory and store marker aside first."}
	}
	if err != nil {
		return Activation{}, err
	}
	a := Activation{Profile: m.Profile, Store: img, RollbackID: img.Generation}
	if installed != "" {
		a.Replaced = []string{"store"}
	}
	if pending != nil {
		a.Replaced = pending.Replaced
	} else if _, err := os.Lstat(filepath.Join(dir, rollbackDir, a.RollbackID)); !errors.Is(err, fs.ErrNotExist) {
		if err == nil {
			err = &Refusal{AlreadyActivated, "this staging directory was activated: its rollback " + a.RollbackID + " exists", "Delete the staging directory, which holds credential hashes; to activate this backup again, stage its archive again."}
		}
		return a, err
	}
	return a, nil
}

// rollBack restores what the incomplete activation pending replaced, then
// removes its rollback directory and its marker. Without a marker there is
// nothing it may undo: a finished activation is restored by hand.
func rollBack(ctx context.Context, dir string, pending *activation, dryRun bool) (Activation, error) {
	if pending == nil {
		return Activation{}, &Refusal{NoActivation, "no activation is incomplete", "A finished activation's rollback directory keeps what it replaced: restore from it by hand as docs/recovery.md describes."}
	}
	a := Activation{Profile: pending.Profile, RollbackID: pending.Generation, Replaced: pending.Replaced}
	if dryRun {
		return a, nil
	}
	if err := revertStore(ctx, dir, pending.Generation); err != nil {
		return a, errors.Join(err, incomplete(dir, pending))
	}
	return a, nil
}

// revertStore restores what the store replaced and removes the rollback
// directory, then the marker.
func revertStore(ctx context.Context, dir, generation string) error {
	rollback := filepath.Join(dir, rollbackDir, generation)
	if err := sqlstore.Revert(ctx, dir, filepath.Join(rollback, "store")); err != nil {
		return err
	}
	if err := os.RemoveAll(rollback); err != nil {
		return err
	}
	// Without the parent, a crash came before the directory existed.
	if err := sqlitedb.SyncDir(filepath.Dir(rollback)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	sqlitedb.Step(ctx, "rollback directory removed")
	return removeMarker(ctx, dir)
}

// PendingActivation refuses with ActivationIncomplete while dir holds an
// activation marker, naming the commands that finish or undo the activation.
// A start calls it holding the installation lock, before it writes anything.
func PendingActivation(dir string) error {
	pending, err := readActivation(dir)
	if err != nil || pending == nil {
		return err
	}
	return incomplete(dir, pending)
}

func incomplete(dir string, a *activation) error {
	command := "zzrouter-node recovery activate " + a.Staging + " --dir " + dir
	return &Refusal{ActivationIncomplete, "the activation of " + a.Staging + " (rollback " + a.Generation + ") did not finish",
		"Keep the node stopped and run " + command + " to finish it, or " + command + " --rollback to undo it."}
}

// readActivation reads dir's activation marker: nil without one.
func readActivation(dir string) (*activation, error) {
	data, err := readBounded(filepath.Join(dir, activationFile), 1<<20)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	var a activation
	if err == nil {
		err = json.Unmarshal(data, &a)
	}
	if err != nil {
		return nil, fmt.Errorf("activation marker %s: %w", activationFile, err)
	}
	return &a, nil
}

// writeRecord writes record to path durably and reports step.
func writeRecord(ctx context.Context, path string, record activation, step string) error {
	data, err := json.Marshal(record)
	if err == nil {
		err = atomicfile.WriteFile(path, data, 0o600)
	}
	if err == nil {
		sqlitedb.Step(ctx, step)
	}
	return err
}

func removeMarker(ctx context.Context, dir string) error {
	if err := errors.Join(os.Remove(filepath.Join(dir, activationFile)), sqlitedb.SyncDir(dir)); err != nil {
		return err
	}
	sqlitedb.Step(ctx, "activation marker removed")
	return nil
}

func stagedImage(staging string) string { return filepath.Join(staging, filepath.FromSlash(imageName)) }

// Rollback is an activation's rollback directory, kept until the operator
// removes it: it holds the access records the activation replaced, credential
// hashes included.
type Rollback struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	SizeBytes int64     `json:"size_bytes"`
}

// Rollbacks lists dir's rollback directories, oldest first.
func Rollbacks(dir string) ([]Rollback, error) {
	root := filepath.Join(dir, rollbackDir)
	entries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var list []Rollback
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || !info.IsDir() {
			continue // removed meanwhile, or not a rollback directory
		}
		// A directory whose record is not written yet dates from its creation.
		r := Rollback{ID: entry.Name(), CreatedAt: info.ModTime().UTC()}
		var record activation
		if data, err := readBounded(filepath.Join(root, r.ID, rollbackFile), 1<<20); err == nil && json.Unmarshal(data, &record) == nil {
			r.CreatedAt = record.Created
		}
		err = filepath.WalkDir(filepath.Join(root, r.ID), func(_ string, d fs.DirEntry, err error) error {
			if err == nil && d.Type().IsRegular() {
				var info fs.FileInfo
				if info, err = d.Info(); err == nil {
					r.SizeBytes += info.Size()
				}
			}
			return err
		})
		if err != nil {
			return nil, err
		}
		list = append(list, r)
	}
	slices.SortFunc(list, func(a, b Rollback) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return list, nil
}
