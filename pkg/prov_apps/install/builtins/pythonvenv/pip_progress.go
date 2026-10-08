package pythonvenv

import (
	"regexp"
	"strconv"
	"strings"
)

// makePipProgressHook returns a Step.StdoutLine function that feeds pip
// stdout into pipProgressState and mirrors every parsed update onto
// the installer's progress handle. Snapshots i.Progress at plan-build
// time so a later SetProgress(nil) in the coordinator's deferred
// cleanup doesn't nil-deref a running scanner.
func (i *Installer) makePipProgressHook() func(string) {
	p := i.Progress
	if p == nil {
		// No handle attached — no-op hook. Plan inspection / tests
		// shouldn't pay any cost.
		return nil
	}
	var state pipProgressState
	return func(line string) {
		update, ok := state.onLine(line)
		if !ok {
			return
		}
		// Mirror to the progress handle. SetDownloadProgress fans out
		// to the jobs stream via InstallProgress.SetJobHandle wiring
		// done in InstallCoordinator.runInstall.
		p.SetDownloadProgress(update.Done, update.Total, update.Percent)
		if update.Desc != "" {
			// Re-emit the current step with an updated description so
			// SSE subscribers see "Downloading torch (1.2 GB)" rather
			// than a stale "Install vllm" label.
			p.SetStep(p.Step, p.TotalSteps, update.Desc)
		}
	}
}

// pip's output when stdout is a pipe (no TTY) is line-oriented:
//
//	Collecting torch==2.5.1
//	  Downloading torch-2.5.1-cp310-cp310-manylinux1_x86_64.whl.metadata (28 kB)
//	Collecting vllm==0.7.0
//	  Downloading vllm-0.7.0-cp310-cp310-manylinux1_x86_64.whl (1234.0 MB)
//	Successfully installed torch-2.5.1 vllm-0.7.0 ...
//
// No byte-by-byte progress bars (pip auto-disables the progress bar
// without a TTY). We infer progress from:
//   * "Collecting" lines → total package count grows as deps resolve
//   * "Downloading …whl (N MB)" → byte size for the current package
//   * "Successfully installed a b c" → terminal marker
//
// This is file-level granularity, not byte-level inside a single wheel
// — that would require pty allocation. File-level is enough to escape
// the "empty N-minute bar" UX that was the real papercut.

// reDownloadingWheel matches lines like
// "  Downloading torch-2.5.1-cp310-cp310-manylinux1_x86_64.whl (1234.0 MB)"
// (leading whitespace is variable, units are B / kB / MB / GB).
var reDownloadingWheel = regexp.MustCompile(
	`^\s*Downloading\s+(\S+\.whl)\s+\(([\d.]+)\s*([KkMGTmgt]?B)\)\s*$`,
)

// reCollecting matches the announcement line for a new package. Version
// spec is optional (e.g. "Collecting vllm" without pin). We capture the
// package name so the TUI can show which dep is being resolved.
var reCollecting = regexp.MustCompile(`^Collecting\s+(\S+)`)

// reSuccessfullyInstalled is the terminal marker. pip emits this exactly
// once near the end of a successful install. Not currently used for
// progress math but kept so a future pass can flip the step to
// "installing from cache" after it appears (wheels already resolved).
var reSuccessfullyInstalled = regexp.MustCompile(`^Successfully installed\s+`)

// reProgressBar matches pip's live within-file progress line emitted
// when pip is attached to a TTY (via Step.UsePTY=true). Examples:
//
//	"  ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━ 567.8/1234.0 MB 32.0 MB/s eta 0:00:21"
//	"  ━━━━━━━━━━━━━━━━━━━━━━ 12.3 MB 4.5 MB/s eta 0:00:05"
//
// Captured: (done) (done-unit) (total)? (total-unit)?. The total is
// optional because the first few ticks before pip has the full
// content-length only emit a single running counter.
var reProgressBar = regexp.MustCompile(
	`\s([\d.]+)\s*([KkMGTmgt]?B)(?:/([\d.]+)\s*([KkMGTmgt]?B))?\s`,
)

// unitBytes maps pip's unit suffix to a byte multiplier. pip uses
// base-10 (MB = 1e6) per PEP standards. Returns 0 for unknown units.
func unitBytes(u string) int64 {
	switch strings.ToUpper(u) {
	case "B":
		return 1
	case "KB":
		return 1_000
	case "MB":
		return 1_000_000
	case "GB":
		return 1_000_000_000
	case "TB":
		return 1_000_000_000_000
	}
	return 0
}

// pipProgressState tracks cumulative bytes observed across pip's
// "Downloading foo.whl (N MB)" announcements plus the per-tick bar
// lines pip emits under a TTY. When Step.UsePTY is true, progress
// bar lines advance `currentDone` within the active wheel; otherwise
// only file-level announcements contribute.
type pipProgressState struct {
	totalBytes     int64 // sum of announced file sizes across completed wheels
	downloadedWhls int   // count of Downloading lines seen
	collecting     []string
	currentTotal   int64  // announced size of the in-flight wheel
	currentDone    int64  // latest bar reading for the in-flight wheel
	lastEmitted    int64  // last emitted (totalBytes + currentDone) — dedupe repeat ticks
	currentFile    string // name of the in-flight wheel
}

// pipProgressUpdate is what the parser emits when a line advances
// state. Callers translate to InstallProgress.SetDownloadProgress.
type pipProgressUpdate struct {
	Desc    string
	Done    int64 // cumulative bytes announced so far
	Total   int64 // same as Done — pip gives no forward estimate without resolving all deps first
	Percent int   // 0 for in-progress; set to 99 on "Successfully installed"
}

// onLine processes a single stdout/stderr line from pip. Returns the
// update + true when a line advances progress; zero + false otherwise.
func (s *pipProgressState) onLine(line string) (pipProgressUpdate, bool) {
	if m := reDownloadingWheel.FindStringSubmatch(line); m != nil {
		// New wheel starting — flush the previous one's remainder into
		// totalBytes and reset the in-flight counter.
		if s.currentTotal > 0 {
			s.totalBytes += s.currentTotal // count whatever we didn't finish as done
		}
		filename := m[1]
		size, _ := strconv.ParseFloat(m[2], 64)
		unit := unitBytes(m[3])
		fileBytes := int64(size * float64(unit))
		s.downloadedWhls++
		s.currentTotal = fileBytes
		s.currentDone = 0
		s.currentFile = filename
		return pipProgressUpdate{
			Desc:  "Downloading " + filename,
			Done:  s.totalBytes,
			Total: s.totalBytes + fileBytes,
		}, true
	}
	if m := reCollecting.FindStringSubmatch(line); m != nil {
		pkg := m[1]
		s.collecting = append(s.collecting, pkg)
		return pipProgressUpdate{
			Desc:  "Collecting " + pkg,
			Done:  s.totalBytes,
			Total: s.totalBytes + s.currentTotal,
		}, true
	}
	if reSuccessfullyInstalled.MatchString(line) {
		// Final flush — any in-flight wheel is now complete.
		if s.currentTotal > 0 {
			s.totalBytes += s.currentTotal
			s.currentTotal = 0
			s.currentDone = 0
		}
		return pipProgressUpdate{
			Desc:    "Finalizing install",
			Done:    s.totalBytes,
			Total:   s.totalBytes,
			Percent: 99,
		}, true
	}
	// Within-file progress bar (TTY path only). Gated by an active
	// download so random numeric-unit lines don't spoof progress.
	if s.currentTotal > 0 {
		if m := reProgressBar.FindStringSubmatch(line); m != nil {
			doneVal, _ := strconv.ParseFloat(m[1], 64)
			doneU := unitBytes(m[2])
			if doneU == 0 {
				return pipProgressUpdate{}, false
			}
			s.currentDone = int64(doneVal * float64(doneU))
			// Dedupe identical consecutive readings — pip re-emits the
			// same bar state occasionally and the parser shouldn't
			// flood the jobs stream.
			total := s.totalBytes + s.currentTotal
			done := s.totalBytes + s.currentDone
			if done == s.lastEmitted {
				return pipProgressUpdate{}, false
			}
			s.lastEmitted = done
			desc := "Downloading " + s.currentFile
			if desc == "Downloading " {
				desc = "Downloading"
			}
			return pipProgressUpdate{
				Desc:  desc,
				Done:  done,
				Total: total,
			}, true
		}
	}
	return pipProgressUpdate{}, false
}
