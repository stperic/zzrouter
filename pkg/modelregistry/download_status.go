package modelregistry

import "time"

// DownloadStatus tracks a single in-flight download. Emitted through
// the download-tracker callback and surfaced to the TUI / public status
// API. Source-agnostic — the HF and Ollama async-pull paths both feed
// DownloadTracker entries of this shape.
type DownloadStatus struct {
	ModelName       string    `json:"model_name"`
	RequestedFile   string    `json:"requested_file,omitempty"`   // Specific file/variant requested (e.g., Q4_K_M.gguf)
	Status          string    `json:"status"`                     // Clean state: starting, downloading, completed, failed, cancelled
	CurrentFile     string    `json:"current_file,omitempty"`     // Current file being downloaded
	FilesDownloaded int       `json:"files_downloaded,omitempty"` // Number of files completed
	FilesTotal      int       `json:"files_total,omitempty"`      // Total number of files
	Progress        float64   `json:"progress"`
	Downloaded      int64     `json:"downloaded"`
	TotalSize       int64     `json:"total_size"`
	Speed           int64     `json:"speed_bps"` // bytes per second
	ETA             time.Time `json:"eta"`
	StartedAt       time.Time `json:"started_at"`
	UpdatedAt       time.Time `json:"updated_at"`
	Message         string    `json:"message,omitempty"` // General status message
	Error           string    `json:"error,omitempty"`   // Error message (only for failed status)
	Generation      uint64    `json:"-"`                 // Internal generation counter; prevents stale goroutines from overwriting newer downloads
}
