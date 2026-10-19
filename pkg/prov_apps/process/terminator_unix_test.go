//go:build unix

package process

import (
	"bufio"
	"context"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/host"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stretchr/testify/require"
)

func TestTerminateGroupHelper(t *testing.T) {
	if !slices.Contains(os.Args, "zzrouter-group-helper") {
		return
	}
	if slices.Contains(os.Args, "ignore-term") {
		signal.Ignore(syscall.SIGTERM)
	}
	if n := slices.Index(os.Args, "pid-file"); n >= 0 {
		if err := os.WriteFile(os.Args[n+1], []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
			os.Exit(2)
		}
	}
	_, _ = io.WriteString(os.Stdout, "ready\n")
	_, _ = io.Copy(io.Discard, os.Stdin)
}

func groupMember(t *testing.T, group int, ignoreTerm bool) (int, io.WriteCloser, <-chan error) {
	t.Helper()
	binary, err := os.Executable()
	require.NoError(t, err)
	cmd := host.Command(binary, "-test.run=^TestTerminateGroupHelper$", "--", "zzrouter-group-helper")
	if ignoreTerm {
		cmd.Args = append(cmd.Args, "ignore-term")
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: group}
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	done := make(chan error, 1)
	go func() { done <- cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("group member was not reaped")
		}
	})
	ready, err := bufio.NewReader(stdout).ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "ready\n", ready)
	return cmd.Process.Pid, stdin, done
}

func TestTerminateWaitsForProcessGroup(t *testing.T) {
	leader, _, leaderDone := groupMember(t, 0, false)
	child, stdin, childDone := groupMember(t, leader, true)
	token, err := StartToken(leader)
	require.NoError(t, err)
	stopped := make(chan error, 1)
	go func() {
		stopped <- Terminate(context.Background(), TerminateRequest{PID: leader, StartToken: token, ProcessGroupID: leader, Graceful: 5 * time.Second})
	}()
	<-leaderDone
	require.True(t, ProcessExists(int32(child)))
	select {
	case err := <-stopped:
		t.Fatalf("termination returned before the group drained: %v", err)
	case <-time.After(400 * time.Millisecond):
	}
	require.NoError(t, stdin.Close())
	require.NoError(t, <-childDone)
	require.NoError(t, <-stopped)
	require.ErrorIs(t, syscall.Kill(-leader, 0), syscall.ESRCH)
}

func TestTerminateForceKillsSurvivingGroup(t *testing.T) {
	leader, _, leaderDone := groupMember(t, 0, false)
	_, _, childDone := groupMember(t, leader, true)
	token, err := StartToken(leader)
	require.NoError(t, err)
	require.NoError(t, Terminate(t.Context(), TerminateRequest{PID: leader, StartToken: token, ProcessGroupID: leader, Graceful: 400 * time.Millisecond}))
	<-leaderDone
	select {
	case err := <-childDone:
		var exit *exec.ExitError
		require.ErrorAs(t, err, &exit)
		require.Equal(t, syscall.SIGKILL, exit.Sys().(syscall.WaitStatus).Signal())
	case <-time.After(time.Second):
		t.Fatal("surviving group member was not killed")
	}
	require.ErrorIs(t, syscall.Kill(-leader, 0), syscall.ESRCH)
}

func TestTerminateWaitsForWrappedProcessGroup(t *testing.T) {
	dir := t.TempDir()
	wrapper := os.Getenv("ZZROUTER_TEST_LAUNCHER")
	if wrapper == "" {
		wrapper = filepath.Join(dir, "zzrouter-launcher")
		output, err := host.Command("go", "build", "-o", wrapper, "../../../cmd/zzrouter-launcher").CombinedOutput()
		require.NoError(t, err, "%s", output)
	}
	body, err := os.ReadFile(wrapper)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "zzrouter-launcher"), body, 0700))
	t.Setenv("PATH", dir)
	binary, err := os.Executable()
	require.NoError(t, err)
	pidFile := filepath.Join(dir, "engine.pid")
	inst := instance.NewInstance("wrapped-stop", "test", "model", 0, 0, 8)
	result, err := NewLauncher(&SecurityConfig{}, true, dir).Launch(t.Context(), inst, binary,
		[]string{"-test.run=^TestTerminateGroupHelper$", "--", "zzrouter-group-helper", "pid-file", pidFile}, nil, nil)
	require.NoError(t, err)
	done := make(chan struct{})
	go func() { _, _ = result.Wait(); close(done) }()
	t.Cleanup(func() { _ = syscall.Kill(-result.PID, syscall.SIGKILL); <-done })
	require.Eventually(t, func() bool { _, err := os.Stat(pidFile); return err == nil }, 5*time.Second, 10*time.Millisecond)
	require.True(t, inst.IsUsingLauncher())
	require.Equal(t, result.PID, result.ProcessGroupID)
	pid, err := os.ReadFile(pidFile)
	require.NoError(t, err)
	engine, err := strconv.Atoi(string(pid))
	require.NoError(t, err)
	group, err := syscall.Getpgid(engine)
	require.NoError(t, err)
	require.Equal(t, result.ProcessGroupID, group)
	_, stdin, childDone := groupMember(t, group, true)
	stopped := make(chan error, 1)
	go func() {
		stopped <- Terminate(context.Background(), TerminateRequest{PID: result.PID, StartToken: result.StartToken, ProcessGroupID: result.ProcessGroupID, Graceful: 5 * time.Second})
	}()
	<-done
	select {
	case err := <-stopped:
		t.Fatalf("wrapped stop returned before the group drained: %v", err)
	case <-time.After(400 * time.Millisecond):
	}
	require.NoError(t, stdin.Close())
	require.NoError(t, <-childDone)
	require.NoError(t, <-stopped)
}
