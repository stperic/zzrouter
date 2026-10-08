package host

import (
	"context"
	"os/exec"
)

// Command is a drop-in replacement for exec.Command that applies
// HideWindow on Windows so console-subsystem children don't flash a
// console window when spawned from a detached/service parent.
//
// On non-Windows platforms, this is equivalent to exec.Command.
//
// Project policy (enforced via golangci-lint/forbidigo): use
// sysx.Command and sysx.CommandContext everywhere instead of
// exec.Command / exec.CommandContext, except inside this package.
func Command(name string, arg ...string) *exec.Cmd {
	//nolint:forbidigo // only legal exec.Command call site
	cmd := exec.Command(name, arg...)
	hideWindow(cmd)
	return cmd
}

// CommandContext is the exec.CommandContext equivalent.
func CommandContext(ctx context.Context, name string, arg ...string) *exec.Cmd {
	//nolint:forbidigo // only legal exec.CommandContext call site
	cmd := exec.CommandContext(ctx, name, arg...)
	hideWindow(cmd)
	return cmd
}
