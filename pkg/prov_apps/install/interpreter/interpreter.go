// Package interpreter selects a Python interpreter that satisfies a
// provider's PythonRequirement range. It is a deliberate sibling of
// pkg/prov_apps/install/preflight — preflight stays a pure probe+report
// layer, while interpreter owns the "pick one" decision so the chosen
// path can flow directly into the installer without threading a chosen-
// path field through a reporting type.
//
// Background: the install incident that motivated this package was that
// `python3 -m venv` resolves `python3` via the shell's PATH at install
// time, and on macOS that alias tracks whatever minor Homebrew has
// chosen as its default `python` formula. When that alias ticks to a
// minor a runtime library hasn't been validated against (mlx_lm on
// 3.14 deadlocks inside lock_PyThread_acquire_lock mid-SSE stream),
// every new install is born broken. Select eliminates the drift by
// probing a candidate list, running `--version` on each, picking the
// highest that satisfies the provider's declared [Min, Max) range, and
// returning an absolute path the installer pins into the venv.
package interpreter

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/host"
)

// ErrNoMatch is returned by Select when no probed interpreter satisfies
// the requested PythonRequirement range. Wrapped with a hint message
// that names the missing minor range and the platform-appropriate fix.
var ErrNoMatch = errors.New("no Python interpreter satisfies requirement")

// Choice is the resolved outcome of a successful Select. Path is the
// stable alias used for venv creation and later integrity checks;
// Realpath is the dereferenced absolute path captured at selection
// time so a later brew point-upgrade that shifts Path → different
// underlying binary can be detected.
type Choice struct {
	Path     string // Stable alias (e.g., /opt/homebrew/opt/python@3.13/bin/python3.13).
	Realpath string // filepath.EvalSymlinks(Path) at selection time.
	Version  string // Parsed "3.13.4" — major.minor.patch.
}

// Options are the injection seams that let unit tests feed fake candidate
// lists + fake --version output without shelling out. Production callers
// pass a zero Options{} and get the platform defaults.
type Options struct {
	// Candidates overrides the probe list. Platform defaults when nil.
	Candidates []string
	// LookPath overrides exec.LookPath. Production: nil.
	LookPath func(name string) (string, error)
	// RunVersion overrides the `<path> --version` runner. Must return a
	// string shaped like "Python 3.13.4" (stderr/stdout combined is fine).
	// Production: nil.
	RunVersion func(ctx context.Context, path string) (string, error)
	// EvalSymlinks overrides filepath.EvalSymlinks. Production: nil.
	EvalSymlinks func(path string) (string, error)
}

// Select probes the candidate list in order and returns the Choice with
// the highest (major, minor, patch) tuple that satisfies req. A nil or
// zero-valued req means "any Python 3.x"; an empty result is an error.
//
// Ordering matters: candidates are tried once each, but the returned
// Choice is the highest-ranking *match*, not the first. That's what
// lets us prefer python3.13 over python3 even if both exist and both
// fall within range — we want the most specific alias so later PATH
// churn on `python3` doesn't redirect future operations.
func Select(ctx context.Context, req *config.PythonRequirement, opts Options) (Choice, error) {
	candidates := opts.Candidates
	if candidates == nil {
		candidates = defaultCandidates()
	}
	lookPath := opts.LookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	runVersion := opts.RunVersion
	if runVersion == nil {
		runVersion = runVersionDefault
	}
	evalSymlinks := opts.EvalSymlinks
	if evalSymlinks == nil {
		evalSymlinks = filepath.EvalSymlinks
	}

	min, minOK := parseMinorTuple(reqField(req, true))
	max, maxOK := parseMinorTuple(reqField(req, false))

	type found struct {
		path    string
		version string
		major   int
		minor   int
		patch   int
	}
	var matches []found
	seen := make(map[string]struct{}) // dedupe when python3 resolves to the same binary as python3.13
	for _, name := range candidates {
		path, err := lookPath(name)
		if err != nil {
			continue
		}
		abs, err := evalSymlinks(path)
		if err != nil {
			abs = path
		}
		if _, dup := seen[abs]; dup {
			continue
		}
		seen[abs] = struct{}{}

		probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
		out, err := runVersion(probeCtx, path)
		cancel()
		if err != nil {
			continue
		}
		ver, maj, min2, pat, ok := parseVersionOutput(out)
		if !ok {
			continue
		}
		if maj != pythonMajor {
			continue
		}
		if minOK && (maj < min.major || (maj == min.major && min2 < min.minor)) {
			continue
		}
		if maxOK && (maj > max.major || (maj == max.major && min2 >= max.minor)) {
			continue
		}
		matches = append(matches, found{path: path, version: ver, major: maj, minor: min2, patch: pat})
	}

	if len(matches) == 0 {
		return Choice{}, fmt.Errorf("%w: %s (checked: %s)",
			ErrNoMatch, describeRange(req), strings.Join(candidates, ", "))
	}

	best := matches[0]
	for _, m := range matches[1:] {
		if m.minor > best.minor || (m.minor == best.minor && m.patch > best.patch) {
			best = m
		}
	}
	realpath, err := evalSymlinks(best.path)
	if err != nil {
		realpath = best.path
	}
	return Choice{Path: best.path, Realpath: realpath, Version: best.version}, nil
}

// InstallHint returns an actionable, platform-aware message telling the
// user how to install a missing interpreter minor. Callers surface this
// through preflight when Select returns ErrNoMatch.
func InstallHint(req *config.PythonRequirement) string {
	target := preferredInstallTarget(req)
	return installHintForTarget(target)
}

// --- internals ---

// pythonMajor is the only major we support; 2.x is gone and 4.x isn't
// on the horizon. Keeping this named prevents reviewers from having to
// stare at bare 3s in the range logic wondering what the magic number means.
const pythonMajor = 3

// probeTimeout caps each `--version` call. 3s is generous — Python
// startup on macOS is sub-100ms cold — but safe against a pathologically
// slow NFS mount or a misconfigured interpreter that hangs on import.
const probeTimeout = 3 * time.Second

var versionRe = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)

type minorTuple struct {
	major int
	minor int
}

func parseMinorTuple(s string) (minorTuple, bool) {
	if s == "" {
		return minorTuple{}, false
	}
	m := versionRe.FindStringSubmatch(s + ".0") // tolerate "3.13" as well as "3.13.4"
	if len(m) < 3 {
		return minorTuple{}, false
	}
	maj, _ := strconv.Atoi(m[1])
	mn, _ := strconv.Atoi(m[2])
	return minorTuple{major: maj, minor: mn}, true
}

func parseVersionOutput(s string) (ver string, maj, mn, pat int, ok bool) {
	m := versionRe.FindStringSubmatch(s)
	if len(m) < 4 {
		return "", 0, 0, 0, false
	}
	maj, _ = strconv.Atoi(m[1])
	mn, _ = strconv.Atoi(m[2])
	pat, _ = strconv.Atoi(m[3])
	return fmt.Sprintf("%d.%d.%d", maj, mn, pat), maj, mn, pat, true
}

func reqField(req *config.PythonRequirement, wantMin bool) string {
	if req == nil {
		return ""
	}
	if wantMin {
		return req.Min
	}
	return req.Max
}

func describeRange(req *config.PythonRequirement) string {
	if req == nil || (req.Min == "" && req.Max == "") {
		return "any Python 3.x"
	}
	switch {
	case req.Min != "" && req.Max != "":
		return fmt.Sprintf(">=%s,<%s", req.Min, req.Max)
	case req.Min != "":
		return ">=" + req.Min
	default:
		return "<" + req.Max
	}
}

// runVersionDefault is the production --version runner. Isolated so the
// Options.RunVersion seam can swap the whole thing out under test.
func runVersionDefault(ctx context.Context, path string) (string, error) {
	out, err := host.CommandContext(ctx, path, "--version").CombinedOutput()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
