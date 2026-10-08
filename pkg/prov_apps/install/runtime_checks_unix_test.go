//go:build unix

package install

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	gprocess "github.com/shirou/gopsutil/v4/process"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRuntimeCancellationStopsDescendant(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	scratchFile := filepath.Join(t.TempDir(), "scratch.path")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	executable := filepath.Join(t.TempDir(), "python")
	require.NoError(t, os.WriteFile(executable, []byte("#!/bin/sh\nmkdir \"$6/cache\"; echo scratch > \"$6/cache/file\"; echo \"$6\" > '"+scratchFile+"'\nsleep 30 & echo $! > '"+pidFile+"'; wait\n"), 0o700))
	checks := schema.RuntimeChecks{Imports: []string{"json"}, Checks: []string{"pip_check", "imports"}, Kernels: "unknown"}
	done := make(chan []RuntimeCheck, 1)
	go func() { done <- RunRuntimeChecks(ctx, executable, checks) }()
	var pid int32
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(pidFile)
		if err != nil {
			return false
		}
		parsed, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 32)
		pid = int32(parsed)
		return err == nil && pid > 1
	}, 3*time.Second, 10*time.Millisecond)
	t.Cleanup(func() {
		if child, err := gprocess.NewProcess(pid); err == nil {
			_ = child.Kill()
		}
	})
	cancel()
	select {
	case result := <-done:
		require.Len(t, result, 1)
		assert.False(t, result[0].Passed)
		assert.Contains(t, result[0].Reason, context.Canceled.Error())
	case <-time.After(3 * time.Second):
		t.Fatal("probe did not stop after cancellation")
	}
	scratch, err := os.ReadFile(scratchFile)
	require.NoError(t, err)
	_, err = os.Stat(strings.TrimSpace(string(scratch)))
	assert.ErrorIs(t, err, os.ErrNotExist, "cancelled probe left its cache directory")
	assert.Eventually(t, func() bool {
		child, err := gprocess.NewProcess(pid)
		if err != nil {
			return true
		}
		states, err := child.Status()
		if err != nil {
			return true
		}
		for _, state := range states {
			if state == "zombie" {
				return true
			}
		}
		return !process.ProcessExists(pid)
	}, 3*time.Second, 10*time.Millisecond, "probe descendant remained active")
}

func TestSnapshotProbeRejectsDangerousEnvironmentWithoutSpawning(t *testing.T) {
	directory := t.TempDir()
	marker := filepath.Join(directory, "spawned")
	executable := filepath.Join(directory, "python")
	require.NoError(t, os.WriteFile(executable, []byte("#!/bin/sh\ntouch "+marker+"\nprintf 'ZZROUTER_RUNTIME_CHECKS=[]\\n'\n"), 0700))
	_, err := runRuntimeProbeSnapshot(t.Context(), executable, []byte(`{"checks":[]}`), map[string]string{"LD_PRELOAD": "/not-a-library"}, "verify", []string{"PATH=/usr/bin:/bin"})
	require.ErrorIs(t, err, process.ErrDangerousEnvVar)
	_, err = os.Stat(marker)
	assert.ErrorIs(t, err, os.ErrNotExist)
}
