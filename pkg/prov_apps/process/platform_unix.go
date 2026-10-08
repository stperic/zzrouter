//go:build unix

package process

import (
	"os"
	"os/exec"
	"syscall"
)

// setProcAttributes sets Unix-specific process attributes.
// Setpgid creates a new process group so we can kill the entire tree.
func setProcAttributes(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	// A PTY session already owns its process group; Setpgid would fail after Setsid.
	cmd.SysProcAttr.Setpgid = !cmd.SysProcAttr.Setsid
}

// setSpawnAttributes configures process attributes based on whether the
// zzrouter-launcher wrapper is in use.
// Unix: when NOT using the launcher, set Setpgid. When using the launcher,
// skip — the launcher creates its own group for the child internally.
func setSpawnAttributes(cmd *exec.Cmd, usingLauncher bool) {
	if !usingLauncher {
		setProcAttributes(cmd)
	}
}

// getProcessGroupID returns the process group ID for a process.
func getProcessGroupID(process *os.Process) int {
	if process == nil {
		return 0
	}
	pgid, err := syscall.Getpgid(process.Pid)
	if err != nil {
		return 0
	}
	return pgid
}

// terminateProcessTree sends SIGTERM to the entire process group for graceful shutdown.
// Falls back to signaling the individual process if pgid is unavailable.
// req.StdinPipe and req.JobHandle are ignored on Unix.
func terminateProcessTree(req TerminateRequest) error {
	if req.ProcessGroupID > 0 {
		return syscall.Kill(-req.ProcessGroupID, syscall.SIGTERM)
	}
	proc, err := os.FindProcess(req.PID)
	if err != nil {
		return nil //nolint:nilerr // process already gone: terminate is idempotent
	}
	return proc.Signal(syscall.SIGTERM)
}

// assignToJobObject is a no-op on Unix (Job Objects are a Windows concept).
func assignToJobObject(_ int) (uintptr, error) { return 0, nil }

// CloseJobHandle is a no-op on Unix.
func CloseJobHandle(_ uintptr) {}

// killProcessTree sends SIGKILL to the entire process group for forced termination.
// Falls back to killing the individual process if pgid is unavailable.
func killProcessTree(req TerminateRequest) error {
	if req.ProcessGroupID > 0 {
		return syscall.Kill(-req.ProcessGroupID, syscall.SIGKILL)
	}
	proc, err := os.FindProcess(req.PID)
	if err != nil {
		return nil //nolint:nilerr // process already gone: kill is idempotent
	}
	return proc.Kill()
}
