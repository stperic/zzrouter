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
	"strings"

	"github.com/stperic/common/secrets"
	"github.com/stperic/zzrouter/pkg/access/sqlstore"
	"github.com/stperic/zzrouter/pkg/security"
	"github.com/stperic/zzrouter/pkg/utils"
)

// Check validates a bundle of either profile without writing installation
// state or creating stores. A database image streams to a private temporary
// file, where sqlstore.CheckImage checks it, and must match the manifest.
func Check(ctx context.Context, archive, keyFile string) (Manifest, error) {
	return readArchive(ctx, archive, keyFile, "")
}

// Restore writes only into a new, protected staging directory. Nothing is
// activated. A staged database image is prepared to replace a store's content
// and checked again, and the staging manifest records it in Staged, so nothing
// publishes an image as archived. A failed operation leaves INCOMPLETE; it
// never overwrites state.
func Restore(ctx context.Context, archive, keyFile, destination string) (Manifest, error) {
	if destination == "" {
		return Manifest{}, errors.New("a new restore staging directory is required")
	}
	if _, err := Check(ctx, archive, keyFile); err != nil {
		return Manifest{}, err
	}
	return readArchive(ctx, archive, keyFile, destination)
}

func readArchive(ctx context.Context, archive, keyFile, destination string) (Manifest, error) {
	var manifest Manifest
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
	key, err := recoveryKey(manifest, keyFile)
	if err != nil {
		return manifest, err
	}
	defer clear(key)
	root, err := stagingRoot(destination)
	if err != nil {
		return manifest, err
	}
	if root != nil {
		defer func() { _ = root.Close() }()
	}
	images := root
	if images == nil && manifest.Store != nil {
		temp, err := privateDir()
		if err != nil {
			return manifest, err
		}
		defer func() { _ = os.RemoveAll(temp) }()
		if images, err = os.OpenRoot(temp); err != nil {
			return manifest, err
		}
		defer func() { _ = images.Close() }()
	}
	snapshots, err := readEntries(ctx, reader, manifest, root, images)
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
	if err := validateSnapshots(snapshots, key); err != nil {
		return manifest, err
	}
	// Last, so a refusal for a newer schema means everything else passed.
	if manifest.Store != nil {
		if err := checkImage(ctx, images, *manifest.Store); err != nil {
			return manifest, err
		}
	}
	return manifest, finishStaging(ctx, root, destination, &manifest, key)
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

// checkImage refuses an image sqlstore.CheckImage refuses, with its *Refusal,
// or whose meta is not the manifest's store description.
func checkImage(ctx context.Context, images *os.Root, want StoreMeta) error {
	img, err := sqlstore.CheckImage(ctx, imagePath(images))
	if err != nil {
		return err
	}
	if StoreMeta(img.Meta) != want {
		return errors.New("the database image does not match the manifest's store description")
	}
	return nil
}

// prepareImage readies the staged image to replace a store's content, checks
// it again and describes it for the staging manifest.
func prepareImage(ctx context.Context, root *os.Root) (*Staged, error) {
	prepared, err := sqlstore.PrepareImage(ctx, imagePath(root))
	if err != nil {
		return nil, err
	}
	checked, err := sqlstore.CheckImage(ctx, imagePath(root))
	if err != nil {
		return nil, err
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
	if destination == "" {
		return nil, nil
	}
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

func finishStaging(ctx context.Context, root *os.Root, destination string, manifest *Manifest, key []byte) error {
	if root == nil {
		return nil
	}
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
		// A marker without its database would stop the restored node as missing.
		if seen["config/"+sqlstore.MarkerName] && !image {
			return errors.New("the store marker requires the database image")
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
