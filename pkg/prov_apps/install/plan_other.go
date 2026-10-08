//go:build !windows

package install

import (
	"context"
	"os/exec"
)

// newWindowsShellCommand is only called on Windows. This stub exists so
// non-Windows builds type-check cleanly despite the reference in plan.go's
// shellCommand(). Compile-only dead code on Unix.
func newWindowsShellCommand(_ context.Context, _ string) (*exec.Cmd, error) {
	panic("newWindowsShellCommand called on non-Windows platform")
}
