//go:build !windows

package host

import "os/exec"

func hideWindow(_ *exec.Cmd) {}
