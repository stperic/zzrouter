package update

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	ps "github.com/mitchellh/go-ps"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/utils"
)

// The handoff is how an unprivileged node asks a privileged updater to
// replace its binaries.
//
// On a managed install the node runs as its own service user and the
// install tree is root-owned, so the node cannot update itself and must
// not be able to: a service account that can rewrite the binary an
// operator later runs under sudo is a straightforward path from "the
// node process was compromised" to "root on the box". Teleport and
// Elastic Agent both split it the same way, with the agent asking and a
// privileged updater doing.
//
// The bridge is two files in two directories, and WHICH directory each
// file lives in is the security boundary. The node writes a request into
// a directory it owns; a systemd path unit watching for that file starts
// a root oneshot, which claims the request, does the work, and publishes
// what happened into a ROOT-owned directory the node can only read.
// Nothing needs polkit, a dbus call, a sudoers rule, or any privilege
// the node does not already have -- the node's entire capability here is
// "create a file in its own state directory".
//
// SECURITY -- each side writes only where it owns. Root must never
// create a file in a directory the service user controls: the service
// user can pre-plant that name as a symlink, and O_CREATE follows
// symlinks, so root's write lands wherever the link points. That is a
// straight local privilege escalation, and it is the reason the status
// file does not sit next to the request file. For the same reason root
// opens the request with O_NOFOLLOW.
//
// SECURITY -- the request crosses a privilege boundary, so its contents
// are untrusted input to a root process. It therefore carries no path,
// no URL, no filename and no release source: only an action from a
// closed set and a version string. Everything about *where* a binary
// comes from is decided by the privileged side from configuration the
// service user cannot write. Adding a field to Request that names a
// location would hand a compromised node the ability to have root
// install a binary of its choosing.

// handoffDirName names both halves of the handoff: the request
// directory under the node's state directory, and the status directory
// under the root-owned install tree.
const handoffDirName = "update"

const (
	requestFileName = "request.json"
	statusFileName  = "status.json"

	// Process enumeration can briefly fail under full-suite or host load.
	processLookupAttempts = 3
	processLookupBackoff  = 25 * time.Millisecond
)

// RequestAction is what the node is asking the privileged updater to
// do. A closed set on purpose: it is parsed from a file the service
// user controls.
type RequestAction string

const (
	// ActionCheck asks only for a check against the release feed.
	ActionCheck RequestAction = "check"

	// ActionApply asks for the pending update to be installed.
	ActionApply RequestAction = "apply"

	// ActionRollback asks to go back to the previous version.
	ActionRollback RequestAction = "rollback"
)

// ErrUnknownAction reports a request naming something that is not one
// of the three actions.
var ErrUnknownAction = errors.New("not an update action")

// Valid reports whether a is one of the recognised actions.
func (a RequestAction) Valid() bool {
	switch a {
	case ActionCheck, ActionApply, ActionRollback:
		return true
	default:
		return false
	}
}

// Request is what the node leaves for the privileged updater.
type Request struct {
	// Action is the operation being asked for.
	Action RequestAction `json:"action"`

	// TargetVersion optionally pins what to move to. It is compared
	// against what the release feed offers and is never used to build a
	// path: the version directory is named after the release the
	// privileged side resolved for itself.
	TargetVersion string `json:"target_version,omitempty"`

	// JobID ties the run back to the job the node opened for it, so an
	// API caller watching a job stream sees the privileged run's
	// progress.
	JobID string `json:"job_id,omitempty"`

	// RequestedAt and RequestedBy are for the operator reading the log,
	// not for any decision the updater makes.
	RequestedAt time.Time `json:"requested_at"`
	RequestedBy string    `json:"requested_by,omitempty"`
}

// Validate checks a request read across the privilege boundary.
func (r *Request) Validate() error {
	if !r.Action.Valid() {
		return fmt.Errorf("%w: %q", ErrUnknownAction, r.Action)
	}
	if r.TargetVersion != "" {
		if err := ValidateVersion(r.TargetVersion); err != nil {
			return fmt.Errorf("target_version %q is not a version: %w", r.TargetVersion, err)
		}
	}
	return nil
}

// RunStatus is what the privileged updater publishes about the run it
// is doing or has finished, for the node to report through its API.
type RunStatus struct {
	// PID identifies the owner so a crashed updater does not block recovery.
	PID         int           `json:"pid,omitempty"`
	Action      RequestAction `json:"action"`
	State       UpdateState   `json:"state"`
	Phase       string        `json:"phase,omitempty"`
	Progress    int           `json:"progress,omitempty"`
	FromVersion string        `json:"from_version,omitempty"`
	ToVersion   string        `json:"to_version,omitempty"`
	JobID       string        `json:"job_id,omitempty"`
	StartedAt   time.Time     `json:"started_at"`
	FinishedAt  *time.Time    `json:"finished_at,omitempty"`
	Success     bool          `json:"success"`
	Error       string        `json:"error,omitempty"`
}

// Finished reports whether the run this status describes is over.
func (s *RunStatus) Finished() bool { return s != nil && s.FinishedAt != nil }

// Handoff is the pair of files, seen from either side.
type Handoff struct {
	mu sync.Mutex
	// requestDir is owned by the service user: the node creates the
	// request here and root only reads and unlinks.
	requestDir string

	// statusDir is root-owned and world-readable: root creates the
	// status here and the node only reads. Separate from requestDir
	// because a root process must not create files where an
	// unprivileged principal can pre-plant the name as a symlink.
	statusDir string
}

// NewHandoff builds a handoff whose two directions live in two
// directories. Tests may pass the same one; production must not.
func NewHandoff(requestDir, statusDir string) *Handoff {
	return &Handoff{requestDir: requestDir, statusDir: statusDir}
}

// DefaultHandoff is the handoff a managed Linux install uses.
//
// The path is fixed rather than resolved through PathResolver, because
// the two sides resolve it differently and would silently never meet:
// as the service user the data directory is /var/lib/zzrouter, but as
// root it is /opt/zzrouter. A handoff only exists on a managed install,
// where the service-user answer is the only correct one.
func DefaultHandoff() *Handoff {
	return NewHandoff(
		filepath.Join(config.LinuxDataDir, handoffDirName),
		DefaultStatusDir(),
	)
}

// DefaultStatusDir is the root-owned directory the privileged updater
// publishes into, and uses for its own downloads, backups and history.
func DefaultStatusDir() string {
	return filepath.Join(config.LinuxRootDir, handoffDirName)
}

// RequestDir is the service-user-owned half.
func (h *Handoff) RequestDir() string { return h.requestDir }

// RequestPath is the file a systemd path unit watches for.
func (h *Handoff) RequestPath() string { return filepath.Join(h.requestDir, requestFileName) }

// StatusPath is where the privileged side publishes progress.
func (h *Handoff) StatusPath() string { return filepath.Join(h.statusDir, statusFileName) }

// Submit leaves a request for the privileged updater. Called by the
// node, as the service user.
//
// Written under a temporary name and renamed into place so the watcher
// cannot start a run against a half-written file: the path unit fires
// on the name appearing, which with a rename means it appears complete.
func (h *Handoff) Submit(req *Request) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.submit(req)
}

// ApplyVersion accepts duplicate delivery without queueing a second install.
func (h *Handoff) ApplyVersion(target string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if pending, err := h.Pending(); err != nil {
		return "", err
	} else if pending != nil && pending.Action == ActionApply && pending.TargetVersion == target {
		return pending.JobID, nil
	}
	if run, err := h.LastRun(); err != nil {
		return "", err
	} else if run != nil && !run.Finished() && run.Action == ActionApply && run.ToVersion == target {
		process, err := ps.FindProcess(run.PID)
		if err != nil {
			return "", err
		}
		if run.PID == 0 || process != nil {
			return run.JobID, nil
		}
	}
	req := &Request{Action: ActionApply, TargetVersion: target, JobID: uuid.NewString()}
	return req.JobID, h.submit(req)
}

func (h *Handoff) submit(req *Request) error {
	if err := req.Validate(); err != nil {
		return err
	}
	if pending, err := h.Pending(); err != nil {
		return err
	} else if pending != nil {
		return ErrApplyInFlight
	}
	if run, err := h.LastRun(); err != nil {
		return err
	} else if run != nil && !run.Finished() {
		if run.PID == 0 {
			return ErrApplyInFlight
		}
		var process ps.Process
		var err error
		for attempt := 0; attempt < processLookupAttempts; attempt++ {
			process, err = ps.FindProcess(run.PID)
			if err == nil {
				break
			}
			if attempt+1 < processLookupAttempts {
				time.Sleep(processLookupBackoff)
			}
		}
		if err != nil {
			return fmt.Errorf("observe updater process: %w", err)
		}
		if process != nil {
			return ErrApplyInFlight
		}
	}
	if req.RequestedAt.IsZero() {
		req.RequestedAt = utils.NowUTC()
	}
	if err := os.MkdirAll(h.requestDir, 0750); err != nil {
		return fmt.Errorf("create %s: %w", h.requestDir, err)
	}
	return writeJSONAtomic(h.RequestPath(), req, 0640)
}

// Pending returns the request waiting to be claimed, or nil.
func (h *Handoff) Pending() (*Request, error) {
	req := &Request{}
	switch err := readJSONNoFollow(h.RequestPath(), req); {
	case os.IsNotExist(err):
		return nil, nil //nolint:nilnil // "nothing waiting" is the common case
	case err != nil:
		return nil, err
	}
	if err := req.Validate(); err != nil {
		return nil, err
	}
	return req, nil
}

// Claim takes the pending request and removes it. Called by the
// privileged updater.
//
// Removing it is what re-arms the systemd path unit: PathExists fires
// on the file appearing and stays quiet while it is still there, so a
// request left in place means the next one is never noticed. Removed
// before the work rather than after, so a run that dies partway cannot
// have the watcher restart it in a loop.
func (h *Handoff) Claim() (*Request, error) {
	req, err := h.Pending()
	if err != nil {
		// Remove it anyway. A request that cannot be parsed will never
		// become parseable, and leaving it there wedges the watcher
		// against every future request.
		_ = os.Remove(h.RequestPath())
		return nil, err
	}
	if req == nil {
		return nil, nil //nolint:nilnil // nothing waiting
	}
	// Publish ownership before removing the pending request, closing the claim gap.
	if err := h.Publish(&RunStatus{PID: os.Getpid(), Action: req.Action, State: StateChecking, ToVersion: req.TargetVersion, JobID: req.JobID, StartedAt: utils.NowUTC()}); err != nil {
		return nil, err
	}
	if err := os.Remove(h.RequestPath()); err != nil {
		return nil, fmt.Errorf("claim update request: %w", err)
	}
	return req, nil
}

// Publish records what the privileged run is doing. Called repeatedly
// during a run and once at the end.
//
// Failures are returned but callers should not abort a run over one:
// this file is how the run is *reported*, not how it is performed, and
// an update that succeeded while its status file could not be written
// is still an update that succeeded.
func (h *Handoff) Publish(status *RunStatus) error {
	// 0755 and root-owned: the node has to reach in to read the status,
	// and must not be able to write anything here.
	if err := os.MkdirAll(h.statusDir, 0755); err != nil {
		return fmt.Errorf("create %s: %w", h.statusDir, err)
	}
	if err := os.Chmod(h.statusDir, 0755); err != nil {
		return fmt.Errorf("make %s readable: %w", h.statusDir, err)
	}
	// World-readable: written by root, read by the service user, which
	// is the only way the node can report what the updater did.
	return writeJSONAtomic(h.StatusPath(), status, statusFileMode)
}

// LastRun reads what the privileged updater last reported, or nil when
// it has never run.
func (h *Handoff) LastRun() (*RunStatus, error) {
	status := &RunStatus{}
	switch err := readJSON(h.StatusPath(), status); {
	case os.IsNotExist(err):
		return nil, nil //nolint:nilnil // never run is not an error
	case err != nil:
		return nil, err
	}
	return status, nil
}

// statusFileMode must survive the node's umask. The process sets 0027
// at startup (security.SetSecureUmask), which turns a 0644 create into
// 0640 -- root-owned and unreadable to the service user, so the node's
// own /update/status could not say what the privileged updater did.
const statusFileMode = 0644

func writeJSONAtomic(path string, v any, mode os.FileMode) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", filepath.Base(path), err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	// WriteFile's mode is masked by the umask; Chmod is not. Applied
	// before the rename so the file is never visible with the wrong one.
	if err := os.Chmod(tmp, mode); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("set mode on %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("move %s into place: %w", filepath.Base(path), err)
	}
	return nil
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	return nil
}

// maxRequestSize bounds what the privileged side will read. The request
// is a handful of short fields; anything larger is not one.
const maxRequestSize = 64 << 10

// readJSONNoFollow reads a file that an unprivileged principal owns.
//
// O_NOFOLLOW because the service user owns the directory this lives in
// and can replace the name with a symlink. Without it a root process
// can be steered into reading whatever the link points at -- a device
// that never ends, or a file it was never meant to open. The read is
// bounded for the same reason.
func readJSONNoFollow(path string, v any) error {
	f, err := os.OpenFile(path, os.O_RDONLY|openNoFollow, 0)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(io.LimitReader(f, maxRequestSize))
	if err != nil {
		return fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	return nil
}
