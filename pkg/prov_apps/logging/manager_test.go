package logging

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestManager_CreateLogFile(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(ManagerConfig{
		BaseDir:     dir,
		MaxFileSize: 1024 * 1024,
		MaxAge:      7 * 24 * time.Hour,
		MaxBackups:  3,
	})

	f, path, err := m.CreateLogFile("test-instance", "vllm", "llama3")
	require.NoError(t, err)
	defer f.Close()

	assert.NotEmpty(t, path)
	assert.FileExists(t, path)
	assert.Contains(t, path, "test-instance.log")
}

func TestManager_GetLogPath(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(ManagerConfig{
		BaseDir:     dir,
		MaxFileSize: 1024 * 1024,
		MaxAge:      7 * 24 * time.Hour,
		MaxBackups:  3,
	})

	f, _, err := m.CreateLogFile("abc123def456", "vllm", "llama3")
	require.NoError(t, err)
	f.Close()

	path := m.GetLogPath("abc123def456")
	assert.NotEmpty(t, path)
	assert.Contains(t, path, "abc123def456")
}

func TestManager_Rotation(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(ManagerConfig{
		BaseDir:     dir,
		MaxFileSize: 100, // Very small for testing
		MaxAge:      7 * 24 * time.Hour,
		MaxBackups:  2,
	})

	// Create a file that exceeds MaxFileSize
	logPath := filepath.Join(dir, "test.log")
	err := os.WriteFile(logPath, make([]byte, 200), 0644)
	require.NoError(t, err)

	err = m.CheckRotation(logPath)
	require.NoError(t, err)

	// Original file should be gone (renamed)
	_, err = os.Stat(logPath)
	assert.True(t, os.IsNotExist(err))
}

func TestManager_ListLogFiles(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(ManagerConfig{
		BaseDir:     dir,
		MaxFileSize: 1024 * 1024,
		MaxAge:      7 * 24 * time.Hour,
		MaxBackups:  3,
	})

	// Create a few log files
	for _, name := range []string{"inst1.log", "inst2.log", "notlog.txt"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("data"), 0644))
	}

	files, err := m.ListLogFiles()
	require.NoError(t, err)
	assert.Len(t, files, 2) // .txt should be excluded
}

func TestManager_CleanupOldLogs(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(ManagerConfig{
		BaseDir:     dir,
		MaxFileSize: 1024 * 1024,
		MaxAge:      1 * time.Millisecond, // Expire immediately
		MaxBackups:  3,
	})

	require.NoError(t, os.WriteFile(filepath.Join(dir, "old.log"), []byte("data"), 0644))
	time.Sleep(5 * time.Millisecond) // Ensure file is older than MaxAge

	err := m.CleanupOldLogs()
	require.NoError(t, err)

	_, err = os.Stat(filepath.Join(dir, "old.log"))
	assert.True(t, os.IsNotExist(err))
}

func TestSanitizeForFilename(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"simple", "simple"},
		{"with/slash", "with-slash"},
		{"with:colon", "with-colon"},
		{"with space", "with-space"},
		{"multi---dash", "multi-dash"},
		{"", "unnamed"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			assert.Equal(t, tt.expected, sanitizeForFilename(tt.input))
		})
	}
}

func TestManager_RotateOversizedCopyTruncate(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(ManagerConfig{
		BaseDir:     dir,
		MaxFileSize: 1024, // 1KB for test
		MaxAge:      24 * time.Hour,
		MaxBackups:  3,
		Compression: true,
	})

	// Oversized provider-instance log
	big := filepath.Join(dir, "vllm-abc123.log")
	payload := bytes.Repeat([]byte("x"), 2048) // > MaxFileSize
	require.NoError(t, os.WriteFile(big, payload, 0o644))

	// Under-size instance log — should be left alone
	small := filepath.Join(dir, "vllm-small.log")
	require.NoError(t, os.WriteFile(small, []byte("tiny"), 0o644))

	// Excluded (node log prefix) — must not be touched even if oversized
	excluded := filepath.Join(dir, "zzrouter-node.log")
	require.NoError(t, os.WriteFile(excluded, bytes.Repeat([]byte("y"), 2048), 0o644))

	require.NoError(t, m.RotateOversizedCopyTruncate("zzrouter-"))

	// Original oversized file still exists but is truncated.
	info, err := os.Stat(big)
	require.NoError(t, err, "original path should still exist after copy-truncate")
	assert.Equal(t, int64(0), info.Size(), "original should be truncated to zero")

	// Rotated sibling present, gzipped, contains the original payload.
	rotated := filepath.Join(dir, "vllm-abc123.log.1.gz")
	require.FileExists(t, rotated)
	f, err := os.Open(rotated)
	require.NoError(t, err)
	defer f.Close()
	gz, err := gzip.NewReader(f)
	require.NoError(t, err)
	defer gz.Close()
	got, err := io.ReadAll(gz)
	require.NoError(t, err)
	assert.Equal(t, payload, got)

	// Under-size file unchanged.
	smallInfo, err := os.Stat(small)
	require.NoError(t, err)
	assert.Equal(t, int64(4), smallInfo.Size())

	// Excluded file unchanged.
	exInfo, err := os.Stat(excluded)
	require.NoError(t, err)
	assert.Equal(t, int64(2048), exInfo.Size(), "excluded prefix must be skipped")
}
