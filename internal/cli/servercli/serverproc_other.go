//go:build !linux

package servercli

// isServerProcess reports whether pid is a node that is actually
// serving.
//
// Only Linux can answer this, by reading /proc/<pid>/cmdline, and only
// Linux needs to: the privileged updater that runs this same binary
// alongside a starting node is a systemd arrangement. Everywhere else
// the executable name is the whole answer, exactly as before.
func isServerProcess(int) bool { return true }
