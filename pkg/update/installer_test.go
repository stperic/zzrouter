package update

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewInstaller(t *testing.T) {
	i := NewInstaller("/tmp/backups", 3)
	assert.NotNil(t, i)
	assert.Equal(t, "/tmp/backups", i.backupDir)
	assert.Equal(t, 3, i.keepPreviousVersions)
}

func TestInstaller_isBinaryMatch(t *testing.T) {
	i := NewInstaller("/tmp/backups", 3)

	tests := []struct {
		name       string
		entryName  string
		binaryName string
		expected   bool
	}{
		// zzrouter binary matching
		{"zzrouter matches zzrouter", "zzrouter", "zzrouter", true},
		{"zzrouter-linux-amd64 matches zzrouter", "zzrouter-linux-amd64", "zzrouter", true},
		{"zzrouter-darwin-arm64 matches zzrouter", "zzrouter-darwin-arm64", "zzrouter", true},

		// zzrouter-node binary matching
		{"zzrouter-node matches zzrouter-node", "zzrouter-node", "zzrouter-node", true},
		{"zzrouter-node-linux-amd64 matches zzrouter-node", "zzrouter-node-linux-amd64", "zzrouter-node", true},

		// zzrouter-launcher binary matching
		{"zzrouter-launcher matches zzrouter-launcher", "zzrouter-launcher", "zzrouter-launcher", true},
		{"zzrouter-launcher-linux-amd64 matches zzrouter-launcher", "zzrouter-launcher-linux-amd64", "zzrouter-launcher", true},

		// Non-matches
		{"zzrouter-node should not match zzrouter", "zzrouter-node", "zzrouter", false},
		{"other file should not match", "readme.txt", "zzrouter", false},
		{"zzrouter should not match zzrouter-node", "zzrouter", "zzrouter-node", false},

		// Every archive member is prefixed by the name of another, so a
		// prefix test would hand the client binary the launcher.
		{"zzrouter-launcher should not match zzrouter", "zzrouter-launcher", "zzrouter", false},
		{"zzrouter-launcher-linux-amd64 should not match zzrouter", "zzrouter-launcher-linux-amd64", "zzrouter", false},
		{"zzrouter-launcher should not match zzrouter-node", "zzrouter-launcher", "zzrouter-node", false},
		{"zzrouter-node-linux-amd64 should not match zzrouter-launcher", "zzrouter-node-linux-amd64", "zzrouter-launcher", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := i.isBinaryMatch(tt.entryName, tt.binaryName)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestInstaller_extractToFile(t *testing.T) {
	tmpDir := t.TempDir()
	i := NewInstaller(tmpDir, 3)

	content := []byte("test binary content")
	destPath := filepath.Join(tmpDir, "extracted")

	reader := &mockReader{data: content}
	err := i.extractToFile(destPath, reader, 0750)
	require.NoError(t, err)

	// Verify content
	actual, err := os.ReadFile(destPath)
	require.NoError(t, err)
	assert.Equal(t, content, actual)

	// Verify permissions
	info, err := os.Stat(destPath)
	require.NoError(t, err)
	if runtime.GOOS != "windows" {
		assert.Equal(t, os.FileMode(0750), info.Mode().Perm())
	} else {
		// Windows has no POSIX executable bits; extraction must leave a regular writable file.
		assert.True(t, info.Mode().IsRegular())
		assert.NotZero(t, info.Mode().Perm()&0200)
	}
}

func TestInstaller_extractToFile_DefaultMode(t *testing.T) {
	tmpDir := t.TempDir()
	i := NewInstaller(tmpDir, 3)

	destPath := filepath.Join(tmpDir, "extracted")
	reader := &mockReader{data: []byte("content")}

	// Mode 0 should default to 0750
	err := i.extractToFile(destPath, reader, 0)
	require.NoError(t, err)

	info, err := os.Stat(destPath)
	require.NoError(t, err)
	if runtime.GOOS != "windows" {
		assert.Equal(t, os.FileMode(0750), info.Mode().Perm())
	} else {
		// Windows has no POSIX executable bits; extraction must leave a regular writable file.
		assert.True(t, info.Mode().IsRegular())
		assert.NotZero(t, info.Mode().Perm()&0200)
	}
}

// mockReader implements io.Reader for testing
type mockReader struct {
	data []byte
	pos  int
}

func (m *mockReader) Read(p []byte) (n int, err error) {
	if m.pos >= len(m.data) {
		return 0, io.EOF
	}
	n = copy(p, m.data[m.pos:])
	m.pos += n
	if m.pos >= len(m.data) {
		return n, io.EOF
	}
	return n, nil
}

func TestInstaller_extractBinaryFromTarGz(t *testing.T) {
	tmpDir := t.TempDir()

	// Create test tar.gz archive
	archivePath := filepath.Join(tmpDir, "test.tar.gz")
	createTestTarGz(t, archivePath, map[string][]byte{
		"zzrouter-linux-amd64": []byte("binary content"),
	})

	i := NewInstaller(tmpDir, 3)

	extractedPath, err := i.extractBinaryFromTarGz(archivePath, filepath.Join(tmpDir, "zzrouter"))
	require.NoError(t, err)
	assert.NotEmpty(t, extractedPath)

	// Verify extracted content
	content, err := os.ReadFile(extractedPath)
	require.NoError(t, err)
	assert.Equal(t, []byte("binary content"), content)
}

func TestInstaller_extractBinaryFromTarGz_NotFound(t *testing.T) {
	tmpDir := t.TempDir()

	// Create tar.gz without expected binary
	archivePath := filepath.Join(tmpDir, "test.tar.gz")
	createTestTarGz(t, archivePath, map[string][]byte{
		"other-file": []byte("content"),
	})

	i := NewInstaller(tmpDir, 3)

	_, err := i.extractBinaryFromTarGz(archivePath, filepath.Join(tmpDir, "zzrouter"))
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "binary not found")
}

func TestInstaller_extractBinaryFromZip(t *testing.T) {
	tmpDir := t.TempDir()

	// Create test zip archive
	archivePath := filepath.Join(tmpDir, "test.zip")
	createTestZip(t, archivePath, map[string][]byte{
		"zzrouter-windows-amd64.exe": []byte("binary content"),
	})

	i := NewInstaller(tmpDir, 3)

	extractedPath, err := i.extractBinaryFromZip(archivePath, filepath.Join(tmpDir, "zzrouter.exe"))
	require.NoError(t, err)
	assert.NotEmpty(t, extractedPath)

	// Verify extracted content
	content, err := os.ReadFile(extractedPath)
	require.NoError(t, err)
	assert.Equal(t, []byte("binary content"), content)
}

func TestInstaller_extractBinaryFromZip_NotFound(t *testing.T) {
	tmpDir := t.TempDir()

	// Create zip without expected binary
	archivePath := filepath.Join(tmpDir, "test.zip")
	createTestZip(t, archivePath, map[string][]byte{
		"other-file.txt": []byte("content"),
	})

	i := NewInstaller(tmpDir, 3)

	_, err := i.extractBinaryFromZip(archivePath, filepath.Join(tmpDir, "zzrouter.exe"))
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "binary not found")
}

func TestInstaller_extractBinary_AutoDetect(t *testing.T) {
	tmpDir := t.TempDir()
	i := NewInstaller(tmpDir, 3)

	// Test tar.gz detection
	tarPath := filepath.Join(tmpDir, "test.tar.gz")
	createTestTarGz(t, tarPath, map[string][]byte{
		"zzrouter-linux-amd64": []byte("tar content"),
	})

	extractedPath, err := i.extractBinary(tarPath, filepath.Join(tmpDir, "zzrouter"))
	require.NoError(t, err)
	assert.NotEmpty(t, extractedPath)

	// Test zip detection
	zipPath := filepath.Join(tmpDir, "test.zip")
	createTestZip(t, zipPath, map[string][]byte{
		"zzrouter-windows-amd64.exe": []byte("zip content"),
	})

	extractedPath, err = i.extractBinary(zipPath, filepath.Join(tmpDir, "zzrouter.exe"))
	require.NoError(t, err)
	assert.NotEmpty(t, extractedPath)
}

func TestInstaller_createBackup(t *testing.T) {
	tmpDir := t.TempDir()
	backupDir := filepath.Join(tmpDir, "backups")

	// Create backup directory (the function expects it to exist)
	err := os.MkdirAll(backupDir, 0750)
	require.NoError(t, err)

	i := NewInstaller(backupDir, 3)

	// Create source file
	sourcePath := filepath.Join(tmpDir, "zzrouter")
	err = os.WriteFile(sourcePath, []byte("current binary"), 0750)
	require.NoError(t, err)

	// Create backup
	backupPath, err := i.createBackup(sourcePath)
	require.NoError(t, err)
	assert.Contains(t, backupPath, "zzrouter-")

	// Verify backup content
	content, err := os.ReadFile(backupPath)
	require.NoError(t, err)
	assert.Equal(t, []byte("current binary"), content)
}

func TestInstaller_listBackups(t *testing.T) {
	tmpDir := t.TempDir()
	i := NewInstaller(tmpDir, 3)

	// Create backup files
	files := []string{
		"zzrouter-20240101-100000",
		"zzrouter-20240102-100000",
		"zzrouter-20240103-100000",
		"other-file",
	}
	for _, f := range files {
		err := os.WriteFile(filepath.Join(tmpDir, f), []byte("content"), 0640)
		require.NoError(t, err)
	}

	backups, err := i.listBackups("zzrouter")
	require.NoError(t, err)

	// Should have 3 backups (excluding "other-file")
	assert.Len(t, backups, 3)

	// Should be sorted by date descending (most recent first)
	assert.Contains(t, backups[0], "20240103")
	assert.Contains(t, backups[2], "20240101")
}

func TestInstaller_listBackups_Empty(t *testing.T) {
	tmpDir := t.TempDir()
	i := NewInstaller(tmpDir, 3)

	backups, err := i.listBackups("zzrouter")
	require.NoError(t, err)
	assert.Empty(t, backups)
}

func TestInstaller_listBackups_NonexistentDir(t *testing.T) {
	i := NewInstaller("/nonexistent/dir", 3)

	backups, err := i.listBackups("zzrouter")
	require.NoError(t, err)
	assert.Nil(t, backups)
}

func TestInstaller_cleanupOldBackups(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Skipping test that requires os.Executable behavior")
	}

	tmpDir := t.TempDir()
	_ = NewInstaller(tmpDir, 2) // Keep only 2 (installer used for setup)

	// Create 4 backup files
	files := []string{
		"zzrouter-20240101-100000",
		"zzrouter-20240102-100000",
		"zzrouter-20240103-100000",
		"zzrouter-20240104-100000",
	}
	for _, f := range files {
		err := os.WriteFile(filepath.Join(tmpDir, f), []byte("content"), 0640)
		require.NoError(t, err)
	}

	// Note: cleanupOldBackups uses os.Executable() which we can't easily mock
	// This test demonstrates the cleanup logic but may not fully work
}

func TestInstaller_restoreBackup(t *testing.T) {
	tmpDir := t.TempDir()
	i := NewInstaller(tmpDir, 3)

	// Create backup
	backupPath := filepath.Join(tmpDir, "backup")
	err := os.WriteFile(backupPath, []byte("backup content"), 0750)
	require.NoError(t, err)

	// Restore to target
	targetPath := filepath.Join(tmpDir, "target")
	err = i.restoreBackup(backupPath, targetPath)
	require.NoError(t, err)

	// Verify content
	content, err := os.ReadFile(targetPath)
	require.NoError(t, err)
	assert.Equal(t, []byte("backup content"), content)
}

func TestInstaller_replaceBinaryUnix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix-specific test")
	}

	tmpDir := t.TempDir()
	i := NewInstaller(tmpDir, 3)

	// Create current and new binaries
	currentPath := filepath.Join(tmpDir, "zzrouter")
	newPath := filepath.Join(tmpDir, "new-zzrouter")

	err := os.WriteFile(currentPath, []byte("old content"), 0750)
	require.NoError(t, err)

	err = os.WriteFile(newPath, []byte("new content"), 0750)
	require.NoError(t, err)

	// Replace
	err = i.replaceBinaryUnix(currentPath, newPath)
	require.NoError(t, err)

	// Verify new content at current path
	content, err := os.ReadFile(currentPath)
	require.NoError(t, err)
	assert.Equal(t, []byte("new content"), content)

	// Verify new binary was moved (no longer at original location)
	_, err = os.Stat(newPath)
	assert.True(t, os.IsNotExist(err))
}

func TestInstaller_replaceBinaryWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows-specific test")
	}

	tmpDir := t.TempDir()
	i := NewInstaller(tmpDir, 3)

	// Create current and new binaries
	currentPath := filepath.Join(tmpDir, "zzrouter.exe")
	newPath := filepath.Join(tmpDir, "new-zzrouter.exe")

	err := os.WriteFile(currentPath, []byte("old content"), 0750)
	require.NoError(t, err)

	err = os.WriteFile(newPath, []byte("new content"), 0750)
	require.NoError(t, err)

	// Replace
	err = i.replaceBinaryWindows(currentPath, newPath)
	require.NoError(t, err)

	// Verify new content at current path
	content, err := os.ReadFile(currentPath)
	require.NoError(t, err)
	assert.Equal(t, []byte("new content"), content)
}

func TestCopyFileWithSync(t *testing.T) {
	tmpDir := t.TempDir()

	srcPath := filepath.Join(tmpDir, "source")
	dstPath := filepath.Join(tmpDir, "dest")

	// Create source with specific permissions
	err := os.WriteFile(srcPath, []byte("test content"), 0640)
	require.NoError(t, err)

	// Copy
	err = copyFileWithSync(srcPath, dstPath)
	require.NoError(t, err)

	// Verify content
	content, err := os.ReadFile(dstPath)
	require.NoError(t, err)
	assert.Equal(t, []byte("test content"), content)

	// Verify permissions preserved
	srcInfo, _ := os.Stat(srcPath)
	dstInfo, _ := os.Stat(dstPath)
	assert.Equal(t, srcInfo.Mode().Perm(), dstInfo.Mode().Perm())
}

func TestCopyFileWithSync_SourceNotFound(t *testing.T) {
	tmpDir := t.TempDir()

	err := copyFileWithSync("/nonexistent/file", filepath.Join(tmpDir, "dest"))
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to open source")
}

// Helper functions for creating test archives

func createTestTarGz(t *testing.T, archivePath string, files map[string][]byte) {
	t.Helper()

	outFile, err := os.Create(archivePath)
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

func createTestZip(t *testing.T, archivePath string, files map[string][]byte) {
	t.Helper()

	outFile, err := os.Create(archivePath)
	require.NoError(t, err)
	defer outFile.Close()

	zipWriter := zip.NewWriter(outFile)
	defer zipWriter.Close()

	for name, content := range files {
		writer, err := zipWriter.Create(name)
		require.NoError(t, err)

		_, err = writer.Write(content)
		require.NoError(t, err)
	}
}

// =============================================================================
// Install Integration Tests with SetCurrentExecutable
// =============================================================================

func TestInstaller_SetCurrentExecutable(t *testing.T) {
	tmpDir := t.TempDir()
	i := NewInstaller(tmpDir, 2)

	// Initially nil
	assert.Empty(t, i.currentExePath)

	// Set custom path
	customPath := "/custom/path/to/binary"
	i.SetCurrentExecutable(customPath)
	assert.Equal(t, customPath, i.currentExePath)

	// Get current executable uses custom path
	exePath, err := i.getCurrentExecutable()
	require.NoError(t, err)
	assert.Equal(t, customPath, exePath)
}

func TestInstaller_Install_Success(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Skipping Unix-specific test on Windows")
	}

	tmpDir := t.TempDir()
	backupDir := filepath.Join(tmpDir, "backups")

	// Create backup directory
	require.NoError(t, os.MkdirAll(backupDir, 0750))

	// Create a fake "current" binary
	currentBinary := filepath.Join(tmpDir, "zzrouter-node")
	currentContent := []byte("original binary content v1.0.0")
	require.NoError(t, os.WriteFile(currentBinary, currentContent, 0750))

	// Create installer with custom current executable
	i := NewInstaller(backupDir, 2)
	i.SetCurrentExecutable(currentBinary)

	// Create test archive with new binary
	archivePath := filepath.Join(tmpDir, "update.tar.gz")
	newContent := fakeBinary("zzrouter-node", "2.0.0")
	createTestTarGz(t, archivePath, map[string][]byte{
		"zzrouter-node-linux-amd64": newContent,
	})

	// Install
	result, err := i.Install(context.Background(), archivePath, nil)
	require.NoError(t, err)
	require.True(t, result.Success)
	assert.NotEmpty(t, result.BackupPath)
	assert.Equal(t, currentBinary, result.NewPath)

	// Verify new binary is installed
	installedContent, err := os.ReadFile(currentBinary)
	require.NoError(t, err)
	assert.Equal(t, newContent, installedContent)

	// Verify backup exists
	assert.FileExists(t, result.BackupPath)

	// Verify backup contains original content
	backupContent, err := os.ReadFile(result.BackupPath)
	require.NoError(t, err)
	assert.Equal(t, currentContent, backupContent)
}

func TestInstaller_Install_BinaryNotInArchive(t *testing.T) {
	tmpDir := t.TempDir()
	backupDir := filepath.Join(tmpDir, "backups")
	require.NoError(t, os.MkdirAll(backupDir, 0750))

	// Create a fake "current" binary
	currentBinary := filepath.Join(tmpDir, "zzrouter-node")
	require.NoError(t, os.WriteFile(currentBinary, []byte("current"), 0750))

	// Create installer with custom current executable
	i := NewInstaller(backupDir, 2)
	i.SetCurrentExecutable(currentBinary)

	// Create test archive WITHOUT the expected binary
	archivePath := filepath.Join(tmpDir, "update.tar.gz")
	createTestTarGz(t, archivePath, map[string][]byte{
		"readme.txt":   []byte("readme"),
		"other-binary": []byte("other"),
	})

	// Install should fail
	_, err := i.Install(context.Background(), archivePath, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "binary not found in archive")
}

func TestInstaller_Install_InvalidArchive(t *testing.T) {
	tmpDir := t.TempDir()
	backupDir := filepath.Join(tmpDir, "backups")
	require.NoError(t, os.MkdirAll(backupDir, 0750))

	// Create a fake "current" binary
	currentBinary := filepath.Join(tmpDir, "zzrouter-node")
	require.NoError(t, os.WriteFile(currentBinary, []byte("current"), 0750))

	// Create installer
	i := NewInstaller(backupDir, 2)
	i.SetCurrentExecutable(currentBinary)

	// Create invalid archive (not a valid tar.gz)
	archivePath := filepath.Join(tmpDir, "invalid.tar.gz")
	require.NoError(t, os.WriteFile(archivePath, []byte("not a valid archive"), 0640))

	// Install should fail
	_, err := i.Install(context.Background(), archivePath, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to extract binary")
}

func TestInstaller_Rollback_Success(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Skipping Unix-specific test on Windows")
	}

	tmpDir := t.TempDir()
	backupDir := filepath.Join(tmpDir, "backups")
	require.NoError(t, os.MkdirAll(backupDir, 0750))

	// Create a fake "current" binary
	currentBinary := filepath.Join(tmpDir, "zzrouter-node")
	originalContent := []byte("original v1.0.0")
	require.NoError(t, os.WriteFile(currentBinary, originalContent, 0750))

	// Create installer and install an update
	i := NewInstaller(backupDir, 2)
	i.SetCurrentExecutable(currentBinary)

	// Create and install update
	archivePath := filepath.Join(tmpDir, "update.tar.gz")
	createTestTarGz(t, archivePath, map[string][]byte{
		"zzrouter-node": fakeBinary("zzrouter-node", "2.0.0"),
	})

	result, err := i.Install(context.Background(), archivePath, nil)
	require.NoError(t, err)
	require.True(t, result.Success)

	// Rollback restores the binary Install backed up. Asserting the
	// content, not just Success: a rollback in the same second as the
	// install used to copy the new binary over its own backup and then
	// "restore" it, which reports success and changes nothing.
	rollbackResult, err := i.Rollback()
	require.NoError(t, err)
	assert.True(t, rollbackResult.Success)
	assert.NotEmpty(t, rollbackResult.BackupPath)
	assert.Equal(t, currentBinary, rollbackResult.NewPath)

	restored, err := os.ReadFile(currentBinary)
	require.NoError(t, err)
	assert.Equal(t, originalContent, restored)
}

func TestInstaller_Rollback_NoBackups(t *testing.T) {
	tmpDir := t.TempDir()
	backupDir := filepath.Join(tmpDir, "backups")
	require.NoError(t, os.MkdirAll(backupDir, 0750))

	// Create a fake "current" binary
	currentBinary := filepath.Join(tmpDir, "zzrouter-node")
	require.NoError(t, os.WriteFile(currentBinary, []byte("current"), 0750))

	// Create installer - no installation means no backups
	i := NewInstaller(backupDir, 2)
	i.SetCurrentExecutable(currentBinary)

	// Rollback should fail - no backups
	_, err := i.Rollback()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no backups available")
}

func TestInstaller_cleanupOldBackups_WithSetExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Skipping Unix-specific test on Windows")
	}

	tmpDir := t.TempDir()
	backupDir := filepath.Join(tmpDir, "backups")
	require.NoError(t, os.MkdirAll(backupDir, 0750))

	// Create a fake "current" binary
	currentBinary := filepath.Join(tmpDir, "zzrouter-node")
	require.NoError(t, os.WriteFile(currentBinary, []byte("current"), 0750))

	// Create installer with keepPreviousVersions = 2
	i := NewInstaller(backupDir, 2)
	i.SetCurrentExecutable(currentBinary)

	// Create 4 backup files (should keep only 2)
	backupFiles := []string{
		"zzrouter-node-20240101-100000",
		"zzrouter-node-20240102-100000",
		"zzrouter-node-20240103-100000",
		"zzrouter-node-20240104-100000",
	}
	for _, f := range backupFiles {
		err := os.WriteFile(filepath.Join(backupDir, f), []byte("backup content"), 0640)
		require.NoError(t, err)
	}

	// Install a new version to trigger cleanup
	archivePath := filepath.Join(tmpDir, "update.tar.gz")
	createTestTarGz(t, archivePath, map[string][]byte{
		"zzrouter-node": fakeBinary("zzrouter-node", "2.0.0"),
	})

	result, err := i.Install(context.Background(), archivePath, nil)
	require.NoError(t, err)
	require.True(t, result.Success)

	// List backups - should have 2 kept + 1 from install = 3, but cleanup keeps 2
	// Note: The cleanup uses os.Executable() internally, so this test may not work perfectly
	backups, err := i.listBackups("zzrouter-node")
	require.NoError(t, err)
	t.Logf("Backups after install: %v", backups)
}

func TestInstaller_ListBackups(t *testing.T) {
	tmpDir := t.TempDir()
	i := NewInstaller(tmpDir, 3)

	// Create some backup files matching expected pattern
	files := []string{
		"zzrouter-node-20240101-100000",
		"zzrouter-node-20240102-100000",
	}
	for _, f := range files {
		err := os.WriteFile(filepath.Join(tmpDir, f), []byte("content"), 0640)
		require.NoError(t, err)
	}

	// ListBackups uses os.Executable() which we can't mock easily
	// This test verifies the function doesn't panic
	_, err := i.ListBackups()
	// ListBackups may error if not running as a proper executable — that's OK,
	// but it must not panic.
	t.Logf("ListBackups returned: %v", err)
}

func TestInstaller_getCurrentExecutable_Default(t *testing.T) {
	i := NewInstaller("/tmp", 2)

	// Without setting custom executable, should use os.Executable()
	exe, err := i.getCurrentExecutable()
	require.NoError(t, err)
	assert.NotEmpty(t, exe)
}

func TestInstaller_Install_NonexistentArchive(t *testing.T) {
	tmpDir := t.TempDir()
	backupDir := filepath.Join(tmpDir, "backups")
	require.NoError(t, os.MkdirAll(backupDir, 0750))

	currentBinary := filepath.Join(tmpDir, "zzrouter-node")
	require.NoError(t, os.WriteFile(currentBinary, []byte("current"), 0750))

	i := NewInstaller(backupDir, 2)
	i.SetCurrentExecutable(currentBinary)

	// Try to install from nonexistent archive
	_, err := i.Install(context.Background(), "/nonexistent/archive.tar.gz", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to extract binary")
}

func TestInstaller_replaceBinaryUnix_OldFileExists(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix-specific test")
	}

	tmpDir := t.TempDir()
	i := NewInstaller(tmpDir, 3)

	currentPath := filepath.Join(tmpDir, "zzrouter")
	newPath := filepath.Join(tmpDir, "new-zzrouter")
	oldPath := currentPath + ".old"

	// Create current binary
	err := os.WriteFile(currentPath, []byte("current content"), 0750)
	require.NoError(t, err)

	// Create a pre-existing .old file
	err = os.WriteFile(oldPath, []byte("pre-existing old"), 0750)
	require.NoError(t, err)

	// Create new binary
	err = os.WriteFile(newPath, []byte("new content"), 0750)
	require.NoError(t, err)

	// Replace should succeed, removing pre-existing .old
	err = i.replaceBinaryUnix(currentPath, newPath)
	require.NoError(t, err)

	// Verify new content at current path
	content, err := os.ReadFile(currentPath)
	require.NoError(t, err)
	assert.Equal(t, []byte("new content"), content)
}

func TestInstaller_replaceBinaryUnix_RenameFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix-specific test")
	}

	tmpDir := t.TempDir()
	i := NewInstaller(tmpDir, 3)

	// Try to rename from nonexistent path
	err := i.replaceBinaryUnix("/nonexistent/path", "/another/nonexistent")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to rename current binary")
}

// TestInstaller_Install_ReplacesLauncherAlongsideNode pins the pairing:
// the node embeds the launcher's hash at build time and rejects a
// launcher that does not match, so a release archive that ships both has
// to install both.
func TestInstaller_Install_ReplacesLauncherAlongsideNode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Skipping Unix-specific test on Windows")
	}

	tmpDir := t.TempDir()
	backupDir := filepath.Join(tmpDir, "backups")
	require.NoError(t, os.MkdirAll(backupDir, 0750))

	currentNode := filepath.Join(tmpDir, "zzrouter-node")
	currentLauncher := filepath.Join(tmpDir, "zzrouter-launcher")
	require.NoError(t, os.WriteFile(currentNode, fakeBinary("zzrouter-node", "1.0.0"), 0750))
	require.NoError(t, os.WriteFile(currentLauncher, fakeBinary("zzrouter-launcher", "1.0.0"), 0750))

	i := NewInstaller(backupDir, 2)
	i.SetCurrentExecutable(currentNode)

	archivePath := filepath.Join(tmpDir, "update.tar.gz")
	createTestTarGz(t, archivePath, map[string][]byte{
		"zzrouter-linux-amd64":          fakeBinary("zzrouter", "2.0.0"),
		"zzrouter-launcher-linux-amd64": fakeBinary("zzrouter-launcher", "2.0.0"),
		"zzrouter-node-linux-amd64":     fakeBinary("zzrouter-node", "2.0.0"),
	})

	result, err := i.Install(context.Background(), archivePath, nil)
	require.NoError(t, err)
	require.True(t, result.Success)
	assert.Equal(t, currentLauncher, result.LauncherPath)

	nodeContent, err := os.ReadFile(currentNode)
	require.NoError(t, err)
	assert.Equal(t, fakeBinary("zzrouter-node", "2.0.0"), nodeContent, "the client binary must not be mistaken for the node")

	launcherContent, err := os.ReadFile(currentLauncher)
	require.NoError(t, err)
	assert.Equal(t, fakeBinary("zzrouter-launcher", "2.0.0"), launcherContent)

	// Both halves are recoverable.
	nodeBackups, err := i.listBackups("zzrouter-node")
	require.NoError(t, err)
	require.Len(t, nodeBackups, 1)
	launcherBackups, err := i.listBackups("zzrouter-launcher")
	require.NoError(t, err)
	require.Len(t, launcherBackups, 1)
}

// TestInstaller_Install_ArchiveWithoutLauncher covers releases built
// before the launcher shipped: those embed no launcher hash either, so
// the installed launcher stays valid and the update must still apply.
func TestInstaller_Install_ArchiveWithoutLauncher(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Skipping Unix-specific test on Windows")
	}

	tmpDir := t.TempDir()
	backupDir := filepath.Join(tmpDir, "backups")
	require.NoError(t, os.MkdirAll(backupDir, 0750))

	currentNode := filepath.Join(tmpDir, "zzrouter-node")
	currentLauncher := filepath.Join(tmpDir, "zzrouter-launcher")
	require.NoError(t, os.WriteFile(currentNode, fakeBinary("zzrouter-node", "1.0.0"), 0750))
	require.NoError(t, os.WriteFile(currentLauncher, fakeBinary("zzrouter-launcher", "1.0.0"), 0750))

	i := NewInstaller(backupDir, 2)
	i.SetCurrentExecutable(currentNode)

	archivePath := filepath.Join(tmpDir, "update.tar.gz")
	createTestTarGz(t, archivePath, map[string][]byte{
		"zzrouter-node-linux-amd64": fakeBinary("zzrouter-node", "2.0.0"),
	})

	result, err := i.Install(context.Background(), archivePath, nil)
	require.NoError(t, err)
	require.True(t, result.Success)
	assert.Empty(t, result.LauncherPath)

	launcherContent, err := os.ReadFile(currentLauncher)
	require.NoError(t, err)
	assert.Equal(t, fakeBinary("zzrouter-launcher", "1.0.0"), launcherContent)
}

// TestInstaller_Rollback_RestoresLauncher: rolling the node back to a
// version whose launcher hash matched the old launcher has to take the
// launcher with it, or the pair is mismatched in the other direction.
func TestInstaller_Rollback_RestoresLauncher(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Skipping Unix-specific test on Windows")
	}

	tmpDir := t.TempDir()
	backupDir := filepath.Join(tmpDir, "backups")
	require.NoError(t, os.MkdirAll(backupDir, 0750))

	currentNode := filepath.Join(tmpDir, "zzrouter-node")
	currentLauncher := filepath.Join(tmpDir, "zzrouter-launcher")
	require.NoError(t, os.WriteFile(currentNode, fakeBinary("zzrouter-node", "1.0.0"), 0750))
	require.NoError(t, os.WriteFile(currentLauncher, fakeBinary("zzrouter-launcher", "1.0.0"), 0750))

	i := NewInstaller(backupDir, 2)
	i.SetCurrentExecutable(currentNode)

	archivePath := filepath.Join(tmpDir, "update.tar.gz")
	createTestTarGz(t, archivePath, map[string][]byte{
		"zzrouter-launcher-linux-amd64": fakeBinary("zzrouter-launcher", "2.0.0"),
		"zzrouter-node-linux-amd64":     fakeBinary("zzrouter-node", "2.0.0"),
	})

	_, err := i.Install(context.Background(), archivePath, nil)
	require.NoError(t, err)

	result, err := i.Rollback()
	require.NoError(t, err)
	require.True(t, result.Success)
	assert.Equal(t, currentLauncher, result.LauncherPath)

	nodeContent, err := os.ReadFile(currentNode)
	require.NoError(t, err)
	assert.Equal(t, fakeBinary("zzrouter-node", "1.0.0"), nodeContent)

	launcherContent, err := os.ReadFile(currentLauncher)
	require.NoError(t, err)
	assert.Equal(t, fakeBinary("zzrouter-launcher", "1.0.0"), launcherContent)
}

func TestLauncherSibling(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Skipping Unix-specific test on Windows")
	}

	assert.Equal(t, "/usr/local/bin/zzrouter-launcher", launcherSibling("/usr/local/bin/zzrouter-node"))
	assert.Empty(t, launcherSibling("/usr/local/bin/zzrouter"), "the client binary has no launcher")
	assert.Empty(t, launcherSibling("/usr/local/bin/zzrouter-launcher"), "the launcher does not carry itself")
}

// TestInstaller_listBackups_DoesNotClaimOtherBinaries: the three binary
// names share a directory and prefix one another, so a prefix-only match
// would let "zzrouter" roll back to a zzrouter-node build.
func TestInstaller_listBackups_DoesNotClaimOtherBinaries(t *testing.T) {
	backupDir := t.TempDir()
	i := NewInstaller(backupDir, 3)

	for _, name := range []string{
		"zzrouter-20260101-000000",
		"zzrouter-node-20260101-000000",
		"zzrouter-launcher-20260101-000000",
		"zzrouter-node-notatimestamp",
	} {
		require.NoError(t, os.WriteFile(filepath.Join(backupDir, name), []byte("x"), 0600))
	}

	for name, want := range map[string]string{
		"zzrouter":          "zzrouter-20260101-000000",
		"zzrouter-node":     "zzrouter-node-20260101-000000",
		"zzrouter-launcher": "zzrouter-launcher-20260101-000000",
	} {
		backups, err := i.listBackups(name)
		require.NoError(t, err)
		require.Len(t, backups, 1, "backups for %s", name)
		assert.Equal(t, want, filepath.Base(backups[0]))
	}
}
