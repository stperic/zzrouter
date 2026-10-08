package client

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/stperic/zzrouter/pkg/apipath"
)

// Self-update client. Every route here acts on the node the request
// reaches — they read that node's scheduler and take no node selector —
// so what these methods report is one node's business, not the
// cluster's.
//
// The types below mirror the wire rather than importing pkg/update:
// client-side code does not depend on server-side packages, the same
// reason the TUI's key and team views go through this package instead
// of pkg/keys.

// UpdateVersion is a released version as the API renders it: parts, not
// a string. String() puts it back together for display.
type UpdateVersion struct {
	Major      int    `json:"major"`
	Minor      int    `json:"minor"`
	Patch      int    `json:"patch"`
	PreRelease string `json:"pre_release,omitempty"`
	BuildMeta  string `json:"build_meta,omitempty"`
}

// String renders the version the way the node's own --version does.
func (v *UpdateVersion) String() string {
	if v == nil {
		return ""
	}
	out := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if v.PreRelease != "" {
		out += "-" + v.PreRelease
	}
	if v.BuildMeta != "" {
		out += "+" + v.BuildMeta
	}
	return out
}

// UpdateRelease is the release a node would install.
type UpdateRelease struct {
	Version     *UpdateVersion `json:"version"`
	TagName     string         `json:"tag_name"`
	Name        string         `json:"name"`
	Body        string         `json:"body"`
	PublishedAt time.Time      `json:"published_at"`
	HTMLURL     string         `json:"html_url"`
	Prerelease  bool           `json:"prerelease"`
}

// UpdatePrivilegedRun is what the privileged updater last did. On a
// service install the node cannot watch that work — it happens in a
// process it does not own — so this record is the only account of it.
type UpdatePrivilegedRun struct {
	Action      string     `json:"action"`
	State       string     `json:"state"`
	Phase       string     `json:"phase,omitempty"`
	Progress    int        `json:"progress,omitempty"`
	FromVersion string     `json:"from_version,omitempty"`
	ToVersion   string     `json:"to_version,omitempty"`
	JobID       string     `json:"job_id,omitempty"`
	StartedAt   time.Time  `json:"started_at"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
	Success     bool       `json:"success"`
	Error       string     `json:"error,omitempty"`
}

// Finished reports whether the run is over. A caller watching a
// delegated action has nothing else to wait on.
func (r *UpdatePrivilegedRun) Finished() bool {
	return r != nil && r.FinishedAt != nil
}

// UpdatePendingConfirm is set while the node runs a version that has not
// proven itself yet: it clears once the node answers its own health
// endpoint, and enough unconfirmed boots roll the update back.
type UpdatePendingConfirm struct {
	FromVersion        string `json:"from_version"`
	ToVersion          string `json:"to_version"`
	BackupPath         string `json:"backup_path,omitempty"`
	LauncherBackupPath string `json:"launcher_backup_path,omitempty"`
	Attempts           int    `json:"attempts"`
}

// UpdateStatus is one node's view of its own updates.
type UpdateStatus struct {
	State                   string                `json:"state"`
	CurrentVersion          *UpdateVersion        `json:"current_version"`
	LatestVersion           *UpdateVersion        `json:"latest_version,omitempty"`
	PendingRelease          *UpdateRelease        `json:"pending_release,omitempty"`
	LastCheckTime           *time.Time            `json:"last_check_time,omitempty"`
	NextCheckTime           *time.Time            `json:"next_check_time,omitempty"`
	LastUpdateTime          *time.Time            `json:"last_update_time,omitempty"`
	Error                   string                `json:"error,omitempty"`
	DownloadProgress        int                   `json:"download_progress,omitempty"`
	Channel                 string                `json:"channel"`
	UpdateAvailable         bool                  `json:"update_available"`
	MaintenanceWindowActive bool                  `json:"maintenance_window_active,omitempty"`
	RestartRequired         bool                  `json:"restart_required,omitempty"`
	RestartRequiredReason   string                `json:"restart_required_reason,omitempty"`
	Source                  string                `json:"source,omitempty"`
	Blocked                 string                `json:"blocked,omitempty"`
	DelegatedTo             string                `json:"delegated_to,omitempty"`
	PrivilegedRun           *UpdatePrivilegedRun  `json:"privileged_run,omitempty"`
	PendingConfirm          *UpdatePendingConfirm `json:"pending_confirm,omitempty"`
}

// UpdateCheckResult is what a check found.
type UpdateCheckResult struct {
	UpdateAvailable bool           `json:"update_available"`
	CurrentVersion  string         `json:"current_version"`
	LatestRelease   *UpdateRelease `json:"latest_release,omitempty"`
	SkippedReason   string         `json:"skipped_reason,omitempty"`
}

// UpdateActionResult is the answer to an apply or a rollback.
//
// The three fields are mutually exclusive by role and which one arrives
// says who is doing the work. JobID means this node is, and there is a
// stream to follow. DelegatedTo means a privileged updater took the
// request — there is no job to subscribe to, because the work runs in a
// process this one does not own, and status is the only witness.
// BackupPath is a rollback this node performed itself.
type UpdateActionResult struct {
	JobID       string `json:"job_id,omitempty"`
	DelegatedTo string `json:"delegated_to,omitempty"`
	Action      string `json:"action,omitempty"`
	BackupPath  string `json:"backup_path,omitempty"`
}

// Delegated reports whether the work was handed to the privileged
// updater, which is the case with nothing to stream.
func (r *UpdateActionResult) Delegated() bool {
	return r != nil && r.DelegatedTo != ""
}

// UpdateHistoryEntry is one past update or rollback.
type UpdateHistoryEntry struct {
	ID          string         `json:"id"`
	Timestamp   time.Time      `json:"timestamp"`
	FromVersion *UpdateVersion `json:"from_version"`
	ToVersion   *UpdateVersion `json:"to_version"`
	Success     bool           `json:"success"`
	Error       string         `json:"error,omitempty"`
	Duration    time.Duration  `json:"duration"`
	Automatic   bool           `json:"automatic"`
}

// UpdateHistory is the recorded history. It arrives at the top level of
// the response, not inside the success envelope every other update
// route uses.
type UpdateHistory struct {
	Entries     []UpdateHistoryEntry `json:"entries"`
	LastUpdated time.Time            `json:"last_updated"`
}

// UpdateDisabled reports the auto-update system being switched off on
// this node. Every update route answers 503 for it rather than 404, so
// that a caller can tell a disabled feature from a route that does not
// exist — and a UI can say which.
func UpdateDisabled(err error) bool { return updateStatusIs(err, http.StatusServiceUnavailable) }

// UpdateNothingToApply reports an apply with no update waiting. An
// ordinary answer, not a failure.
func UpdateNothingToApply(err error) bool { return updateStatusIs(err, http.StatusConflict) }

// UpdateNoBackups reports a rollback on a node with nothing to go back
// to.
func UpdateNoBackups(err error) bool { return updateStatusIs(err, http.StatusNotFound) }

func updateStatusIs(err error, status int) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == status
}

// GetUpdateStatus reports what this node knows about its own updates.
// GET /zzrouter/v1/update/status
func (c *Client) GetUpdateStatus() (*UpdateStatus, error) {
	var result struct {
		Data UpdateStatus `json:"data"`
	}
	if err := c.doJSON("GET", apipath.UpdateStatus, nil, &result, "get update status"); err != nil {
		return nil, err
	}
	return &result.Data, nil
}

// CheckForUpdate asks the node to consult its release feed now.
// POST /zzrouter/v1/update/check
func (c *Client) CheckForUpdate() (*UpdateCheckResult, error) {
	var result struct {
		Data UpdateCheckResult `json:"data"`
	}
	// The check reaches an external release feed, so it gets the client
	// that tolerates a slow one.
	if err := c.doLongJSON("POST", apipath.UpdateCheck, nil, &result, "check for update"); err != nil {
		return nil, err
	}
	return &result.Data, nil
}

// ApplyUpdate starts the pending update, or hands it to the privileged
// updater on a node that does not install its own.
// POST /zzrouter/v1/update/apply
func (c *Client) ApplyUpdate() (*UpdateActionResult, error) {
	var result struct {
		Data UpdateActionResult `json:"data"`
	}
	if err := c.doLongJSON("POST", apipath.UpdateApply, nil, &result, "apply update"); err != nil {
		return nil, err
	}
	return &result.Data, nil
}

// RollbackUpdate returns the node to the version it came from.
// POST /zzrouter/v1/update/rollback
func (c *Client) RollbackUpdate() (*UpdateActionResult, error) {
	var result struct {
		Data UpdateActionResult `json:"data"`
	}
	if err := c.doLongJSON("POST", apipath.UpdateRollback, nil, &result, "roll back update"); err != nil {
		return nil, err
	}
	return &result.Data, nil
}

// GetUpdateHistory returns what this node has installed and rolled back.
// GET /zzrouter/v1/update/history
func (c *Client) GetUpdateHistory() (*UpdateHistory, error) {
	// Unwrapped, unlike its siblings: the handler writes the history
	// object itself.
	var history UpdateHistory
	if err := c.doJSON("GET", apipath.UpdateHistory, nil, &history, "get update history"); err != nil {
		return nil, err
	}
	return &history, nil
}
