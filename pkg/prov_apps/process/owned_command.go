package process

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/stperic/zzrouter/pkg/constants"
)

// RunOwnedCommand bounds a command and its descendants by the caller's context.
// cmd must be constructed with host.CommandContext using the same context.
func RunOwnedCommand(ctx context.Context, cmd *exec.Cmd) error {
	return RunOwnedCommandStarted(ctx, cmd, nil, constants.NoGrace)
}

// RunOwnedCommandStarted publishes ownership; positive grace enables Unix service cancellation.
func RunOwnedCommandStarted(ctx context.Context, cmd *exec.Cmd, started func(int), grace time.Duration) error {
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
		req := TerminateRequest{PID: cmd.Process.Pid, ProcessGroupID: cmd.Process.Pid, JobHandle: jobHandle, Graceful: grace}
		return cancelOwnedCommand(ctx, req)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	mu.Lock()
	jobHandle, assignErr := assignToJobObject(cmd.Process.Pid)
	mu.Unlock()
	if assignErr != nil {
		_ = cmd.Cancel()
		_ = cmd.Wait()
		return assignErr
	}
	defer CloseJobHandle(jobHandle)
	if started != nil {
		started(cmd.Process.Pid)
	}
	err := cmd.Wait()
	// An import must not leave a daemon behind after a successful probe either.
	_ = killProcessTree(TerminateRequest{PID: cmd.Process.Pid, ProcessGroupID: cmd.Process.Pid, JobHandle: jobHandle})
	if ctx.Err() != nil {
		return errors.Join(ctx.Err(), err)
	}
	return err
}
