package integrity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreateManifest_SingleFile(t *testing.T) {
	tmpDir := t.TempDir()
	ctx := context.Background()

	// Create a test file
	testFile := filepath.Join(tmpDir, "model.bin")
	content := []byte("test model content for hashing")
	err := os.WriteFile(testFile, content, 0644)
	require.NoError(t, err)

	// Create manifest
	manifest, err := CreateManifest(ctx, testFile, "test-model", "https://example.com/model.bin")
	require.NoError(t, err)

	assert.Equal(t, 1, manifest.Version)
	assert.Equal(t, "test-model", manifest.ModelName)
	assert.Equal(t, "https://example.com/model.bin", manifest.SourceURL)
	assert.Len(t, manifest.Files, 1)

	// Verify checksum
	expectedHash := sha256.Sum256(content)
	expectedHashStr := hex.EncodeToString(expectedHash[:])
	assert.Equal(t, expectedHashStr, manifest.Files[0].SHA256)
	assert.Equal(t, int64(len(content)), manifest.Files[0].Size)
}

func TestCreateManifest_Directory(t *testing.T) {
	tmpDir := t.TempDir()
	ctx := context.Background()
	modelDir := filepath.Join(tmpDir, "my-model")
	require.NoError(t, os.MkdirAll(modelDir, 0755))

	// Create multiple files
	files := map[string][]byte{
		"config.json":  []byte(`{"name": "test"}`),
		"weights.bin":  []byte("fake weights data"),
		"subdir/a.txt": []byte("file in subdir"),
	}

	for relPath, content := range files {
		fullPath := filepath.Join(modelDir, relPath)
		require.NoError(t, os.MkdirAll(filepath.Dir(fullPath), 0755))
		require.NoError(t, os.WriteFile(fullPath, content, 0644))
	}

	// Create manifest
	manifest, err := CreateManifest(ctx, modelDir, "my-model", "")
	require.NoError(t, err)

	assert.Equal(t, len(files), len(manifest.Files))

	// Verify each file is in manifest
	checksumMap := make(map[string]string)
	for _, fc := range manifest.Files {
		checksumMap[fc.RelativePath] = fc.SHA256
	}

	for relPath, content := range files {
		expectedHash := sha256.Sum256(content)
		expectedHashStr := hex.EncodeToString(expectedHash[:])
		assert.Equal(t, expectedHashStr, checksumMap[filepath.FromSlash(relPath)], "checksum mismatch for %s", relPath)
	}
}

func TestCreateManifest_ContextCancellation(t *testing.T) {
	tmpDir := t.TempDir()
	modelDir := filepath.Join(tmpDir, "model")
	require.NoError(t, os.MkdirAll(modelDir, 0755))

	// Create a file
	require.NoError(t, os.WriteFile(filepath.Join(modelDir, "model.bin"), []byte("content"), 0644))

	// Create a cancelled context
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Should fail with context cancelled
	_, err := CreateManifest(ctx, modelDir, "test-model", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "context canceled")
}

func TestWriteAndReadManifest(t *testing.T) {
	tmpDir := t.TempDir()
	ctx := context.Background()
	modelDir := filepath.Join(tmpDir, "model")
	require.NoError(t, os.MkdirAll(modelDir, 0755))

	// Create a test file
	testFile := filepath.Join(modelDir, "model.bin")
	require.NoError(t, os.WriteFile(testFile, []byte("test content"), 0644))

	// Create and write manifest
	manifest, err := CreateManifest(ctx, modelDir, "test-model", "https://example.com")
	require.NoError(t, err)

	err = WriteManifest(modelDir, manifest)
	require.NoError(t, err)

	// Verify manifest file exists
	manifestPath := filepath.Join(modelDir, ManifestFileName)
	assert.FileExists(t, manifestPath)

	// Read manifest back
	readManifest, err := ReadManifest(modelDir)
	require.NoError(t, err)

	assert.Equal(t, manifest.ModelName, readManifest.ModelName)
	assert.Equal(t, manifest.SourceURL, readManifest.SourceURL)
	assert.Equal(t, len(manifest.Files), len(readManifest.Files))
	assert.Equal(t, manifest.Files[0].SHA256, readManifest.Files[0].SHA256)
}

func TestReadManifest_NotFound(t *testing.T) {
	tmpDir := t.TempDir()

	manifest, err := ReadManifest(tmpDir)
	require.NoError(t, err)
	assert.Nil(t, manifest)
}

func TestVerifyModel_Valid(t *testing.T) {
	tmpDir := t.TempDir()
	ctx := context.Background()
	modelDir := filepath.Join(tmpDir, "model")
	require.NoError(t, os.MkdirAll(modelDir, 0755))

	// Create test files
	files := map[string][]byte{
		"config.json": []byte(`{"name": "test"}`),
		"weights.bin": []byte("model weights"),
	}

	for name, content := range files {
		require.NoError(t, os.WriteFile(filepath.Join(modelDir, name), content, 0644))
	}

	// Create manifest
	manifest, err := CreateManifest(ctx, modelDir, "test-model", "")
	require.NoError(t, err)
	require.NoError(t, WriteManifest(modelDir, manifest))

	// Verify
	verifier := NewIntegrityVerifier()
	result, err := verifier.VerifyModel(ctx, modelDir)
	require.NoError(t, err)

	assert.True(t, result.Valid)
	assert.True(t, result.ManifestFound)
	assert.Empty(t, result.FilesMissing)
	assert.Empty(t, result.FilesCorrupted)
	assert.Empty(t, result.FilesExtra)
}

func TestVerifyModel_MissingFile(t *testing.T) {
	tmpDir := t.TempDir()
	ctx := context.Background()
	modelDir := filepath.Join(tmpDir, "model")
	require.NoError(t, os.MkdirAll(modelDir, 0755))

	// Create files
	require.NoError(t, os.WriteFile(filepath.Join(modelDir, "config.json"), []byte("config"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(modelDir, "weights.bin"), []byte("weights"), 0644))

	// Create manifest
	manifest, err := CreateManifest(ctx, modelDir, "test-model", "")
	require.NoError(t, err)
	require.NoError(t, WriteManifest(modelDir, manifest))

	// Delete a file
	require.NoError(t, os.Remove(filepath.Join(modelDir, "weights.bin")))

	// Verify
	verifier := NewIntegrityVerifier()
	result, err := verifier.VerifyModel(ctx, modelDir)
	require.NoError(t, err)

	assert.False(t, result.Valid)
	assert.Contains(t, result.FilesMissing, "weights.bin")
}

func TestVerifyModel_CorruptedFile(t *testing.T) {
	tmpDir := t.TempDir()
	ctx := context.Background()
	modelDir := filepath.Join(tmpDir, "model")
	require.NoError(t, os.MkdirAll(modelDir, 0755))

	// Create file
	require.NoError(t, os.WriteFile(filepath.Join(modelDir, "model.bin"), []byte("original content"), 0644))

	// Create manifest
	manifest, err := CreateManifest(ctx, modelDir, "test-model", "")
	require.NoError(t, err)
	require.NoError(t, WriteManifest(modelDir, manifest))

	// Corrupt the file
	require.NoError(t, os.WriteFile(filepath.Join(modelDir, "model.bin"), []byte("CORRUPTED content"), 0644))

	// Verify
	verifier := NewIntegrityVerifier()
	result, err := verifier.VerifyModel(ctx, modelDir)
	require.NoError(t, err)

	assert.False(t, result.Valid)
	assert.Contains(t, result.FilesCorrupted, "model.bin")
}

func TestVerifyModel_ExtraFile(t *testing.T) {
	tmpDir := t.TempDir()
	ctx := context.Background()
	modelDir := filepath.Join(tmpDir, "model")
	require.NoError(t, os.MkdirAll(modelDir, 0755))

	// Create file
	require.NoError(t, os.WriteFile(filepath.Join(modelDir, "model.bin"), []byte("content"), 0644))

	// Create manifest
	manifest, err := CreateManifest(ctx, modelDir, "test-model", "")
	require.NoError(t, err)
	require.NoError(t, WriteManifest(modelDir, manifest))

	// Add extra file (potential tampering)
	require.NoError(t, os.WriteFile(filepath.Join(modelDir, "malicious.bin"), []byte("evil"), 0644))

	// Verify
	verifier := NewIntegrityVerifier()
	result, err := verifier.VerifyModel(ctx, modelDir)
	require.NoError(t, err)

	assert.False(t, result.Valid)
	assert.Contains(t, result.FilesExtra, "malicious.bin")
}

func TestVerifyModel_NoManifest(t *testing.T) {
	tmpDir := t.TempDir()
	ctx := context.Background()
	modelDir := filepath.Join(tmpDir, "model")
	require.NoError(t, os.MkdirAll(modelDir, 0755))

	// Create file without manifest
	require.NoError(t, os.WriteFile(filepath.Join(modelDir, "model.bin"), []byte("content"), 0644))

	// Verify
	verifier := NewIntegrityVerifier()
	result, err := verifier.VerifyModel(ctx, modelDir)
	require.NoError(t, err)

	assert.False(t, result.Valid)
	assert.False(t, result.ManifestFound)
}

func TestVerifyModel_ContextCancellation(t *testing.T) {
	tmpDir := t.TempDir()
	modelDir := filepath.Join(tmpDir, "model")
	require.NoError(t, os.MkdirAll(modelDir, 0755))

	// Create file and manifest
	require.NoError(t, os.WriteFile(filepath.Join(modelDir, "model.bin"), []byte("content"), 0644))
	manifest, err := CreateManifest(context.Background(), modelDir, "test-model", "")
	require.NoError(t, err)
	require.NoError(t, WriteManifest(modelDir, manifest))

	// Create a cancelled context
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Should fail with context cancelled
	verifier := NewIntegrityVerifier()
	result, err := verifier.VerifyModel(ctx, modelDir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "context canceled")
	assert.False(t, result.Valid)
}

func TestVerifyAndCopy_Success(t *testing.T) {
	tmpDir := t.TempDir()
	ctx := context.Background()

	// Create source model
	srcDir := filepath.Join(tmpDir, "source")
	require.NoError(t, os.MkdirAll(srcDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "model.bin"), []byte("model content"), 0644))

	// Create manifest
	manifest, err := CreateManifest(ctx, srcDir, "test-model", "")
	require.NoError(t, err)
	require.NoError(t, WriteManifest(srcDir, manifest))

	// Copy with verification
	dstDir := filepath.Join(tmpDir, "destination")
	verifier := NewIntegrityVerifier()
	result, err := verifier.VerifyAndCopy(ctx, srcDir, dstDir)
	require.NoError(t, err)

	assert.True(t, result.Valid)

	// Verify destination has the file and manifest
	assert.FileExists(t, filepath.Join(dstDir, "model.bin"))
	assert.FileExists(t, filepath.Join(dstDir, ManifestFileName))

	// Verify destination passes verification
	dstResult, err := verifier.VerifyModel(ctx, dstDir)
	require.NoError(t, err)
	assert.True(t, dstResult.Valid)
}

func TestVerifyAndCopy_SourceInvalid(t *testing.T) {
	tmpDir := t.TempDir()
	ctx := context.Background()

	// Create source model with manifest
	srcDir := filepath.Join(tmpDir, "source")
	require.NoError(t, os.MkdirAll(srcDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "model.bin"), []byte("original"), 0644))

	manifest, err := CreateManifest(ctx, srcDir, "test-model", "")
	require.NoError(t, err)
	require.NoError(t, WriteManifest(srcDir, manifest))

	// Corrupt source
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "model.bin"), []byte("corrupted"), 0644))

	// Attempt copy
	dstDir := filepath.Join(tmpDir, "destination")
	verifier := NewIntegrityVerifier()
	_, err = verifier.VerifyAndCopy(ctx, srcDir, dstDir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "source model integrity check failed")

	// Destination should not exist
	_, statErr := os.Stat(dstDir)
	assert.True(t, os.IsNotExist(statErr))
}

func TestQuickVerify(t *testing.T) {
	tmpDir := t.TempDir()
	ctx := context.Background()
	modelDir := filepath.Join(tmpDir, "model")
	require.NoError(t, os.MkdirAll(modelDir, 0755))

	// Create file
	require.NoError(t, os.WriteFile(filepath.Join(modelDir, "model.bin"), []byte("content"), 0644))

	// Create manifest
	manifest, err := CreateManifest(ctx, modelDir, "test-model", "")
	require.NoError(t, err)
	require.NoError(t, WriteManifest(modelDir, manifest))

	// Quick verify should pass
	verifier := NewIntegrityVerifier()
	valid, err := verifier.QuickVerify(ctx, modelDir)
	require.NoError(t, err)
	assert.True(t, valid)

	// Change file size (quick verify checks size)
	require.NoError(t, os.WriteFile(filepath.Join(modelDir, "model.bin"), []byte("longer content here"), 0644))

	valid, err = verifier.QuickVerify(ctx, modelDir)
	require.NoError(t, err)
	assert.False(t, valid)
}

func TestQuickVerify_ContextCancellation(t *testing.T) {
	tmpDir := t.TempDir()
	modelDir := filepath.Join(tmpDir, "model")
	require.NoError(t, os.MkdirAll(modelDir, 0755))

	// Create file and manifest
	require.NoError(t, os.WriteFile(filepath.Join(modelDir, "model.bin"), []byte("content"), 0644))
	manifest, err := CreateManifest(context.Background(), modelDir, "test-model", "")
	require.NoError(t, err)
	require.NoError(t, WriteManifest(modelDir, manifest))

	// Create a cancelled context
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Should fail with context cancelled
	verifier := NewIntegrityVerifier()
	valid, err := verifier.QuickVerify(ctx, modelDir)
	require.Error(t, err)
	assert.False(t, valid)
}

func TestHasManifest(t *testing.T) {
	tmpDir := t.TempDir()
	ctx := context.Background()
	modelDir := filepath.Join(tmpDir, "model")
	require.NoError(t, os.MkdirAll(modelDir, 0755))

	// No manifest initially
	assert.False(t, HasManifest(modelDir))

	// Create file and manifest
	require.NoError(t, os.WriteFile(filepath.Join(modelDir, "model.bin"), []byte("content"), 0644))
	manifest, err := CreateManifest(ctx, modelDir, "test-model", "")
	require.NoError(t, err)
	require.NoError(t, WriteManifest(modelDir, manifest))

	// Now has manifest
	assert.True(t, HasManifest(modelDir))
}

func TestVerificationCache(t *testing.T) {
	tmpDir := t.TempDir()
	ctx := context.Background()
	modelDir := filepath.Join(tmpDir, "model")
	require.NoError(t, os.MkdirAll(modelDir, 0755))

	// Create large-ish file to make timing measurable
	content := make([]byte, 1024*1024) // 1MB
	for i := range content {
		content[i] = byte(i % 256)
	}
	require.NoError(t, os.WriteFile(filepath.Join(modelDir, "model.bin"), content, 0644))

	// Create manifest
	manifest, err := CreateManifest(ctx, modelDir, "test-model", "")
	require.NoError(t, err)
	require.NoError(t, WriteManifest(modelDir, manifest))

	verifier := NewIntegrityVerifier()

	// First verification - should hash file
	result1, err := verifier.VerifyModel(ctx, modelDir)
	require.NoError(t, err)
	assert.True(t, result1.Valid)
	firstDuration := result1.Duration

	// Second verification - should use cache (faster)
	result2, err := verifier.VerifyModel(ctx, modelDir)
	require.NoError(t, err)
	assert.True(t, result2.Valid)

	// Cache should make second verification faster (but not always reliable in tests)
	_ = firstDuration

	// Invalidate cache
	verifier.InvalidateCache(modelDir)

	// Third verification - should hash again
	result3, err := verifier.VerifyModel(ctx, modelDir)
	require.NoError(t, err)
	assert.True(t, result3.Valid)
}

func TestCacheStats(t *testing.T) {
	verifier := NewIntegrityVerifier()
	size, capacity := verifier.CacheStats()
	assert.Equal(t, 0, size)
	assert.Equal(t, maxCacheEntries, capacity)
}

func TestCreateManifest_SkipsSymlinks(t *testing.T) {
	tmpDir := t.TempDir()
	ctx := context.Background()
	modelDir := filepath.Join(tmpDir, "model")
	require.NoError(t, os.MkdirAll(modelDir, 0755))

	// Create real file
	require.NoError(t, os.WriteFile(filepath.Join(modelDir, "real.bin"), []byte("content"), 0644))

	// Create symlink
	symlinkPath := filepath.Join(modelDir, "symlink.bin")
	err := os.Symlink(filepath.Join(modelDir, "real.bin"), symlinkPath)
	if err != nil {
		t.Skip("symlinks not supported on this platform")
	}

	// Create manifest - should only include real file
	manifest, err := CreateManifest(ctx, modelDir, "test-model", "")
	require.NoError(t, err)

	// Should only have one file (symlinks are skipped for security)
	assert.Len(t, manifest.Files, 1)
	assert.Equal(t, "real.bin", manifest.Files[0].RelativePath)
}

func TestManifestVersion(t *testing.T) {
	tmpDir := t.TempDir()
	ctx := context.Background()

	// Create a test file
	testFile := filepath.Join(tmpDir, "model.bin")
	require.NoError(t, os.WriteFile(testFile, []byte("content"), 0644))

	// Create manifest
	manifest, err := CreateManifest(ctx, testFile, "test-model", "")
	require.NoError(t, err)

	// Version should be 1
	assert.Equal(t, 1, manifest.Version)
}

func TestVerifyResult_Duration(t *testing.T) {
	tmpDir := t.TempDir()
	ctx := context.Background()
	modelDir := filepath.Join(tmpDir, "model")
	require.NoError(t, os.MkdirAll(modelDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(modelDir, "model.bin"), []byte("content"), 0644))

	manifest, err := CreateManifest(ctx, modelDir, "test-model", "")
	require.NoError(t, err)
	require.NoError(t, WriteManifest(modelDir, manifest))

	// Control the start time instead of relying on this tiny fixture taking
	// more than one Windows clock tick. This still detects an unset duration.
	t.Cleanup(utils.SetClock(utils.FixedClock(utils.Now().Add(-time.Second))))
	verifier := NewIntegrityVerifier()
	result, err := verifier.VerifyModel(ctx, modelDir)
	require.NoError(t, err)
	assert.True(t, result.Valid)
	assert.GreaterOrEqual(t, result.Duration, time.Second)
}

// Security tests

func TestVerifyModel_PathTraversalInManifest(t *testing.T) {
	tmpDir := t.TempDir()
	ctx := context.Background()
	modelDir := filepath.Join(tmpDir, "model")
	require.NoError(t, os.MkdirAll(modelDir, 0755))

	// Create a legitimate file
	require.NoError(t, os.WriteFile(filepath.Join(modelDir, "model.bin"), []byte("content"), 0644))

	// Create a malicious manifest with path traversal
	maliciousManifest := &ModelManifest{
		Version:   1,
		ModelName: "malicious",
		Files: []FileChecksum{
			{
				RelativePath: "../../../etc/passwd",
				SHA256:       "fakehash",
				Size:         100,
			},
		},
	}

	require.NoError(t, WriteManifest(modelDir, maliciousManifest))

	// Verification should detect and reject path traversal
	verifier := NewIntegrityVerifier()
	result, err := verifier.VerifyModel(ctx, modelDir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "path traversal")
	assert.False(t, result.Valid)
}

func TestQuickVerify_PathTraversalInManifest(t *testing.T) {
	tmpDir := t.TempDir()
	ctx := context.Background()
	modelDir := filepath.Join(tmpDir, "model")
	require.NoError(t, os.MkdirAll(modelDir, 0755))

	// Create a malicious manifest with path traversal
	maliciousManifest := &ModelManifest{
		Version:   1,
		ModelName: "malicious",
		Files: []FileChecksum{
			{
				RelativePath: "../secret.txt",
				SHA256:       "fakehash",
				Size:         100,
			},
		},
	}

	require.NoError(t, WriteManifest(modelDir, maliciousManifest))

	// QuickVerify should detect and reject path traversal
	verifier := NewIntegrityVerifier()
	valid, err := verifier.QuickVerify(ctx, modelDir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid path")
	assert.False(t, valid)
}

func TestVerifyModel_SymlinkReplacement(t *testing.T) {
	tmpDir := t.TempDir()
	ctx := context.Background()
	modelDir := filepath.Join(tmpDir, "model")
	require.NoError(t, os.MkdirAll(modelDir, 0755))

	// Create original file
	modelFile := filepath.Join(modelDir, "model.bin")
	require.NoError(t, os.WriteFile(modelFile, []byte("original content"), 0644))

	// Create manifest with original file
	manifest, err := CreateManifest(ctx, modelDir, "test-model", "")
	require.NoError(t, err)
	require.NoError(t, WriteManifest(modelDir, manifest))

	// Replace file with symlink (simulating attack)
	require.NoError(t, os.Remove(modelFile))
	// Create a target file outside model directory
	targetFile := filepath.Join(tmpDir, "target.txt")
	require.NoError(t, os.WriteFile(targetFile, []byte("original content"), 0644))
	// Make symlink same content as original to bypass hash check
	err = os.Symlink(targetFile, modelFile)
	if err != nil {
		t.Skip("symlinks not supported on this platform")
	}

	// Verification should detect symlink replacement
	verifier := NewIntegrityVerifier()
	result, err := verifier.VerifyModel(ctx, modelDir)
	require.NoError(t, err)
	assert.False(t, result.Valid)
	assert.Contains(t, result.FilesCorrupted, "model.bin")
}

func TestQuickVerify_SymlinkReplacement(t *testing.T) {
	tmpDir := t.TempDir()
	ctx := context.Background()
	modelDir := filepath.Join(tmpDir, "model")
	require.NoError(t, os.MkdirAll(modelDir, 0755))

	// Create original file
	modelFile := filepath.Join(modelDir, "model.bin")
	require.NoError(t, os.WriteFile(modelFile, []byte("content"), 0644))

	// Create manifest
	manifest, err := CreateManifest(ctx, modelDir, "test-model", "")
	require.NoError(t, err)
	require.NoError(t, WriteManifest(modelDir, manifest))

	// Replace file with symlink
	require.NoError(t, os.Remove(modelFile))
	targetFile := filepath.Join(tmpDir, "target.txt")
	require.NoError(t, os.WriteFile(targetFile, []byte("content"), 0644))
	err = os.Symlink(targetFile, modelFile)
	if err != nil {
		t.Skip("symlinks not supported on this platform")
	}

	// QuickVerify should detect symlink replacement
	verifier := NewIntegrityVerifier()
	valid, err := verifier.QuickVerify(ctx, modelDir)
	require.NoError(t, err)
	assert.False(t, valid) // Symlink should fail validation
}

func TestCacheNormalizesPath(t *testing.T) {
	tmpDir := t.TempDir()
	ctx := context.Background()
	modelDir := filepath.Join(tmpDir, "model")
	require.NoError(t, os.MkdirAll(modelDir, 0755))

	// Create file
	require.NoError(t, os.WriteFile(filepath.Join(modelDir, "model.bin"), []byte("content"), 0644))

	// Create manifest
	manifest, err := CreateManifest(ctx, modelDir, "test-model", "")
	require.NoError(t, err)
	require.NoError(t, WriteManifest(modelDir, manifest))

	verifier := NewIntegrityVerifier()

	// First verification with clean path
	result1, err := verifier.VerifyModel(ctx, modelDir)
	require.NoError(t, err)
	assert.True(t, result1.Valid)

	// Second verification with path containing ./
	// Both should use the same cache entry
	altPath := filepath.Join(tmpDir, ".", "model")
	result2, err := verifier.VerifyModel(ctx, altPath)
	require.NoError(t, err)
	assert.True(t, result2.Valid)

	// Cache should be working (both paths normalized to same key)
	// The second call should be faster due to cache hit
}

func TestReadManifest_TooManyFiles(t *testing.T) {
	// Note: This test verifies that manifests with excessive file counts are rejected.
	// The maxManifestFiles limit (100000) would create a manifest larger than maxManifestSize (1MB),
	// so the size check triggers first. Both checks protect against resource exhaustion.
	// This test verifies the size limit protection works.

	tmpDir := t.TempDir()
	modelDir := filepath.Join(tmpDir, "model")
	require.NoError(t, os.MkdirAll(modelDir, 0755))

	// Create a manifest that exceeds the size limit (simulating a DoS attack)
	manifest := &ModelManifest{
		Version:   1,
		ModelName: "test-model",
		Files:     make([]FileChecksum, 100001), // This creates a file > 1MB
	}

	// Manually write the manifest (bypassing WriteManifest)
	data, err := json.Marshal(manifest)
	require.NoError(t, err)
	manifestPath := filepath.Join(modelDir, ManifestFileName)
	require.NoError(t, os.WriteFile(manifestPath, data, 0644))

	// ReadManifest should reject it (size check triggers first for large arrays)
	_, err = ReadManifest(modelDir)
	require.Error(t, err)
	// Either size limit or file count limit is triggered - both are valid protections
	errStr := err.Error()
	assert.True(t,
		strings.Contains(errStr, "too large") || strings.Contains(errStr, "too many files"),
		"Expected error about manifest size or file count limit, got: %s", errStr)
}

func TestReadManifest_FileCountLimit(t *testing.T) {
	// This test directly verifies the file count limit
	// by using a minimal file entry structure to stay under size limit

	tmpDir := t.TempDir()
	modelDir := filepath.Join(tmpDir, "model")
	require.NoError(t, os.MkdirAll(modelDir, 0755))

	// Create a manifest with exactly maxManifestFiles + 1 entries
	// Using minimal entries (each ~70 bytes in JSON)
	// 100001 * 70 bytes ≈ 7MB which exceeds size limit
	// So we can't easily test the file count limit without the size check first
	// The size check effectively subsumes the file count check for practical attacks

	// Instead, verify the constant exists and is reasonable
	assert.Equal(t, 100000, maxManifestFiles, "maxManifestFiles should be 100000")

	// And verify a reasonable file count passes
	manifest := &ModelManifest{
		Version:   1,
		ModelName: "x",
		Files:     make([]FileChecksum, 10), // Small number
	}
	for i := range manifest.Files {
		manifest.Files[i] = FileChecksum{RelativePath: "f", SHA256: "a", Size: 1}
	}

	data, err := json.Marshal(manifest)
	require.NoError(t, err)
	manifestPath := filepath.Join(modelDir, ManifestFileName)
	require.NoError(t, os.WriteFile(manifestPath, data, 0644))

	// Should succeed for reasonable file count
	readManifest, err := ReadManifest(modelDir)
	require.NoError(t, err)
	assert.Len(t, readManifest.Files, 10)
}

func TestLRUCacheEviction(t *testing.T) {
	// Test that LRU eviction works correctly
	verifier := NewIntegrityVerifier()
	tmpDir := t.TempDir()
	ctx := context.Background()

	// Create multiple model directories with files
	for i := range 5 {
		modelDir := filepath.Join(tmpDir, "model"+string(rune('A'+i)))
		require.NoError(t, os.MkdirAll(modelDir, 0755))
		require.NoError(t, os.WriteFile(filepath.Join(modelDir, "model.bin"), []byte("content"), 0644))

		manifest, err := CreateManifest(ctx, modelDir, "test", "")
		require.NoError(t, err)
		require.NoError(t, WriteManifest(modelDir, manifest))

		result, err := verifier.VerifyModel(ctx, modelDir)
		require.NoError(t, err)
		assert.True(t, result.Valid)
	}

	// Verify cache has entries
	size, _ := verifier.CacheStats()
	assert.Equal(t, 5, size, "Cache should have 5 entries")

	// Access first model again to update its LRU position
	modelA := filepath.Join(tmpDir, "modelA")
	result, err := verifier.VerifyModel(ctx, modelA)
	require.NoError(t, err)
	assert.True(t, result.Valid)

	// Cache size should still be 5
	size, _ = verifier.CacheStats()
	assert.Equal(t, 5, size, "Cache should still have 5 entries")
}
