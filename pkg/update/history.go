package update

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/stperic/zzrouter/pkg/utils"
)

const (
	// MaxHistoryEntries is the maximum number of history entries to keep
	MaxHistoryEntries = 50
)

// History manages update history persistence
type History struct {
	filePath string
	mu       sync.RWMutex
}

// NewHistory creates a new history manager
func NewHistory(filePath string) *History {
	return &History{
		filePath: filePath,
	}
}

// Load loads update history from disk
func (h *History) Load() (*UpdateHistory, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	return h.loadLocked()
}

// loadLocked loads history without acquiring locks (caller must hold lock)
func (h *History) loadLocked() (*UpdateHistory, error) {
	history := &UpdateHistory{
		Entries:     []UpdateHistoryEntry{},
		LastUpdated: time.Time{},
	}

	data, err := os.ReadFile(h.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return history, nil
		}
		return nil, fmt.Errorf("failed to read history file: %w", err)
	}

	if err := json.Unmarshal(data, history); err != nil {
		return nil, fmt.Errorf("failed to parse history file: %w", err)
	}

	return history, nil
}

// Save saves update history to disk
func (h *History) Save(history *UpdateHistory) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.saveLocked(history)
}

// saveLocked saves history without acquiring locks (caller must hold write lock)
func (h *History) saveLocked(history *UpdateHistory) error {
	// Ensure directory exists
	dir := filepath.Dir(h.filePath)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return fmt.Errorf("failed to create history directory: %w", err)
	}

	history.LastUpdated = utils.NowUTC()

	data, err := json.MarshalIndent(history, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal history: %w", err)
	}

	if err := utils.AtomicWriteFile(h.filePath, data, 0o640); err != nil {
		return fmt.Errorf("save update history: %w", err)
	}
	return nil
}

// Add adds a new entry to the history
// This method holds the write lock for the entire load-modify-save operation
// to prevent race conditions.
func (h *History) Add(entry UpdateHistoryEntry) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	// Load current history (using internal method that doesn't acquire locks)
	history, err := h.loadLocked()
	if err != nil {
		// If we can't load, start fresh
		history = &UpdateHistory{
			Entries: []UpdateHistoryEntry{},
		}
	}

	// Prepend new entry (most recent first)
	history.Entries = append([]UpdateHistoryEntry{entry}, history.Entries...)

	// Trim to max entries
	if len(history.Entries) > MaxHistoryEntries {
		history.Entries = history.Entries[:MaxHistoryEntries]
	}

	// Save (using internal method that doesn't acquire locks)
	return h.saveLocked(history)
}

// GetLatest returns the most recent history entry (or nil if none)
func (h *History) GetLatest() (*UpdateHistoryEntry, error) {
	history, err := h.Load()
	if err != nil {
		return nil, err
	}

	if len(history.Entries) == 0 {
		return nil, nil
	}

	return &history.Entries[0], nil
}

// GetSuccessful returns only successful update entries
func (h *History) GetSuccessful() ([]UpdateHistoryEntry, error) {
	history, err := h.Load()
	if err != nil {
		return nil, err
	}

	var successful []UpdateHistoryEntry
	for _, entry := range history.Entries {
		if entry.Success {
			successful = append(successful, entry)
		}
	}

	return successful, nil
}

// GetFailed returns only failed update entries
func (h *History) GetFailed() ([]UpdateHistoryEntry, error) {
	history, err := h.Load()
	if err != nil {
		return nil, err
	}

	var failed []UpdateHistoryEntry
	for _, entry := range history.Entries {
		if !entry.Success {
			failed = append(failed, entry)
		}
	}

	return failed, nil
}

// Clear clears all history
func (h *History) Clear() error {
	h.mu.Lock()
	defer h.mu.Unlock()

	return os.Remove(h.filePath)
}

// GetRecent returns the most recent N entries
func (h *History) GetRecent(n int) ([]UpdateHistoryEntry, error) {
	history, err := h.Load()
	if err != nil {
		return nil, err
	}

	if n >= len(history.Entries) {
		return history.Entries, nil
	}

	return history.Entries[:n], nil
}
