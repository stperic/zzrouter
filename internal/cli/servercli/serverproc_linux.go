//go:build linux

package servercli

import (
	"bytes"
	"fmt"
	"os"
	"slices"
)

// serverSubcommands are the argv words that mean "this process is
// serving", as opposed to running some other subcommand out of the same
// binary.
var serverSubcommands = []string{"start", "serve", "run-server"}

// isServerProcess reports whether pid is a node that is actually
// serving, as opposed to any process that happens to run the same
// binary.
//
// The distinction is load-bearing on Linux because the privileged
// updater IS this binary: systemd starts
// `/opt/zzrouter/bin/zzrouter-node update run` as root, and it stays
// alive across the restart it performs so it can watch the node come
// back. Matching on the executable name alone, the restarted node sees
// the updater, decides a server is already running, and exits 1 --
// forever, since the updater is waiting for exactly the health check
// that will now never pass. The node restart-loops, the update times
// out, and it gets rolled back for a fault that was never in it.
//
// /proc/<pid>/cmdline is the argv, NUL-separated, so it can answer the
// question the executable name cannot.
func isServerProcess(pid int) bool {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		// The process may have exited, or be one this user cannot read.
		// Fall back to the old answer: treat it as a server, because
		// refusing to start is the safe direction when the alternative
		// is two servers on one host.
		return true
	}

	return argvIsServer(raw)
}

// argvIsServer reads a NUL-separated argv and reports whether it names
// a server subcommand.
//
// Split out from the /proc read so the decision is testable without
// having to keep a process alive long enough to look at, which is its
// own source of flakes: a process that has exited but not been reaped
// has an empty cmdline and no read error.
//
// argv[0] is skipped. It is the path to the binary, and on a managed
// install the updater's argv[0] is the same
// /opt/zzrouter/versions/<v>/bin/zzrouter-node as the node's -- which
// is the whole reason the executable name could not answer this.
func argvIsServer(raw []byte) bool {
	raw = bytes.TrimRight(raw, "\x00")
	if len(raw) == 0 {
		// A process with no readable argv is not serving. It is on its
		// way out: this is what a zombie looks like.
		return false
	}
	args := bytes.Split(raw, []byte{0})
	for _, arg := range args[1:] {
		if slices.Contains(serverSubcommands, string(arg)) {
			return true
		}
	}
	return false
}
