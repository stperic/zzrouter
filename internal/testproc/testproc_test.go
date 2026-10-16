package testproc

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/host"
	"github.com/stretchr/testify/require"
)

const helperEnv = "TESTPROC_HELPER"

// TestHelperProcess is a stand-in owner (blocks on stdin) or an owned helper.
func TestHelperProcess(t *testing.T) {
	switch os.Getenv(helperEnv) {
	case "owner":
		_, _ = io.Copy(io.Discard, os.Stdin)
	case "owned":
		AwaitOwner()
	}
}

// startOwner runs a stand-in test process; end stops it and reaps it, since
// a zombie still answers kill -0.
func startOwner(t *testing.T) (pid int, end func()) {
	t.Helper()
	owner := host.Command(os.Args[0], "-test.run=^TestHelperProcess$")
	owner.Env = append(os.Environ(), helperEnv+"=owner")
	stdin, err := owner.StdinPipe()
	require.NoError(t, err)
	require.NoError(t, owner.Start())
	end = func() { _ = stdin.Close(); _ = owner.Wait() }
	t.Cleanup(end)
	return owner.Process.Pid, end
}

// requireEndsWithOwner requires fixture to run while its owner runs and to end after.
func requireEndsWithOwner(t *testing.T, fixture *exec.Cmd, endOwner func()) {
	t.Helper()
	require.NoError(t, fixture.Start())
	done := make(chan error, 1)
	go func() { done <- fixture.Wait() }()
	t.Cleanup(func() { _ = fixture.Process.Kill() })
	select {
	case err := <-done:
		t.Fatalf("fixture ended while its owner runs: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	endOwner()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("fixture outlived its owner")
	}
}

func TestScriptEndsWithItsOwner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixture")
	}
	owner, end := startOwner(t)
	path := filepath.Join(t.TempDir(), "fake")
	require.NoError(t, os.WriteFile(path, []byte(script(owner, "while kill -0 $test_owner; do /bin/sleep 0.01; done\n")), 0o700))
	requireEndsWithOwner(t, host.Command(path), end)
}

func TestAwaitOwnerEndsWithItsOwner(t *testing.T) {
	owner, end := startOwner(t)
	helper := host.Command(os.Args[0], "-test.run=^TestHelperProcess$")
	helper.Env = append(os.Environ(), helperEnv+"=owned", ownerEnv+"="+strconv.Itoa(owner))
	requireEndsWithOwner(t, helper, end)
}
