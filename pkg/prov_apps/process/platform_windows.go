//go:build windows

package process

import (
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"syscall"

	"github.com/shirou/gopsutil/v4/process"
	"github.com/stperic/zzrouter/pkg/prov_apps/process/jobobj"
)

// setProcAttributes is a no-op on Windows.
func setProcAttributes(_ *exec.Cmd) {}

// setSpawnAttributes configures process attributes for the launcher.
// When using the launcher, we add CREATE_NEW_PROCESS_GROUP for the
// stdin-pipe-based shutdown to work correctly (the process group is
// needed so the launcher can cleanly forward signals to its child).
// We keep CREATE_NO_WINDOW because graceful shutdown is now via stdin
// pipe (not GenerateConsoleCtrlEvent), so no console is needed.
func setSpawnAttributes(cmd *exec.Cmd, usingLauncher bool) {
	if usingLauncher {
		if cmd.SysProcAttr == nil {
			cmd.SysProcAttr = &syscall.SysProcAttr{}
		}
		cmd.SysProcAttr.CreationFlags |= syscall.CREATE_NEW_PROCESS_GROUP
	}
}

// getProcessGroupID returns 0 on Windows (process groups work differently).
func getProcessGroupID(_ *os.Process) int {
	return 0
}

// assignToJobObject creates a new Windows Job Object with
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE and assigns the given process to it.
// Returns the job handle as uintptr for storage on the Instance struct.
//
// If zzrouter-node crashes, the kernel closes the handle, and all processes
// in the job die automatically — zero orphaned GPU processes.
func assignToJobObject(pid int) (uintptr, error) {
	job, err := jobobj.Create()
	if err != nil {
		return 0, err
	}
	if err := job.AssignProcess(pid); err != nil {
		job.Close()
		return 0, err
	}
	return job.Handle(), nil
}

// terminateProcessTree initiates graceful shutdown on Windows.
// Closes the stdin pipe to the launcher, signaling it to begin provider
// shutdown. This works in any session (including session 0 / Windows Service)
// because it does not depend on console availability.
func terminateProcessTree(req TerminateRequest) error {
	if req.StdinPipe != nil {
		slog.Debug("closing stdin pipe for graceful shutdown", "pid", req.PID)
		_ = req.StdinPipe.Close()
	}
	return nil
}

// killProcessTree forcefully kills the entire process tree on Windows.
// If a Job Object handle is available, uses TerminateJobObject (atomic,
// kernel-level, catches all descendants). Falls back to recursive gopsutil
// walk for processes not in a job.
func killProcessTree(req TerminateRequest) error {
	if req.JobHandle != 0 {
		slog.Debug("terminating via Job Object", "pid", req.PID)
		job := jobobj.FromHandle(req.JobHandle)
		if err := job.Terminate(1); err != nil {
			slog.Warn("TerminateJobObject failed, falling back to process walk",
				"pid", req.PID, "error", err)
		} else {
			return nil
		}
	}

	// Fallback: recursive gopsutil walk (handles non-job-object processes)
	killDescendants(int32(req.PID))
	p, err := process.NewProcess(int32(req.PID))
	if errors.Is(err, process.ErrorProcessNotRunning) {
		return nil
	}
	if err != nil {
		return err
	}
	return p.Kill()
}

// killDescendants recursively kills all descendants of pid, depth-first.
func killDescendants(pid int32) {
	p, err := process.NewProcess(pid)
	if err != nil {
		return
	}
	children, err := p.Children()
	if err != nil || len(children) == 0 {
		return
	}
	for _, child := range children {
		killDescendants(child.Pid)
		_ = child.Kill()
	}
}

// CloseJobHandle closes a Windows Job Object handle by uintptr.
func CloseJobHandle(h uintptr) {
	if h != 0 {
		job := jobobj.FromHandle(h)
		_ = job.Close()
	}
}
