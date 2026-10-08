//go:build windows

package process

import (
	"context"
	"io"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"

	"github.com/stperic/zzrouter/pkg/host"
)

func TestTerminateWindowsHelper(t *testing.T) {
	if !slices.Contains(os.Args, "zzrouter-terminate-helper") {
		return
	}
	_, _ = io.Copy(io.Discard, os.Stdin) // Remain alive until the parent closes stdin.
}

func startTerminateWindowsHelper(t *testing.T) (int, windows.Handle, io.WriteCloser, <-chan error) {
	t.Helper()
	binary, err := os.Executable()
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	cmd := host.CommandContext(ctx, binary, "-test.run=^TestTerminateWindowsHelper$", "--", "zzrouter-terminate-helper")
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	wait := make(chan error, 1)
	go func() {
		wait <- cmd.Wait() // Sole reaper, including failure cleanup.
		close(wait)
	}()
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		select {
		case <-wait:
		case <-time.After(5 * time.Second):
			t.Error("helper process did not exit")
		}
	})
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(cmd.Process.Pid))
	require.NoError(t, err)
	t.Cleanup(func() { _ = windows.CloseHandle(handle) })
	return cmd.Process.Pid, handle, stdin, wait
}

func TestTerminateWindowsExitedProcessWithRetainedHandle(t *testing.T) {
	pid, handle, stdin, wait := startTerminateWindowsHelper(t)
	require.True(t, ProcessExists(int32(pid)))
	require.NoError(t, stdin.Close())
	require.NoError(t, <-wait)
	event, err := windows.WaitForSingleObject(handle, 0)
	require.NoError(t, err)
	require.EqualValues(t, windows.WAIT_OBJECT_0, event)
	assert.False(t, ProcessExists(int32(pid)), "retained process object is not a live process")
	require.NoError(t, Terminate(t.Context(), TerminateRequest{PID: pid, JobHandle: uintptr(windows.InvalidHandle)}))
}

func TestTerminateWindowsStaleJobHandleFallsBackToLiveProcess(t *testing.T) {
	pid, handle, _, wait := startTerminateWindowsHelper(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel() // Exercise forced termination immediately, with a stale Job Object handle.
	require.NoError(t, Terminate(ctx, TerminateRequest{PID: pid, JobHandle: uintptr(windows.InvalidHandle)}))
	require.Error(t, <-wait)
	event, err := windows.WaitForSingleObject(handle, 0)
	require.NoError(t, err)
	require.EqualValues(t, windows.WAIT_OBJECT_0, event)
	assert.False(t, ProcessExists(int32(pid)))
}
