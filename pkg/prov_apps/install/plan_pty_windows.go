//go:build windows

package install

import (
	"context"
	"os/exec"
)

// runStepPTY on Windows falls back to the non-PTY streaming path.
// Windows does support pseudo-terminals via ConPTY, but the creack/pty
// wrapper doesn't expose it, and pulling in a ConPTY-specific shim
// (golang.org/x/term, go-conpty) adds dep weight for a use case that
// already works without live progress bars — pip's file-level
// "Downloading" announcements still flow through runStepStreaming.
func runStepPTY(ctx context.Context, cmd *exec.Cmd, onLine func(string)) error {
	return runStepStreaming(ctx, cmd, onLine)
}
