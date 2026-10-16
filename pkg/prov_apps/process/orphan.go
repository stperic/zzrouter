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

// PIDTracker shares identity files with the launcher for cross-restart cleanup.
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

// Track records a PID and its fork identity for a given instance.
func (pt *PIDTracker) Track(instanceID string, pid int, startToken int64) error {
	pt.mu.Lock()
	defer pt.mu.Unlock()

	data, err := json.Marshal(launcherPIDFile{PID: pid, StartToken: startToken})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(pt.dir, instanceID+".pid"), data, 0640)
}

// Untrack removes a tracked PID.
func (pt *PIDTracker) Untrack(instanceID string) {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	_ = os.Remove(filepath.Join(pt.dir, instanceID+".pid"))
}

// launcherPIDFile reads the leader identity, never the launcher's child PID.
type launcherPIDFile struct {
	PID        int   `json:"pid"`
	StartToken int64 `json:"start_token,omitempty"`
}

func (pt *PIDTracker) listTracked() map[string]launcherPIDFile {
	pt.mu.Lock()
	defer pt.mu.Unlock()

	result := make(map[string]launcherPIDFile)
	entries, _ := os.ReadDir(pt.dir)
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".pid") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(pt.dir, name))
		if err != nil {
			continue
		}
		if identity := parsePIDFile(data); identity.PID > 0 {
			result[strings.TrimSuffix(name, ".pid")] = identity
		}
	}
	return result
}

// parsePIDFile preserves integer and legacy launcher compatibility.
func parsePIDFile(data []byte) launcherPIDFile {
	if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 0 {
		return launcherPIDFile{PID: pid}
	}
	var identity launcherPIDFile
	if json.Unmarshal(data, &identity) == nil && identity.PID > 0 && identity.StartToken >= 0 {
		return identity
	}
	return launcherPIDFile{}
}

// CleanupOrphans terminates inactive tracked identities, retaining unconfirmed exits.
func (pt *PIDTracker) CleanupOrphans(activeInstanceIDs map[string]bool) int {
	return pt.cleanupOrphans(activeInstanceIDs, Terminate)
}

func (pt *PIDTracker) cleanupOrphans(activeInstanceIDs map[string]bool, terminate func(context.Context, TerminateRequest) error) int {
	cleaned := 0
	for instanceID, identity := range pt.listTracked() {
		if activeInstanceIDs[instanceID] {
			continue
		}
		if !ProcessExists(int32(identity.PID)) {
			pt.Untrack(instanceID)
			continue
		}
		if err := terminate(context.Background(), TerminateRequest{PID: identity.PID, StartToken: identity.StartToken}); err != nil {
			slog.Warn("failed to terminate orphan process", "pid", identity.PID, "instance", instanceID, "error", err)
			continue
		}
		if !ProcessExists(int32(identity.PID)) {
			pt.Untrack(instanceID)
			cleaned++
		}
	}
	return cleaned
}

// Inspector permits per-operation inspection without shared mutable hooks.
type Inspector struct {
	ReadStartToken func(int) (int64, error)
	Environ        func(int32) ([]string, error)
}

// Owns uses recorded fork identity; only legacy records need environment evidence.
func (s Inspector) Owns(pid int, startToken int64) bool {
	if !IsSafeToKill(int32(pid)) || !ProcessExists(int32(pid)) {
		return false
	}
	if startToken != 0 {
		read := s.ReadStartToken
		if read == nil {
			read = StartToken
		}
		live, err := read(pid)
		return err == nil && live == startToken
	}
	return s.HasMarker(int32(pid))
}

// HasMarker preserves legacy ownership on platforms without environment access.
func (s Inspector) HasMarker(pid int32) bool {
	proc, err := process.NewProcess(pid)
	if err != nil {
		return false
	}
	read := s.Environ
	if read == nil {
		read = func(int32) ([]string, error) { return proc.Environ() }
	}
	envs, err := read(pid)
	if err != nil {
		return true
	}
	return slices.Contains(envs, ManagedProcessEnvKey+"="+ManagedProcessEnvValue)
}
