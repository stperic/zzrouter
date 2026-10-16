package process

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

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
	require.NoError(t, pt.Track("inst-1", 12345, 6789))
	data, err := os.ReadFile(filepath.Join(pt.dir, "inst-1.pid"))
	require.NoError(t, err)
	assert.JSONEq(t, `{"pid":12345,"start_token":6789}`, string(data))

	// Should be listed
	tracked := pt.listTracked()
	assert.Equal(t, 12345, tracked["inst-1"].PID)
	assert.EqualValues(t, 6789, tracked["inst-1"].StartToken)

	// Track another
	require.NoError(t, pt.Track("inst-2", 67890, 0))
	tracked = pt.listTracked()
	assert.Len(t, tracked, 2)

	// Untrack first
	pt.Untrack("inst-1")
	tracked = pt.listTracked()
	assert.Len(t, tracked, 1)
	assert.Equal(t, 67890, tracked["inst-2"].PID)

	// Untrack nonexistent — no error
	pt.Untrack("nonexistent")
}

func TestPIDTracker_TrackOverwrites(t *testing.T) {
	pt, err := NewPIDTracker(t.TempDir())
	require.NoError(t, err)

	require.NoError(t, pt.Track("inst-1", 100, 0))
	require.NoError(t, pt.Track("inst-1", 200, 0))

	tracked := pt.listTracked()
	assert.Equal(t, 200, tracked["inst-1"].PID)
}

func TestParsePIDFile_PlainInteger(t *testing.T) {
	assert.Equal(t, 12345, parsePIDFile([]byte("12345")).PID)
	assert.Equal(t, 12345, parsePIDFile([]byte("12345\n")).PID)
	assert.Equal(t, 12345, parsePIDFile([]byte("  12345  ")).PID)
}

func TestParsePIDFile_LauncherJSON(t *testing.T) {
	data, err := json.Marshal(launcherPIDFile{PID: 111})
	require.NoError(t, err)

	assert.Equal(t, 111, parsePIDFile(data).PID)
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
	assert.Equal(t, 42, parsePIDFile(data).PID)
}

func TestParsePIDFile_Invalid(t *testing.T) {
	assert.Equal(t, 0, parsePIDFile([]byte("")).PID)
	assert.Equal(t, 0, parsePIDFile([]byte("not a number")).PID)
	assert.Equal(t, 0, parsePIDFile([]byte("{}")).PID)          // JSON with no pid
	assert.Equal(t, 0, parsePIDFile([]byte(`{"pid": 0}`)).PID)  // zero PID
	assert.Equal(t, 0, parsePIDFile([]byte(`{"pid": -1}`)).PID) // negative PID
}

func TestPIDTracker_ListTracked_MixedFormats(t *testing.T) {
	dir := t.TempDir()
	pt, err := NewPIDTracker(dir)
	require.NoError(t, err)

	// Write a plain PID file (PIDTracker format)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "plain.pid"), []byte("100"), 0640))

	// Write a JSON PID file (launcher format)
	jsonData, _ := json.Marshal(launcherPIDFile{PID: 200})
	require.NoError(t, os.WriteFile(filepath.Join(dir, "launcher.pid"), jsonData, 0640))

	// Write an invalid file (should be skipped)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bad.pid"), []byte("garbage"), 0640))

	// Write a non-pid file (should be skipped)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("123"), 0640))

	tracked := pt.listTracked()
	assert.Len(t, tracked, 2)
	assert.Equal(t, 100, tracked["plain"].PID)
	assert.Equal(t, 200, tracked["launcher"].PID)
}

func TestPIDTracker_CleanupOrphans_StalePIDFile(t *testing.T) {
	dir := t.TempDir()
	pt, err := NewPIDTracker(dir)
	require.NoError(t, err)

	// Write a PID file for a process that doesn't exist (PID 999999999)
	require.NoError(t, pt.Track("dead-instance", 999999999, 0))

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
	require.NoError(t, pt.Track("active-instance", os.Getpid(), 0))

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

func TestPIDTracker_CleanupOrphans_UnmarkedLivePID(t *testing.T) {
	pid, _, _, _ := identityChild(t)
	pt, err := NewPIDTracker(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(pt.dir, "legacy.pid"), []byte(strconv.Itoa(pid)), 0600))
	require.False(t, (Inspector{}).HasMarker(int32(pid)))
	assert.Zero(t, pt.CleanupOrphans(nil))
	assert.True(t, ProcessExists(int32(pid)), "an unmarked unrelated process must survive cleanup")
	assert.Contains(t, pt.listTracked(), "legacy")
}

func TestPIDTracker_CleanupOrphans_RecordedIdentity(t *testing.T) {
	for _, recycled := range []bool{false, true} {
		t.Run(strconv.FormatBool(recycled), func(t *testing.T) {
			pid, start, _, done := identityChild(t)
			pt, err := NewPIDTracker(t.TempDir())
			require.NoError(t, err)
			if recycled {
				start++
			}
			require.NoError(t, pt.Track("recorded", pid, start))
			assert.EqualValues(t, start, pt.listTracked()["recorded"].StartToken)
			if recycled {
				assert.Zero(t, pt.CleanupOrphans(nil))
				assert.True(t, ProcessExists(int32(pid)))
				assert.Equal(t, launcherPIDFile{PID: pid, StartToken: start}, pt.listTracked()["recorded"])
			} else {
				inspectDone := make(chan int, 1)
				go func() { inspectDone <- pt.CleanupOrphans(nil) }()
				select {
				case count := <-inspectDone:
					assert.Equal(t, 1, count)
				case <-time.After(12 * time.Second):
					t.Fatal("recorded orphan was not terminated")
				}
				<-done
				assert.Empty(t, pt.listTracked())
			}
		})
	}
}

func TestPIDTrackerCleanupRetainsTerminationError(t *testing.T) {
	for _, exited := range []bool{false, true} {
		t.Run(strconv.FormatBool(exited), func(t *testing.T) {
			pid, token, stdin, done := identityChild(t)
			pt, err := NewPIDTracker(t.TempDir())
			require.NoError(t, err)
			require.NoError(t, pt.Track("retry", pid, token))
			calls := 0
			assert.Zero(t, pt.cleanupOrphans(nil, func(_ context.Context, req TerminateRequest) error {
				calls++
				assert.Equal(t, pid, req.PID)
				assert.Equal(t, token, req.StartToken)
				if exited {
					assert.NoError(t, stdin.Close())
					<-done
				}
				return os.ErrPermission
			}))
			assert.Equal(t, 1, calls)
			assert.Equal(t, launcherPIDFile{PID: pid, StartToken: token}, pt.listTracked()["retry"])
			assert.Equal(t, !exited, ProcessExists(int32(pid)))
			want := 1
			if exited {
				want = 0 // Already gone on retry: discard the stale record without counting a termination.
			}
			assert.Equal(t, want, pt.CleanupOrphans(nil), "the retained record must be retried")
			<-done
			assert.Empty(t, pt.listTracked())
		})
	}
}
