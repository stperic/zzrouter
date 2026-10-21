package process

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/shirou/gopsutil/v4/process"
	"github.com/stperic/zzrouter/pkg/constants"
)

// Termination timeouts. These are longer than constants.ProcessGracefulTimeout (3s)
// because LLM processes (vLLM, llama.cpp) may need time to flush GPU memory and
// save state. The 10s graceful window avoids premature SIGKILL on heavy workloads.
const (
	GracefulTimeout = constants.EngineGracefulTimeout
	ForceTimeout    = constants.EngineForceTimeout
)

// ShutdownGrace uses the configured grace, defaulting nonpositive values.
func ShutdownGrace(grace time.Duration) time.Duration {
	if grace <= 0 {
		grace = GracefulTimeout
	}
	return grace
}

// TerminateRequest carries the identified leader through both shutdown stages.
type TerminateRequest struct {
	PID        int
	StartToken int64
	Inspector
	ProcessGroupID int
	StdinPipe      func()  // Windows: close to signal launcher
	JobHandle      uintptr // Windows: Job Object handle; 0 on Unix
	Graceful       time.Duration
	BudgetExpired  <-chan struct{} // Only the node's run budget may shorten grace.
}

// Terminate gracefully stops a process and its entire process tree, escalating
// to forced kill if needed. Cancellation abandons an unsignalled stop; afterwards
// only BudgetExpired may shorten grace.
//
// On Unix:  SIGTERM tree → wait → SIGKILL tree → wait.
// On Windows (with Job Object): close stdin pipe → wait → TerminateJobObject.
func Terminate(ctx context.Context, req TerminateRequest) error {
	if req.PID <= 0 {
		return nil
	}

	graceful := ShutdownGrace(req.Graceful)

	// Identity may change between inspection and signal; the existing narrow race is accepted.
	if !req.Owns(req.PID, req.StartToken) {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	// Graceful: platform-specific (SIGTERM on Unix, stdin-pipe close on Windows)
	if err := terminateProcessTree(req); err != nil {
		if !processTreeExists(req) {
			return nil
		}
	}

	// Wait for every member to release its resources.
	gracefulTimer := time.After(graceful)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-req.BudgetExpired:
			return forceKill(req)
		case <-gracefulTimer:
			return forceKill(req)
		case <-ticker.C:
			if !processTreeExists(req) {
				return nil
			}
		}
	}
}

// forceKill sends SIGKILL (Unix) or TerminateJobObject (Windows) to the
// process tree.
func forceKill(req TerminateRequest) error {
	if !req.Owns(req.PID, req.StartToken) && (req.ProcessGroupID <= 0 || ProcessExists(int32(req.PID))) {
		return nil
	}

	// A process already exiting refuses the kill (Windows: access denied), so a
	// kill error is reported only if the process outlives the force wait.
	killErr := killProcessTree(req)

	forceTimer := time.After(ForceTimeout)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-forceTimer:
			if !processTreeExists(req) {
				return nil
			}
			if killErr != nil {
				return fmt.Errorf("failed to kill process tree (pid=%d): %w", req.PID, killErr)
			}
			return fmt.Errorf("process %d did not exit after force kill", req.PID)
		case <-ticker.C:
			if !processTreeExists(req) {
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
