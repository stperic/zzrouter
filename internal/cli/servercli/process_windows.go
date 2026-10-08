//go:build windows
// +build windows

package servercli

import (
	"os"
	"syscall"

	"github.com/stperic/zzrouter/pkg/host"
)

const detachedProcess = 0x00000008

func getSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | detachedProcess | host.CreateNoWindow,
		HideWindow:    true,
	}
}

// gracefulStopSignal returns the signal used to request a graceful
// shutdown. Windows uses os.Interrupt, which the Go runtime maps to
// CTRL_BREAK_EVENT for processes in a new console group — matching
// what the server's signal.Notify(os.Interrupt) handler catches.
func gracefulStopSignal() os.Signal {
	return os.Interrupt
}
