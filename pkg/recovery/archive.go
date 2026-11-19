package recovery

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/stperic/common/secrets"
	"github.com/stperic/zzrouter/pkg/access/sqlstore"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/security"
	"github.com/stperic/zzrouter/pkg/utils"
)

const maxFiles = 20000
const maxManifest = 4 << 20
const maxTotal = int64(4 << 30)

// imageName is the access database image's entry, distinct from every live
// path, so a raw copy of the live database can never pass for it.
const imageName = "store/image.sqlite"

// Profile is a bundle's scope. The set is closed: an unknown profile is refused.
type Profile string

const (
	// ProfileInstallation is an installation's recovery state, with its access
	// database image when it holds a store.
	ProfileInstallation Profile = "installation"
	// ProfileDatabase is the access database image alone.
	ProfileDatabase Profile = "database"
)

// StoreMeta describes the database image a bundle carries: sqlstore.Meta
// under its wire names.
type StoreMeta struct {
	StoreID       string `json:"store_id"`
	Generation    string `json:"generation"`
	SchemaVersion int    `json:"schema_version"`
	Revision      uint64 `json:"revision"`
	Sequence      uint64 `json:"sequence"`
}

// Staged records, only in a staging directory's manifest, the image restore
// prepared: its digest, its new generation and when. The image's files entry
// keeps the archived digest.
type Staged struct {
	ImageSHA256 string    `json:"image_sha256"`
	Generation  string    `json:"generation"`
	PreparedAt  time.Time `json:"prepared_at"`
}

// File is one bounded, integrity-checked archive entry.
type File struct {
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	SHA256     string `json:"sha256"`
	Executable bool   `json:"executable,omitempty"`
	source     string
	root       string
}

// Manifest describes a recovery bundle and its separately held protection key.
// Store is present exactly when the bundle carries imageName; Inventory only
// in an installation bundle.
type Manifest struct {
	Version         int        `json:"version"`
	Profile         Profile    `json:"profile"`
	Created         time.Time  `json:"created"`
	Store           *StoreMeta `json:"store,omitempty"`
	Inventory       *Inventory `json:"inventory,omitempty"`
	KeySHA256       string     `json:"key_sha256,omitempty"`
	Files           []File     `json:"files"`
	MissingOptional []string   `json:"missing_optional,omitempty"`
	Staged          *Staged    `json:"staged,omitempty"`
	keyInfo         fs.FileInfo
}

// Backup requires a stopped installation and a fresh output path. The key is
// exported separately, never packed with the secret-bearing recovery archive.
// The access database travels as an image sqlstore.CaptureStopped takes; a
// store that refuses to open stops the backup. Temporaries live in private
// .tmp directories beside the outputs, and the outputs are published by rename
// at the end, so a failed or cancelled backup leaves no output.
func Backup(ctx context.Context, dir, output, keyOutput string) (Manifest, error) {
	var manifest Manifest
	if _, err := Read(ctx, dir); err != nil {
		return manifest, err
	}
	// Only the daemon creates this inode, under its own service account. A
	// privileged failed backup must not leave a root-owned lock behind.
	lockFile, err := openRegular(filepath.Join(dir, "installation.lock"))
	if err != nil {
		return manifest, fmt.Errorf("missing installation lock; start this installation with the current release first: %w", err)
	}
	if err := lockFile.Close(); err != nil {
		return manifest, err
	}
	lock, err := config.LockInstallation(dir)
	if err != nil {
		return manifest, err
	}
	defer func() { _ = lock.Close() }()
	inventory, err := Read(ctx, dir)
	if err != nil {
		return manifest, err
	}
	for _, outputPath := range []string{output, keyOutput} {
		if outputPath == "" {
			continue
		}
		absolute, err := canonicalOutput(outputPath)
		if err != nil {
			return manifest, err
		}
		for _, source := range inventory.Sources {
			sourcePath, err := filepath.EvalSymlinks(source.Path)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return manifest, err
			}
			rel, err := filepath.Rel(sourcePath, absolute)
			if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return manifest, errors.New("backup output must be outside the installation sources")
			}
		}
	}
	manifest = Manifest{Version: 1, Profile: ProfileInstallation, Created: utils.NowUTC(), Inventory: &inventory}
	work, err := privateDir(output)
	if err != nil {
		return manifest, err
	}
	defer func() { _ = os.RemoveAll(work) }()
	if err := captureImage(ctx, &manifest, dir, work); err != nil {
		return manifest, err
	}
	if err := collect(ctx, &manifest); err != nil {
		return manifest, err
	}
	keyWork, err := exportKey(&manifest, output, keyOutput)
	if keyWork != "" {
		defer func() { _ = os.RemoveAll(keyWork) }()
	}
	if err != nil {
		return manifest, err
	}
	archive := filepath.Join(work, "archive.tmp")
	if err := writeBundle(ctx, manifest, archive); err != nil {
		return manifest, err
	}
	// The key first, so a published archive always has its key.
	if keyWork != "" {
		if err := publish(filepath.Join(keyWork, "key.tmp"), keyOutput); err != nil {
			return manifest, err
		}
	}
	if err := publish(archive, output); err != nil {
		if keyWork != "" {
			err = errors.Join(err, os.Remove(keyOutput))
		}
		return manifest, err
	}
	return manifest, nil
}

// captureImage adds the image of the stopped store in dir to manifest; a
// worker or a node that never ran as a coordinator has none.
func captureImage(ctx context.Context, manifest *Manifest, dir, temp string) error {
	const name = "image.sqlite"
	img, err := sqlstore.CaptureStopped(ctx, dir, filepath.Join(temp, name))
	if errors.Is(err, sqlstore.ErrNoStore) {
		return nil
	}
	if err != nil {
		return err
	}
	size, sum, err := hashFile(ctx, temp, name)
	if err != nil {
		return err
	}
	store := StoreMeta(img.Meta)
	manifest.Store = &store
	manifest.Files = append(manifest.Files, File{Name: imageName, Size: size, SHA256: sum, root: temp, source: name})
	return nil
}

// privateDir creates a directory only its owner can enter beside path, named
// after it with a .tmp suffix; the caller removes it. Leftovers of a killed
// process keep the suffix, which keeps them out of every source and input.
func privateDir(path string) (string, error) {
	dir, err := os.MkdirTemp(filepath.Dir(path), filepath.Base(path)+"-*.tmp")
	if err != nil {
		return "", err
	}
	if err := security.RestrictDirectoryPermissions(dir); err != nil {
		return "", errors.Join(err, os.RemoveAll(dir))
	}
	return dir, nil
}

// publish renames a finished temporary file to path, refusing an existing path.
func publish(temp, path string) error {
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		if err == nil {
			err = fmt.Errorf("%w: %s", fs.ErrExist, path)
		}
		return err
	}
	return os.Rename(temp, path)
}

// refuseLeftover refuses an input named like a temporary file, which a backup
// killed before it finished can leave.
func refuseLeftover(path string) error {
	if strings.HasSuffix(strings.ToLower(path), ".tmp") {
		return fmt.Errorf("%s is a leftover of an interrupted backup, not a recovery file; delete it", path)
	}
	return nil
}

// exportKey writes the protection key, for its separate export, to key.tmp in
// the private directory it returns beside keyOutput; none without keyOutput.
func exportKey(manifest *Manifest, output, keyOutput string) (string, error) {
	needsKey := slices.ContainsFunc(manifest.Files, func(file File) bool { return file.Name == "config/credentials.enc" })
	if needsKey && keyOutput == "" {
		return "", errors.New("protected connections require --key-output in a separate secure backup location")
	}
	if keyOutput == "" {
		return "", nil
	}
	outPath, err := canonicalOutput(output)
	if err != nil {
		return "", err
	}
	keyPath, err := canonicalOutput(keyOutput)
	if err != nil {
		return "", err
	}
	if strings.EqualFold(filepath.Dir(outPath), filepath.Dir(keyPath)) {
		return "", errors.New("store the protection-key backup in a separate directory from the archive")
	}
	key, err := secrets.LoadKeyFile(manifest.Inventory.KeyFile)
	if err != nil {
		return "", fmt.Errorf("read protection key for separate export: %w", err)
	}
	defer clear(key)
	work, err := privateDir(keyOutput)
	if err != nil {
		return "", err
	}
	manifest.KeySHA256 = digest(key)
	return work, writeExclusive(filepath.Join(work, "key.tmp"), key)
}

// writeBundle writes manifest and its files to output, which must not exist;
// a failure leaves no output.
func writeBundle(ctx context.Context, manifest Manifest, output string) error {
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	if len(encoded) > maxManifest {
		return errors.New("recovery manifest exceeds 4 MiB")
	}
	return writePrivate(output, func(f io.Writer) error {
		gzipWriter := gzip.NewWriter(f)
		writer := tar.NewWriter(gzipWriter)
		if err := writer.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0o600, Size: int64(len(encoded))}); err != nil {
			return err
		}
		if _, err := writer.Write(encoded); err != nil {
			return err
		}
		for _, file := range manifest.Files {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := writer.WriteHeader(&tar.Header{Name: file.Name, Mode: 0o600, Size: file.Size}); err != nil {
				return err
			}
			input, err := openSourceFile(file.root, file.source)
			if err != nil {
				return err
			}
			hash := sha256.New()
			n, copyErr := copyContext(ctx, io.MultiWriter(writer, hash), input, file.Size)
			closeErr := input.Close()
			if err := errors.Join(copyErr, closeErr); err != nil {
				return err
			}
			if n != file.Size || hex.EncodeToString(hash.Sum(nil)) != file.SHA256 {
				return fmt.Errorf("source changed during backup: %s", file.Name)
			}
		}
		return errors.Join(writer.Close(), gzipWriter.Close())
	})
}

func collect(ctx context.Context, manifest *Manifest) error {
	keyInfo, err := os.Stat(manifest.Inventory.KeyFile)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	manifest.keyInfo = keyInfo
	seen := make(map[string]bool)
	var total int64
	for _, file := range manifest.Files { // the image counts toward the limits
		total += file.Size
	}
	for _, source := range manifest.Inventory.Sources {
		if !safeName(source.Name) || !filepath.IsAbs(source.Path) {
			return errors.New("invalid recovery source")
		}
		err := collectSource(ctx, manifest, source, seen, &total)
		if err != nil {
			return err
		}
	}
	if !seen["config/node.yaml"] {
		return errors.New("recovery bundle requires node.yaml")
	}
	slices.SortFunc(manifest.Files, func(a, b File) int { return strings.Compare(a.Name, b.Name) })
	return nil
}

func collectSource(ctx context.Context, manifest *Manifest, source Source, seen map[string]bool, total *int64) error {
	base, relative, err := sourcePath(*manifest.Inventory, source)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(base)
	if errors.Is(err, os.ErrNotExist) {
		return missingSource(manifest, source)
	}
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if err := rejectLinks(root, relative); errors.Is(err, os.ErrNotExist) {
		return missingSource(manifest, source)
	} else if err != nil {
		return err
	}
	return fs.WalkDir(root.FS(), filepath.ToSlash(relative), func(full string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(relative, filepath.FromSlash(full))
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if excluded(rel, source.Exclude) {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry.IsDir() || filepath.Join(base, filepath.FromSlash(full)) == manifest.Inventory.KeyFile {
			return nil
		}
		name := path.Join(source.Name, rel)
		if !safeName(name) || seen[strings.ToLower(name)] {
			return fmt.Errorf("unsafe or duplicate recovery path: %s", name)
		}
		input, err := openRootRegular(root, filepath.FromSlash(full))
		if err != nil {
			return err
		}
		defer func() { _ = input.Close() }()
		info, err := input.Stat()
		if err != nil {
			return err
		}
		if manifest.keyInfo != nil && os.SameFile(manifest.keyInfo, info) {
			return nil
		}
		*total += info.Size()
		if *total > maxTotal || len(manifest.Files) >= maxFiles {
			return errors.New("recovery bundle exceeds 4 GiB or 20000 files; preserve large provider data separately before continuing")
		}
		sum, err := hashReader(ctx, input, info.Size())
		if err != nil {
			return err
		}
		seen[strings.ToLower(name)] = true
		manifest.Files = append(manifest.Files, File{Name: name, Size: info.Size(), SHA256: sum, Executable: info.Mode()&0o100 != 0, root: base, source: filepath.FromSlash(full)})
		return nil
	})
}

// hashFile returns the size and SHA-256 of the regular file relative under base.
func hashFile(ctx context.Context, base, relative string) (int64, string, error) {
	f, err := openSourceFile(base, relative)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return 0, "", err
	}
	sum, err := hashReader(ctx, f, info.Size())
	return info.Size(), sum, err
}

// hashReader returns the SHA-256 of the size bytes r holds.
func hashReader(ctx context.Context, r io.Reader, size int64) (string, error) {
	hash := sha256.New()
	if _, err := copyContext(ctx, hash, r, size); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func missingSource(manifest *Manifest, source Source) error {
	if source.Required {
		return fmt.Errorf("previously present recovery source is unavailable: %s; restore access to its storage before backup", source.Name)
	}
	manifest.MissingOptional = append(manifest.MissingOptional, source.Name)
	return nil
}

func sourcePath(inventory Inventory, source Source) (string, string, error) {
	var base string
	switch strings.SplitN(source.Name, "/", 2)[0] {
	case "config":
		base = inventory.ConfigDir
	case "data":
		base = inventory.DataDir
	case "providers":
		base = inventory.ProviderDir
	default:
		return "", "", errors.New("unknown recovery source root")
	}
	relative, err := filepath.Rel(base, source.Path)
	if err != nil || !filepath.IsLocal(relative) {
		return "", "", errors.New("recovery source is outside its recorded root")
	}
	return base, relative, nil
}

func rejectLinks(root *os.Root, relative string) error {
	current := "."
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, err := root.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("recovery source must not contain links: %s", current)
		}
	}
	return nil
}

func openSourceFile(base, relative string) (*os.File, error) {
	root, err := os.OpenRoot(base)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	return openRootRegular(root, relative)
}

func openRootRegular(root *os.Root, relative string) (*os.File, error) {
	if err := rejectLinks(root, relative); err != nil {
		return nil, err
	}
	info, err := root.Lstat(relative)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("recovery source must be a regular file: %s", relative)
	}
	f, err := root.Open(relative)
	if err != nil {
		return nil, err
	}
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		_ = f.Close()
		return nil, fmt.Errorf("recovery source changed: %s", relative)
	}
	return f, nil
}

func canonicalOutput(output string) (string, error) {
	absolute, err := filepath.Abs(output)
	if err != nil {
		return "", err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(absolute)), nil
}

func excluded(name string, exclusions []string) bool {
	name = strings.ToLower(name)
	if strings.HasSuffix(name, ".lock") || strings.HasSuffix(name, ".pid") || strings.HasSuffix(name, ".tmp") {
		return true
	}
	for _, exclusion := range exclusions {
		exclusion = strings.ToLower(exclusion)
		if name == exclusion || strings.HasPrefix(name, exclusion+"/") {
			return true
		}
	}
	return false
}

func safeName(name string) bool {
	if name == "" || path.Clean(name) != name || strings.HasPrefix(name, "/") || strings.ContainsAny(name, "\\:\x00") {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." || part == "." || strings.TrimRight(part, " .") != part {
			return false
		}
		base := strings.ToUpper(strings.SplitN(part, ".", 2)[0])
		if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" || len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9' {
			return false
		}
	}
	return true
}

func openRegular(file string) (*os.File, error) {
	return openSourceFile(filepath.Dir(file), filepath.Base(file))
}

func readBounded(path string, limit int64) ([]byte, error) {
	f, err := openRegular(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("recovery file exceeds size limit: %s", path)
	}
	return data, nil
}

// writePrivate creates path, which must not exist, restricts it to its owner
// before any byte is written, fills and syncs it; a failure leaves no file.
func writePrivate(path string, fill func(io.Writer) error) (err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = f.Close()
			err = errors.Join(err, os.Remove(path))
		}
	}()
	// Restrict before writing secret bytes, including on Windows.
	if err := security.RestrictFilePermissions(path); err != nil {
		return err
	}
	if err := fill(f); err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

func writeExclusive(path string, data []byte) error {
	return writePrivate(path, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	})
}

func copyContext(ctx context.Context, dst io.Writer, src io.Reader, size int64) (int64, error) {
	var copied int64
	buffer := make([]byte, 64<<10)
	for copied < size {
		if err := ctx.Err(); err != nil {
			return copied, err
		}
		n, err := io.ReadFull(src, buffer[:min(int64(len(buffer)), size-copied)])
		if n > 0 {
			written, writeErr := dst.Write(buffer[:n])
			copied += int64(written)
			if writeErr != nil {
				return copied, writeErr
			}
			if written != n {
				return copied, io.ErrShortWrite
			}
		}
		if err != nil {
			return copied, err
		}
	}
	return copied, nil
}

func digest(data []byte) string { value := sha256.Sum256(data); return hex.EncodeToString(value[:]) }
