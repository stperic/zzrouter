//go:build windows

package main

import (
	"os"
	"os/exec"
	"syscall"

	"github.com/stperic/zzrouter/pkg/host"
	"github.com/stperic/zzrouter/pkg/prov_apps/process/jobobj"
	"golang.org/x/sys/windows"
)

// childJob holds the Job Object for the child process tree. Written once
// by assignChildToJob (main goroutine, before any concurrent access),
// read by forceKillChild (same goroutine via select). Single-goroutine
// invariant — no synchronization needed.
var childJob *jobobj.Job

func setupProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | host.CreateNoWindow,
		HideWindow:    true,
	}
}

// assignChildToJob creates a Job Object with KILL_ON_JOB_CLOSE and assigns
// the child process to it. If the launcher crashes after this point, the
// kernel closes the handle and kills the entire child tree automatically.
func assignChildToJob(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	job, err := jobobj.Create()
	if err != nil {
		return
	}
	if err := job.AssignProcess(cmd.Process.Pid); err != nil {
		job.Close()
		return
	}
	childJob = job
}

func shutdownSignals() []os.Signal {
	return []os.Signal{os.Interrupt}
}

// forwardSignalToChild sends a CTRL_BREAK_EVENT to the child's process group.
// The child must have been created with CREATE_NEW_PROCESS_GROUP.
func forwardSignalToChild(cmd *exec.Cmd, _ os.Signal) {
	if cmd.Process == nil {
		return
	}
	_ = windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(cmd.Process.Pid))
}

// forceKillChild terminates the entire child process tree. Uses the Job
// Object if available (atomic tree kill), otherwise falls back to
// TerminateProcess on the direct child.
func forceKillChild(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if childJob != nil {
		_ = childJob.Terminate(1)
		return
	}
	_ = cmd.Process.Kill()
}
