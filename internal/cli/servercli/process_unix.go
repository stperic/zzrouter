//go:build !windows
// +build !windows

package servercli

import (
	"os"
	"syscall"
)

// getSysProcAttr returns platform-specific process attributes for daemonization
func getSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		// Set process group ID to detach from parent
		// This is enough to keep process alive when parent exits
		Setpgid: true,
	}
}

// gracefulStopSignal returns the signal used to request a graceful
// shutdown. SIGTERM on Unix — caught by the server's signal handler,
// runs Server.Stop() which fires the worker goodbye notify and
// flushes spend state before exiting.
func gracefulStopSignal() os.Signal {
	return syscall.SIGTERM
}
