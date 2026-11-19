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
// rollback directory's ID, which is the image's generation, and absolute path,
// the targets whose previous content that directory keeps, and those it
// replaced without keeping it, because the directory was deleted while the
// activation was incomplete.
type Activation struct {
	Profile     Profile
	Store       sqlstore.Image
	RollbackID  string
	RollbackDir string
	Replaced    []string
	Lost        []string
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
// run first, in order and read-only: a staging directory outside the
// installation, no INCOMPLETE mark, a staging manifest of a staged database
// bundle, exactly its entries, the image's digest, check and generation as
// staged, the installation's store identity (sqlstore.AdmitStoreID), and a
// generation that is neither live nor kept in a rollback directory. A refusal
// changes nothing. It then writes
// the activation marker and the rollback directory, replaces the store
// (sqlstore.Replace) with the bytes it checked, gives what it wrote the
// installation's owner, and removes the marker and the staging directory. A
// crash in between leaves the marker, which stops the node
// (PendingActivation): a rerun completes the activation, and Rollback instead
// restores what the rollback directory keeps. DryRun stops after the checks.
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
	if dir, err = filepath.Abs(dir); err == nil {
		staging, err = filepath.Abs(staging)
	}
	if err != nil {
		return Activation{}, err
	}
	pending, err := readActivation(dir)
	if err != nil {
		return Activation{}, err
	}
	if pending != nil && pending.Staging != staging {
		return Activation{}, incomplete(dir, pending, nil)
	}
	if o.Rollback {
		return rollBack(ctx, dir, pending, o.DryRun)
	}
	a, sum, err := checkStaging(ctx, dir, staging, pending)
	if err != nil && pending != nil {
		return a, incomplete(dir, pending, err)
	}
	if err != nil || o.DryRun {
		return a, err
	}
	record := activation{Version: 1, Profile: a.Profile, Staging: staging, Generation: a.RollbackID, Created: utils.NowUTC(), Replaced: a.Replaced}
	if pending != nil {
		record = *pending
	} else if err := writeRecord(ctx, filepath.Join(dir, activationFile), record, "activation marker"); err != nil {
		return a, err
	}
	lost, err := replaceStore(ctx, dir, staging, sum, record)
	if err != nil {
		return a, incomplete(dir, &record, err)
	}
	if lost {
		a.Replaced, a.Lost = nil, a.Replaced
	}
	if err := os.RemoveAll(staging); err != nil {
		return a, fmt.Errorf("the activation finished, but its staging directory, which holds credential hashes, could not be removed; delete it: %w", err)
	}
	return a, nil
}

// replaceStore runs the steps between the activation marker and its removal:
// the store gets the bytes checked at sum, and everything written gets the
// installation's owner before the marker goes. It reports whether what the
// store replaced is lost (sqlstore.Replacement).
func replaceStore(ctx context.Context, dir, staging, sum string, record activation) (bool, error) {
	marker := filepath.Join(dir, activationFile)
	if err := sqlitedb.GiveOwner(ctx, dir, marker); err != nil {
		return false, err
	}
	rollback := filepath.Join(dir, rollbackDir, record.Generation)
	if err := sqlitedb.MakeDir(ctx, rollback); err != nil {
		return false, err
	}
	if err := writeRecord(ctx, filepath.Join(rollback, rollbackFile), record, "rollback record"); err != nil {
		return false, err
	}
	if err := sqlitedb.GiveOwner(ctx, dir, filepath.Dir(rollback), rollback, filepath.Join(rollback, rollbackFile)); err != nil {
		return false, err
	}
	rep, err := sqlstore.Replace(ctx, dir, stagedImage(staging), sum, filepath.Join(rollback, "store"), false)
	if err == nil && rep.Generation != record.Generation {
		err = fmt.Errorf("the published image is generation %s, not the staged %s", rep.Generation, record.Generation)
	}
	if err != nil {
		return false, err
	}
	return rep.Lost, removeMarker(ctx, dir)
}

// checkStaging runs Activate's checks, in order and read-only, and returns the
// activation and the staged image's SHA-256.
func checkStaging(ctx context.Context, dir, staging string, pending *activation) (Activation, string, error) {
	restage := "Stage the database bundle again with zzrouter-node recovery restore into a new directory, and activate that one."
	invalid := func(detail, next string) (Activation, string, error) {
		return Activation{}, "", &Refusal{StagingInvalid, detail, next}
	}
	if err := CheckStagingDir(dir, staging); err != nil {
		return Activation{}, "", err
	}
	if _, err := os.Lstat(filepath.Join(staging, incompleteName)); !errors.Is(err, fs.ErrNotExist) {
		if err != nil {
			return Activation{}, "", err
		}
		return invalid("the staging directory is marked "+incompleteName, restage)
	}
	data, err := readBounded(filepath.Join(staging, "manifest.json"), maxManifest)
	var m Manifest
	if err == nil {
		m, err = decodeManifest(data)
	}
	switch {
	case err != nil:
		return invalid("no staging manifest: "+err.Error(), restage)
	case m.Profile == ProfileInstallation:
		return invalid("it holds an installation bundle, which activate does not publish", "Stop the service, then restore it by hand as docs/recovery.md describes under Restore an installation bundle by hand.")
	case m.Profile == ProfileHandoff:
		return invalid("it holds a handoff bundle, whose activation is not in this release", "Keep the staging directory; docs/recovery.md describes what a handoff needs under Coordinator handoff.")
	case m.Profile != ProfileDatabase || m.Staged == nil || m.Store == nil:
		return invalid(fmt.Sprintf("it is not a staged database bundle (profile %q)", m.Profile), restage)
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
		return Activation{}, "", err
	}
	if !slices.Equal(entries, []string{"manifest.json", path.Dir(imageName), imageName}) {
		return invalid("it holds entries other than manifest.json and "+imageName, restage)
	}
	_, sum, err := hashFile(ctx, staging, filepath.FromSlash(imageName))
	if err != nil {
		return Activation{}, "", err
	}
	if sum != m.Staged.ImageSHA256 {
		return invalid("the image's SHA-256 is not staged.image_sha256", restage)
	}
	img, err := sqlstore.CheckImage(ctx, stagedImage(staging))
	if err != nil {
		return Activation{}, "", archiveImage(err)
	}
	if img.Generation != m.Staged.Generation {
		return invalid("the image's generation is not staged.generation", restage)
	}
	installed, err := sqlstore.AdmitStoreID(ctx, dir, img.StoreID, false)
	if errors.Is(err, sqlstore.ErrStoreIdentity) {
		err = &Refusal{ForeignStore, err.Error(), "Activate a backup of this installation's own access store; to replace it with another installation's, move this installation's access directory and store marker aside first."}
	}
	if err != nil {
		return Activation{}, "", err
	}
	rollback := filepath.Join(dir, rollbackDir, img.Generation)
	a := Activation{Profile: m.Profile, Store: img, RollbackID: img.Generation, RollbackDir: rollback}
	if installed.StoreID != "" {
		a.Replaced = []string{"store"}
	}
	if pending != nil {
		a.Replaced = pending.Replaced
		return a, sum, nil
	}
	again := "Delete the staging directory, which holds credential hashes. To activate this backup again, restore its archive into a new staging directory, which gives it a new generation."
	if installed.Generation == img.Generation {
		return a, "", &Refusal{AlreadyActivated, "the access store already is generation " + img.Generation, again}
	}
	if _, err := os.Lstat(rollback); !errors.Is(err, fs.ErrNotExist) {
		if err == nil {
			err = &Refusal{AlreadyActivated, "generation " + img.Generation + " was activated here before: " + rollback + " exists", again}
		}
		return a, "", err
	}
	return a, sum, nil
}

// CheckStagingDir refuses, as staging_invalid, a staging directory inside, equal
// to or holding the installation in dir: a later installation backup would pack
// what it holds, and an activation removes it.
func CheckStagingDir(dir, staging string) error {
	if within(dir, staging) || within(staging, dir) {
		return &Refusal{StagingInvalid, "the staging directory and the installation overlap", "Use a staging directory outside the installation's configuration directory."}
	}
	return nil
}

// within reports whether path is root or inside it.
func within(root, path string) bool {
	rel, err := filepath.Rel(resolved(root), resolved(path))
	return err == nil && filepath.IsLocal(rel)
}

// resolved is path made absolute with its links resolved, or those of its
// parent when it does not exist yet.
func resolved(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	if parent, err := filepath.EvalSymlinks(filepath.Dir(abs)); err == nil {
		return filepath.Join(parent, filepath.Base(abs))
	}
	return abs
}

// rollBack restores what the incomplete activation pending replaced and gives
// it the installation's owner, then removes the rollback directory and the
// marker. Without a marker there is nothing it may undo: a finished activation
// is restored by hand.
func rollBack(ctx context.Context, dir string, pending *activation, dryRun bool) (Activation, error) {
	if pending == nil {
		return Activation{}, &Refusal{NoActivation, "no activation is incomplete", "A finished activation's rollback directory keeps what it replaced: restore from it by hand as docs/recovery.md describes."}
	}
	rollback := filepath.Join(dir, rollbackDir, pending.Generation)
	a := Activation{Profile: pending.Profile, RollbackID: pending.Generation, RollbackDir: rollback, Replaced: pending.Replaced}
	if dryRun {
		return a, nil
	}
	err := sqlstore.Revert(ctx, dir, filepath.Join(rollback, "store"), pending.Generation)
	if errors.Is(err, sqlstore.ErrKeptLost) {
		finish := "zzrouter-node recovery activate " + pending.Staging + " --dir " + dir
		return a, &Refusal{ActivationIncomplete, "the activation of " + pending.Staging + " cannot be rolled back: " + rollback + " no longer holds what it replaced, and the access store already is the staged image",
			"Stop the service, then run " + finish + " to finish it; without that staging directory, delete " + filepath.Join(dir, activationFile) + " to finish it by hand."}
	}
	if err == nil {
		err = os.RemoveAll(rollback)
	}
	// The parent, which this activation may have created, goes when empty and
	// is handed over otherwise, before the marker.
	if err == nil {
		err = sqlitedb.RemoveDirIfEmpty(filepath.Dir(rollback))
	}
	if err == nil {
		sqlitedb.Step(ctx, "rollback directory removed")
		err = sqlitedb.GiveOwner(ctx, dir, filepath.Dir(rollback))
	}
	if err == nil {
		err = removeMarker(ctx, dir)
	}
	if err != nil {
		return a, incomplete(dir, pending, err)
	}
	return a, nil
}

// PendingActivation refuses with ActivationIncomplete while dir holds an
// activation marker, naming the commands that finish or undo the activation.
// A start calls it holding the installation lock, before it writes anything.
func PendingActivation(dir string) error {
	pending, err := readActivation(dir)
	if err != nil || pending == nil {
		return err
	}
	return incomplete(dir, pending, nil)
}

// incomplete refuses with ActivationIncomplete for the activation a records,
// after cause, a failure of the activation or its rollback, if any. Finishing
// needs the staging directory: without one, or when cause is a refusal of it,
// only the rollback is offered.
func incomplete(dir string, a *activation, cause error) error {
	if absolute, err := filepath.Abs(dir); err == nil {
		dir = absolute
	}
	command := "zzrouter-node recovery activate " + a.Staging + " --dir " + dir
	refused := &Refusal{ActivationIncomplete, "the activation of " + a.Staging + " (rollback " + a.Generation + ") did not finish",
		"Stop the service, then run " + command + " to finish it, or " + command + " --rollback to undo it."}
	var staging *Refusal
	var image *imageRefusal
	switch _, err := os.Stat(a.Staging); {
	case errors.As(cause, &staging):
		refused.Detail += ", and its staging directory cannot finish it: " + string(staging.Reason) + ", " + staging.Detail
	case errors.As(cause, &image):
		refused.Detail += ", and its staging directory cannot finish it: " + image.Error()
	case errors.Is(err, fs.ErrNotExist):
		refused.Detail += ", and its staging directory is gone"
	case cause == nil:
		return refused
	default:
		return errors.Join(refused, cause)
	}
	refused.NextAction = "Stop the service, then run " + command + " --rollback to undo it."
	if staging != nil || image != nil || cause == nil {
		return refused
	}
	return errors.Join(refused, cause)
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
// hashes included. Pending is the rollback of an activation that did not
// finish: rolling it back needs the directory, so it must not be removed.
type Rollback struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	SizeBytes int64     `json:"size_bytes"`
	Pending   bool      `json:"pending"`
}

// Rollbacks lists dir's rollback directories, oldest first, each dated by its
// directory.
func Rollbacks(dir string) ([]Rollback, error) {
	pending, err := readActivation(dir)
	if err != nil {
		return nil, err
	}
	root := filepath.Join(dir, rollbackDir)
	entries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return []Rollback{}, nil
	}
	if err != nil {
		return nil, err
	}
	list := []Rollback{}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || !info.IsDir() {
			continue // removed meanwhile, or not a rollback directory
		}
		r := Rollback{ID: entry.Name(), CreatedAt: info.ModTime().UTC(), Pending: pending != nil && pending.Generation == entry.Name()}
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
