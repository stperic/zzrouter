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
	"github.com/stperic/zzrouter/pkg/security"
)

// Check validates a bundle without writing installation state or creating stores.
func Check(ctx context.Context, archive, keyFile string) (Manifest, error) {
	return readArchive(ctx, archive, keyFile, "")
}

// Restore writes only into a new, protected staging directory. Nothing is
// activated. A failed operation leaves INCOMPLETE; it never overwrites state.
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
	manifest, data, err := readManifest(reader)
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
	snapshots, err := readEntries(ctx, reader, manifest, root)
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
	return manifest, finishStaging(root, destination, data, key)
}

func readManifest(reader *tar.Reader) (Manifest, []byte, error) {
	var manifest Manifest
	header, err := reader.Next()
	if err != nil || header.Name != "manifest.json" || header.Typeflag != tar.TypeReg || header.Size < 1 || header.Size > maxManifest {
		return manifest, nil, errors.New("invalid recovery manifest header")
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return manifest, nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return manifest, nil, err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return manifest, nil, errors.New("recovery manifest contains trailing data")
	}
	return manifest, data, validateManifest(manifest)
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

func finishStaging(root *os.Root, destination string, data, key []byte) error {
	if root == nil {
		return nil
	}
	if len(key) > 0 {
		if err := root.Mkdir("protection", 0o700); err != nil {
			return err
		}
		if err := writeExclusive(filepath.Join(destination, "protection", "credentials.key"), key); err != nil {
			return err
		}
	}
	if err := writeExclusive(filepath.Join(destination, "manifest.json"), data); err != nil {
		return err
	}
	return root.Remove("INCOMPLETE")
}

func readEntries(ctx context.Context, reader *tar.Reader, manifest Manifest, root *os.Root) (map[string][]byte, error) {
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
		var target *os.File
		if root != nil {
			if err := root.MkdirAll(filepath.FromSlash(path.Dir(expected.Name)), 0o700); err != nil {
				return snapshots, err
			}
			mode := os.FileMode(0o600)
			if expected.Executable {
				mode = 0o700
			}
			target, err = root.OpenFile(filepath.FromSlash(expected.Name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
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
	if manifest.Version != 1 || manifest.Inventory.Version != 1 || len(manifest.Files) == 0 || len(manifest.Files) > maxFiles {
		return errors.New("unsupported or empty recovery bundle")
	}
	seen := make(map[string]bool)
	spellings := make(map[string]string)
	var total int64
	for _, file := range manifest.Files {
		lower := strings.ToLower(file.Name)
		if validateContent(lower) && file.Name != lower {
			return fmt.Errorf("authority path must use canonical spelling: %s", file.Name)
		}
		if !safeName(file.Name) || !strings.HasPrefix(file.Name, "config/") && !strings.HasPrefix(file.Name, "data/") && !strings.HasPrefix(file.Name, "providers/") || strings.HasPrefix(lower, "config/") && excluded(strings.TrimPrefix(lower, "config/"), configExclusions()) || excluded(lower, nil) || seen[lower] {
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
	if !seen["config/node.yaml"] {
		return errors.New("recovery bundle requires node.yaml")
	}
	if seen["config/credentials.enc"] && len(manifest.KeySHA256) != 64 {
		return errors.New("encrypted state requires its separate protection-key fingerprint")
	}
	return nil
}
