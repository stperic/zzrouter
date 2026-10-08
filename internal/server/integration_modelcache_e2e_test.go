package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	modelcache "github.com/stperic/zzrouter/pkg/model/integrity"
	modelsync "github.com/stperic/zzrouter/pkg/model/sync"
	"github.com/stperic/zzrouter/pkg/modelregistry"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestModelCacheE2E_SharedToCache tests the shared storage → local cache flow
// with integrity verification at each step.
//
// Scenario:
// 1. Coordinator has model in its local storage (acts as shared storage)
// 2. Worker syncs model from coordinator
// 3. Worker's resolveModelPathWithCache copies from shared → cache
// 4. Integrity verification at each step
func TestModelCacheE2E_SharedToCache(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()

	// ============================================================
	// SETUP: Create directory structure simulating a cluster
	// ============================================================

	// Coordinator's models directory (this IS the shared storage)
	coordinatorModelsDir := t.TempDir()

	// Worker's local cache (separate from shared)
	workerCacheDir := t.TempDir()

	// Worker's "shared" mount point (simulates NFS mount)
	// In real deployment, this would be the same path as coordinatorModelsDir
	// but for testing, we'll copy files to simulate the mount
	workerSharedDir := t.TempDir()

	// Test model setup
	testModelName := "test-org/test-model"
	testFormat := "gguf"
	testFileName := "model.gguf"
	testModelContent := []byte("This is test model content for cache integrity testing. " +
		"The content should be verified at each transfer point using SHA256 checksums.")

	// Calculate expected checksum
	hasher := sha256.New()
	hasher.Write(testModelContent)
	expectedChecksum := hex.EncodeToString(hasher.Sum(nil))

	t.Logf("Test setup:")
	t.Logf("  Model: %s/%s", testModelName, testFormat)
	t.Logf("  Size: %d bytes", len(testModelContent))
	t.Logf("  Checksum: %s", expectedChecksum)

	// ============================================================
	// STEP 1: Create model on coordinator with integrity manifest
	// ============================================================
	t.Run("Step1_CreateModelOnCoordinator", func(t *testing.T) {
		modelDir := filepath.Join(coordinatorModelsDir, testModelName, testFormat)
		require.NoError(t, os.MkdirAll(modelDir, 0755))

		modelPath := filepath.Join(modelDir, testFileName)
		require.NoError(t, os.WriteFile(modelPath, testModelContent, 0644))

		// Create integrity manifest (simulating what happens after download)
		sourceURL := "https://huggingface.co/" + testModelName
		manifest, err := modelcache.CreateManifest(ctx, modelDir, testModelName, sourceURL)
		require.NoError(t, err)
		require.NoError(t, modelcache.WriteManifest(modelDir, manifest))

		// Verify manifest was created
		assert.True(t, modelcache.HasManifest(modelDir))
		assert.Len(t, manifest.Files, 1)
		assert.Equal(t, expectedChecksum, manifest.Files[0].SHA256)

		t.Logf("Created model on coordinator with manifest (%d files)", len(manifest.Files))
	})

	// ============================================================
	// STEP 2: Simulate shared storage mount (copy to worker's shared view)
	// ============================================================
	t.Run("Step2_SimulateSharedStorageMount", func(t *testing.T) {
		// In production, this would be an NFS mount
		// For testing, we copy the directory
		srcDir := filepath.Join(coordinatorModelsDir, testModelName, testFormat)
		dstDir := filepath.Join(workerSharedDir, testModelName, testFormat)

		require.NoError(t, modelcache.CopyDir(srcDir, dstDir))

		// Also copy the manifest
		srcManifest := filepath.Join(srcDir, modelcache.ManifestFileName)
		dstManifest := filepath.Join(dstDir, modelcache.ManifestFileName)
		if modelcache.FileExists(srcManifest) {
			data, err := os.ReadFile(srcManifest)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(dstManifest, data, 0640))
		}

		// Verify shared storage has the model
		assert.True(t, modelcache.HasManifest(dstDir))
		assert.FileExists(t, filepath.Join(dstDir, testFileName))

		t.Logf("Simulated shared storage mount at: %s", dstDir)
	})

	// ============================================================
	// STEP 3: Test shared → cache flow with integrity verification
	// ============================================================
	t.Run("Step3_SharedToCacheWithVerification", func(t *testing.T) {
		verifier := modelcache.NewIntegrityVerifier()

		sharedPath := filepath.Join(workerSharedDir, testModelName, testFormat)
		cachePath := filepath.Join(workerCacheDir, testModelName, testFormat)

		// Verify shared storage is valid before copying
		result, err := verifier.VerifyModel(ctx, sharedPath)
		require.NoError(t, err)
		assert.True(t, result.Valid, "Shared storage should be valid")
		assert.True(t, result.ManifestFound, "Manifest should exist in shared storage")

		t.Logf("Verified shared storage: %d files checked", result.FilesChecked)

		// Use VerifyAndCopy (zero-trust copy)
		copyResult, err := verifier.VerifyAndCopy(ctx, sharedPath, cachePath)
		require.NoError(t, err)
		assert.True(t, copyResult.Valid, "Cache copy should be valid")

		t.Logf("Copied to cache with verification: %d files", copyResult.FilesChecked)

		// Verify cache has manifest
		assert.True(t, modelcache.HasManifest(cachePath))

		// Verify cached file content
		cachedContent, err := os.ReadFile(filepath.Join(cachePath, testFileName))
		require.NoError(t, err)
		assert.Equal(t, testModelContent, cachedContent)
	})

	// ============================================================
	// STEP 4: Test cache hit path (fast, uses QuickVerify)
	// ============================================================
	t.Run("Step4_CacheHitPath", func(t *testing.T) {
		verifier := modelcache.NewIntegrityVerifier()
		cachePath := filepath.Join(workerCacheDir, testModelName, testFormat)

		// QuickVerify should pass (metadata check only)
		valid, err := verifier.QuickVerify(ctx, cachePath)
		require.NoError(t, err)
		assert.True(t, valid, "Cache should pass quick verification")

		t.Logf("Quick verification passed for cached model")

		// Full verification should also pass
		result, err := verifier.VerifyModel(ctx, cachePath)
		require.NoError(t, err)
		assert.True(t, result.Valid)

		// Second call should use cache (faster)
		result2, err := verifier.VerifyModel(ctx, cachePath)
		require.NoError(t, err)
		assert.True(t, result2.Valid)
		// Can't reliably test timing, but cache is being used

		t.Logf("Full verification passed, cache working")
	})

	// ============================================================
	// STEP 5: Test corruption detection and re-cache
	// ============================================================
	t.Run("Step5_CorruptionDetectionAndReCache", func(t *testing.T) {
		verifier := modelcache.NewIntegrityVerifier()
		cachePath := filepath.Join(workerCacheDir, testModelName, testFormat)
		sharedPath := filepath.Join(workerSharedDir, testModelName, testFormat)
		cachedFilePath := filepath.Join(cachePath, testFileName)

		// Corrupt the cached file
		require.NoError(t, os.WriteFile(cachedFilePath, []byte("CORRUPTED DATA"), 0644))

		// Invalidate cache
		verifier.InvalidateCache(cachePath)

		// QuickVerify should fail (size mismatch)
		valid, err := verifier.QuickVerify(ctx, cachePath)
		require.NoError(t, err)
		assert.False(t, valid, "Corrupted cache should fail quick verification")

		t.Logf("Corruption detected via QuickVerify")

		// Full verification should also fail
		result, err := verifier.VerifyModel(ctx, cachePath)
		require.NoError(t, err)
		assert.False(t, result.Valid)
		assert.Contains(t, result.FilesCorrupted, testFileName)

		t.Logf("Corruption confirmed via full verification")

		// Re-cache from shared storage
		// First, remove corrupted cache
		require.NoError(t, os.RemoveAll(cachePath))

		// Copy again from shared storage
		copyResult, err := verifier.VerifyAndCopy(ctx, sharedPath, cachePath)
		require.NoError(t, err)
		assert.True(t, copyResult.Valid)

		t.Logf("Re-cached successfully from shared storage")
	})
}

// TestModelCacheE2E_TwoNodeSyncWithIntegrity tests the full flow:
// Coordinator download → Worker sync → Cache with integrity verification
func TestModelCacheE2E_TwoNodeSyncWithIntegrity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()

	// Create separate directories for each node
	coordinatorModelsDir := t.TempDir()
	workerModelsDir := t.TempDir()

	// Test model
	testModelName := "integrity-test/model"
	testFormat := "gguf"
	testModelContent := []byte("Model content for two-node sync with integrity verification test")

	hasher := sha256.New()
	hasher.Write(testModelContent)
	expectedChecksum := hex.EncodeToString(hasher.Sum(nil))

	// Setup coordinator with model and manifest
	modelDir := filepath.Join(coordinatorModelsDir, testModelName)
	require.NoError(t, os.MkdirAll(modelDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(modelDir, "model.gguf"), testModelContent, 0644))

	// Create integrity manifest on coordinator
	manifest, err := modelcache.CreateManifest(ctx, modelDir, testModelName, "")
	require.NoError(t, err)
	require.NoError(t, modelcache.WriteManifest(modelDir, manifest))

	// Create coordinator server
	coordinatorNode := createTestNodeWithModelsDir(t, TestNodeConfig{
		AdminKey:   TestAdminKey,
		ClusterKey: TestClusterKey,
	}, coordinatorModelsDir, "integrity-coordinator")

	coordinatorHTTPNode := httptest.NewServer(coordinatorNode.engine)
	defer closeHTTPTestServer(coordinatorHTTPNode)

	t.Logf("Coordinator URL: %s", coordinatorHTTPNode.URL)
	t.Logf("Model checksum: %s", expectedChecksum)

	// Test: Worker syncs model and verifies integrity
	t.Run("WorkerSyncWithIntegrityVerification", func(t *testing.T) {
		t.Skip("mTLS migration: sync now requires a clusternode-provided mTLS client; test needs a full-cluster harness to exercise the path")
		// Create sync client
		syncClient := modelsync.NewClient(modelsync.ClientConfig{})

		// Override models root for this test
		modelregistry.SetModelsRootDirOverride(workerModelsDir)
		defer modelregistry.ClearModelsRootDirOverride()

		// Perform full sync
		result, err := syncClient.SyncModel(
			ctx,
			coordinatorHTTPNode.URL,
			metadata.DownloadRequest{Repo: testModelName},
			testFormat,
			nil, // no progress callback
		)
		require.NoError(t, err)
		assert.True(t, result.Success)
		assert.Equal(t, 1, result.FilesDownloaded)
		assert.Equal(t, 0, result.FilesFailed)

		t.Logf("Sync completed: %d downloaded, %d skipped, %d failed",
			result.FilesDownloaded, result.FilesSkipped, result.FilesFailed)

		// Verify integrity manifest was created on worker
		workerModelDir := filepath.Join(workerModelsDir, testModelName)
		assert.True(t, modelcache.HasManifest(workerModelDir),
			"Integrity manifest should be created after sync")

		// Verify model integrity on worker
		verifier := modelcache.NewIntegrityVerifier()
		verifyResult, err := verifier.VerifyModel(ctx, workerModelDir)
		require.NoError(t, err)
		assert.True(t, verifyResult.Valid, "Synced model should pass integrity verification")
		assert.True(t, verifyResult.ManifestFound)
		assert.Empty(t, verifyResult.FilesCorrupted)
		assert.Empty(t, verifyResult.FilesMissing)

		t.Logf("Integrity verification passed: %d files checked", verifyResult.FilesChecked)
	})
}

// TestModelCacheE2E_ManifestTransfer tests that manifests are transferred during sync
func TestModelCacheE2E_ManifestTransfer(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()

	coordinatorModelsDir := t.TempDir()

	testModelName := "manifest-transfer/model"
	testFormat := "gguf"

	// Create model with multiple files
	modelDir := filepath.Join(coordinatorModelsDir, testModelName)
	require.NoError(t, os.MkdirAll(modelDir, 0755))

	files := map[string][]byte{
		"model.gguf":    []byte("Main model weights"),
		"tokenizer.bin": []byte("Tokenizer data"),
		"config.json":   []byte(`{"model_type": "test"}`),
	}

	for name, content := range files {
		require.NoError(t, os.WriteFile(filepath.Join(modelDir, name), content, 0644))
	}

	// Create manifest on coordinator
	manifest, err := modelcache.CreateManifest(ctx, modelDir, testModelName, "")
	require.NoError(t, err)
	require.NoError(t, modelcache.WriteManifest(modelDir, manifest))

	assert.Len(t, manifest.Files, 3, "Manifest should have 3 files")

	// Create coordinator server
	coordinatorNode := createTestNodeWithModelsDir(t, TestNodeConfig{
		AdminKey:   TestAdminKey,
		ClusterKey: TestClusterKey,
	}, coordinatorModelsDir, "manifest-coordinator")

	coordinatorHTTPNode := httptest.NewServer(coordinatorNode.engine)
	defer closeHTTPTestServer(coordinatorHTTPNode)

	t.Skip("mTLS migration: sync now requires a clusternode-provided mTLS client; test needs a full-cluster harness")
	// Sync to worker
	syncClient := modelsync.NewClient(modelsync.ClientConfig{})

	workerModelsDir := t.TempDir()
	modelregistry.SetModelsRootDirOverride(workerModelsDir)
	defer modelregistry.ClearModelsRootDirOverride()

	result, err := syncClient.SyncModel(ctx, coordinatorHTTPNode.URL, metadata.DownloadRequest{Repo: testModelName}, testFormat, nil)
	require.NoError(t, err)
	assert.True(t, result.Success)
	assert.Equal(t, 3, result.FilesDownloaded)

	// Verify all files and manifest on worker
	workerModelDir := filepath.Join(workerModelsDir, testModelName)
	assert.True(t, modelcache.HasManifest(workerModelDir))

	verifier := modelcache.NewIntegrityVerifier()
	verifyResult, err := verifier.VerifyModel(ctx, workerModelDir)
	require.NoError(t, err)
	assert.True(t, verifyResult.Valid)
	assert.Equal(t, 3, verifyResult.FilesChecked)

	t.Logf("Multi-file model synced and verified: %d files", verifyResult.FilesChecked)
}

// TestModelCacheE2E_ContextCancellation tests that long operations can be cancelled
func TestModelCacheE2E_ContextCancellation(t *testing.T) {
	tmpDir := t.TempDir()
	modelDir := filepath.Join(tmpDir, "model")
	require.NoError(t, os.MkdirAll(modelDir, 0755))

	// Create a file
	require.NoError(t, os.WriteFile(filepath.Join(modelDir, "model.bin"), []byte("content"), 0644))

	// Create manifest
	manifest, err := modelcache.CreateManifest(context.Background(), modelDir, "test", "")
	require.NoError(t, err)
	require.NoError(t, modelcache.WriteManifest(modelDir, manifest))

	// Test cancellation during VerifyModel
	t.Run("VerifyModelCancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // Cancel immediately

		verifier := modelcache.NewIntegrityVerifier()
		_, err := verifier.VerifyModel(ctx, modelDir)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "context canceled")
	})

	// Test cancellation during CreateManifest
	t.Run("CreateManifestCancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		_, err := modelcache.CreateManifest(ctx, modelDir, "test", "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "context canceled")
	})
}

// TestModelCacheE2E_WorkerFirstUseFromShared tests the full flow:
// 1. Coordinator has a model (simulating download completion)
// 2. Worker has shared storage pointing to coordinator's models
// 3. Worker queries the model for the first time
// 4. resolveModelPathWithCache copies from shared → cache with integrity verification
//
// This tests the production scenario where:
// - Coordinator downloads models to shared storage (NFS/cluster FS)
// - Workers mount this shared storage read-only
// - On first model use, workers copy to local cache with integrity verification
func TestModelCacheE2E_WorkerFirstUseFromShared(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()

	// ============================================================
	// SETUP: Directory structure simulating production deployment
	// ============================================================

	// Coordinator's model storage (acts as shared storage source)
	coordinatorModelsDir := t.TempDir()

	// Worker's local cache (separate from shared)
	workerCacheDir := t.TempDir()

	// Worker's shared storage mount (in production, this would be same as coordinatorModelsDir via NFS)
	// For testing, we'll use symlink to simulate NFS mount
	workerSharedDir := t.TempDir()

	// Test model setup
	testModelName := "first-use-test/llama-7b"
	testFormat := "gguf"
	testFiles := map[string][]byte{
		"model.gguf":             []byte("This is the main model weights file content for testing first-use caching flow."),
		"tokenizer.json":         []byte(`{"type": "BPE", "vocab_size": 32000, "model_type": "llama"}`),
		"config.json":            []byte(`{"hidden_size": 4096, "num_attention_heads": 32, "num_hidden_layers": 32}`),
		"generation_config.json": []byte(`{"max_length": 2048, "temperature": 0.7}`),
	}

	t.Logf("Test setup:")
	t.Logf("  Coordinator models dir: %s", coordinatorModelsDir)
	t.Logf("  Worker shared dir: %s", workerSharedDir)
	t.Logf("  Worker cache dir: %s", workerCacheDir)
	t.Logf("  Model: %s/%s (%d files)", testModelName, testFormat, len(testFiles))

	// ============================================================
	// STEP 1: Simulate coordinator downloading model
	// ============================================================
	t.Run("Step1_CoordinatorDownloadsModel", func(t *testing.T) {
		modelDir := filepath.Join(coordinatorModelsDir, testModelName, testFormat)
		require.NoError(t, os.MkdirAll(modelDir, 0755))

		// Write all model files
		for name, content := range testFiles {
			require.NoError(t, os.WriteFile(filepath.Join(modelDir, name), content, 0644))
		}

		// Create integrity manifest (this happens after download in production)
		sourceURL := "https://huggingface.co/" + testModelName
		manifest, err := modelcache.CreateManifest(ctx, modelDir, testModelName, sourceURL)
		require.NoError(t, err)
		require.NoError(t, modelcache.WriteManifest(modelDir, manifest))

		// Verify
		assert.True(t, modelcache.HasManifest(modelDir))
		assert.Len(t, manifest.Files, len(testFiles))

		t.Logf("Coordinator: Downloaded model with %d files and created manifest", len(manifest.Files))
		for _, f := range manifest.Files {
			t.Logf("  - %s (SHA256: %s...)", f.RelativePath, f.SHA256[:16])
		}
	})

	// ============================================================
	// STEP 2: Simulate shared storage mount
	// ============================================================
	t.Run("Step2_SimulateSharedStorageMount", func(t *testing.T) {
		// In production, this would be an NFS mount pointing to coordinatorModelsDir
		// For testing, we copy the files (simulating the mount contents)
		srcDir := filepath.Join(coordinatorModelsDir, testModelName, testFormat)
		dstDir := filepath.Join(workerSharedDir, testModelName, testFormat)

		require.NoError(t, modelcache.CopyDir(srcDir, dstDir))

		// Copy the manifest
		srcManifest := filepath.Join(srcDir, modelcache.ManifestFileName)
		dstManifest := filepath.Join(dstDir, modelcache.ManifestFileName)
		manifestData, err := os.ReadFile(srcManifest)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(dstManifest, manifestData, 0640))

		// Verify shared storage
		assert.True(t, modelcache.HasManifest(dstDir))
		for name := range testFiles {
			assert.FileExists(t, filepath.Join(dstDir, name))
		}

		t.Logf("Worker: Shared storage mounted at %s", dstDir)
	})

	// ============================================================
	// STEP 3: Worker first use - triggers copy from shared to cache
	// ============================================================
	t.Run("Step3_WorkerFirstUseCopiesFromSharedToCache", func(t *testing.T) {
		sharedPath := filepath.Join(workerSharedDir, testModelName, testFormat)
		cachePath := filepath.Join(workerCacheDir, testModelName, testFormat)

		// Verify cache is empty before first use
		_, err := os.Stat(cachePath)
		assert.True(t, os.IsNotExist(err), "Cache should be empty before first use")

		// Simulate resolveModelPathWithCache behavior
		verifier := modelcache.NewIntegrityVerifier()

		// 1. Check if manifest exists in shared storage
		assert.True(t, modelcache.HasManifest(sharedPath), "Shared storage should have manifest")

		// 2. Verify shared storage integrity before copying
		sharedResult, err := verifier.VerifyModel(ctx, sharedPath)
		require.NoError(t, err)
		assert.True(t, sharedResult.Valid, "Shared storage should pass verification")
		t.Logf("Shared storage verification: %d files checked", sharedResult.FilesChecked)

		// 3. Copy with verification (zero-trust)
		copyResult, err := verifier.VerifyAndCopy(ctx, sharedPath, cachePath)
		require.NoError(t, err)
		assert.True(t, copyResult.Valid, "Copy to cache should succeed")
		t.Logf("Copied to cache: %d files verified", copyResult.FilesChecked)

		// 4. Verify cache now exists and is valid
		assert.True(t, modelcache.HasManifest(cachePath), "Cache should have manifest")

		cacheResult, err := verifier.VerifyModel(ctx, cachePath)
		require.NoError(t, err)
		assert.True(t, cacheResult.Valid, "Cache should pass verification")
		assert.Equal(t, len(testFiles), cacheResult.FilesChecked)

		t.Logf("Cache verification passed: %d files", cacheResult.FilesChecked)
	})

	// ============================================================
	// STEP 4: Worker subsequent use - cache hit (no copy)
	// ============================================================
	t.Run("Step4_WorkerSubsequentUseCacheHit", func(t *testing.T) {
		cachePath := filepath.Join(workerCacheDir, testModelName, testFormat)
		verifier := modelcache.NewIntegrityVerifier()

		// QuickVerify should pass (fast path)
		valid, err := verifier.QuickVerify(ctx, cachePath)
		require.NoError(t, err)
		assert.True(t, valid, "Cache should pass quick verification")

		t.Logf("Subsequent use: QuickVerify passed (cache hit)")
	})

	// ============================================================
	// STEP 5: Corruption detection and automatic re-cache
	// ============================================================
	t.Run("Step5_CorruptionDetectionAndAutoReCache", func(t *testing.T) {
		cachePath := filepath.Join(workerCacheDir, testModelName, testFormat)
		sharedPath := filepath.Join(workerSharedDir, testModelName, testFormat)
		verifier := modelcache.NewIntegrityVerifier()

		// Corrupt a file in cache
		corruptedFile := filepath.Join(cachePath, "model.gguf")
		require.NoError(t, os.WriteFile(corruptedFile, []byte("CORRUPTED BY DISK ERROR"), 0644))

		// Invalidate cache entry
		verifier.InvalidateCache(cachePath)

		// QuickVerify should fail
		valid, err := verifier.QuickVerify(ctx, cachePath)
		require.NoError(t, err)
		assert.False(t, valid, "Corrupted cache should fail quick verification")

		t.Logf("Corruption detected in cache")

		// Full verification should identify the corrupted file
		result, err := verifier.VerifyModel(ctx, cachePath)
		require.NoError(t, err)
		assert.False(t, result.Valid)
		assert.Contains(t, result.FilesCorrupted, "model.gguf")

		t.Logf("Corrupted file identified: %v", result.FilesCorrupted)

		// Simulate automatic re-cache from shared storage
		require.NoError(t, os.RemoveAll(cachePath))
		copyResult, err := verifier.VerifyAndCopy(ctx, sharedPath, cachePath)
		require.NoError(t, err)
		assert.True(t, copyResult.Valid)

		t.Logf("Re-cached from shared storage: %d files", copyResult.FilesChecked)
	})

	// ============================================================
	// STEP 6: Verify final state
	// ============================================================
	t.Run("Step6_VerifyFinalState", func(t *testing.T) {
		cachePath := filepath.Join(workerCacheDir, testModelName, testFormat)
		verifier := modelcache.NewIntegrityVerifier()

		// All files should exist and be valid
		for name, expectedContent := range testFiles {
			filePath := filepath.Join(cachePath, name)
			assert.FileExists(t, filePath)

			actualContent, err := os.ReadFile(filePath)
			require.NoError(t, err)
			assert.Equal(t, expectedContent, actualContent, "File %s content mismatch", name)
		}

		// Final integrity check
		result, err := verifier.VerifyModel(ctx, cachePath)
		require.NoError(t, err)
		assert.True(t, result.Valid)
		assert.Equal(t, len(testFiles), result.FilesChecked)
		assert.Empty(t, result.FilesCorrupted)
		assert.Empty(t, result.FilesMissing)

		t.Logf("Final state verified: %d files, all valid", result.FilesChecked)
	})
}

// TestModelCacheE2E_ServerResolveModelPathWithCache tests the actual server method
// resolveModelPathWithCache which is called during model loading.
func TestModelCacheE2E_ServerResolveModelPathWithCache(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()

	// Setup directories
	sharedDir := t.TempDir()
	cacheDir := t.TempDir()

	testModelName := "resolve-test/model"
	testFormat := "gguf"
	testContent := []byte("Test model content for resolve path testing")

	// Create model in shared storage with manifest
	sharedModelDir := filepath.Join(sharedDir, testModelName, testFormat)
	require.NoError(t, os.MkdirAll(sharedModelDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(sharedModelDir, "model.gguf"), testContent, 0644))

	manifest, err := modelcache.CreateManifest(ctx, sharedModelDir, testModelName, "")
	require.NoError(t, err)
	require.NoError(t, modelcache.WriteManifest(sharedModelDir, manifest))

	t.Logf("Created model in shared storage: %s", sharedModelDir)

	// Create test server with shared storage configuration
	server := createTestNodeWithSharedStorage(t, TestNodeConfig{
		AdminKey:   TestAdminKey,
		ClusterKey: TestClusterKey,
	}, cacheDir, sharedDir, "resolve-test-server")

	// Create model metadata pointing to shared storage
	modelFullPath := filepath.Join(sharedModelDir, "model.gguf")
	model := &metadata.ModelMetadata{
		Name:           testModelName,
		FullPath:       modelFullPath,
		DownloadedFrom: "https://huggingface.co/" + testModelName,
	}

	t.Run("FirstCallCopiesFromSharedToCache", func(t *testing.T) {
		cachePath := filepath.Join(cacheDir, testModelName, testFormat)

		// Verify cache is empty
		_, err := os.Stat(cachePath)
		assert.True(t, os.IsNotExist(err), "Cache should be empty before resolve")

		// Call resolveModelPathWithCache
		// Note: This expects the model directory, not the file path
		model.FullPath = sharedModelDir
		resolvedPath, err := server.resolveModelPathWithCache(context.Background(), model)
		require.NoError(t, err)

		// Should return cache path (not shared path)
		assert.Contains(t, resolvedPath, cacheDir, "Resolved path should be in cache directory")
		t.Logf("Resolved path: %s", resolvedPath)

		// Verify cache was populated with integrity verification
		assert.True(t, modelcache.HasManifest(resolvedPath), "Cache should have manifest")
	})

	t.Run("SecondCallUsesCacheDirectly", func(t *testing.T) {
		cachePath := filepath.Join(cacheDir, testModelName, testFormat)

		// Cache should now exist
		assert.DirExists(t, cachePath)

		// Second call should use cache (fast path)
		model.FullPath = sharedModelDir
		resolvedPath, err := server.resolveModelPathWithCache(context.Background(), model)
		require.NoError(t, err)

		assert.Contains(t, resolvedPath, cacheDir)
		t.Logf("Second resolve used cache: %s", resolvedPath)
	})
}

func TestModelCacheE2E_CancelledRequestStillPreparesSharedCache(t *testing.T) {
	sharedDir, cacheDir := t.TempDir(), t.TempDir()
	server := createTestNodeWithSharedStorage(t, TestNodeConfig{
		AdminKey: TestAdminKey, ClusterKey: TestClusterKey,
	}, cacheDir, sharedDir, "cancelled-cache-test")

	for _, name := range []string{"verified-source", "plain-source", "cached-manifest"} {
		t.Run(name, func(t *testing.T) {
			sharedPath := filepath.Join(sharedDir, name)
			cachePath := filepath.Join(cacheDir, name)
			fixturePath := sharedPath
			if name == "cached-manifest" {
				fixturePath = cachePath
			}
			content := []byte("synthetic shared-cache model weights")
			require.NoError(t, os.MkdirAll(fixturePath, 0755))
			require.NoError(t, os.WriteFile(filepath.Join(fixturePath, "model.gguf"), content, 0644))
			if name != "plain-source" {
				manifest, err := modelcache.CreateManifest(context.Background(), fixturePath, name, "")
				require.NoError(t, err)
				require.NoError(t, modelcache.WriteManifest(fixturePath, manifest))
			}

			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			resolved, err := server.resolveModelPathWithCache(ctx, &metadata.ModelMetadata{
				Name: name, FullPath: sharedPath,
			})
			require.NoError(t, err)
			require.Equal(t, cachePath, resolved)
			data, err := os.ReadFile(filepath.Join(cachePath, "model.gguf"))
			require.NoError(t, err)
			require.Equal(t, content, data)
			require.True(t, modelcache.HasManifest(cachePath))
		})
	}
}

// createTestNodeWithSharedStorage creates a test server with separate shared and cache directories.
func createTestNodeWithSharedStorage(t *testing.T, cfg TestNodeConfig, cacheDir, sharedDir, serverName string) *Server {
	t.Helper()

	modelregistry.SetModelsRootDirOverride(cacheDir)
	t.Cleanup(func() {
		modelregistry.ClearModelsRootDirOverride()
	})

	server := createTestNode(t, cfg)
	server.config.Node.Name = serverName

	// Configure shared storage
	server.config.Models.Cache = cacheDir
	server.config.Models.Shared = sharedDir

	return server
}
