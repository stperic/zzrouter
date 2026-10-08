//go:build !unix

package shared

import "os/exec"

// detachProcess is a no-op where the platform has no session concept to
// detach from; Windows launchers already survive the parent.
func detachProcess(_ *exec.Cmd) {}
