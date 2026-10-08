package modelregistry

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stperic/zzrouter/pkg/utils"
)

// IsTerminalStatus returns true if the status represents a final state.
func IsTerminalStatus(status string) bool {
	return constants.Status(status).IsTerminal()
}

// DownloadTracker tracks ongoing downloads
type DownloadTracker struct {
	downloads          map[string]*DownloadStatus
	cancelFuncs        map[string]context.CancelFunc // Cancel functions for stopping downloads
	generations        map[string]uint64             // Generation counter per key; prevents stale goroutines from overwriting newer downloads
	mu                 sync.RWMutex
	onCompleteCallback func() // Callback fired when a download transitions to completed

	// Persistence fields
	stateFilePath string      // Path to downloads.json
	persistMu     sync.Mutex  // Separate lock for file writes (not the RWMutex)
	persistTimer  *time.Timer // Debounce timer for periodic saves
	persistOff    bool        // Set by Stop under persistMu: no write may follow the final one
}

// PersistedState is the JSON file format for download state persistence
type PersistedState struct {
	Version   int                           `json:"version"`
	Downloads map[string]*PersistedDownload `json:"downloads"`
}

// PersistedDownload contains the non-volatile fields of a download for persistence
type PersistedDownload struct {
	ModelName       string    `json:"model_name"`
	RequestedFile   string    `json:"requested_file,omitempty"`
	Status          string    `json:"status"`
	Progress        float64   `json:"progress"`
	TotalSize       int64     `json:"total_size"`
	FilesDownloaded int       `json:"files_downloaded,omitempty"`
	FilesTotal      int       `json:"files_total,omitempty"`
	StartedAt       time.Time `json:"started_at"`
	UpdatedAt       time.Time `json:"updated_at"`
	Error           string    `json:"error,omitempty"`
}

// NewDownloadTrackerWithPersistence creates a tracker and loads existing state from file.
// Any entries with status "downloading" or "pending" are marked as "failed" with error "interrupted (node restarted)".
func NewDownloadTrackerWithPersistence(stateFilePath string) *DownloadTracker {
	dt := &DownloadTracker{
		downloads:     make(map[string]*DownloadStatus),
		cancelFuncs:   make(map[string]context.CancelFunc),
		generations:   make(map[string]uint64),
		stateFilePath: stateFilePath,
	}
	dt.loadState()
	return dt
}

// SetOnCompleteCallback sets the callback fired when a download transitions
// to completed. The callback runs in its own goroutine (so the tracker lock
// is not held across arbitrary caller code) and is invoked once per
// completion event — no debounce. Consumers that need coalescing should
// do it on their side.
func (dt *DownloadTracker) SetOnCompleteCallback(callback func()) {
	dt.mu.Lock()
	defer dt.mu.Unlock()
	dt.onCompleteCallback = callback
}

// StartDownload registers a new download
// requestedFile is optional - if provided, indicates a specific file/variant to download (e.g., Q4_K_M.gguf)
// If a download with the same key already exists, it is cancelled and replaced.
// Returns the generation number for this download — callers should pass it to UpdateDownloadFull
// so that stale goroutines from a previous download cannot overwrite the new one.
func (dt *DownloadTracker) StartDownload(key, modelName string, requestedFile ...string) uint64 {
	dt.mu.Lock()

	// Cancel any existing download for this key (stop-then-repull scenario)
	if cancel, exists := dt.cancelFuncs[key]; exists {
		cancel()
		delete(dt.cancelFuncs, key)
	}

	// Increment generation so old goroutines' updates are rejected
	dt.generations[key]++
	gen := dt.generations[key]

	// For GGUF models, use the filename (without extension) as the display name for consistency with list command
	// This matches the format shown in "zzrouter list" which uses the actual filename
	displayModelName := modelName
	if len(requestedFile) > 0 && requestedFile[0] != "" {
		fileName := requestedFile[0]
		// Check if this is a GGUF file
		if strings.HasSuffix(strings.ToLower(fileName), ".gguf") {
			// Use filename without extension as display name (e.g., "Llama-3.2-3B-Instruct-Q4_K_M.gguf" -> "Llama-3.2-3B-Instruct-Q4_K_M")
			displayModelName = strings.TrimSuffix(filepath.Base(fileName), filepath.Ext(fileName))
		} else {
			// For non-GGUF files, extract quantization and append to model name (backward compatibility)
			quant := extractQuantizationFromFilename(fileName)
			if quant != "" {
				displayModelName = fmt.Sprintf("%s#%s", modelName, quant)
			}
		}
	}

	status := &DownloadStatus{
		ModelName:  displayModelName, // Store model name (filename without extension for GGUF, or org/model#quant for others)
		Status:     string(constants.StatusPending),
		Progress:   0,
		StartedAt:  utils.Now(),
		UpdatedAt:  utils.Now(),
		Generation: gen,
	}

	// Store requested file/variant if provided
	if len(requestedFile) > 0 && requestedFile[0] != "" {
		status.RequestedFile = requestedFile[0]
	}

	dt.downloads[key] = status
	dt.mu.Unlock()

	dt.schedulePersist(false)
	return gen
}

// extractQuantizationFromFilename extracts quantization method from
// a GGUF filename, delegating to the shared helper in metadata.
func extractQuantizationFromFilename(filename string) string {
	return metadata.ExtractQuantization(filename)
}

// UpdateDownloadFullGen updates download status with all fields and
// rejects the write if the generation doesn't match the current
// download's generation. The generation check prevents a stale
// goroutine (from a previous download that was stopped) from
// overwriting a newer download's state.
func (dt *DownloadTracker) UpdateDownloadFullGen(key string, gen uint64, status, currentFile string, filesDownloaded, filesTotal int, progress float64, downloaded, totalSize, speed int64) {
	dt.mu.Lock()

	isTerminal := false
	if download, exists := dt.downloads[key]; exists {
		// Reject writes from stale goroutines (generation mismatch)
		if download.Generation != gen {
			dt.mu.Unlock()
			return
		}

		// Don't overwrite terminal states (cancelled, failed, completed)
		if IsTerminalStatus(download.Status) {
			dt.mu.Unlock()
			return
		}

		wasCompleted := download.Status == string(constants.StatusCompleted)

		download.Status = status
		download.CurrentFile = currentFile
		download.FilesDownloaded = filesDownloaded
		download.FilesTotal = filesTotal
		download.Progress = progress
		download.Downloaded = downloaded
		download.TotalSize = totalSize
		download.Speed = speed
		download.UpdatedAt = utils.Now()

		// Calculate ETA
		if speed > 0 && totalSize > downloaded {
			remainingBytes := totalSize - downloaded
			etaSeconds := float64(remainingBytes) / float64(speed)
			download.ETA = utils.Now().Add(time.Duration(etaSeconds) * time.Second)
		}

		isTerminal = IsTerminalStatus(status)

		// Fire the completion callback on the completed-transition edge.
		// No debounce: the caller's cache-invalidation path must not be
		// gated on a timer, and two consecutive completions correctly
		// invalidate twice. Run in its own goroutine so the tracker lock
		// isn't held across callback code.
		if status == string(constants.StatusCompleted) && !wasCompleted && dt.onCompleteCallback != nil {
			cb := dt.onCompleteCallback
			go cb()
		}
	}

	dt.mu.Unlock()

	dt.schedulePersist(isTerminal)
}

// CompleteDownload marks a download as completed
func (dt *DownloadTracker) CompleteDownload(key string) {
	dt.mu.Lock()

	if download, exists := dt.downloads[key]; exists {
		download.Status = string(constants.StatusCompleted)
		download.Progress = 100
		download.UpdatedAt = utils.Now()
	}

	dt.mu.Unlock()

	dt.schedulePersist(true)
}

// FailDownload marks a download as failed
func (dt *DownloadTracker) FailDownload(key, errorMsg string) {
	dt.mu.Lock()

	if download, exists := dt.downloads[key]; exists {
		download.Status = string(constants.StatusFailed)
		download.Error = errorMsg
		download.UpdatedAt = utils.Now()
	}

	dt.mu.Unlock()

	dt.schedulePersist(true)
}

// DownloadEntry represents a download with its key
type DownloadEntry struct {
	Key    string         `json:"key"`
	Status DownloadStatus `json:"status"`
}

// GetStatus returns all download statuses sorted by start time (earliest first)
func (dt *DownloadTracker) GetStatus() []DownloadEntry {
	dt.mu.RLock()
	defer dt.mu.RUnlock()

	// Collect all downloads
	entries := make([]DownloadEntry, 0, len(dt.downloads))
	for key, status := range dt.downloads {
		entries = append(entries, DownloadEntry{
			Key:    key,
			Status: *status,
		})
	}

	// Sort by StartedAt (earliest first)
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Status.StartedAt.Before(entries[j].Status.StartedAt)
	})

	return entries
}

// GetDownloadStatus returns the status of a specific download, or nil if not found
func (dt *DownloadTracker) GetDownloadStatus(key string) *DownloadStatus {
	dt.mu.RLock()
	defer dt.mu.RUnlock()

	if download, exists := dt.downloads[key]; exists {
		copyStatus := *download
		return &copyStatus
	}
	return nil
}

// RemoveDownload removes a download from tracking
func (dt *DownloadTracker) RemoveDownload(key string) {
	dt.mu.Lock()

	delete(dt.downloads, key)

	dt.mu.Unlock()

	dt.schedulePersist(true)
}

// CleanCompleted removes completed, failed, and cancelled downloads
func (dt *DownloadTracker) CleanCompleted() {
	dt.mu.Lock()

	for key, status := range dt.downloads {
		if IsTerminalStatus(status.Status) {
			delete(dt.downloads, key)
			delete(dt.cancelFuncs, key)
		}
	}

	dt.mu.Unlock()

	dt.schedulePersist(true)
}

// RegisterCancelFunc stores the cancel function for a download
func (dt *DownloadTracker) RegisterCancelFunc(key string, cancel context.CancelFunc) {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	dt.cancelFuncs[key] = cancel
}

// StopDownload cancels a specific download
func (dt *DownloadTracker) StopDownload(key string) bool {
	dt.mu.Lock()

	if cancel, exists := dt.cancelFuncs[key]; exists {
		cancel()
		delete(dt.cancelFuncs, key)
		if download, ok := dt.downloads[key]; ok {
			download.Status = string(constants.StatusCancelled)
			download.UpdatedAt = utils.Now()
		}
		dt.mu.Unlock()
		dt.schedulePersist(true)
		return true
	}

	dt.mu.Unlock()
	return false
}

// StopAll cancels all ongoing downloads and returns the list of cancelled keys
func (dt *DownloadTracker) StopAll() []string {
	dt.mu.Lock()

	cancelledKeys := make([]string, 0, len(dt.cancelFuncs))
	for key, cancel := range dt.cancelFuncs {
		cancel()
		if download, ok := dt.downloads[key]; ok {
			download.Status = string(constants.StatusCancelled)
			download.UpdatedAt = utils.Now()
		}
		cancelledKeys = append(cancelledKeys, key)
	}

	// Clear all cancel functions
	dt.cancelFuncs = make(map[string]context.CancelFunc)

	dt.mu.Unlock()

	if len(cancelledKeys) > 0 {
		dt.schedulePersist(true)
	}

	return cancelledKeys
}

// ============================================================================
// Persistence
// ============================================================================

// schedulePersist schedules a persist operation. For terminal states, persists immediately.
// For progress updates, debounces to every 5 seconds.
// MUST be called AFTER releasing dt.mu.
func (dt *DownloadTracker) schedulePersist(immediate bool) {
	if dt.stateFilePath == "" {
		return
	}

	if immediate {
		go dt.persistAsync()
		return
	}

	// Debounced persist for non-terminal states — only schedule if no timer is pending.
	// If a timer is already running, let it fire naturally to avoid starvation
	// (frequent progress updates would otherwise keep resetting the timer indefinitely).
	dt.persistMu.Lock()
	if dt.persistTimer == nil {
		dt.persistTimer = time.AfterFunc(5*time.Second, func() {
			dt.persistMu.Lock()
			dt.persistTimer = nil
			dt.persistMu.Unlock()
			dt.persistAsync()
		})
	}
	dt.persistMu.Unlock()
}

// persistAsync takes a snapshot under RLock, then writes to disk under persistMu.
// A snapshot taken before Stop must not land after it: this goroutine is detached,
// so without the persistOff check it can win the lock after the final save and
// overwrite shutdown state with an older picture.
func (dt *DownloadTracker) persistAsync() {
	dt.mu.RLock()
	snapshot := dt.snapshotDownloads()
	dt.mu.RUnlock()

	dt.persistMu.Lock()
	defer dt.persistMu.Unlock()
	if dt.persistOff {
		return
	}
	atomicWriteState(dt.stateFilePath, snapshot)
}

// snapshotDownloads creates a copy of persisted fields from all downloads.
// Must be called under dt.mu.RLock().
func (dt *DownloadTracker) snapshotDownloads() map[string]*PersistedDownload {
	snapshot := make(map[string]*PersistedDownload, len(dt.downloads))
	for key, dl := range dt.downloads {
		snapshot[key] = &PersistedDownload{
			ModelName:       dl.ModelName,
			RequestedFile:   dl.RequestedFile,
			Status:          dl.Status,
			Progress:        dl.Progress,
			TotalSize:       dl.TotalSize,
			FilesDownloaded: dl.FilesDownloaded,
			FilesTotal:      dl.FilesTotal,
			StartedAt:       dl.StartedAt,
			UpdatedAt:       dl.UpdatedAt,
			Error:           dl.Error,
		}
	}
	return snapshot
}

// atomicWriteState writes download state to disk using atomic temp-file + rename.
func atomicWriteState(path string, downloads map[string]*PersistedDownload) {
	state := PersistedState{
		Version:   1,
		Downloads: downloads,
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		slog.Error("Failed to marshal download state", "error", err)
		return
	}

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		slog.Error("Failed to create download state directory", "error", err)
		return
	}

	if err := utils.AtomicWriteFile(path, data, 0o600); err != nil {
		slog.Error("Failed to write download state", "error", err, "path", path)
	}
}

// loadState reads the JSON state file and restores downloads.
// Entries with status "downloading" or "pending" are marked as "failed".
// Backward compat: old persisted values "starting" and "error" are normalized.
func (dt *DownloadTracker) loadState() {
	if dt.stateFilePath == "" {
		return
	}

	data, err := os.ReadFile(dt.stateFilePath)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Error("Failed to read download state file", "error", err, "path", dt.stateFilePath)
		}
		return
	}

	var state PersistedState
	if err := json.Unmarshal(data, &state); err != nil {
		slog.Error("Failed to unmarshal download state file", "error", err, "path", dt.stateFilePath)
		return
	}

	restored := 0
	interrupted := 0
	for key, pd := range state.Downloads {
		status := &DownloadStatus{
			ModelName:       pd.ModelName,
			RequestedFile:   pd.RequestedFile,
			Status:          pd.Status,
			Progress:        pd.Progress,
			TotalSize:       pd.TotalSize,
			FilesDownloaded: pd.FilesDownloaded,
			FilesTotal:      pd.FilesTotal,
			StartedAt:       pd.StartedAt,
			UpdatedAt:       pd.UpdatedAt,
			Error:           pd.Error,
		}

		// Backward compat: normalize old persisted values to new vocabulary.
		switch status.Status {
		case "starting":
			status.Status = string(constants.StatusPending)
		case "error":
			status.Status = string(constants.StatusFailed)
		}

		// Mark in-progress downloads as failed on restart
		if status.Status == string(constants.StatusDownloading) || status.Status == string(constants.StatusPending) {
			status.Status = string(constants.StatusFailed)
			status.Error = "interrupted (node restarted)"
			status.UpdatedAt = utils.Now()
			interrupted++
		}

		dt.downloads[key] = status
		restored++
	}

	if restored > 0 {
		slog.Info("Restored download state from disk", "restored", restored, "interrupted", interrupted, "path", dt.stateFilePath)
	}
}

// Stop stops the debounce timer and performs a final synchronous save.
// Should be called during graceful shutdown.
func (dt *DownloadTracker) Stop() {
	if dt == nil {
		return
	}
	if dt.stateFilePath == "" {
		return
	}

	// Stop debounce timer
	dt.persistMu.Lock()
	if dt.persistTimer != nil {
		dt.persistTimer.Stop()
		dt.persistTimer = nil
	}
	dt.persistMu.Unlock()

	// Final synchronous save
	dt.mu.RLock()
	snapshot := dt.snapshotDownloads()
	dt.mu.RUnlock()

	dt.persistMu.Lock()
	atomicWriteState(dt.stateFilePath, snapshot)
	dt.persistOff = true
	dt.persistMu.Unlock()

	slog.Info("Download state persisted on shutdown", "path", dt.stateFilePath)
}
