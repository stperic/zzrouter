//go:build unix

package shared

import (
	"os/exec"
	"syscall"
)

// detachProcess puts the launcher in its own session so a Ctrl+C aimed at
// the TUI does not also reach the application it opened. This matters on
// Linux, where xdg-open's fallback execs the application in place instead
// of handing off to a service manager.
func detachProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
