package process

import (
	"context"
	"runtime"
	"strings"
	"testing"

	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
)

// A provider launched without zzrouter-launcher starts and serves
// normally; the only symptom is that stopping it later does not work,
// because nothing is tracking its PID or forwarding it a signal. That
// makes silence here expensive: the failure surfaces minutes or days
// after the launch that caused it, on a completely different command.
//
// verifyLauncher already logs a hash mismatch. A launcher that is
// simply absent fails os.Stat and LookPath without a word, which is
// the common case -- a hand-built tree, a partial copy, a release
// archive that shipped one binary and not the other.
func TestLaunch_SaysSoWhenTheLauncherIsMissing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX command path for the child process")
	}
	// No launcher on PATH, and none beside the test binary either, so
	// both lookups miss regardless of what this machine has installed.
	t.Setenv("PATH", t.TempDir())

	inst := &instance.Instance{
		ID:       "test-instance",
		Provider: "vllm",
		Logs:     make(chan string, 8),
	}

	l := NewLauncher(nil, true, "")
	res, err := l.Launch(context.Background(), inst, "/bin/echo", []string{"hello"}, nil, nil)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	t.Cleanup(func() {
		if res != nil && res.Process != nil {
			_, _ = res.Wait()
		}
	})

	if inst.IsUsingLauncher() {
		t.Fatal("instance reports it is using a launcher that is not on this machine")
	}

	var said bool
	for len(inst.Logs) > 0 {
		if strings.Contains(<-inst.Logs, "Launcher unavailable") {
			said = true
		}
	}
	if !said {
		t.Error("a process launched with no PID tracking said nothing about it in its own log")
	}
}
