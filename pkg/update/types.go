// Package update provides auto-update functionality for zzrouter-node.
// It includes GitHub Releases API integration, binary verification,
// atomic replacement, and scheduled updates with maintenance windows.
package update

import (
	"time"

	"github.com/stperic/zzrouter/pkg/version"
)

// UpdateState represents the current state of the update system
type UpdateState string

const (
	// StateIdle means no update activity
	StateIdle UpdateState = "idle"

	// StateChecking means checking for updates
	StateChecking UpdateState = "checking"

	// StateDownloading means downloading an update
	StateDownloading UpdateState = "downloading"

	// StateVerifying means verifying the downloaded update
	StateVerifying UpdateState = "verifying"

	// StateWaitingForWindow means update is ready but waiting for maintenance window
	StateWaitingForWindow UpdateState = "waiting_for_window"

	// StateApplying means applying the update
	StateApplying UpdateState = "applying"

	// StateRestarting means restarting the service
	StateRestarting UpdateState = "restarting"

	// StateFailed means the last operation failed
	StateFailed UpdateState = "failed"
)

// ReleaseInfo represents information about a GitHub release
type ReleaseInfo struct {
	// Version is the release version (parsed from tag)
	Version *version.Version `json:"version"`

	// TagName is the raw tag name (e.g., "v1.2.3")
	TagName string `json:"tag_name"`

	// Name is the release title
	Name string `json:"name"`

	// Body is the release notes (markdown)
	Body string `json:"body"`

	// PublishedAt is when the release was published
	PublishedAt time.Time `json:"published_at"`

	// HTMLURL is the URL to the release page
	HTMLURL string `json:"html_url"`

	// Assets contains the release assets
	Assets []ReleaseAsset `json:"assets"`

	// Prerelease indicates if this is a pre-release
	Prerelease bool `json:"prerelease"`

	// Draft indicates if this is a draft release
	Draft bool `json:"draft"`
}

// ReleaseAsset represents a single file attached to a release
type ReleaseAsset struct {
	// Name is the filename
	Name string `json:"name"`

	// Size is the file size in bytes
	Size int64 `json:"size"`

	// DownloadURL is the URL to download this asset
	DownloadURL string `json:"browser_download_url"`

	// ContentType is the MIME type
	ContentType string `json:"content_type"`
}

// UpdateStatus represents the current status of the update system
type UpdateStatus struct {
	// Operation correlates acceptance and completion across node restarts.
	Operation *RunStatus `json:"operation,omitempty"`
	// Enabled controls scheduled checks, not explicit API updates.
	Enabled bool `json:"enabled"`
	// ConfirmationError prevents an unreadable record from implying success.
	ConfirmationError string `json:"confirmation_error,omitempty"`
	// State is the current update state
	State UpdateState `json:"state"`

	// CurrentVersion is the currently running version
	CurrentVersion *version.Version `json:"current_version"`

	// LatestVersion is the latest available version (nil if not checked)
	LatestVersion *version.Version `json:"latest_version,omitempty"`

	// PendingRelease is the release waiting to be applied (nil if none)
	PendingRelease *ReleaseInfo `json:"pending_release,omitempty"`

	// LastCheckTime is when we last checked for updates
	LastCheckTime *time.Time `json:"last_check_time,omitempty"`

	// NextCheckTime is when the next check is scheduled
	NextCheckTime *time.Time `json:"next_check_time,omitempty"`

	// LastUpdateTime is when an update was last applied
	LastUpdateTime *time.Time `json:"last_update_time,omitempty"`

	// Error contains the last error message (if State is StateFailed)
	Error string `json:"error,omitempty"`

	// DownloadProgress is the download progress (0-100) during StateDownloading
	DownloadProgress int `json:"download_progress,omitempty"`

	// Channel is the configured release channel
	Channel string `json:"channel"`

	// UpdateAvailable is true if an update is available
	UpdateAvailable bool `json:"update_available"`

	// MaintenanceWindowActive is true if we're in a maintenance window
	MaintenanceWindowActive bool `json:"maintenance_window_active,omitempty"`

	// RestartRequired is true when an update was installed but the
	// running process is still executing the previous binary. The new
	// version takes effect on the next restart.
	RestartRequired bool `json:"restart_required,omitempty"`

	// RestartRequiredReason explains why the node did not restart
	// itself — auto-restart turned off, or no supervisor that would
	// bring it back.
	RestartRequiredReason string `json:"restart_required_reason,omitempty"`

	// Source names the release feed when it is not the public one, so an
	// operator reading the status can tell a node that was pointed at a
	// test feed and left there.
	Source string `json:"source,omitempty"`

	// Blocked explains why this node could not install an update even if
	// one were available, so the answer arrives before a release does
	// rather than as a failed apply afterwards. Empty when nothing is in
	// the way.
	Blocked string `json:"blocked,omitempty"`

	// DelegatedTo is set when this node does not install its own
	// updates: the tree is root-owned and a privileged updater does the
	// work. The value is where a request is left for it. Distinct from
	// Blocked, which means nothing can install anything.
	DelegatedTo string `json:"delegated_to,omitempty"`

	// PrivilegedRun is what the privileged updater last did. The only
	// account of it there is -- that work happens in a process this one
	// cannot observe.
	PrivilegedRun *RunStatus `json:"privileged_run,omitempty"`

	// PendingConfirm is set while this node is running a version that
	// has not yet proven itself. It clears once the node answers its own
	// health endpoint; until then, a restart counts against the update
	// and enough failures roll it back.
	PendingConfirm *PendingConfirm `json:"pending_confirm,omitempty"`
}

// UpdateHistoryEntry represents a past update event
type UpdateHistoryEntry struct {
	// ID is a unique identifier for this entry
	ID string `json:"id"`

	// Timestamp is when this event occurred
	Timestamp time.Time `json:"timestamp"`

	// FromVersion is the version before the update
	FromVersion *version.Version `json:"from_version"`

	// ToVersion is the version after the update
	ToVersion *version.Version `json:"to_version"`

	// Success indicates if the update was successful
	Success bool `json:"success"`

	// Error contains the error message if Success is false
	Error string `json:"error,omitempty"`

	// Duration is how long the update took
	Duration time.Duration `json:"duration"`

	// BackupPath is where the previous binary was backed up
	BackupPath string `json:"backup_path,omitempty"`

	// Automatic indicates if this was an automatic or manual update
	Automatic bool `json:"automatic"`
}

// UpdateHistory contains the history of updates
type UpdateHistory struct {
	// Entries is a list of update events (most recent first)
	Entries []UpdateHistoryEntry `json:"entries"`

	// LastUpdated is when this history was last modified
	LastUpdated time.Time `json:"last_updated"`
}

// CheckResult represents the result of an update check
type CheckResult struct {
	// UpdateAvailable is true if a newer version is available
	UpdateAvailable bool `json:"update_available"`

	// CurrentVersion is the currently running version
	CurrentVersion *version.Version `json:"current_version"`

	// LatestRelease is the latest release info (nil if no update)
	LatestRelease *ReleaseInfo `json:"latest_release,omitempty"`

	// MatchesPin indicates if the latest release matches the version pin
	MatchesPin bool `json:"matches_pin"`

	// SkippedReason is why the update was skipped (if any)
	SkippedReason string `json:"skipped_reason,omitempty"`
}

// DownloadResult represents the result of downloading an update
type DownloadResult struct {
	// FilePath is where the downloaded file is stored
	FilePath string `json:"file_path"`

	// Checksum is the SHA256 hash of the downloaded file
	Checksum string `json:"checksum"`

	// Size is the file size in bytes
	Size int64 `json:"size"`

	// Duration is how long the download took
	Duration time.Duration `json:"duration"`

	// Resumed indicates if the download was resumed from a partial file
	Resumed bool `json:"resumed"`

	// ResumedFromBytes is the byte offset from which download was resumed (0 if not resumed)
	ResumedFromBytes int64 `json:"resumed_from_bytes,omitempty"`
}

// VerifyResult represents the result of verifying an update
type VerifyResult struct {
	// Valid is true if verification passed
	Valid bool `json:"valid"`

	// ChecksumValid is true if the checksum matched
	ChecksumValid bool `json:"checksum_valid"`

	// SignatureValid is true if the Sigstore signature was valid
	SignatureValid bool `json:"signature_valid"`

	// Error contains the verification error (if Valid is false)
	Error string `json:"error,omitempty"`
}

// InstallResult represents the result of installing an update
type InstallResult struct {
	// Success is true if installation succeeded
	Success bool `json:"success"`

	// BackupPath is where the old binary was backed up
	BackupPath string `json:"backup_path"`

	// NewPath is where the new binary was installed
	NewPath string `json:"new_path"`

	// LauncherPath is where the companion zzrouter-launcher was
	// installed alongside the node, empty when the release shipped none.
	LauncherPath string `json:"launcher_path,omitempty"`

	// LauncherBackupPath is the launcher this install displaced, so a
	// rollback can put the pair back as it found it.
	LauncherBackupPath string `json:"launcher_backup_path,omitempty"`

	// Version is what now sits behind the symlink on a managed install,
	// and PreviousVersion is what a rollback would return to. Both are
	// empty on an in-place install, where a version is not an addressable
	// thing on disk and BackupPath is the only handle there is.
	Version         string `json:"version,omitempty"`
	PreviousVersion string `json:"previous_version,omitempty"`

	// Error contains the installation error (if Success is false)
	Error string `json:"error,omitempty"`
}
