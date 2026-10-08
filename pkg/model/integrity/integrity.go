// Package integrity provides utilities for managing local model caches
// with zero-trust integrity verification.
//
// # Thread Safety
//
// IntegrityVerifier is safe for concurrent use. All public methods can be
// called from multiple goroutines simultaneously.
//
// # Error Handling
//
// Methods return errors for system-level failures (I/O errors, path resolution failures,
// security violations like path traversal). The VerifyResult struct contains validation
// details (missing files, corrupted files, etc.) that are part of normal verification
// and do not indicate system failures.
//
// When a method returns both a VerifyResult and an error:
//   - error != nil: A system-level failure occurred; VerifyResult.Error contains the message
//   - error == nil && !result.Valid: Verification completed but found integrity issues
//   - error == nil && result.Valid: Verification passed successfully
package integrity

import (
	"container/heap"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/stperic/zzrouter/pkg/utils"
)

const (
	// maxManifestSize is the maximum allowed size for manifest files (1MB)
	// Prevents memory exhaustion from crafted large manifests
	maxManifestSize = 1024 * 1024

	// maxCacheEntries is the maximum number of entries in the verification cache
	maxCacheEntries = 10000

	// maxManifestFiles is the maximum number of files allowed in a manifest
	// Prevents memory exhaustion from crafted manifests with millions of entries
	maxManifestFiles = 100000

	// defaultHashBufferSize is the buffer size for hashing operations (64KB)
	defaultHashBufferSize = 64 * 1024
)

// ManifestFileName is the name of the integrity manifest file
const ManifestFileName = ".zzrouter-manifest.json"

// partialSuffix marks a file still being downloaded into a model directory.
const partialSuffix = ".zzpart"

// CreatePartial creates the file a download writes dest into until it is
// complete. It sits beside dest, so the final rename never crosses a
// filesystem, and its name is unique, so two downloads of one file never
// write into each other.
func CreatePartial(dest string) (*os.File, error) {
	return os.CreateTemp(filepath.Dir(dest), "."+filepath.Base(dest)+".*"+partialSuffix)
}

// IsBookkeeping reports whether name is the manifest (or a write of it) or
// a download in progress, which no walk of a model directory counts as
// model content.
func IsBookkeeping(name string) bool {
	return strings.HasPrefix(name, ManifestFileName) || strings.HasSuffix(name, partialSuffix)
}

// ModelManifest contains integrity information for a model
type ModelManifest struct {
	// Version of the manifest format
	Version int `json:"version"`

	// ModelName is the display name of the model
	ModelName string `json:"model_name"`

	// SourceURL is where the model was downloaded from (for provenance)
	SourceURL string `json:"source_url,omitempty"`

	// Files contains checksums for all files in the model
	Files []FileChecksum `json:"files"`

	// TotalSize is the sum of all file sizes
	TotalSize int64 `json:"total_size"`

	// CreatedAt is when this manifest was created
	CreatedAt time.Time `json:"created_at"`

	// VerifiedAt is the last time the model was verified
	VerifiedAt time.Time `json:"verified_at"`

	// SourceDigest is the original digest from the source (e.g., Ollama)
	SourceDigest string `json:"source_digest,omitempty"`
}

// FileChecksum contains integrity information for a single file
type FileChecksum struct {
	// RelativePath is the path relative to the model directory
	RelativePath string `json:"path"`

	// SHA256 is the hex-encoded SHA256 hash
	SHA256 string `json:"sha256"`

	// Size is the file size in bytes
	Size int64 `json:"size"`

	// ModTime is the file modification time
	ModTime time.Time `json:"mod_time"`

	// Feature names the model feature the file belongs to (e.g. "vision");
	// empty for the weights and the repo's other files.
	Feature string `json:"feature,omitempty"`
}

// IntegrityVerifier provides zero-trust verification for model files.
// It is safe for concurrent use from multiple goroutines.
type IntegrityVerifier struct {
	mu sync.RWMutex

	// verificationCache caches recent verification results to avoid
	// re-hashing files that haven't changed. Uses LRU eviction.
	verificationCache map[string]*verificationCacheEntry

	// lruHeap maintains LRU ordering for cache eviction
	lruHeap *cacheHeap

	// heapIndex maps cache keys to their index in the heap
	heapIndex map[string]int
}

type verificationCacheEntry struct {
	Path           string // Absolute path (cache key)
	Valid          bool
	CheckedAt      time.Time
	LastAccessedAt time.Time // For LRU eviction
	FileSize       int64
	FileMode       os.FileMode
	ModTime        time.Time
	heapIndex      int // Index in the LRU heap (-1 if not in heap)
}

// cacheHeap implements heap.Interface for LRU eviction
type cacheHeap []*verificationCacheEntry

func (h cacheHeap) Len() int           { return len(h) }
func (h cacheHeap) Less(i, j int) bool { return h[i].LastAccessedAt.Before(h[j].LastAccessedAt) }
func (h cacheHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].heapIndex = i
	h[j].heapIndex = j
}

func (h *cacheHeap) Push(x any) {
	entry, _ := x.(*verificationCacheEntry)
	entry.heapIndex = len(*h)
	*h = append(*h, entry)
}

func (h *cacheHeap) Pop() any {
	old := *h
	n := len(old)
	entry := old[n-1]
	old[n-1] = nil // avoid memory leak
	entry.heapIndex = -1
	*h = old[0 : n-1]
	return entry
}

// NewIntegrityVerifier creates a new integrity verifier
func NewIntegrityVerifier() *IntegrityVerifier {
	h := &cacheHeap{}
	heap.Init(h)
	return &IntegrityVerifier{
		verificationCache: make(map[string]*verificationCacheEntry),
		lruHeap:           h,
		heapIndex:         make(map[string]int),
	}
}

// CreateManifest creates an integrity manifest for a model directory or file.
// The context can be used to cancel long-running operations on large models.
func CreateManifest(ctx context.Context, modelPath, modelName, sourceURL string) (*ModelManifest, error) {
	// Check context before starting
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("context cancelled: %w", err)
	}

	info, err := os.Stat(modelPath)
	if err != nil {
		return nil, fmt.Errorf("model path not found: %w", err)
	}

	manifest := &ModelManifest{
		Version:   1,
		ModelName: modelName,
		SourceURL: sourceURL,
		CreatedAt: utils.Now(),
		Files:     make([]FileChecksum, 0),
	}

	if info.IsDir() {
		// Walk directory and hash all files
		// Use filepath.WalkDir for better performance and explicit symlink handling
		err = filepath.WalkDir(modelPath, func(path string, d os.DirEntry, err error) error {
			// Check context periodically
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}

			if err != nil {
				return err
			}

			// Skip directories and manifest files
			if d.IsDir() || IsBookkeeping(filepath.Base(path)) {
				return nil
			}

			// Get file info using Lstat to detect symlinks (Stat follows symlinks)
			fileInfo, err := os.Lstat(path)
			if err != nil {
				return fmt.Errorf("failed to stat %s: %w", path, err)
			}

			// Skip symlinks (security: prevent symlink attacks)
			if fileInfo.Mode()&os.ModeSymlink != 0 {
				return nil
			}

			// Skip non-regular files (devices, sockets, etc.)
			if !fileInfo.Mode().IsRegular() {
				return nil
			}

			relPath, err := filepath.Rel(modelPath, path)
			if err != nil {
				return fmt.Errorf("failed to get relative path: %w", err)
			}

			// Security: Validate relative path doesn't escape model directory
			if strings.HasPrefix(relPath, "..") || filepath.IsAbs(relPath) {
				return fmt.Errorf("invalid relative path: %s", relPath)
			}

			checksum, err := hashFileWithContext(ctx, path)
			if err != nil {
				return fmt.Errorf("failed to hash %s: %w", relPath, err)
			}

			manifest.Files = append(manifest.Files, FileChecksum{
				RelativePath: relPath,
				SHA256:       checksum,
				Size:         fileInfo.Size(),
				ModTime:      fileInfo.ModTime(),
			})
			manifest.TotalSize += fileInfo.Size()

			return nil
		})
		if err != nil {
			return nil, err
		}
	} else {
		// Single file
		checksum, err := hashFileWithContext(ctx, modelPath)
		if err != nil {
			return nil, fmt.Errorf("failed to hash file: %w", err)
		}

		manifest.Files = append(manifest.Files, FileChecksum{
			RelativePath: filepath.Base(modelPath),
			SHA256:       checksum,
			Size:         info.Size(),
			ModTime:      info.ModTime(),
		})
		manifest.TotalSize = info.Size()
	}

	if info.IsDir() {
		if err := keepFeatures(modelPath, manifest); err != nil {
			return nil, err
		}
	}
	return manifest, nil
}

// keepFeatures carries each file's feature over from the manifest being
// replaced: hashing the directory again says what the bytes are, not what
// they are for.
func keepFeatures(modelPath string, manifest *ModelManifest) error {
	prev, err := ReadManifest(modelPath)
	if err != nil || prev == nil {
		return err
	}
	features := make(map[string]string, len(prev.Files))
	for _, f := range prev.Files {
		features[f.RelativePath] = f.Feature
	}
	for i := range manifest.Files {
		manifest.Files[i].Feature = features[manifest.Files[i].RelativePath]
	}
	return nil
}

// Holds reports whether dir holds the file at rel as the manifest records
// it: an entry at that path with size, and sha256 when one is given, and
// the file on disk at that size. It reads no file content, so it answers
// for multi-gigabyte weights at the cost of a stat.
func (m *ModelManifest) Holds(dir, rel string, size int64, sha256 string) (FileChecksum, bool) {
	if m == nil {
		return FileChecksum{}, false
	}
	want := strings.ToLower(strings.TrimSpace(sha256))
	for _, f := range m.Files {
		if filepath.ToSlash(f.RelativePath) != rel {
			continue
		}
		if f.Size != size || (want != "" && f.SHA256 != want) {
			return FileChecksum{}, false
		}
		info, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel)))
		return f, err == nil && info.Size() == size
	}
	return FileChecksum{}, false
}

// manifestLocks coordinates materialization and manifest updates by repository.
var manifestLocks sync.Map // model dir -> *directoryLocks

type directoryLocks struct {
	manifest    sync.Mutex
	materialize chan struct{}
}

func locksForDirectory(dir string) (*directoryLocks, error) {
	key, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	parent := key
	var suffix []string
	for {
		resolved, err := filepath.EvalSymlinks(parent)
		if err == nil {
			key = resolved
			for i := len(suffix) - 1; i >= 0; i-- {
				key = filepath.Join(key, suffix[i])
			}
			break
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		next := filepath.Dir(parent)
		if next == parent {
			return nil, err
		}
		suffix = append(suffix, filepath.Base(parent))
		parent = next
	}
	v, _ := manifestLocks.LoadOrStore(key, &directoryLocks{materialize: make(chan struct{}, 1)})
	locks, ok := v.(*directoryLocks)
	if !ok {
		return nil, fmt.Errorf("invalid repository lock for %q", dir)
	}
	return locks, nil
}

// AcquireMaterialization serializes repository writes across registry downloads
// and peer sync. Waiting is cancellable; callers release after recording files.
func AcquireMaterialization(ctx context.Context, dir string) (func(), error) {
	lock, err := locksForDirectory(dir)
	if err != nil {
		return nil, err
	}
	select {
	case lock.materialize <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-lock.materialize
			return nil, err
		}
		return func() { <-lock.materialize }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// RecordFiles adds files to the manifest of modelPath, replacing any entry
// at the same path, and creates the manifest when there is none. A
// download records each file as it lands rather than re-hashing the whole
// directory.
func RecordFiles(modelPath, modelName, sourceURL string, files ...FileChecksum) error {
	locks, err := locksForDirectory(modelPath)
	if err != nil {
		return err
	}
	mu := &locks.manifest
	mu.Lock()
	defer mu.Unlock()

	manifest, err := ReadManifest(modelPath)
	if err != nil {
		return err
	}
	if manifest == nil {
		manifest = &ModelManifest{Version: 1, ModelName: modelName, SourceURL: sourceURL, CreatedAt: utils.Now()}
	}
	for _, f := range files {
		i := slices.IndexFunc(manifest.Files, func(e FileChecksum) bool { return e.RelativePath == f.RelativePath })
		if i < 0 {
			manifest.Files = append(manifest.Files, f)
		} else {
			manifest.Files[i] = f
		}
	}
	manifest.TotalSize = 0
	for _, f := range manifest.Files {
		manifest.TotalSize += f.Size
	}
	return WriteManifest(modelPath, manifest)
}

// WriteManifest writes a manifest file to the model directory
func WriteManifest(modelPath string, manifest *ModelManifest) error {
	manifestPath := getManifestPath(modelPath)

	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal manifest: %w", err)
	}

	// Write atomically using temp file + rename
	// Use random suffix to prevent symlink attacks on predictable temp file names
	randomBytes := make([]byte, 8)
	if _, err := rand.Read(randomBytes); err != nil {
		return fmt.Errorf("failed to generate random suffix: %w", err)
	}
	tempPath := manifestPath + ".tmp." + hex.EncodeToString(randomBytes)

	// Use O_EXCL to ensure exclusive creation - prevents symlink attacks
	// where attacker pre-creates a symlink at tempPath
	f, err := os.OpenFile(tempPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0640)
	if err != nil {
		return fmt.Errorf("failed to create temp manifest file: %w", err)
	}

	_, writeErr := f.Write(data)
	closeErr := f.Close()

	if writeErr != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("failed to write manifest: %w", writeErr)
	}
	if closeErr != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("failed to close manifest file: %w", closeErr)
	}

	if err := os.Rename(tempPath, manifestPath); err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("failed to finalize manifest: %w", err)
	}

	return nil
}

// ReadManifest reads a manifest file from a model directory.
// Returns (nil, nil) if no manifest exists (model not yet verified).
func ReadManifest(modelPath string) (*ModelManifest, error) {
	manifestPath := getManifestPath(modelPath)

	// Check file size before reading to prevent memory exhaustion
	info, err := os.Stat(manifestPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // No manifest = model not yet verified
		}
		return nil, fmt.Errorf("failed to stat manifest: %w", err)
	}

	if info.Size() > maxManifestSize {
		return nil, fmt.Errorf("manifest file too large: %d bytes (max %d)", info.Size(), maxManifestSize)
	}

	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read manifest: %w", err)
	}

	var manifest ModelManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("failed to parse manifest: %w", err)
	}

	// Validate manifest doesn't have excessive number of files
	// Prevents memory exhaustion from crafted manifests
	if len(manifest.Files) > maxManifestFiles {
		return nil, fmt.Errorf("manifest has too many files: %d (max %d)", len(manifest.Files), maxManifestFiles)
	}

	return &manifest, nil
}

// VerifyResult contains the result of a verification operation.
//
// Error Semantics:
//   - When the parent function returns an error, VerifyResult.Error contains the same message
//   - When Valid is false but no error was returned, check FilesMissing, FilesCorrupted,
//     and FilesExtra for details about what failed validation
type VerifyResult struct {
	// Valid is true only if all files match their expected checksums
	// and no extra/missing files were found
	Valid bool `json:"valid"`

	// ManifestFound indicates whether a manifest file was found
	ManifestFound bool `json:"manifest_found"`

	// FilesChecked is the number of files that were verified
	FilesChecked int `json:"files_checked"`

	// FilesMissing contains relative paths of files in the manifest but not on disk
	FilesMissing []string `json:"files_missing,omitempty"`

	// FilesCorrupted contains relative paths of files with checksum mismatches
	FilesCorrupted []string `json:"files_corrupted,omitempty"`

	// FilesExtra contains relative paths of files on disk but not in the manifest
	FilesExtra []string `json:"files_extra,omitempty"`

	// Error contains the error message if a system-level failure occurred.
	// This is set when the parent function also returns an error.
	Error string `json:"error,omitempty"`

	// Duration is how long the verification took
	Duration time.Duration `json:"duration"`

	// ExpectedSize is the total size from the manifest
	ExpectedSize int64 `json:"expected_size"`

	// ActualSize is the total size of verified files on disk
	ActualSize int64 `json:"actual_size"`
}

// VerifyModel verifies a model's integrity against its manifest.
// The context can be used to cancel long-running verification on large models.
//
// Returns:
//   - (result, nil) with result.Valid=true: verification passed
//   - (result, nil) with result.Valid=false: verification completed but found issues
//   - (result, error): a system-level failure occurred
//
//nolint:gocyclo,cyclop // verification flow has many optional fail-paths; linearizing them keeps the audit trail readable
func (v *IntegrityVerifier) VerifyModel(ctx context.Context, modelPath string) (*VerifyResult, error) {
	startTime := utils.Now()
	result := &VerifyResult{
		Valid:          false,
		FilesMissing:   make([]string, 0),
		FilesCorrupted: make([]string, 0),
		FilesExtra:     make([]string, 0),
	}

	// Check context before starting
	if err := ctx.Err(); err != nil {
		result.Error = err.Error()
		result.Duration = time.Since(startTime)
		return result, err
	}

	// Read manifest
	manifest, err := ReadManifest(modelPath)
	if err != nil {
		result.Error = err.Error()
		result.Duration = time.Since(startTime)
		return result, err
	}

	if manifest == nil {
		result.ManifestFound = false
		result.Duration = time.Since(startTime)
		// Not an error - just no manifest found
		return result, nil
	}

	result.ManifestFound = true
	result.ExpectedSize = manifest.TotalSize

	// Build map of expected files for efficient lookup
	expectedFiles := make(map[string]*FileChecksum)
	for i := range manifest.Files {
		expectedFiles[manifest.Files[i].RelativePath] = &manifest.Files[i]
	}

	// Track files we've seen
	seenFiles := make(map[string]bool)

	// Verify each expected file
	for relPath, expected := range expectedFiles {
		// Check context periodically
		if err := ctx.Err(); err != nil {
			result.Error = err.Error()
			result.Duration = time.Since(startTime)
			return result, err
		}

		// Security: Validate relative path doesn't escape model directory
		// This prevents path traversal attacks via crafted manifest files
		if strings.HasPrefix(relPath, "..") || filepath.IsAbs(relPath) || strings.Contains(relPath, ".."+string(filepath.Separator)) {
			result.Error = fmt.Sprintf("invalid path in manifest (potential path traversal): %s", relPath)
			result.Duration = time.Since(startTime)
			return result, fmt.Errorf("path traversal detected in manifest")
		}

		filePath := filepath.Join(modelPath, relPath)

		// Double-check: verify the resolved path is still within modelPath
		absModelPath, err := filepath.Abs(modelPath)
		if err != nil {
			result.Error = fmt.Sprintf("failed to resolve model path: %v", err)
			result.Duration = time.Since(startTime)
			return result, fmt.Errorf("failed to resolve model path: %w", err)
		}
		absFilePath, err := filepath.Abs(filePath)
		if err != nil {
			result.Error = fmt.Sprintf("failed to resolve file path: %v", err)
			result.Duration = time.Since(startTime)
			return result, fmt.Errorf("failed to resolve file path: %w", err)
		}
		if !strings.HasPrefix(absFilePath, absModelPath+string(filepath.Separator)) && absFilePath != absModelPath {
			result.Error = fmt.Sprintf("path escapes model directory: %s", relPath)
			result.Duration = time.Since(startTime)
			return result, fmt.Errorf("path traversal detected")
		}

		// Use Lstat to detect symlinks (consistent with CreateManifest)
		// Stat follows symlinks, which could mask symlink replacement attacks
		info, err := os.Lstat(filePath)
		if err != nil {
			if os.IsNotExist(err) {
				result.FilesMissing = append(result.FilesMissing, relPath)
				continue
			}
			result.Error = fmt.Sprintf("failed to stat %s: %v", relPath, err)
			result.Duration = time.Since(startTime)
			return result, fmt.Errorf("failed to stat %s: %w", relPath, err)
		}

		// Security: reject symlinks - file was replaced with symlink after manifest creation
		if info.Mode()&os.ModeSymlink != 0 {
			result.FilesCorrupted = append(result.FilesCorrupted, relPath)
			continue
		}

		// Security: reject non-regular files (devices, sockets, etc.)
		if !info.Mode().IsRegular() {
			result.FilesCorrupted = append(result.FilesCorrupted, relPath)
			continue
		}

		result.ActualSize += info.Size()
		seenFiles[relPath] = true

		// Quick check: size mismatch
		if info.Size() != expected.Size {
			result.FilesCorrupted = append(result.FilesCorrupted, relPath)
			continue
		}

		// Check cache for recent verification
		if v.isCacheValid(filePath, info) {
			result.FilesChecked++
			continue
		}

		// Full hash verification
		actualHash, err := hashFileWithContext(ctx, filePath)
		if err != nil {
			if ctx.Err() != nil {
				result.Error = ctx.Err().Error()
				result.Duration = time.Since(startTime)
				return result, ctx.Err()
			}
			result.Error = fmt.Sprintf("failed to hash %s: %v", relPath, err)
			result.Duration = time.Since(startTime)
			return result, fmt.Errorf("failed to hash %s: %w", relPath, err)
		}

		if actualHash != expected.SHA256 {
			result.FilesCorrupted = append(result.FilesCorrupted, relPath)
			v.setCacheEntry(filePath, false, info)
		} else {
			v.setCacheEntry(filePath, true, info)
		}

		result.FilesChecked++
	}

	// Check for extra files (potential tampering)
	// Use WalkDir + Lstat for consistency with CreateManifest (detects symlinks)
	modelInfo, _ := os.Lstat(modelPath)
	if modelInfo != nil && modelInfo.IsDir() {
		walkErr := filepath.WalkDir(modelPath, func(path string, d os.DirEntry, err error) error {
			// Check context periodically
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}

			if err != nil || d.IsDir() {
				return nil //nolint:nilerr // keep walking on per-entry errors; model integrity is best-effort
			}

			if IsBookkeeping(filepath.Base(path)) {
				return nil
			}

			// Use Lstat to detect symlinks
			fileInfo, err := os.Lstat(path)
			if err != nil {
				return nil //nolint:nilerr // skip unstatable entries; tampering check is best-effort
			}

			// Skip symlinks - they were skipped during manifest creation
			if fileInfo.Mode()&os.ModeSymlink != 0 {
				return nil
			}

			// Skip non-regular files
			if !fileInfo.Mode().IsRegular() {
				return nil
			}

			relPath, _ := filepath.Rel(modelPath, path)
			if !seenFiles[relPath] {
				// Check if it's in expected files but we missed it
				if _, expected := expectedFiles[relPath]; !expected {
					result.FilesExtra = append(result.FilesExtra, relPath)
				}
			}
			return nil
		})

		if walkErr != nil && ctx.Err() != nil {
			result.Error = ctx.Err().Error()
			result.Duration = time.Since(startTime)
			return result, ctx.Err()
		}
	}

	// Determine overall validity
	result.Valid = len(result.FilesMissing) == 0 &&
		len(result.FilesCorrupted) == 0 &&
		len(result.FilesExtra) == 0

	result.Duration = time.Since(startTime)
	return result, nil
}

// VerifyAndCopy verifies a model from source, copies to destination, and verifies the copy.
// This is the zero-trust copy operation: verify source → copy → verify destination.
// The context can be used to cancel the operation.
func (v *IntegrityVerifier) VerifyAndCopy(ctx context.Context, srcPath, dstPath string) (*VerifyResult, error) {
	// Step 1: Verify source
	srcResult, err := v.VerifyModel(ctx, srcPath)
	if err != nil {
		return srcResult, fmt.Errorf("source verification failed: %w", err)
	}

	if !srcResult.Valid {
		return srcResult, fmt.Errorf("source model integrity check failed")
	}

	// Check context before copy
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Step 2: Copy files
	srcInfo, err := os.Stat(srcPath)
	if err != nil {
		return nil, fmt.Errorf("source not found: %w", err)
	}

	if srcInfo.IsDir() {
		if err := CopyDir(srcPath, dstPath); err != nil {
			return nil, fmt.Errorf("copy failed: %w", err)
		}
		// Also copy manifest
		srcManifest := getManifestPath(srcPath)
		dstManifest := getManifestPath(dstPath)
		if FileExists(srcManifest) {
			if err := copyFile(srcManifest, dstManifest, 0640); err != nil {
				return nil, fmt.Errorf("failed to copy manifest: %w", err)
			}
		}
	} else {
		// Single file - create parent directory
		if err := os.MkdirAll(filepath.Dir(dstPath), 0755); err != nil {
			return nil, fmt.Errorf("failed to create destination directory: %w", err)
		}
		if err := copyFile(srcPath, dstPath, srcInfo.Mode()); err != nil {
			return nil, fmt.Errorf("copy failed: %w", err)
		}
	}

	// Check context before destination verification
	if err := ctx.Err(); err != nil {
		_ = os.RemoveAll(dstPath)
		return nil, err
	}

	// Step 3: Verify destination
	dstResult, err := v.VerifyModel(ctx, dstPath)
	if err != nil {
		// Clean up failed copy
		_ = os.RemoveAll(dstPath)
		return dstResult, fmt.Errorf("destination verification failed: %w", err)
	}

	if !dstResult.Valid {
		// Clean up corrupt copy
		_ = os.RemoveAll(dstPath)
		return dstResult, fmt.Errorf("destination model integrity check failed after copy")
	}

	return dstResult, nil
}

// QuickVerify performs a fast verification using file metadata only (no hashing).
// Use this for frequent checks; use VerifyModel for thorough verification.
// The context can be used to cancel the operation.
//
// Returns:
//   - (true, nil): quick verification passed
//   - (false, nil): verification failed (missing file, size mismatch, etc.)
//   - (false, error): a system-level failure occurred
func (v *IntegrityVerifier) QuickVerify(ctx context.Context, modelPath string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}

	manifest, err := ReadManifest(modelPath)
	if err != nil {
		return false, err
	}
	if manifest == nil {
		return false, nil // No manifest = unverified
	}

	absModelPath, err := filepath.Abs(modelPath)
	if err != nil {
		return false, fmt.Errorf("failed to resolve model path: %w", err)
	}

	// Quick checks only: file existence, type, and size
	for _, expected := range manifest.Files {
		// Check context periodically
		if err := ctx.Err(); err != nil {
			return false, err
		}

		// Security: Validate relative path doesn't escape model directory
		if strings.HasPrefix(expected.RelativePath, "..") || filepath.IsAbs(expected.RelativePath) {
			return false, fmt.Errorf("invalid path in manifest: %s", expected.RelativePath)
		}

		filePath := filepath.Join(modelPath, expected.RelativePath)

		// Double-check the resolved path is within model directory
		absFilePath, err := filepath.Abs(filePath)
		if err != nil {
			return false, fmt.Errorf("failed to resolve file path: %w", err)
		}
		if !strings.HasPrefix(absFilePath, absModelPath+string(filepath.Separator)) && absFilePath != absModelPath {
			return false, fmt.Errorf("path traversal detected: %s", expected.RelativePath)
		}

		// Use Lstat to detect symlinks (consistent with VerifyModel)
		info, err := os.Lstat(filePath)
		if err != nil {
			return false, nil //nolint:nilerr // missing file is a data outcome (invalid=false), not a caller error
		}

		// Security: reject symlinks and non-regular files
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return false, nil // File replaced with symlink or non-regular file
		}

		if info.Size() != expected.Size {
			return false, nil // Size mismatch
		}
	}

	return true, nil
}

// InvalidateCache removes a model from the verification cache.
// Call this when a model file is modified.
func (v *IntegrityVerifier) InvalidateCache(modelPath string) {
	v.mu.Lock()
	defer v.mu.Unlock()

	// Normalize the model path for consistent comparison
	absModelPath, err := filepath.Abs(modelPath)
	if err != nil {
		// If we can't resolve the path, clear the entire cache to be safe
		v.verificationCache = make(map[string]*verificationCacheEntry)
		v.lruHeap = &cacheHeap{}
		heap.Init(v.lruHeap)
		v.heapIndex = make(map[string]int)
		return
	}

	// Collect keys to remove
	keysToRemove := make([]string, 0)
	for key := range v.verificationCache {
		// Check if key is under the model path (using string prefix with path separator)
		if strings.HasPrefix(key, absModelPath+string(filepath.Separator)) || key == absModelPath {
			keysToRemove = append(keysToRemove, key)
		}
	}

	// Remove entries
	for _, key := range keysToRemove {
		v.removeCacheEntryLocked(key)
	}
}

// CacheStats returns statistics about the verification cache
func (v *IntegrityVerifier) CacheStats() (size int, capacity int) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return len(v.verificationCache), maxCacheEntries
}

// helper functions

func getManifestPath(modelPath string) string {
	info, _ := os.Stat(modelPath)
	if info != nil && info.IsDir() {
		return filepath.Join(modelPath, ManifestFileName)
	}
	// For single files, manifest goes in parent directory with model name prefix
	return filepath.Join(filepath.Dir(modelPath), filepath.Base(modelPath)+".manifest.json")
}

// hashFileWithContext hashes a file with context cancellation support
func hashFileWithContext(ctx context.Context, path string) (string, error) {
	// Verify file is a regular file before attempting to hash
	// This prevents hangs on device files, FIFOs, sockets, etc.
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("cannot hash non-regular file: %s (mode: %s)", path, info.Mode())
	}

	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	hash := sha256.New()
	buf := make([]byte, defaultHashBufferSize)

	for {
		// Check context periodically (every buffer read)
		if err := ctx.Err(); err != nil {
			return "", err
		}

		n, err := file.Read(buf)
		if n > 0 {
			hash.Write(buf[:n])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
	}

	return hex.EncodeToString(hash.Sum(nil)), nil
}

func (v *IntegrityVerifier) isCacheValid(path string, info os.FileInfo) bool {
	v.mu.Lock()
	defer v.mu.Unlock()

	// Normalize path for consistent cache key lookup
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false // Can't normalize, assume cache miss
	}

	entry, ok := v.verificationCache[absPath]
	if !ok {
		return false
	}

	// Cache is valid if file size, mode, and modtime haven't changed
	// and verification was recent (within 1 hour)
	if entry.FileSize != info.Size() || entry.FileMode != info.Mode() {
		return false
	}
	// Check modification time - file could be modified in-place without size change
	if !entry.ModTime.Equal(info.ModTime()) {
		return false
	}
	if time.Since(entry.CheckedAt) > time.Hour {
		return false
	}

	// Update access time for LRU (still holding lock)
	entry.LastAccessedAt = utils.Now()
	heap.Fix(v.lruHeap, entry.heapIndex)

	return entry.Valid
}

func (v *IntegrityVerifier) setCacheEntry(path string, valid bool, info os.FileInfo) {
	v.mu.Lock()
	defer v.mu.Unlock()

	// Normalize path for consistent cache key storage
	absPath, err := filepath.Abs(path)
	if err != nil {
		return // Can't normalize, skip caching
	}

	now := utils.Now()

	// Check if entry already exists
	if existing, ok := v.verificationCache[absPath]; ok {
		// Update existing entry
		existing.Valid = valid
		existing.CheckedAt = now
		existing.LastAccessedAt = now
		existing.FileSize = info.Size()
		existing.FileMode = info.Mode()
		existing.ModTime = info.ModTime()
		heap.Fix(v.lruHeap, existing.heapIndex)
		return
	}

	// Evict entries if at capacity using LRU
	for len(v.verificationCache) >= maxCacheEntries && v.lruHeap.Len() > 0 {
		oldest, _ := heap.Pop(v.lruHeap).(*verificationCacheEntry)
		delete(v.verificationCache, oldest.Path)
		delete(v.heapIndex, oldest.Path)
	}

	// Create new entry
	entry := &verificationCacheEntry{
		Path:           absPath,
		Valid:          valid,
		CheckedAt:      now,
		LastAccessedAt: now,
		FileSize:       info.Size(),
		FileMode:       info.Mode(),
		ModTime:        info.ModTime(),
	}

	v.verificationCache[absPath] = entry
	heap.Push(v.lruHeap, entry)
	v.heapIndex[absPath] = entry.heapIndex
}

func (v *IntegrityVerifier) removeCacheEntryLocked(key string) {
	entry, ok := v.verificationCache[key]
	if !ok {
		return
	}

	// Remove from heap
	if entry.heapIndex >= 0 && entry.heapIndex < v.lruHeap.Len() {
		heap.Remove(v.lruHeap, entry.heapIndex)
	}

	// Remove from maps
	delete(v.verificationCache, key)
	delete(v.heapIndex, key)
}

// HasManifest checks if a model has an integrity manifest
func HasManifest(modelPath string) bool {
	manifestPath := getManifestPath(modelPath)
	return FileExists(manifestPath)
}
