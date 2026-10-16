//go:build unix

package process

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/host"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLaunchTokenReadFailureStopsTree(t *testing.T) {
	dir := t.TempDir()
	wrapper := os.Getenv("ZZROUTER_TEST_LAUNCHER")
	if wrapper == "" {
		wrapper = filepath.Join(dir, "zzrouter-launcher")
		output, err := host.Command("go", "build", "-o", wrapper, "../../../cmd/zzrouter-launcher").CombinedOutput()
		require.NoError(t, err, "%s", output)
	}
	data, err := os.ReadFile(wrapper)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "zzrouter-launcher"), data, 0700))
	for _, wrapped := range []bool{false, true} {
		t.Run(strconv.FormatBool(wrapped), func(t *testing.T) {
			path := t.TempDir()
			if wrapped {
				path = dir
			}
			t.Setenv("PATH", path)
			pidFile := filepath.Join(t.TempDir(), "engine.pid")
			inst := &instance.Instance{ID: "failed-token", Provider: "test", Logs: make(chan string, 8)}
			launcher := NewLauncher(&SecurityConfig{}, true, t.TempDir())
			failure := errors.New("token read failed")
			var leader int
			var engine int32
			var memberDone <-chan error
			launcher.startToken = func(pid int) (int64, error) {
				leader = pid
				assert.Eventually(t, func() bool {
					contents, readErr := os.ReadFile(pidFile)
					parsed, parseErr := strconv.ParseInt(string(contents), 10, 32)
					engine = int32(parsed)
					return readErr == nil && parseErr == nil && engine > 1
				}, 5*time.Second, 5*time.Millisecond, "engine must start before inspection fails")
				t.Cleanup(func() {
					if engine > 1 {
						_ = syscall.Kill(int(engine), syscall.SIGKILL)
					}
				})
				if !wrapped {
					// Parent this group member here so the fixture can reap it on Linux too.
					binary, exeErr := os.Executable()
					assert.NoError(t, exeErr)
					member := host.Command(binary, "-test.run=TestIdentityHelper", "--", "zzrouter-identity-helper", "ignore-term")
					member.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: pid}
					stdin, pipeErr := member.StdinPipe()
					assert.NoError(t, pipeErr)
					stdout, pipeErr := member.StdoutPipe()
					assert.NoError(t, pipeErr)
					if startErr := member.Start(); !assert.NoError(t, startErr) {
						return 0, failure
					}
					ready, readErr := bufio.NewReader(stdout).ReadString('\n')
					assert.NoError(t, readErr)
					assert.Equal(t, "ready\n", ready)
					wait := make(chan error, 1)
					memberDone = wait
					go func() { wait <- member.Wait(); close(wait) }()
					t.Cleanup(func() { _ = stdin.Close(); _ = member.Process.Kill(); <-wait })
				}
				return 0, failure
			}
			result, launchErr := launcher.Launch(t.Context(), inst, "/bin/sh", []string{"-c", `trap '' TERM INT; printf '%s' "$$" > "$1"; while :; do :; done`, "engine", pidFile}, nil, nil)
			assert.ErrorIs(t, launchErr, failure)
			assert.Nil(t, result)
			assert.Equal(t, wrapped, inst.IsUsingLauncher())
			require.Greater(t, leader, 1)
			assert.False(t, ProcessExists(int32(leader)), "failed launch must reap the leader")
			require.Greater(t, engine, int32(1))
			assert.False(t, ProcessExists(engine), "engine survived token-read failure")
			if memberDone != nil {
				select {
				case <-memberDone:
				case <-time.After(2 * time.Second):
					t.Error("process group member survived token-read failure")
				}
			}
		})
	}
}
