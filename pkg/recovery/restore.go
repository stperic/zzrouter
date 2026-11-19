package recovery

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/stperic/common/secrets"
	"github.com/stperic/zzrouter/pkg/access/sqlstore"
	"github.com/stperic/zzrouter/pkg/security"
	"github.com/stperic/zzrouter/pkg/utils"
)

// Check validates a bundle of either profile without writing installation
// state or creating stores. A database image streams to a private directory in
// the system's temporary directory, where sqlstore.CheckImage checks it, and
// must match the manifest; the directory is removed on return.
func Check(ctx context.Context, archive, keyFile string) (Manifest, error) {
	return readArchive(ctx, archive, keyFile, extraction{scratch: filepath.Join(os.TempDir(), "zzrouter-recovery")})
}

// Restore writes only into a new, protected staging directory. Nothing is
// activated. It first checks the archive as Check does, with the image inside
// the staging directory, and removes that directory if the check fails. A
// staged database image is prepared to replace a store's content and checked
// again, and the staging manifest records it in Staged, so nothing publishes
// an image as archived. A later failure leaves INCOMPLETE; it never
// overwrites state.
func Restore(ctx context.Context, archive, keyFile, destination string) (Manifest, error) {
	if destination == "" {
		return Manifest{}, errors.New("a new restore staging directory is required")
	}
	root, err := stagingRoot(destination)
	if err != nil {
		return Manifest{}, err
	}
	defer func() { _ = root.Close() }()
	checked, err := readArchive(ctx, archive, keyFile, extraction{scratch: filepath.Join(destination, "check")})
	if err != nil {
		return checked, errors.Join(err, root.Close(), os.RemoveAll(destination))
	}
	return readArchive(ctx, archive, keyFile, extraction{root: root, checked: &checked})
}

// extraction says where readArchive writes. A staging pass writes every entry
// into root, and must find the manifest its check pass read: with every
// entry's digest verified again, the staged image is the one that pass
// checked. Otherwise only the image is written, into a private directory
// beside scratch, removed on return.
type extraction struct {
	root    *os.Root
	scratch string
	checked *Manifest
}

func readArchive(ctx context.Context, archive, keyFile string, x extraction) (Manifest, error) {
	var manifest Manifest
	if err := refuseLeftover(archive); err != nil {
		return manifest, err
	}
	f, err := openRegular(archive)
	if err != nil {
		return manifest, err
	}
	defer func() { _ = f.Close() }()
	buffered := bufio.NewReader(f)
	compressed, err := gzip.NewReader(buffered)
	if err != nil {
		return manifest, err
	}
	defer func() { _ = compressed.Close() }()
	compressed.Multistream(false)
	reader := tar.NewReader(io.LimitReader(compressed, maxTotal+maxManifest+maxFiles*1024))
	manifest, err = readManifest(reader)
	if err != nil {
		return manifest, err
	}
	if x.checked != nil && !reflect.DeepEqual(*x.checked, manifest) {
		return manifest, errors.New("the recovery archive changed while it was restored")
	}
	key, err := recoveryKey(manifest, keyFile)
	if err != nil {
		return manifest, err
	}
	defer clear(key)
	images := x.root
	if images == nil && manifest.Store != nil {
		temp, err := privateDir(x.scratch)
		if err != nil {
			return manifest, err
		}
		defer func() { _ = os.RemoveAll(temp) }()
		if images, err = os.OpenRoot(temp); err != nil {
			return manifest, err
		}
		defer func() { _ = images.Close() }()
	}
	snapshots, err := readEntries(ctx, reader, manifest, x.root, images)
	defer func() {
		for _, data := range snapshots {
			clear(data)
		}
	}()
	if err != nil {
		return manifest, err
	}
	if _, err := reader.Next(); err != io.EOF {
		return manifest, errors.New("unexpected or damaged recovery archive entries")
	}
	// Consume the gzip trailer so truncation or corruption beyond tar's terminator
	// cannot be mistaken for a complete archive.
	if extra, err := io.Copy(io.Discard, io.LimitReader(compressed, 1025)); err != nil || extra != 0 {
		return manifest, errors.New("damaged or trailing recovery payload")
	}
	if _, err := buffered.Peek(1); err != io.EOF {
		return manifest, errors.New("trailing recovery archive data")
	}
	if err := validateSnapshots(snapshots, key, manifest.Store); err != nil {
		return manifest, err
	}
	// Last, so a refusal for a newer schema means everything else passed.
	if manifest.Store != nil && x.checked == nil {
		if err := checkImage(ctx, images, *manifest.Store); err != nil {
			return manifest, err
		}
	}
	return manifest, finishStaging(ctx, x.root, &manifest, key)
}

func readManifest(reader *tar.Reader) (Manifest, error) {
	var manifest Manifest
	header, err := reader.Next()
	if err != nil || header.Name != "manifest.json" || header.Typeflag != tar.TypeReg || header.Size < 1 || header.Size > maxManifest {
		return manifest, errors.New("invalid recovery manifest header")
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return manifest, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return manifest, err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return manifest, errors.New("recovery manifest contains trailing data")
	}
	return manifest, validateManifest(manifest)
}

func imagePath(root *os.Root) string {
	return filepath.Join(root.Name(), filepath.FromSlash(imageName))
}

// imageRefusal is an archive's database image its check refused. Its advice
// is about the archive, never the live store; it unwraps to the store's
// *sqlstore.Refusal.
type imageRefusal struct{ refusal *sqlstore.Refusal }

func (e *imageRefusal) Error() string {
	advice := "Do not use this archive: take another backup, or use another archive that passes its check."
	if e.refusal.Reason == sqlstore.SchemaNewer {
		advice = "A newer release wrote it: only that release can check or restore this archive."
	}
	return "the archive's access database image is refused: " + e.refusal.Summary() + ". " + advice
}

func (e *imageRefusal) Unwrap() error { return e.refusal }

// archiveImage gives a refusal of an archive's image, or of its staged copy,
// the archive's advice instead of the live store's.
func archiveImage(err error) error {
	var refusal *sqlstore.Refusal
	if errors.As(err, &refusal) {
		return &imageRefusal{refusal}
	}
	return err
}

// checkImage refuses an image sqlstore.CheckImage refuses, or whose meta is
// not the manifest's store description.
func checkImage(ctx context.Context, images *os.Root, want StoreMeta) error {
	img, err := sqlstore.CheckImage(ctx, imagePath(images))
	if err != nil {
		return archiveImage(err)
	}
	if StoreMeta(img.Meta) != want {
		return errors.New("the database image does not match the manifest's store description")
	}
	return nil
}

// prepareImage readies the staged image to replace a store's content, checks
// the file it wrote and describes it for the staging manifest. The check pass
// judged these bytes already, so a refusal here comes from the staged copy
// itself, such as a read error of its medium that SQLite reports as
// corruption; its advice is still about the restore, never the live store.
func prepareImage(ctx context.Context, root *os.Root) (*Staged, error) {
	prepared, err := sqlstore.PrepareImage(ctx, imagePath(root))
	if err != nil {
		return nil, archiveImage(err)
	}
	checked, err := sqlstore.CheckImage(ctx, imagePath(root))
	if err != nil {
		return nil, archiveImage(err)
	}
	if checked != prepared {
		return nil, errors.New("the prepared database image does not check as prepared")
	}
	_, sum, err := hashFile(ctx, root.Name(), filepath.FromSlash(imageName))
	if err != nil {
		return nil, err
	}
	return &Staged{ImageSHA256: sum, Generation: prepared.Generation, PreparedAt: utils.NowUTC()}, nil
}

func recoveryKey(manifest Manifest, keyFile string) ([]byte, error) {
	if manifest.KeySHA256 == "" {
		return nil, nil
	}
	if keyFile == "" {
		return nil, errors.New("supply the separate protection-key backup with --key-file")
	}
	if err := refuseLeftover(keyFile); err != nil {
		return nil, err
	}
	key, err := secrets.LoadKeyFile(keyFile)
	if err != nil {
		return nil, err
	}
	if digest(key) != manifest.KeySHA256 {
		clear(key)
		return nil, errors.New("protection key does not match this recovery bundle")
	}
	return key, nil
}

func stagingRoot(destination string) (*os.Root, error) {
	if err := os.Mkdir(destination, 0o700); err != nil {
		return nil, fmt.Errorf("restore requires a new directory: %w", err)
	}
	if err := security.RestrictDirectoryPermissions(destination); err != nil {
		return nil, err
	}
	if err := writeExclusive(filepath.Join(destination, "INCOMPLETE"), []byte("Recovery has not passed validation. Do not activate this directory.\n")); err != nil {
		return nil, err
	}
	return os.OpenRoot(destination)
}

func finishStaging(ctx context.Context, root *os.Root, manifest *Manifest, key []byte) error {
	if root == nil {
		return nil
	}
	destination := root.Name()
	if manifest.Store != nil {
		staged, err := prepareImage(ctx, root)
		if err != nil {
			return err
		}
		manifest.Staged = staged
	}
	if len(key) > 0 {
		if err := root.Mkdir("protection", 0o700); err != nil {
			return err
		}
		if err := writeExclusive(filepath.Join(destination, "protection", "credentials.key"), key); err != nil {
			return err
		}
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	if err := writeExclusive(filepath.Join(destination, "manifest.json"), data); err != nil {
		return err
	}
	return root.Remove("INCOMPLETE")
}

// readEntries verifies every entry, keeping the files validateContent names in
// memory. It writes every entry into root when staging, and the image into
// images, never into memory.
func readEntries(ctx context.Context, reader *tar.Reader, manifest Manifest, root, images *os.Root) (map[string][]byte, error) {
	snapshots := make(map[string][]byte)
	var retained int64
	for _, expected := range manifest.Files {
		if err := ctx.Err(); err != nil {
			return snapshots, err
		}
		header, err := reader.Next()
		if err != nil || header.Name != expected.Name || header.Typeflag != tar.TypeReg || header.Size != expected.Size {
			return snapshots, fmt.Errorf("recovery entry does not match manifest: %s", expected.Name)
		}
		hash := sha256.New()
		writers := []io.Writer{hash}
		var content bytes.Buffer
		if validateContent(expected.Name) {
			retained += expected.Size
			if retained > 96<<20 {
				return snapshots, errors.New("recovery authority state exceeds 96 MiB")
			}
			writers = append(writers, &content)
		}
		dest := root
		if expected.Name == imageName {
			dest = images
		}
		var target *os.File
		if dest != nil {
			if err := dest.MkdirAll(filepath.FromSlash(path.Dir(expected.Name)), 0o700); err != nil {
				return snapshots, err
			}
			mode := os.FileMode(0o600)
			if expected.Executable {
				mode = 0o700
			}
			target, err = dest.OpenFile(filepath.FromSlash(expected.Name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
			if err != nil {
				return snapshots, err
			}
			writers = append(writers, target)
		}
		_, copyErr := copyContext(ctx, io.MultiWriter(writers...), reader, expected.Size)
		if target != nil {
			copyErr = errors.Join(copyErr, target.Sync(), target.Close())
		}
		if validateContent(expected.Name) {
			snapshots[expected.Name] = content.Bytes()
		}
		if copyErr != nil {
			return snapshots, copyErr
		}
		if hex.EncodeToString(hash.Sum(nil)) != expected.SHA256 {
			return snapshots, fmt.Errorf("recovery checksum mismatch: %s", expected.Name)
		}
	}
	return snapshots, nil
}

func validateManifest(manifest Manifest) error {
	if manifest.Version != 1 || len(manifest.Files) == 0 || len(manifest.Files) > maxFiles {
		return errors.New("unsupported or empty recovery bundle")
	}
	if manifest.Staged != nil {
		return errors.New("a staging manifest is not a recovery bundle; restore from the original archive")
	}
	seen := make(map[string]bool)
	spellings := make(map[string]string)
	var total int64
	for _, file := range manifest.Files {
		lower := strings.ToLower(file.Name)
		if validateContent(lower) && file.Name != lower {
			return fmt.Errorf("authority path must use canonical spelling: %s", file.Name)
		}
		if !safeName(file.Name) || !strings.HasPrefix(file.Name, "config/") && !strings.HasPrefix(file.Name, "data/") && !strings.HasPrefix(file.Name, "providers/") && file.Name != imageName || strings.HasPrefix(lower, "config/") && excluded(strings.TrimPrefix(lower, "config/"), configExclusions()) || excluded(lower, nil) || seen[lower] {
			return fmt.Errorf("unsafe or duplicate recovery entry: %s", file.Name)
		}
		if file.Size < 0 || file.Size > maxTotal || len(file.SHA256) != 64 {
			return errors.New("invalid recovery file metadata")
		}
		if _, err := hex.DecodeString(file.SHA256); err != nil {
			return errors.New("invalid recovery checksum")
		}
		total += file.Size
		if total > maxTotal {
			return errors.New("recovery bundle exceeds 4 GiB")
		}
		for component := file.Name; component != "."; component = path.Dir(component) {
			folded := strings.ToLower(component)
			if previous, exists := spellings[folded]; exists && previous != component {
				return errors.New("recovery path has a case alias")
			}
			spellings[folded] = component
		}
		seen[lower] = true
	}
	for name := range seen {
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if seen[parent] {
				return errors.New("recovery path is both a file and a directory")
			}
		}
	}
	return validateProfile(manifest, seen)
}

// validateProfile applies the rules of manifest's profile; seen holds its
// entries' names in lower case.
func validateProfile(manifest Manifest, seen map[string]bool) error {
	image := seen[imageName]
	if image != (manifest.Store != nil) {
		return errors.New("a database image and its store description travel together")
	}
	switch manifest.Profile {
	case ProfileInstallation:
		if manifest.Inventory == nil || manifest.Inventory.Version != 1 {
			return errors.New("an installation bundle requires its inventory")
		}
		if !seen["config/node.yaml"] {
			return errors.New("recovery bundle requires node.yaml")
		}
		if seen["config/credentials.enc"] && len(manifest.KeySHA256) != 64 {
			return errors.New("encrypted state requires its separate protection-key fingerprint")
		}
	case ProfileDatabase:
		if !image || len(manifest.Files) != 1 || manifest.Inventory != nil || manifest.KeySHA256 != "" || len(manifest.MissingOptional) > 0 {
			return errors.New("a database bundle holds the database image alone")
		}
	default:
		return fmt.Errorf("unknown recovery bundle profile %q", manifest.Profile)
	}
	return nil
}
