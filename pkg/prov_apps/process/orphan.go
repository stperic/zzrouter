package process

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/shirou/gopsutil/v4/process"
)

// PIDTracker persists PIDs to disk for cross-restart orphan detection.
// On startup, any tracked PIDs that are still running (and carry the
// ZZROUTER_PROCESS_MARKER env var) are terminated gracefully.
//
// The PID directory is shared with zzrouter-launcher. The launcher writes
// JSON PID files; the tracker writes plain integer PID files. Both formats
// are understood when reading.
type PIDTracker struct {
	dir string
	mu  sync.Mutex
}

// NewPIDTracker creates a PID tracker using the given directory.
func NewPIDTracker(dir string) (*PIDTracker, error) {
	if err := os.MkdirAll(dir, 0750); err != nil {
		return nil, fmt.Errorf("failed to create PID directory: %w", err)
	}
	return &PIDTracker{dir: dir}, nil
}

// Track records a PID for a given instance.
func (pt *PIDTracker) Track(instanceID string, pid int) error {
	pt.mu.Lock()
	defer pt.mu.Unlock()

	path := filepath.Join(pt.dir, instanceID+".pid")
	return os.WriteFile(path, []byte(strconv.Itoa(pid)), 0640)
}

// Untrack removes a tracked PID.
func (pt *PIDTracker) Untrack(instanceID string) {
	pt.mu.Lock()
	defer pt.mu.Unlock()

	path := filepath.Join(pt.dir, instanceID+".pid")
	_ = os.Remove(path)
}

// launcherPIDFile is the JSON format written by zzrouter-launcher.
type launcherPIDFile struct {
	PID      int `json:"pid"`
	ChildPID int `json:"child_pid"`
}

// listTracked returns all tracked instance IDs and their PIDs.
// Handles both plain integer files (written by Track) and JSON files
// (written by zzrouter-launcher). For launcher JSON files, returns the
// launcher PID (the parent that manages signal forwarding to the child).
func (pt *PIDTracker) listTracked() map[string]int {
	pt.mu.Lock()
	defer pt.mu.Unlock()

	result := make(map[string]int)
	entries, err := os.ReadDir(pt.dir)
	if err != nil {
		return result
	}

	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".pid") {
			continue
		}

		instanceID := strings.TrimSuffix(name, ".pid")
		data, err := os.ReadFile(filepath.Join(pt.dir, name))
		if err != nil {
			continue
		}

		if pid := parsePIDFile(data); pid > 0 {
			result[instanceID] = pid
		}
	}
	return result
}

// parsePIDFile extracts a PID from a file that may be either a plain integer
// (written by PIDTracker.Track) or JSON (written by zzrouter-launcher).
func parsePIDFile(data []byte) int {
	text := strings.TrimSpace(string(data))

	// Try plain integer first (most common — written by Track)
	if pid, err := strconv.Atoi(text); err == nil && pid > 0 {
		return pid
	}

	// Try launcher JSON format
	var lf launcherPIDFile
	if json.Unmarshal(data, &lf) == nil && lf.PID > 0 {
		return lf.PID
	}

	return 0
}

// CleanupOrphans terminates tracked processes that are still running but
// no longer have a corresponding instance in the registry.
// Uses graceful SIGTERM → SIGKILL via Terminate() to give GPU processes
// time to flush memory. Returns the number of processes cleaned up.
func (pt *PIDTracker) CleanupOrphans(activeInstanceIDs map[string]bool) int {
	tracked := pt.listTracked()
	cleaned := 0

	for instanceID, pid := range tracked {
		if activeInstanceIDs[instanceID] {
			continue // still active
		}

		if !IsSafeToKill(int32(pid)) || !ProcessExists(int32(pid)) {
			pt.Untrack(instanceID)
			continue
		}

		// Verify the process is ours (prevents killing recycled PIDs)
		if !ProcessHasMarker(int32(pid)) {
			slog.Debug("orphan PID file stale (process recycled)", "pid", pid, "instance", instanceID)
			pt.Untrack(instanceID)
			continue
		}

		// Graceful termination: SIGTERM tree → wait → SIGKILL tree
		slog.Info("terminating orphaned process", "pid", pid, "instance", instanceID)
		if err := Terminate(context.Background(), TerminateRequest{PID: pid, Graceful: GracefulTimeout}); err != nil {
			slog.Warn("failed to terminate orphan process", "pid", pid, "instance", instanceID, "error", err)
		}

		pt.Untrack(instanceID)
		cleaned++
	}
	return cleaned
}

// ProcessHasMarker checks if a process has the ZZROUTER_PROCESS_MARKER env var.
// Used to verify process ownership before termination (prevents killing recycled PIDs).
// Returns true if the marker is found OR if the environment cannot be read
// (platforms where gopsutil Environ() is unsupported). In that case,
// the caller relies on other checks (IsSafeToKill, PID tracker) for safety.
func ProcessHasMarker(pid int32) bool {
	proc, err := process.NewProcess(pid)
	if err != nil {
		return false
	}

	envs, err := proc.Environ()
	if err != nil {
		// Platform doesn't support reading process environment.
		// Assume ownership — the PID was tracked at launch time and
		// IsSafeToKill already filters system PIDs.
		return true
	}

	target := ManagedProcessEnvKey + "=" + ManagedProcessEnvValue
	return slices.Contains(envs, target)
}
