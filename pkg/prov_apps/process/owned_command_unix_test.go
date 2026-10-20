//go:build unix

package process

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/host"
	"github.com/stretchr/testify/require"
)

func TestRunOwnedCommandDefaultKeepsImmediateCleanup(t *testing.T) {
	binary, err := os.Executable()
	require.NoError(t, err)
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "ready")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cmd := host.CommandContext(ctx, binary, "-test.run=^TestTerminateGroupHelper$", "--", "zzrouter-group-helper", "ignore-term", "pid-file", pidFile)
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	defer func() { _ = stdin.Close() }()
	done := make(chan error, 1)
	go func() { done <- RunOwnedCommand(ctx, cmd) }()
	require.Eventually(t, func() bool { _, err := os.Stat(pidFile); return err == nil }, 5*time.Second, time.Millisecond)
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("install/import cancellation must retain immediate cleanup")
	}
}
