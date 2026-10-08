package process

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewPIDTracker(t *testing.T) {
	dir := t.TempDir()
	pidDir := filepath.Join(dir, "pids")

	pt, err := NewPIDTracker(pidDir)
	require.NoError(t, err)
	assert.NotNil(t, pt)

	// Directory should exist
	info, err := os.Stat(pidDir)
	require.NoError(t, err)
	assert.True(t, info.IsDir())
}

func TestNewPIDTracker_InvalidPath(t *testing.T) {
	// A regular file cannot be used as a parent directory on any platform.
	parent := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(parent, []byte("x"), 0600))
	_, err := NewPIDTracker(filepath.Join(parent, "impossible"))
	assert.Error(t, err)
}

func TestPIDTracker_TrackAndUntrack(t *testing.T) {
	pt, err := NewPIDTracker(t.TempDir())
	require.NoError(t, err)

	// Track a PID
	require.NoError(t, pt.Track("inst-1", 12345))

	// Should be listed
	tracked := pt.listTracked()
	assert.Equal(t, 12345, tracked["inst-1"])

	// Track another
	require.NoError(t, pt.Track("inst-2", 67890))
	tracked = pt.listTracked()
	assert.Len(t, tracked, 2)

	// Untrack first
	pt.Untrack("inst-1")
	tracked = pt.listTracked()
	assert.Len(t, tracked, 1)
	assert.Equal(t, 67890, tracked["inst-2"])

	// Untrack nonexistent — no error
	pt.Untrack("nonexistent")
}

func TestPIDTracker_TrackOverwrites(t *testing.T) {
	pt, err := NewPIDTracker(t.TempDir())
	require.NoError(t, err)

	require.NoError(t, pt.Track("inst-1", 100))
	require.NoError(t, pt.Track("inst-1", 200))

	tracked := pt.listTracked()
	assert.Equal(t, 200, tracked["inst-1"])
}

func TestParsePIDFile_PlainInteger(t *testing.T) {
	assert.Equal(t, 12345, parsePIDFile([]byte("12345")))
	assert.Equal(t, 12345, parsePIDFile([]byte("12345\n")))
	assert.Equal(t, 12345, parsePIDFile([]byte("  12345  ")))
}

func TestParsePIDFile_LauncherJSON(t *testing.T) {
	data, err := json.Marshal(launcherPIDFile{PID: 111, ChildPID: 222})
	require.NoError(t, err)

	assert.Equal(t, 111, parsePIDFile(data))
}

func TestParsePIDFile_LauncherJSON_FullFormat(t *testing.T) {
	// Match the exact format zzrouter-launcher writes
	data := []byte(`{
  "pid": 42,
  "child_pid": 99,
  "provider": "mlx",
  "instance_id": "mlx-abc123",
  "started_at": "2026-04-08T12:00:00Z"
}`)
	assert.Equal(t, 42, parsePIDFile(data))
}

func TestParsePIDFile_Invalid(t *testing.T) {
	assert.Equal(t, 0, parsePIDFile([]byte("")))
	assert.Equal(t, 0, parsePIDFile([]byte("not a number")))
	assert.Equal(t, 0, parsePIDFile([]byte("{}")))          // JSON with no pid
	assert.Equal(t, 0, parsePIDFile([]byte(`{"pid": 0}`)))  // zero PID
	assert.Equal(t, 0, parsePIDFile([]byte(`{"pid": -1}`))) // negative PID
}

func TestPIDTracker_ListTracked_MixedFormats(t *testing.T) {
	dir := t.TempDir()
	pt, err := NewPIDTracker(dir)
	require.NoError(t, err)

	// Write a plain PID file (PIDTracker format)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "plain.pid"), []byte("100"), 0640))

	// Write a JSON PID file (launcher format)
	jsonData, _ := json.Marshal(launcherPIDFile{PID: 200, ChildPID: 201})
	require.NoError(t, os.WriteFile(filepath.Join(dir, "launcher.pid"), jsonData, 0640))

	// Write an invalid file (should be skipped)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bad.pid"), []byte("garbage"), 0640))

	// Write a non-pid file (should be skipped)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("123"), 0640))

	tracked := pt.listTracked()
	assert.Len(t, tracked, 2)
	assert.Equal(t, 100, tracked["plain"])
	assert.Equal(t, 200, tracked["launcher"])
}

func TestPIDTracker_CleanupOrphans_StalePIDFile(t *testing.T) {
	dir := t.TempDir()
	pt, err := NewPIDTracker(dir)
	require.NoError(t, err)

	// Write a PID file for a process that doesn't exist (PID 999999999)
	require.NoError(t, pt.Track("dead-instance", 999999999))

	// Cleanup should remove the stale file (process doesn't exist)
	cleaned := pt.CleanupOrphans(nil)
	assert.Equal(t, 0, cleaned) // not "cleaned" — just removed stale file

	// File should be gone
	tracked := pt.listTracked()
	assert.Empty(t, tracked)
}

func TestPIDTracker_CleanupOrphans_SkipsActive(t *testing.T) {
	dir := t.TempDir()
	pt, err := NewPIDTracker(dir)
	require.NoError(t, err)

	// Track our own PID (definitely alive and has the marker... well, probably not,
	// but the point is testing that active instances are skipped)
	require.NoError(t, pt.Track("active-instance", os.Getpid()))

	// Mark it as active
	active := map[string]bool{"active-instance": true}
	cleaned := pt.CleanupOrphans(active)
	assert.Equal(t, 0, cleaned)

	// File should still exist
	tracked := pt.listTracked()
	assert.Contains(t, tracked, "active-instance")
}

func TestPIDTracker_CleanupOrphans_EmptyDir(t *testing.T) {
	pt, err := NewPIDTracker(t.TempDir())
	require.NoError(t, err)

	cleaned := pt.CleanupOrphans(nil)
	assert.Equal(t, 0, cleaned)
}
