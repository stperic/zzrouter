package update

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUpdateFlow_Integration tests the complete update flow:
// 1. Check for updates via GitHub API
// 2. Download the archive
// 3. Download and verify checksums
// 4. Authenticate the signed checksum bundle
// 5. Install the update
func TestUpdateFlow_Integration(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Skipping Unix-specific integration test on Windows")
	}

	// Setup test environment
	tmpDir := t.TempDir()
	downloadDir := filepath.Join(tmpDir, "downloads")
	backupDir := filepath.Join(tmpDir, "backups")
	require.NoError(t, os.MkdirAll(downloadDir, 0750))
	require.NoError(t, os.MkdirAll(backupDir, 0750))

	// Create a fake current binary
	currentBinary := filepath.Join(tmpDir, "zzrouter-node")
	originalContent := []byte("original binary v1.0.0")
	require.NoError(t, os.WriteFile(currentBinary, originalContent, 0750))

	// Create new binary content for the "update"
	newBinaryContent := fakeBinary("zzrouter-node", "99.99.99")

	// Compute checksum of the archive we'll create
	archiveContent := createTarGzContentForIntegration(t, map[string][]byte{
		"zzrouter-node": newBinaryContent,
	})
	archiveChecksum := computeSHA256ForIntegration(archiveContent)

	// Create checksums content and sign it
	assetName := fmt.Sprintf("zzrouter-%s-%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	checksumContent := fmt.Sprintf("%s  %s\n", archiveChecksum, assetName)
	checksumBytes := []byte(checksumContent)

	bundle, material := signedUpdateChecksums(t, checksumBytes, "99.99.99")

	// Create mock GitHub API server
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/test/test/releases" {
			releases := []githubRelease{
				{
					TagName:     "v99.99.99",
					Name:        "Test Release",
					PublishedAt: time.Now().Format(time.RFC3339),
					Prerelease:  false,
					Draft:       false,
					Assets: []githubAsset{
						{
							Name:               assetName,
							Size:               int64(len(archiveContent)),
							BrowserDownloadURL: "DOWNLOAD_URL_PLACEHOLDER",
						},
						{
							Name:               "checksums.txt",
							Size:               int64(len(checksumContent)),
							BrowserDownloadURL: "CHECKSUMS_URL_PLACEHOLDER",
						},
						{
							Name:               "checksums.txt.sigstore.json",
							Size:               int64(len(bundle)),
							BrowserDownloadURL: "BUNDLE_URL_PLACEHOLDER",
						},
					},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(releases)
			return
		}
		http.NotFound(w, r)
	}))
	defer apiServer.Close()

	// Create mock download server
	downloadServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/archive.tar.gz":
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(archiveContent)))
			_, _ = w.Write(archiveContent)
		case "/checksums.txt":
			_, _ = w.Write(checksumBytes)
		case "/checksums.txt.sigstore.json":
			_, _ = w.Write(bundle)
		default:
			http.NotFound(w, r)
		}
	}))
	defer downloadServer.Close()

	// Extract hostname for allowed hosts
	downloadHost := extractHostnameForIntegration(downloadServer.URL)

	// Step 1: Check for updates
	checker := NewChecker("stable", "")
	checker.SetAPIBaseURL(apiServer.URL)
	checker.owner = "test"
	checker.repo = "test"

	ctx := context.Background()
	checkResult, err := checker.Check(ctx)
	require.NoError(t, err)
	assert.True(t, checkResult.UpdateAvailable, "Update should be available")
	require.NotNil(t, checkResult.LatestRelease)
	assert.Equal(t, 99, checkResult.LatestRelease.Version.Major)

	// Step 2: Get assets for download
	release := checkResult.LatestRelease
	platformAsset := checker.GetAssetForPlatform(release)
	require.NotNil(t, platformAsset, "Platform asset should be found")

	checksumAsset := checker.GetChecksumAsset(release)
	require.NotNil(t, checksumAsset, "Checksum asset should be found")

	bundleAsset := checker.GetBundleAsset(release)
	require.NotNil(t, bundleAsset, "Signature bundle must be present")

	// Update download URLs to point to our mock server
	platformAsset.DownloadURL = downloadServer.URL + "/archive.tar.gz"
	checksumAsset.DownloadURL = downloadServer.URL + "/checksums.txt"
	bundleAsset.DownloadURL = downloadServer.URL + "/checksums.txt.sigstore.json"

	// Step 3: Download files
	downloader := NewDownloader(downloadDir)
	downloader.httpClient = downloadServer.Client()
	downloader.SetAllowedHosts([]string{downloadHost})

	// Download archive
	downloadResult, err := downloader.Download(ctx, platformAsset)
	require.NoError(t, err)
	assert.FileExists(t, downloadResult.FilePath)
	assert.Equal(t, archiveChecksum, downloadResult.Checksum)

	// Download checksums
	checksums, err := downloader.DownloadChecksums(ctx, checksumAsset)
	require.NoError(t, err)
	assert.Contains(t, checksums, platformAsset.Name)

	bundlePath, err := downloader.DownloadBundle(ctx, bundleAsset)
	require.NoError(t, err)

	// Authenticate the publisher, release tag and archive together.
	verifier := NewVerifier(DefaultOwner, DefaultRepo)
	verifier.trustedMaterial = material
	verified, err := verifier.Verify(downloadResult, bundlePath, release.Version.String())
	require.NoError(t, err)
	require.True(t, verified.Valid, verified.Error)
	require.True(t, verified.SignatureValid)
	require.True(t, verified.ChecksumValid)

	// Step 5: Install
	installer := NewInstaller(backupDir, 2)
	installer.SetCurrentExecutable(currentBinary)

	installResult, err := installer.Install(context.Background(), downloadResult.FilePath, nil)
	require.NoError(t, err)
	assert.True(t, installResult.Success)
	assert.NotEmpty(t, installResult.BackupPath)

	// Verify the new binary is installed
	installedContent, err := os.ReadFile(currentBinary)
	require.NoError(t, err)
	assert.Equal(t, newBinaryContent, installedContent)

	// Verify backup exists with original content
	backupContent, err := os.ReadFile(installResult.BackupPath)
	require.NoError(t, err)
	assert.Equal(t, originalContent, backupContent)

	t.Log("Integration test passed: Complete update flow works correctly")
}

// TestUpdateFlow_ChecksumMismatch tests that checksum verification catches bad downloads
func TestUpdateFlow_ChecksumMismatch(t *testing.T) {
	tmpDir := t.TempDir()
	downloadDir := filepath.Join(tmpDir, "downloads")
	require.NoError(t, os.MkdirAll(downloadDir, 0750))

	// Create archive with one content
	archiveContent := createTarGzContentForIntegration(t, map[string][]byte{
		"zzrouter-node": []byte("binary content"),
	})

	// But checksum for different content
	wrongChecksum := "0000000000000000000000000000000000000000000000000000000000000000"

	// Create mock download server
	downloadServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(archiveContent)
	}))
	defer downloadServer.Close()

	downloadHost := extractHostnameForIntegration(downloadServer.URL)

	// Download
	downloader := NewDownloader(downloadDir)
	downloader.httpClient = downloadServer.Client()
	downloader.SetAllowedHosts([]string{downloadHost})

	asset := &ReleaseAsset{
		Name:        "test.tar.gz",
		DownloadURL: downloadServer.URL + "/test.tar.gz",
	}

	ctx := context.Background()
	result, err := downloader.Download(ctx, asset)
	require.NoError(t, err)

	// Verify should fail
	verifier := NewVerifier("test", "test")
	checksums := map[string]string{
		"test.tar.gz": wrongChecksum,
	}

	err = verifier.VerifyChecksums(result.FilePath, checksums)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "checksum mismatch")
}

// TestUpdateFlow_RollbackAfterInstall tests rollback functionality
func TestUpdateFlow_RollbackAfterInstall(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Skipping Unix-specific test on Windows")
	}

	tmpDir := t.TempDir()
	backupDir := filepath.Join(tmpDir, "backups")
	require.NoError(t, os.MkdirAll(backupDir, 0750))

	// Create original binary
	currentBinary := filepath.Join(tmpDir, "zzrouter-node")
	originalContent := []byte("original v1.0.0")
	require.NoError(t, os.WriteFile(currentBinary, originalContent, 0750))

	// Create installer
	installer := NewInstaller(backupDir, 2)
	installer.SetCurrentExecutable(currentBinary)

	// Create and install first update
	archivePath := filepath.Join(tmpDir, "update1.tar.gz")
	update1Content := fakeBinary("zzrouter-node", "2.0.0")
	createTarGzFileForIntegration(t, archivePath, map[string][]byte{
		"zzrouter-node": update1Content,
	})

	result1, err := installer.Install(context.Background(), archivePath, nil)
	require.NoError(t, err)
	require.True(t, result1.Success)

	// Verify v2 is installed
	content, _ := os.ReadFile(currentBinary)
	assert.Equal(t, update1Content, content)

	// Create and install second update
	archivePath2 := filepath.Join(tmpDir, "update2.tar.gz")
	update2Content := fakeBinary("zzrouter-node", "3.0.0")
	createTarGzFileForIntegration(t, archivePath2, map[string][]byte{
		"zzrouter-node": update2Content,
	})

	result2, err := installer.Install(context.Background(), archivePath2, nil)
	require.NoError(t, err)
	require.True(t, result2.Success)

	// Verify v3 is installed
	content, err = os.ReadFile(currentBinary)
	require.NoError(t, err)
	assert.Equal(t, update2Content, content)

	// Rollback should work
	rollbackResult, err := installer.Rollback()
	require.NoError(t, err)
	assert.True(t, rollbackResult.Success)

	t.Log("Rollback test passed")
}

// Helper functions for integration tests

func computeSHA256ForIntegration(data []byte) string {
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func extractHostnameForIntegration(urlStr string) string {
	// Remove scheme
	result := urlStr
	if len(result) > 8 && result[:8] == "https://" {
		result = result[8:]
	} else if len(result) > 7 && result[:7] == "http://" {
		result = result[7:]
	}
	// Remove path
	for i, c := range result {
		if c == '/' {
			return result[:i]
		}
	}
	return result
}

func createTarGzContentForIntegration(t *testing.T, files map[string][]byte) []byte {
	t.Helper()

	tmpFile, err := os.CreateTemp("", "test-*.tar.gz")
	require.NoError(t, err)
	defer os.Remove(tmpFile.Name())
	tmpFile.Close()

	createTarGzFileForIntegration(t, tmpFile.Name(), files)

	content, err := os.ReadFile(tmpFile.Name())
	require.NoError(t, err)
	return content
}

func createTarGzFileForIntegration(t *testing.T, path string, files map[string][]byte) {
	t.Helper()

	outFile, err := os.Create(path)
	require.NoError(t, err)
	defer outFile.Close()

	gzWriter := gzip.NewWriter(outFile)
	defer gzWriter.Close()

	tarWriter := tar.NewWriter(gzWriter)
	defer tarWriter.Close()

	for name, content := range files {
		header := &tar.Header{
			Name: name,
			Mode: 0750,
			Size: int64(len(content)),
		}
		err := tarWriter.WriteHeader(header)
		require.NoError(t, err)

		_, err = tarWriter.Write(content)
		require.NoError(t, err)
	}
}
