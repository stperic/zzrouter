package process

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	processinfo "github.com/shirou/gopsutil/v4/process"
	"github.com/stretchr/testify/assert"
)

// The probe exists because a version comparison cannot tell a managed
// daemon from a stranger reporting the same version. Its own answers have
// to be equally careful: "absent" is a claim about the process table, and
// must never be returned when the process table could not be read.
func TestBinaryPresence(t *testing.T) {
	t.Run("finds the process running this test", func(t *testing.T) {
		self, err := os.Executable()
		if err != nil {
			t.Skip("cannot resolve own executable on this platform")
		}

		assert.Equal(t, PresenceRunning, BinaryPresence(self),
			"the test binary is running, so its own path must be found")
		if runtime.GOOS == "windows" {
			assert.Equal(t, PresenceRunning, BinaryPresence(strings.ToUpper(self)),
				"Windows executable paths must match independently of letter case")
		}
	})

	t.Run("a binary that is not running is absent", func(t *testing.T) {
		notRunning := filepath.Join(t.TempDir(), "definitely-not-running")

		assert.Equal(t, PresenceAbsent, BinaryPresence(notRunning))
	})

	// An empty path means the caller had no managed install to name.
	// Reporting that as absent would turn "I have nothing to look for"
	// into "I looked and found nothing".
	t.Run("no path to look for is unknown, not absent", func(t *testing.T) {
		assert.Equal(t, PresenceUnknown, BinaryPresence(""))
	})

	t.Run("resolves through a symlink to the real binary", func(t *testing.T) {
		self, err := os.Executable()
		if err != nil {
			t.Skip("cannot resolve own executable on this platform")
		}
		link := filepath.Join(t.TempDir(), "linked")
		if err := os.Symlink(self, link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if runtime.GOOS == "windows" {
			resolved, resolveErr := filepath.EvalSymlinks(link)
			proc, procErr := processinfo.NewProcess(int32(os.Getpid()))
			if procErr != nil {
				t.Fatalf("own process: %v", procErr)
			}
			exe, exeErr := proc.Exe()
			t.Logf("PID=%d self=%q link=%q resolved=%q resolveErr=%v processExe=%q exeErr=%v canonicalExe=%q canonicalLink=%q",
				os.Getpid(), self, link, resolved, resolveErr, exe, exeErr, canonicalExecutablePath(exe), canonicalExecutablePath(link))
		}

		assert.Equal(t, PresenceRunning, BinaryPresence(link),
			"a managed install reached through a symlink is still the managed install")
	})
}

// selfWithReadableEnv returns this test binary's path, skipping when the
// platform cannot read a process environment at all. gopsutil implements
// Environ on Linux and Windows only; on darwin it reports unsupported,
// which is exactly the case EnvStateUnknown exists to carry.
func selfWithReadableEnv(t *testing.T) (self string, entry string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Skip("cannot resolve own executable on this platform")
	}
	match, err := findByBinary(self)
	if err != nil || match.proc == nil {
		t.Skip("cannot find own process in the process table")
	}
	environ, err := match.proc.Environ()
	if err != nil || len(environ) == 0 {
		t.Skip("reading a process environment is unsupported on this platform")
	}
	for _, e := range environ {
		if strings.Contains(e, "=") {
			return self, e
		}
	}
	t.Skip("own process reports no usable environment entries")
	return "", ""
}

// A managed daemon's environment is baked into the command that starts it,
// so an edited config and a running process that predates it are otherwise
// indistinguishable from the API. This is the probe that separates them.
func TestBinaryEnvState(t *testing.T) {
	t.Run("a value the process was started with is current", func(t *testing.T) {
		self, entry := selfWithReadableEnv(t)
		k, v, _ := strings.Cut(entry, "=")

		assert.Equal(t, EnvStateCurrent, BinaryEnvState(self, map[string]string{k: v}))
	})

	t.Run("a different value for a key it has is stale", func(t *testing.T) {
		self, entry := selfWithReadableEnv(t)
		k, v, _ := strings.Cut(entry, "=")

		assert.Equal(t, EnvStateStale, BinaryEnvState(self, map[string]string{k: v + "-changed"}))
	})

	t.Run("a key the process does not have at all is stale", func(t *testing.T) {
		self, _ := selfWithReadableEnv(t)

		assert.Equal(t, EnvStateStale,
			BinaryEnvState(self, map[string]string{"ZZROUTER_ABSENT_TEST_KEY": "x"}))
	})

	// The config tree gives `key: ""` the distinct meaning "set it to
	// empty", so a missing key must not compare equal to an empty one --
	// a plain map lookup would report both as "".
	t.Run("a key declared empty is checked for presence", func(t *testing.T) {
		self, _ := selfWithReadableEnv(t)

		assert.Equal(t, EnvStateStale,
			BinaryEnvState(self, map[string]string{"ZZROUTER_ABSENT_TEST_KEY": ""}))
	})

	// Every case below is a statement about the CALLER's inputs, not about
	// the process, so none of them may answer "stale" -- that would report
	// drift the probe never observed.
	t.Run("each not-current cause names itself", func(t *testing.T) {
		self, err := os.Executable()
		if err != nil {
			t.Skip("cannot resolve own executable on this platform")
		}

		assert.Equal(t, EnvStateNotRunning, BinaryEnvState("", map[string]string{"A": "1"}),
			"no managed install to name")
		assert.Equal(t, EnvStateUnconfigured, BinaryEnvState(self, nil),
			"a provider that declares no environment cannot have drifted from it")
		assert.Equal(t, EnvStateUnconfigured, BinaryEnvState(self, map[string]string{}))
	})

	t.Run("a binary that is not running says so", func(t *testing.T) {
		notRunning := filepath.Join(t.TempDir(), "definitely-not-running")

		assert.Equal(t, EnvStateNotRunning, BinaryEnvState(notRunning, map[string]string{"A": "1"}),
			"and NOT stale -- there is no process that could have drifted")
	})
}

// The comparison is split out from BinaryEnvState because the process
// read behind it only works on Linux and Windows, so on a macOS dev
// machine every subtest that needs a real environ skips -- leaving the
// rule itself unexercised on the platform it is written on.
func TestEnvironHas(t *testing.T) {
	t.Parallel()

	environ := []string{"PATH=/usr/bin", "OLLAMA_NUM_PARALLEL=2", "EMPTY=", "ODD"}

	for name, tc := range map[string]struct {
		want map[string]string
		ok   bool
	}{
		"exact match":              {map[string]string{"OLLAMA_NUM_PARALLEL": "2"}, true},
		"several at once":          {map[string]string{"PATH": "/usr/bin", "EMPTY": ""}, true},
		"nothing wanted":           {map[string]string{}, true},
		"different value":          {map[string]string{"OLLAMA_NUM_PARALLEL": "3"}, false},
		"key absent entirely":      {map[string]string{"NOT_SET": "x"}, false},
		"extra vars are not drift": {map[string]string{"PATH": "/usr/bin"}, true},

		// The config tree gives `key: ""` the meaning "set it to empty",
		// so a key the process lacks must not compare equal to one it
		// holds as empty -- a bare map lookup reports both as "".
		"declared empty and set empty": {map[string]string{"EMPTY": ""}, true},
		"declared empty but never set": {map[string]string{"MISSING": ""}, false},

		// A malformed entry with no "=" is skipped rather than treated as
		// a key with an empty value.
		"entry without a separator": {map[string]string{"ODD": ""}, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.ok, environHas(environ, tc.want))
		})
	}
}

// Where the environment can be read, a missing declared value is drift.
// Where it cannot, the answer names that rather than "not running", which
// would make a caller treat a healthy provider as broken.
func TestBinaryEnvStateNamesTheReasonItCannotTell(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Skip("cannot resolve own executable on this platform")
	}

	got := BinaryEnvState(self, map[string]string{"PATH": "/nonexistent-on-purpose"})

	switch runtime.GOOS {
	case "linux", "windows", "darwin":
		assert.Equal(t, EnvStateStale, got, "the environment is readable here, so this is a real comparison")
	default:
		assert.Equal(t, EnvStateUnsupported, got,
			"the process IS running; the platform is what cannot answer")
	}
}
