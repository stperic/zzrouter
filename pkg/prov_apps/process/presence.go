package process

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/shirou/gopsutil/v4/process"
)

// DaemonPIDs identifies only processes with the canonical binary and fixed argv.
// An unreadable matching executable refuses migration rather than guessing.
func DaemonPIDs(ctx context.Context, binary string, args []string) ([]int, error) {
	procs, err := process.ProcessesWithContext(ctx)
	if err != nil {
		return nil, err
	}
	want := canonicalExecutablePath(binary)
	resolved, err := filepath.EvalSymlinks(want)
	if err != nil {
		resolved = want
	}
	var pids []int
	for _, proc := range procs {
		exe, err := proc.ExeWithContext(ctx)
		if err != nil || exe == "" {
			continue
		}
		exe = canonicalExecutablePath(exe)
		if !sameExecutablePath(exe, want) && !sameExecutablePath(exe, resolved) {
			continue
		}
		argv, err := proc.CmdlineSliceWithContext(ctx)
		if err != nil || len(argv) == 0 {
			return nil, fmt.Errorf("cannot identify arguments for managed executable PID %d", proc.Pid)
		}
		if slices.Equal(argv[1:], args) {
			pids = append(pids, int(proc.Pid))
		}
	}
	return pids, ctx.Err()
}

func sameExecutablePath(left, right string) bool {
	left, right = filepath.Clean(left), filepath.Clean(right)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func canonicalExecutablePath(path string) string {
	path = filepath.Clean(path)
	if runtime.GOOS != "windows" {
		return path
	}
	// Windows image paths can retain 8.3 names while symlink resolution expands them.
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return filepath.Clean(stripWindowsPathPrefix(path))
}

func stripWindowsPathPrefix(path string) string {
	if rest, ok := strings.CutPrefix(path, `\\?\`); ok {
		path = rest
		if len(path) >= 4 && strings.EqualFold(path[:4], `UNC\`) {
			path = `\\` + path[4:]
		}
	}
	return path
}

// Presence answers whether a process is running from a particular binary.
//
// It is a closed enum rather than a bool because "we could not tell" is a
// real and common answer: reading another user's executable path is not
// permitted on Unix, so a scan can come back genuinely uninformative.
// Reporting that as false would assert the opposite of what was observed.
type Presence string

const (
	PresenceRunning Presence = "running"
	PresenceAbsent  Presence = "absent"
	PresenceUnknown Presence = "unknown"
)

// BinaryPresence reports whether any running process was started from
// binaryPath.
//
// This answers the question a version comparison cannot. A daemon started
// outside zzRouter reports whatever version it is, including the one we
// installed -- that is not hypothetical, it is how a foreign Ollama held
// port 11434 while the managed binary had exited, with every health check
// green. Asking who is actually running is the only probe that separates
// those two worlds.
//
// The permission limit matters less than it looks: the question is whether
// OUR binary is running, and our processes run as us, so the entries a scan
// cannot read are by construction not the ones being asked about. A
// squatter running as another user makes its own line unreadable without
// making our absence any less true.
//
// Cost is one full process enumeration, so this belongs on a single-provider
// lookup and not in a loop over every provider on the node.
func BinaryPresence(binaryPath string) Presence {
	if binaryPath == "" {
		return PresenceUnknown
	}
	match, err := findByBinary(binaryPath)
	if err != nil {
		return PresenceUnknown
	}
	switch {
	case match.proc != nil:
		return PresenceRunning
	case !match.readable:
		// Nothing at all could be read, so "absent" would be a statement
		// about our privileges rather than about the process table.
		return PresenceUnknown
	default:
		return PresenceAbsent
	}
}

// binaryMatch is the outcome of one scan of the process table: the process
// started from the binary if one is running, and whether the scan could
// read anything at all -- which is what separates "not running" from "not
// permitted to look".
type binaryMatch struct {
	proc     *process.Process
	readable bool
}

func findByBinary(binaryPath string) (binaryMatch, error) {
	procs, err := process.Processes()
	if err != nil {
		return binaryMatch{}, err
	}

	// Compare against the resolved path too, so a managed install reached
	// through a symlink (/usr/local/bin -> /opt/homebrew/...) still matches
	// the executable the kernel reports.
	want := canonicalExecutablePath(binaryPath)
	wantResolved := want
	if r, err := filepath.EvalSymlinks(want); err == nil {
		wantResolved = r
	}

	// First match wins. Two live processes from one binary is already a
	// broken state that Presence cannot describe either; picking one is
	// not the thing to fix here.
	var match binaryMatch
	for _, p := range procs {
		exe, err := p.Exe()
		if err != nil || exe == "" {
			continue // not ours to read, or already gone
		}
		match.readable = true
		exe = canonicalExecutablePath(exe)
		if sameExecutablePath(exe, want) || sameExecutablePath(exe, wantResolved) {
			match.proc = p
			return match, nil
		}
	}
	return match, nil
}

// EnvState answers whether the process running from a binary was started
// with the environment its provider config declares.
//
// It exists because a provider whose daemon zzRouter starts has no service
// manager to apply an environment change to: the values are baked into the
// start command, so an edit only takes effect on the next start. Without
// this, a config change and a running daemon that predates it look
// identical from the API.
//
// The not-current answers are separated rather than collapsed into one
// "unknown", because they call for different reactions and a caller that
// cannot tell them apart treats a healthy provider as a broken one:
// "unsupported" means this node cannot read the process, "not_running" is a
// different endpoint's problem, and "unconfigured" is nobody's problem.
// Only "stale" means a restart is owed.
type EnvState string

const (
	EnvStateCurrent EnvState = "current"
	EnvStateStale   EnvState = "stale"
	// EnvStateNotRunning: no process to compare against. Whether that is
	// expected is answered by Presence, not here.
	EnvStateNotRunning EnvState = "not_running"
	// EnvStateUnsupported: the process is there and its environment
	// cannot be read, by platform or by permission.
	EnvStateUnsupported EnvState = "unsupported"
	// EnvStateUnconfigured: the provider declares no environment, so
	// there is nothing it could have drifted from.
	EnvStateUnconfigured EnvState = "unconfigured"
)

// BinaryEnvState reports whether the process started from binaryPath
// carries every key/value in want.
//
// Variables the process has beyond want are not drift -- a daemon inherits
// far more than its config declares. Only a declared key that is missing,
// or holds a different value, proves the process predates the config. A
// key declared as "" is checked for presence, not just for value: the
// config tree gives `key: ""` the distinct meaning "set it to empty".
func BinaryEnvState(binaryPath string, want map[string]string) EnvState {
	if len(want) == 0 {
		return EnvStateUnconfigured
	}
	if binaryPath == "" {
		return EnvStateNotRunning
	}
	match, err := findByBinary(binaryPath)
	if err != nil || match.proc == nil {
		return EnvStateNotRunning
	}
	environ, err := match.proc.Environ()
	if err != nil || len(environ) == 0 {
		// A read that returned nothing is not evidence that the process
		// lacks the values, so it must not read as drift.
		return EnvStateUnsupported
	}
	if environHas(environ, want) {
		return EnvStateCurrent
	}
	return EnvStateStale
}

// environHas reports whether a process environment carries every pair in
// want. Split out from BinaryEnvState so the comparison is testable
// without a process to read: the only platform that can read one is not
// the one this is developed on.
func environHas(environ []string, want map[string]string) bool {
	got := make(map[string]string, len(environ))
	for _, entry := range environ {
		if k, v, ok := strings.Cut(entry, "="); ok {
			got[k] = v
		}
	}
	for k, v := range want {
		if have, ok := got[k]; !ok || have != v {
			return false
		}
	}
	return true
}
