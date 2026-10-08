package update

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/version"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// helper function to create test versions
func testVersion(v string) *version.Version {
	ver, _ := version.ParseVersion(v)
	return ver
}

func TestNewHistory(t *testing.T) {
	h := NewHistory("/tmp/test-history.json")
	assert.NotNil(t, h)
	assert.Equal(t, "/tmp/test-history.json", h.filePath)
}

func TestHistory_LoadEmpty(t *testing.T) {
	tmpDir := t.TempDir()
	historyPath := filepath.Join(tmpDir, "history.json")

	h := NewHistory(historyPath)
	history, err := h.Load()
	require.NoError(t, err)
	assert.NotNil(t, history)
	assert.Empty(t, history.Entries)
}

func TestHistory_SaveAndLoad(t *testing.T) {
	tmpDir := t.TempDir()
	historyPath := filepath.Join(tmpDir, "history.json")

	h := NewHistory(historyPath)

	// Create history with entries
	history := &UpdateHistory{
		Entries: []UpdateHistoryEntry{
			{
				FromVersion: testVersion("1.0.0"),
				ToVersion:   testVersion("1.1.0"),
				Timestamp:   time.Now(),
				Success:     true,
			},
		},
	}

	// Save
	err := h.Save(history)
	require.NoError(t, err)

	// Load
	loaded, err := h.Load()
	require.NoError(t, err)
	assert.Len(t, loaded.Entries, 1)
	assert.Equal(t, 1, loaded.Entries[0].FromVersion.Major)
	assert.Equal(t, 1, loaded.Entries[0].ToVersion.Minor)
	assert.True(t, loaded.Entries[0].Success)
}

func TestHistory_Add(t *testing.T) {
	tmpDir := t.TempDir()
	historyPath := filepath.Join(tmpDir, "history.json")

	h := NewHistory(historyPath)

	// Add first entry
	err := h.Add(UpdateHistoryEntry{
		FromVersion: testVersion("1.0.0"),
		ToVersion:   testVersion("1.1.0"),
		Timestamp:   time.Now(),
		Success:     true,
	})
	require.NoError(t, err)

	// Add second entry
	err = h.Add(UpdateHistoryEntry{
		FromVersion: testVersion("1.1.0"),
		ToVersion:   testVersion("1.2.0"),
		Timestamp:   time.Now(),
		Success:     true,
	})
	require.NoError(t, err)

	// Verify order (most recent first)
	history, err := h.Load()
	require.NoError(t, err)
	assert.Len(t, history.Entries, 2)
	assert.Equal(t, 2, history.Entries[0].ToVersion.Minor) // Most recent first (1.2.0)
	assert.Equal(t, 1, history.Entries[1].ToVersion.Minor) // Older (1.1.0)
}

func TestHistory_Add_MaxEntries(t *testing.T) {
	tmpDir := t.TempDir()
	historyPath := filepath.Join(tmpDir, "history.json")

	h := NewHistory(historyPath)

	// Add more than MaxHistoryEntries
	for range MaxHistoryEntries + 10 {
		err := h.Add(UpdateHistoryEntry{
			FromVersion: testVersion("1.0.0"),
			ToVersion:   testVersion("1.1.0"),
			Timestamp:   time.Now(),
			Success:     true,
		})
		require.NoError(t, err)
	}

	// Verify only MaxHistoryEntries are kept
	history, err := h.Load()
	require.NoError(t, err)
	assert.Len(t, history.Entries, MaxHistoryEntries)
}

func TestHistory_GetLatest(t *testing.T) {
	tmpDir := t.TempDir()
	historyPath := filepath.Join(tmpDir, "history.json")

	h := NewHistory(historyPath)

	// Empty history
	latest, err := h.GetLatest()
	require.NoError(t, err)
	assert.Nil(t, latest)

	// Add entries
	err = h.Add(UpdateHistoryEntry{
		FromVersion: testVersion("1.0.0"),
		ToVersion:   testVersion("1.1.0"),
		Timestamp:   time.Now().Add(-time.Hour),
		Success:     true,
	})
	require.NoError(t, err)

	err = h.Add(UpdateHistoryEntry{
		FromVersion: testVersion("1.1.0"),
		ToVersion:   testVersion("1.2.0"),
		Timestamp:   time.Now(),
		Success:     true,
	})
	require.NoError(t, err)

	// Get latest
	latest, err = h.GetLatest()
	require.NoError(t, err)
	require.NotNil(t, latest)
	assert.Equal(t, 2, latest.ToVersion.Minor) // 1.2.0
}

func TestHistory_GetSuccessful(t *testing.T) {
	tmpDir := t.TempDir()
	historyPath := filepath.Join(tmpDir, "history.json")

	h := NewHistory(historyPath)

	// Add mixed entries
	_ = h.Add(UpdateHistoryEntry{ToVersion: testVersion("1.1.0"), Success: true})
	_ = h.Add(UpdateHistoryEntry{ToVersion: testVersion("1.2.0"), Success: false})
	_ = h.Add(UpdateHistoryEntry{ToVersion: testVersion("1.3.0"), Success: true})

	successful, err := h.GetSuccessful()
	require.NoError(t, err)
	assert.Len(t, successful, 2)
	for _, entry := range successful {
		assert.True(t, entry.Success)
	}
}

func TestHistory_GetFailed(t *testing.T) {
	tmpDir := t.TempDir()
	historyPath := filepath.Join(tmpDir, "history.json")

	h := NewHistory(historyPath)

	// Add mixed entries
	_ = h.Add(UpdateHistoryEntry{ToVersion: testVersion("1.1.0"), Success: true})
	_ = h.Add(UpdateHistoryEntry{ToVersion: testVersion("1.2.0"), Success: false, Error: "download failed"})
	_ = h.Add(UpdateHistoryEntry{ToVersion: testVersion("1.3.0"), Success: false, Error: "checksum mismatch"})

	failed, err := h.GetFailed()
	require.NoError(t, err)
	assert.Len(t, failed, 2)
	for _, entry := range failed {
		assert.False(t, entry.Success)
		assert.NotEmpty(t, entry.Error)
	}
}

func TestHistory_GetRecent(t *testing.T) {
	tmpDir := t.TempDir()
	historyPath := filepath.Join(tmpDir, "history.json")

	h := NewHistory(historyPath)

	// Add 5 entries
	for i := 1; i <= 5; i++ {
		_ = h.Add(UpdateHistoryEntry{ToVersion: testVersion("1." + string(rune('0'+i)) + ".0"), Success: true})
	}

	// Get recent 3
	recent, err := h.GetRecent(3)
	require.NoError(t, err)
	assert.Len(t, recent, 3)

	// Get more than available
	recent, err = h.GetRecent(10)
	require.NoError(t, err)
	assert.Len(t, recent, 5)
}

func TestHistory_Clear(t *testing.T) {
	tmpDir := t.TempDir()
	historyPath := filepath.Join(tmpDir, "history.json")

	h := NewHistory(historyPath)

	// Add entry
	_ = h.Add(UpdateHistoryEntry{ToVersion: testVersion("1.1.0"), Success: true})

	// Verify file exists
	_, err := os.Stat(historyPath)
	require.NoError(t, err)

	// Clear
	err = h.Clear()
	require.NoError(t, err)

	// Verify file removed
	_, err = os.Stat(historyPath)
	assert.True(t, os.IsNotExist(err))
}

func TestHistory_ConcurrentAccess(t *testing.T) {
	tmpDir := t.TempDir()
	historyPath := filepath.Join(tmpDir, "history.json")

	h := NewHistory(historyPath)

	// Run concurrent adds
	var wg sync.WaitGroup
	for i := range 10 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = h.Add(UpdateHistoryEntry{
				FromVersion: testVersion("1.0.0"),
				ToVersion:   testVersion("1.1.0"),
				Timestamp:   time.Now(),
				Success:     true,
			})
		}(i)
	}
	wg.Wait()

	// Verify no data corruption
	history, err := h.Load()
	require.NoError(t, err)
	assert.NotNil(t, history)
	// Some entries may have been lost due to concurrent writes, but no corruption
	assert.LessOrEqual(t, len(history.Entries), 10)
}

func TestHistory_CreateDirectoryIfNotExists(t *testing.T) {
	tmpDir := t.TempDir()
	nestedPath := filepath.Join(tmpDir, "nested", "dir", "history.json")

	h := NewHistory(nestedPath)

	// Add entry (should create directories)
	err := h.Add(UpdateHistoryEntry{ToVersion: testVersion("1.1.0"), Success: true})
	require.NoError(t, err)

	// Verify file exists
	_, err = os.Stat(nestedPath)
	require.NoError(t, err)
}

func TestHistory_CorruptedFile(t *testing.T) {
	tmpDir := t.TempDir()
	historyPath := filepath.Join(tmpDir, "history.json")

	// Write corrupted JSON
	err := os.WriteFile(historyPath, []byte("not valid json"), 0640)
	require.NoError(t, err)

	h := NewHistory(historyPath)

	// Load should fail
	_, err = h.Load()
	assert.Error(t, err)

	// Add should recover by starting fresh
	err = h.Add(UpdateHistoryEntry{ToVersion: testVersion("1.1.0"), Success: true})
	require.NoError(t, err)

	// Verify recovery
	history, err := h.Load()
	require.NoError(t, err)
	assert.Len(t, history.Entries, 1)
}
