package process

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"sync"
)

// RunOwnedCommand bounds a command and its descendants by the caller's context.
// cmd must be constructed with host.CommandContext using the same context.
func RunOwnedCommand(ctx context.Context, cmd *exec.Cmd) error {
	return RunOwnedCommandStarted(ctx, cmd, nil)
}

// RunOwnedCommandStarted publishes the PID after process-tree ownership is set.
func RunOwnedCommandStarted(ctx context.Context, cmd *exec.Cmd, started func(int)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	setProcAttributes(cmd)
	var mu sync.Mutex
	var jobHandle uintptr
	cmd.Cancel = func() error {
		mu.Lock()
		defer mu.Unlock()
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		return killProcessTree(TerminateRequest{PID: cmd.Process.Pid, ProcessGroupID: cmd.Process.Pid, JobHandle: jobHandle})
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	mu.Lock()
	handle, assignErr := assignToJobObject(cmd.Process.Pid)
	jobHandle = handle
	mu.Unlock()
	if assignErr != nil {
		_ = cmd.Cancel()
		_ = cmd.Wait()
		return assignErr
	}
	defer CloseJobHandle(handle)
	if started != nil {
		started(cmd.Process.Pid)
	}
	err := cmd.Wait()
	// An import must not leave a daemon behind after a successful probe either.
	_ = killProcessTree(TerminateRequest{PID: cmd.Process.Pid, ProcessGroupID: cmd.Process.Pid, JobHandle: handle})
	if ctx.Err() != nil {
		return errors.Join(ctx.Err(), err)
	}
	return err
}
