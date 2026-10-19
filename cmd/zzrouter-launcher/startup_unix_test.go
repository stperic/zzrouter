//go:build unix

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stperic/zzrouter/internal/testproc"
	"github.com/stperic/zzrouter/pkg/host"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func buildFixtureLauncher(t *testing.T) string {
	t.Helper()
	if binary := os.Getenv("ZZROUTER_TEST_LAUNCHER"); binary != "" {
		return binary
	}
	binary := filepath.Join(t.TempDir(), "zzrouter-launcher")
	output, err := host.Command("go", "build", "-o", binary, ".").CombinedOutput()
	require.NoError(t, err, "%s", output)
	return binary
}

func TestLauncherStartupShutdown(t *testing.T) {
	binary := buildFixtureLauncher(t)
	for _, tc := range []struct {
		name       string
		signal     os.Signal
		closeStdin bool
		ignore     bool
		traps      []string
		exitCode   int
	}{
		{name: "TERM during startup", signal: syscall.SIGTERM, traps: []string{"TERM"}},
		{name: "INT during startup", signal: syscall.SIGINT, traps: []string{"INT"}},
		{name: "stdin closes during startup", closeStdin: true, traps: []string{"INT"}},
		{name: "parent closes stdin and sends TERM", closeStdin: true, signal: syscall.SIGTERM, traps: []string{"TERM", "INT"}},
		{name: "ignored TERM escalates", signal: syscall.SIGTERM, ignore: true, exitCode: 255},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			pidFile := filepath.Join(dir, "fixture.pid")
			childPIDFile := filepath.Join(dir, "child.pid")
			trapFile := filepath.Join(dir, "trapped")
			require.NoError(t, syscall.Mkfifo(pidFile+".tmp", 0o600))
			engine := filepath.Join(dir, "engine")
			script := "trap 'printf TERM > \"$2\"; exit 0' TERM\ntrap 'printf INT > \"$2\"; exit 0' INT\n"
			if tc.ignore {
				script = "trap '' TERM INT\n"
			}
			script += "printf '%s' \"$$\" > \"$1\"\nwhile kill -0 $test_owner; do :; done\n"
			require.NoError(t, os.WriteFile(engine, []byte(testproc.Script(script)), 0o600))
			cmd := host.Command(binary, "--instance-id=fixture", "--provider=fixture", "--pid-dir", dir,
				"--", "/bin/sh", engine, childPIDFile, trapFile)
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			var output bytes.Buffer
			cmd.Stdout, cmd.Stderr = &output, &output
			input, err := cmd.StdinPipe()
			require.NoError(t, err)
			t.Cleanup(func() { _ = input.Close() })
			require.NoError(t, cmd.Start())
			done := make(chan struct{})
			go func() {
				_ = cmd.Wait()
				close(done)
			}()
			t.Cleanup(func() {
				select {
				case <-done:
					return
				default:
					_ = cmd.Process.Kill()
				}
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Error("fixture launcher was not reaped")
				}
			})
			require.Eventually(t, func() bool {
				data, readErr := os.ReadFile(childPIDFile)
				pid, parseErr := strconv.Atoi(string(data))
				return readErr == nil && parseErr == nil && pid > 1
			}, 5*time.Second, 5*time.Millisecond, "child did not report its identity")
			data, err := os.ReadFile(childPIDFile)
			require.NoError(t, err)
			childPID, err := strconv.Atoi(string(data))
			require.NoError(t, err)
			group, err := syscall.Getpgid(childPID)
			require.NoError(t, err)
			require.Equal(t, childPID, group, "fixture child must own its process group")
			require.NotEqual(t, cmd.Process.Pid, group, "a standalone group leader must still isolate its child")
			t.Cleanup(func() { _ = syscall.Kill(-childPID, syscall.SIGKILL) })
			t.Logf("launcher=%d child=%d group=%d", cmd.Process.Pid, childPID, group)
			if tc.closeStdin {
				require.NoError(t, input.Close())
			}
			if tc.signal != nil {
				require.NoError(t, cmd.Process.Signal(tc.signal))
			}
			startToken, err := process.StartToken(cmd.Process.Pid)
			require.NoError(t, err)
			reader, err := os.OpenFile(pidFile+".tmp", os.O_RDONLY|syscall.O_NONBLOCK, 0)
			require.NoError(t, err)
			t.Cleanup(func() { _ = reader.Close() })

			select {
			case <-done:
			case <-time.After(8 * time.Second):
				t.Logf("launcher alive=%t child group alive=%t", syscall.Kill(cmd.Process.Pid, 0) == nil, syscall.Kill(-childPID, 0) == nil)
				t.Fatal("launcher did not finish bounded child shutdown")
			}
			metadata, err := io.ReadAll(reader)
			require.NoError(t, err)
			var fields map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(metadata, &fields))
			assert.Equal(t, strconv.FormatInt(startToken, 10), string(fields["start_token"]))
			assert.NotContains(t, fields, "start_time")
			var published pidInfo
			require.NoError(t, json.Unmarshal(metadata, &published))
			assert.Equal(t, cmd.Process.Pid, published.PID)
			assert.Equal(t, childPID, published.ChildPID)
			assert.Equal(t, startToken, published.StartToken)
			assert.Positive(t, published.StartToken)
			assert.Equal(t, tc.exitCode, cmd.ProcessState.ExitCode(), "%s", output.String())
			assert.Eventually(t, func() bool {
				return errors.Is(syscall.Kill(-childPID, 0), syscall.ESRCH)
			}, time.Second, 5*time.Millisecond, "the child's process group survived launcher exit")
			if len(tc.traps) != 0 {
				data, err = os.ReadFile(trapFile)
				assert.NoError(t, err)
				assert.Contains(t, tc.traps, string(data), "shutdown must reach the actual child")
			}
			_, err = os.Stat(pidFile)
			assert.True(t, errors.Is(err, os.ErrNotExist), "launcher must remove published PID metadata")
		})
	}
}

func TestLauncherExitCleanup(t *testing.T) {
	binary := buildFixtureLauncher(t)
	for _, tc := range []struct {
		name          string
		command       []string
		priorMetadata bool
		exitCode      int
	}{
		{name: "natural child exit", command: []string{"/bin/sh", "-c", "exit 7"}, exitCode: 7},
		{name: "child start fails", command: []string{"/no/such/fixture-engine"}, exitCode: 1},
		{name: "failed publication preserves prior metadata", command: []string{"/bin/sh", "-c", "exit 7"}, priorMetadata: true, exitCode: 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			pidFile := filepath.Join(dir, "fixture.pid")
			if tc.priorMetadata {
				require.NoError(t, os.WriteFile(pidFile, []byte("prior metadata"), 0o600))
				require.NoError(t, os.Mkdir(pidFile+".tmp", 0o700))
			}
			args := append([]string{"--instance-id=fixture", "--provider=fixture", "--pid-dir", dir, "--"}, tc.command...)
			cmd := host.Command(binary, args...)
			output, _ := cmd.CombinedOutput()
			require.NotNil(t, cmd.ProcessState, "%s", output)
			assert.Equal(t, tc.exitCode, cmd.ProcessState.ExitCode(), "%s", output)
			if tc.priorMetadata {
				data, err := os.ReadFile(pidFile)
				require.NoError(t, err)
				assert.Equal(t, "prior metadata", string(data))
				assert.Contains(t, string(output), "warning: failed to write pid file")
			} else {
				_, err := os.Stat(pidFile)
				assert.True(t, errors.Is(err, os.ErrNotExist), "launcher must remove its PID file on exit: %s", strings.TrimSpace(string(output)))
			}
		})
	}
}

func TestNodeOwnedLauncherCleansUpOnEOFAfterSignal(t *testing.T) {
	binary := buildFixtureLauncher(t)
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	engine := filepath.Join(dir, "engine")
	require.NoError(t, os.WriteFile(engine, []byte(testproc.Script("trap '' TERM\ntrap 'exit 0' INT\nprintf ready > \"$1\"\nwhile kill -0 $test_owner; do :; done\n")), 0600))
	cmd := host.Command(binary, "--node-owned", "--instance-id=orphan", "--provider=fixture", "--pid-dir", dir, "--", "/bin/sh", engine, ready)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	done := make(chan error, 1)
	go func() { done <- cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("node-owned launcher was not reaped")
		}
	})
	pidFile := filepath.Join(dir, "orphan.pid")
	require.Eventually(t, func() bool {
		if _, err := os.Stat(pidFile); err != nil {
			return false
		}
		_, err := os.Stat(ready)
		return err == nil
	}, 5*time.Second, 5*time.Millisecond)
	data, err := os.ReadFile(pidFile)
	require.NoError(t, err)
	var info pidInfo
	require.NoError(t, json.Unmarshal(data, &info))
	group, err := syscall.Getpgid(info.ChildPID)
	require.NoError(t, err)
	require.Equal(t, cmd.Process.Pid, group, "a node-owned child must stay in the node's group")
	require.NoError(t, cmd.Process.Signal(syscall.SIGTERM))
	select {
	case err := <-done:
		t.Fatalf("launcher exited before the node died: %v", err)
	case <-time.After(400 * time.Millisecond):
	}
	require.NoError(t, stdin.Close()) // The node died during the graceful wait.
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(7 * time.Second):
		t.Fatal("launcher ignored node death while waiting for its child")
	}
}

func TestLauncherBeforeChildStartHelper(t *testing.T) {
	ready := os.Getenv("LAUNCHER_BEFORE_START")
	if ready == "" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append(os.Args[:1], os.Args[i+1:]...)
			break
		}
	}
	flag.CommandLine = flag.NewFlagSet("launcher", flag.ExitOnError)
	os.Exit(run(func(cmd *exec.Cmd) error {
		if err := os.WriteFile(ready, nil, 0600); err != nil {
			return err
		}
		if _, err := io.ReadFull(os.Stdin, make([]byte, 1)); err != nil {
			return err
		}
		return cmd.Start()
	}))
}

func TestNodeOwnedLauncherSignalBeforeChildStart(t *testing.T) {
	dir := t.TempDir()
	ready, child := filepath.Join(dir, "ready"), filepath.Join(dir, "child")
	binary, err := os.Executable()
	require.NoError(t, err)
	cmd := host.Command(binary, "-test.run=^TestLauncherBeforeChildStartHelper$", "--", "--node-owned",
		"--instance-id=early-stop", "--provider=fixture", "--pid-dir", dir,
		"--", "/bin/sh", "-c", `printf '%s' "$$" > "$1"`, "engine", child)
	cmd.Env = append(os.Environ(), "LAUNCHER_BEFORE_START="+ready)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	done := make(chan error, 1)
	go func() { done <- cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("launcher was not reaped")
		}
	})
	require.Eventually(t, func() bool { _, err := os.Stat(ready); return err == nil }, 5*time.Second, 5*time.Millisecond)
	require.NoError(t, cmd.Process.Signal(syscall.SIGTERM))
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("launcher swallowed a stop before starting its child")
		_, _ = stdin.Write([]byte{1})
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("launcher did not finish after releasing child startup")
		}
	}
	status := cmd.ProcessState.Sys().(syscall.WaitStatus)
	require.True(t, status.Signaled(), "the stop must terminate the launcher before child startup")
	require.Equal(t, syscall.SIGTERM, status.Signal())
	require.NoFileExists(t, child, "a stopped launcher must never start its child")
}
