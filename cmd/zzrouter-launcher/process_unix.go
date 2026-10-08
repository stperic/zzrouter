//go:build !windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

// setupProcessGroup configures the child to run in its own process group.
func setupProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// assignChildToJob is a no-op on Unix (Job Objects are Windows-only).
func assignChildToJob(_ *exec.Cmd) {}

// shutdownSignals returns the signals the launcher listens for.
func shutdownSignals() []os.Signal {
	return []os.Signal{syscall.SIGTERM, syscall.SIGINT}
}

// forwardSignalToChild sends the given signal to the child's process group.
func forwardSignalToChild(cmd *exec.Cmd, sig os.Signal) {
	if cmd.Process == nil {
		return
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		return
	}
	sysSig, ok := sig.(syscall.Signal)
	if !ok {
		return
	}
	_ = syscall.Kill(-pgid, sysSig)
}

// forceKillChild sends SIGKILL to the child's process group.
func forceKillChild(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err == nil {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}
}
