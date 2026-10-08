package process

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/shirou/gopsutil/v4/process"
)

// Termination timeouts. These are longer than constants.ProcessGracefulTimeout (3s)
// because LLM processes (vLLM, llama.cpp) may need time to flush GPU memory and
// save state. The 10s graceful window avoids premature SIGKILL on heavy workloads.
const (
	GracefulTimeout = 10 * time.Second
	ForceTimeout    = 5 * time.Second
)

// TerminateRequest holds everything needed to stop a process tree.
// On Unix, only PID and ProcessGroupID are used. On Windows, StdinPipe
// and JobHandle enable the stdin-pipe graceful shutdown and Job Object
// atomic kill paths. Callers populate from Instance fields.
type TerminateRequest struct {
	PID            int
	ProcessGroupID int
	StdinPipe      io.WriteCloser // Windows: close to signal launcher; nil on Unix
	JobHandle      uintptr        // Windows: Job Object handle; 0 on Unix
	Graceful       time.Duration
}

// Terminate gracefully stops a process and its entire process tree, escalating
// to forced kill if needed.
//
// On Unix:  SIGTERM tree → wait → SIGKILL tree → wait.
// On Windows (with Job Object): close stdin pipe → wait → TerminateJobObject.
func Terminate(ctx context.Context, req TerminateRequest) error {
	if req.PID <= 0 {
		return nil
	}

	graceful := req.Graceful
	if graceful <= 0 {
		graceful = GracefulTimeout
	}

	if !ProcessExists(int32(req.PID)) {
		return nil
	}

	// Graceful: platform-specific (SIGTERM on Unix, stdin-pipe close on Windows)
	if err := terminateProcessTree(req); err != nil {
		if !ProcessExists(int32(req.PID)) {
			return nil
		}
	}

	// Wait for process to exit
	gracefulTimer := time.After(graceful)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return forceKill(req)
		case <-gracefulTimer:
			return forceKill(req)
		case <-ticker.C:
			if !ProcessExists(int32(req.PID)) {
				return nil
			}
		}
	}
}

// forceKill sends SIGKILL (Unix) or TerminateJobObject (Windows) to the
// process tree.
func forceKill(req TerminateRequest) error {
	if !ProcessExists(int32(req.PID)) {
		return nil
	}

	if err := killProcessTree(req); err != nil {
		if !ProcessExists(int32(req.PID)) {
			return nil
		}
		return fmt.Errorf("failed to kill process tree (pid=%d): %w", req.PID, err)
	}

	forceTimer := time.After(ForceTimeout)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-forceTimer:
			if !ProcessExists(int32(req.PID)) {
				return nil
			}
			return fmt.Errorf("process %d did not exit after force kill", req.PID)
		case <-ticker.C:
			if !ProcessExists(int32(req.PID)) {
				return nil
			}
		}
	}
}

// ProcessExists checks if a process with the given PID is running.
func ProcessExists(pid int32) bool {
	exists, err := process.PidExists(pid)
	if err != nil {
		// Inspection failure is not confirmed exit; keep termination conservative.
		return pid > 0
	}
	return exists
}

// IsSafeToKill returns true if the PID is safe to terminate.
func IsSafeToKill(pid int32) bool {
	if pid <= 1 {
		return false
	}
	if int(pid) == os.Getpid() {
		return false
	}
	return true
}
