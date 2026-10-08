// Package assets owns provider asset files: small files an engine flag
// takes by path, such as a chat template or a grammar. They live in
// providers/<kind>/<name>/assets/ and config refers to them by name only,
// so no path ever comes from config or the API. Nothing outside this
// package reads or writes an asset file.
package assets

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/stperic/zzrouter/pkg/utils"
)

const (
	// DirName is the subdirectory of a provider directory holding its assets.
	DirName = "assets"

	// MaxAssetBytes caps one asset. Templates and grammars are kilobytes;
	// anything near this is a model artifact and belongs in the model store.
	MaxAssetBytes = 1 << 20

	// MaxProviderBytes caps one provider's assets together. They ride
	// inline in every provider sync push, so this bounds that body.
	MaxProviderBytes = 4 << 20

	// MaxNameLen bounds a name well inside every filesystem's limit.
	MaxNameLen = 128

	// MaxAssets caps how many assets a provider holds, so the sync body's
	// per-entry overhead is bounded along with its bytes.
	MaxAssets = 64

	fileMode = 0o600
	dirMode  = 0o755
)

// namePattern admits a single path element with no separator. No leading
// dot, so a name never collides with the hidden temp file of an in-flight
// write; no trailing dot, which Windows strips, so "a." cannot alias "a".
var namePattern = regexp.MustCompile(fmt.Sprintf(`^[A-Za-z0-9](?:[A-Za-z0-9._-]{0,%d}[A-Za-z0-9_-])?$`, MaxNameLen-2))

// windowsDeviceNames are the base names Windows maps to devices whatever
// the extension, so "nul.jinja" cannot be a file on a Windows worker.
var windowsDeviceNames = map[string]bool{
	"con": true, "prn": true, "aux": true, "nul": true,
	"com1": true, "com2": true, "com3": true, "com4": true, "com5": true, "com6": true, "com7": true, "com8": true, "com9": true,
	"lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true, "lpt5": true, "lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true,
}

var (
	ErrInvalidName = errors.New("invalid asset name")
	// ErrNameCollision is two names one file on a case-insensitive
	// filesystem, as macOS and Windows workers have.
	ErrNameCollision    = errors.New("asset names differ only in case")
	ErrNotFound         = errors.New("asset not found")
	ErrNotRegular       = errors.New("asset is not a regular file")
	ErrTooLarge         = errors.New("asset exceeds the per-asset size limit")
	ErrProviderTooLarge = errors.New("assets exceed the per-provider size limit")
	ErrTooMany          = errors.New("too many assets")
	// ErrInvalidSet wraps any reason a whole asset set is refused.
	ErrInvalidSet = errors.New("invalid asset set")
	// ErrStoredSet wraps a write refused for what is already stored, such
	// as a file placed by hand over the caps, not for the bytes written.
	ErrStoredSet = errors.New("stored assets are unreadable or over the limits")
)

// ValidateName reports whether name may name an asset.
func ValidateName(name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("%w: %q (a single file name of letters, digits, '.', '_' or '-', not starting or ending with '.', at most %d characters)",
			ErrInvalidName, name, MaxNameLen)
	}
	base, _, _ := strings.Cut(name, ".")
	if windowsDeviceNames[strings.ToLower(base)] {
		return fmt.Errorf("%w: %q is a device name on Windows", ErrInvalidName, name)
	}
	return nil
}

// Digest is the content identity reported for an asset.
func Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Info describes one stored asset.
type Info struct {
	Name   string
	Size   int64
	SHA256 string
}

// Dir is one provider's asset directory. It holds no state; callers that
// need writes serialized (the config store) hold their own lock.
type Dir struct {
	path string
}

// NewDir returns the asset directory of the provider at providerDir.
func NewDir(providerDir string) Dir {
	return Dir{path: filepath.Join(providerDir, DirName)}
}

// Path returns the absolute path of an existing asset. A symlink or any
// other non-regular file is refused, so a name can only ever resolve to a
// file that was written into this directory.
func (d Dir) Path(name string) (string, error) {
	p, fi, err := d.locate(name)
	if err != nil {
		return "", err
	}
	// A file placed by hand is held to the same cap as one written here,
	// since every read of it rides a sync push.
	if fi.Size() > MaxAssetBytes {
		return "", fmt.Errorf("%w: %q is %d bytes, the limit is %d", ErrTooLarge, name, fi.Size(), MaxAssetBytes)
	}
	return p, nil
}

// Exists reports whether name is an asset here, whatever its size, so a
// file over the caps can still be found to be removed.
func (d Dir) Exists(name string) error {
	_, _, err := d.locate(name)
	return err
}

// locate finds an existing regular file by name, whatever its size.
func (d Dir) locate(name string) (string, os.FileInfo, error) {
	if err := ValidateName(name); err != nil {
		return "", nil, err
	}
	if err := d.check(); err != nil {
		return "", nil, err
	}
	p, err := filepath.Abs(filepath.Join(d.path, name))
	if err != nil {
		return "", nil, fmt.Errorf("resolve asset %q: %w", name, err)
	}
	// A case-insensitive filesystem would let "A" stat as "a"; a name that
	// resolves here but not on a case-sensitive worker must not resolve.
	exact, err := d.hasEntry(name)
	if err != nil {
		return "", nil, err
	}
	if !exact {
		return "", nil, fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	fi, err := os.Lstat(p)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil, fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	if err != nil {
		return "", nil, fmt.Errorf("stat asset %q: %w", name, err)
	}
	if !fi.Mode().IsRegular() {
		return "", nil, fmt.Errorf("%w: %q", ErrNotRegular, name)
	}
	return p, fi, nil
}

// hasEntry reports whether the directory lists name spelled exactly so.
func (d Dir) hasEntry(name string) (bool, error) {
	entries, err := os.ReadDir(d.path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read asset directory: %w", err)
	}
	return slices.ContainsFunc(entries, func(e os.DirEntry) bool { return e.Name() == name }), nil
}

// check refuses an asset directory that is not a real directory: a
// symlinked assets/ would let every name reach files outside it. The zero
// Dir names no directory and holds nothing, so a name can never resolve
// against the working directory.
func (d Dir) check() error {
	if d.path == "" {
		return fmt.Errorf("%w: no asset directory", ErrNotFound)
	}
	fi, err := os.Lstat(d.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat asset directory: %w", err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%w: %s is not a directory", ErrNotRegular, d.path)
	}
	return nil
}

// Read returns an asset's bytes.
func (d Dir) Read(name string) ([]byte, error) {
	p, err := d.Path(name)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(p)
}

// List describes every asset, sorted by name. Entries that are not valid
// asset names or not regular files are not assets and are skipped.
func (d Dir) List() ([]Info, error) {
	all, err := d.ReadAll()
	if err != nil {
		return nil, err
	}
	infos := make([]Info, 0, len(all))
	for _, name := range slices.Sorted(maps.Keys(all)) {
		data := all[name]
		infos = append(infos, Info{Name: name, Size: int64(len(data)), SHA256: Digest(data)})
	}
	return infos, nil
}

// ReadAll returns every asset's bytes keyed by name. A provider with no
// asset directory has no assets.
func (d Dir) ReadAll() (map[string][]byte, error) {
	names, err := d.names()
	if err != nil {
		return nil, err
	}
	out := make(map[string][]byte, len(names))
	for _, name := range names {
		data, err := d.Read(name)
		if err != nil {
			return nil, err
		}
		out[name] = data
	}
	if err := checkSet(out); err != nil {
		return nil, err
	}
	return out, nil
}

// names lists the entries that are assets: regular files with valid
// names. Anything else in the directory is not one.
func (d Dir) names() ([]string, error) {
	if err := d.check(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(d.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read asset directory: %w", err)
	}
	var out []string
	for _, e := range entries {
		if ValidateName(e.Name()) == nil && e.Type().IsRegular() {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// Write creates or replaces an asset and reports whether it was new.
func (d Dir) Write(name string, data []byte) (created bool, err error) {
	if err := ValidateName(name); err != nil {
		return false, err
	}
	existing, err := d.ReadAll()
	if err != nil {
		return false, fmt.Errorf("%w: %w", ErrStoredSet, err)
	}
	_, had := existing[name]
	existing[name] = data
	if err := checkSet(existing); err != nil {
		return false, err
	}
	if err := d.writeFile(name, data); err != nil {
		return false, err
	}
	return !had, nil
}

// Delete removes an asset. A file over the caps can still be removed, as
// removing it is how a provider it blocks is put right.
func (d Dir) Delete(name string) error {
	p, _, err := d.locate(name)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil {
		return fmt.Errorf("remove asset %q: %w", name, err)
	}
	return nil
}

// Matches reports whether the directory holds exactly set, byte for byte.
// An unreadable directory does not match, so the caller rewrites it.
func (d Dir) Matches(set map[string][]byte) bool {
	existing, err := d.ReadAll()
	if err != nil || len(existing) != len(set) {
		return false
	}
	for name, data := range set {
		if have, ok := existing[name]; !ok || !bytes.Equal(have, data) {
			return false
		}
	}
	return true
}

// Replace makes the directory hold exactly set: differing assets are
// written, missing ones created, and any not in set removed. The whole
// set is validated before anything is touched, so a bad name or size
// leaves the directory as it was. What is already on disk is never held
// to the caps here, since it is what is being replaced: an oversized
// stray is removed unread. An I/O failure partway leaves a mix, which
// Matches reports, so the next push repairs it.
func (d Dir) Replace(set map[string][]byte) error {
	if err := ValidateSet(set); err != nil {
		return err
	}
	existing, err := d.names()
	if err != nil {
		return err
	}
	for _, name := range existing {
		if _, keep := set[name]; keep {
			continue
		}
		if err := os.Remove(filepath.Join(d.path, name)); err != nil {
			return fmt.Errorf("remove asset %q: %w", name, err)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(set)) {
		if d.holds(name, set[name]) {
			continue
		}
		if err := d.writeFile(name, set[name]); err != nil {
			return err
		}
	}
	return nil
}

// holds reports whether the asset name already has exactly data, reading
// it only when the sizes agree.
func (d Dir) holds(name string, data []byte) bool {
	p := filepath.Join(d.path, name)
	fi, err := os.Lstat(p)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() != int64(len(data)) {
		return false
	}
	have, err := os.ReadFile(p)
	return err == nil && bytes.Equal(have, data)
}

// ValidateSet reports whether set could be a provider's whole asset set:
// every name valid and every size and the count within the caps. A
// refusal wraps ErrInvalidSet as well as the specific reason.
func ValidateSet(set map[string][]byte) error {
	for name := range set {
		if err := ValidateName(name); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidSet, err)
		}
	}
	if err := checkSet(set); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidSet, err)
	}
	return nil
}

// checkSet holds a set to what every node can store: the caps, and names
// that stay distinct where case does not.
func checkSet(set map[string][]byte) error {
	if err := checkSizes(set); err != nil {
		return err
	}
	folded := make(map[string]string, len(set))
	for _, name := range slices.Sorted(maps.Keys(set)) {
		key := strings.ToLower(name)
		if other, ok := folded[key]; ok {
			return fmt.Errorf("%w: %q and %q", ErrNameCollision, other, name)
		}
		folded[key] = name
	}
	return nil
}

func (d Dir) writeFile(name string, data []byte) error {
	if err := d.check(); err != nil {
		return err
	}
	if err := os.MkdirAll(d.path, dirMode); err != nil {
		return fmt.Errorf("create asset directory: %w", err)
	}
	if err := utils.AtomicWriteFile(filepath.Join(d.path, name), data, fileMode); err != nil {
		return fmt.Errorf("write asset %q: %w", name, err)
	}
	return nil
}

func checkSizes(set map[string][]byte) error {
	if len(set) > MaxAssets {
		return fmt.Errorf("%w: %d, the limit is %d", ErrTooMany, len(set), MaxAssets)
	}
	var total int
	for name, data := range set {
		if len(data) > MaxAssetBytes {
			return fmt.Errorf("%w: %q is %d bytes, the limit is %d", ErrTooLarge, name, len(data), MaxAssetBytes)
		}
		total += len(data)
	}
	if total > MaxProviderBytes {
		return fmt.Errorf("%w: %d bytes, the limit is %d", ErrProviderTooLarge, total, MaxProviderBytes)
	}
	return nil
}
