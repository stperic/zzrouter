//go:build !windows && !darwin && !linux

package interpreter

import "fmt"

// installHintForTarget on non-darwin/non-linux Unix (FreeBSD, NetBSD,
// OpenBSD, Solaris) falls back to a generic "install python{minor}"
// phrasing — these platforms aren't a primary zzRouter target and
// distro-specific guidance would be more misleading than helpful.
func installHintForTarget(target string) string {
	return fmt.Sprintf(
		"Install Python %s using your platform's package manager\n"+
			"(the binary should be named `python%s` on PATH).",
		target, target,
	)
}
