//go:build windows

package host

import (
	"os/exec"
	"syscall"
)

// CREATE_NO_WINDOW suppresses the console window for console-subsystem
// children. STARTF_USESHOWWINDOW/SW_HIDE (HideWindow=true) only hides the
// *main* window of GUI programs; it does not prevent a console from
// allocating, so we combine both to cover both program types.
const CreateNoWindow = 0x08000000

func hideWindow(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= CreateNoWindow
}
