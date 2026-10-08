//go:build windows

package install

import (
	"context"
	"os/exec"
	"syscall"

	"github.com/stperic/zzrouter/pkg/host"
)

// newWindowsShellCommand builds an *exec.Cmd that runs `command` via
// `cmd.exe /S /C`, bypassing Go's default argument quoting by setting
// SysProcAttr.CmdLine directly. This is the only reliable way to pass
// a quoted path (e.g. `mkdir "C:\Users\Foo\AppData\..."`) through
// cmd.exe without the parser mangling the string.
//
// See plan.go shellCommand() for rationale.
func newWindowsShellCommand(ctx context.Context, command string) (*exec.Cmd, error) {
	// All commands come from trusted install plans (hardcoded in Go), not
	// user input. The /S /C "..." pattern passes inner content verbatim to
	// cmd.exe — inner quotes, pipes, and PowerShell syntax are all safe
	// because the outer quote pair is stripped by /S and no shell expansion
	// occurs on the inner string beyond what cmd.exe normally does.

	// Name is ignored when SysProcAttr.CmdLine is set, but Go still needs
	// a path to stat -- use cmd.exe so LookPath succeeds. Use sysx so
	// HideWindow defaults propagate and the forbidigo rule is satisfied.
	cmd := host.CommandContext(ctx, "cmd")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		// /S + outer quotes: cmd strips exactly the outer pair and
		// treats the rest verbatim. Any quotes inside `command` are
		// preserved as the shell sees them.
		CmdLine:       `cmd /S /C "` + command + `"`,
		HideWindow:    true,
		CreationFlags: host.CreateNoWindow,
	}
	return cmd, nil
}
