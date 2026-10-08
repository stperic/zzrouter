package gpu

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMemorySnapshotRejectsUnknownFigures(t *testing.T) {
	for _, free := range []string{"[N/A]", "", "-1", "10001", "NaN", "0", "6000"} {
		rows := []nvidiaRow{{nvidiaFieldUUID: "GPU-A", nvidiaFieldIndex: "0", nvidiaFieldMemoryTotalMiB: "10000", nvidiaFieldMemoryFreeMiB: free}}
		devices, err := memoryDevices(rows)
		if free == "0" || free == "6000" {
			require.NoError(t, err)
			require.Len(t, devices, 1)
		} else {
			require.Error(t, err)
		}
	}
	_, err := memoryDevices(nil)
	require.Error(t, err)
}

func TestMemorySnapshotPropagatesCancelledAttribution(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix driver fixture")
	}
	root := t.TempDir()
	marker := filepath.Join(root, "attribution")
	script := "#!/bin/sh\ncase \"$1\" in\n--query-gpu=*) echo 'GPU-A, 0, 10000, 6000' ;;\n*) echo reached > '" + marker + "'; exec /bin/sleep 60 ;;\nesac\n"
	require.NoError(t, os.WriteFile(filepath.Join(root, "nvidia-smi"), []byte(script), 0o700))
	t.Setenv("PATH", root)
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	devices, err := MemorySnapshotContext(ctx)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Nil(t, devices)
	_, err = os.Stat(marker)
	require.NoError(t, err, "the memory query must succeed before attribution is cancelled")
}

func TestMemorySnapshotOutputIsBoundedAndDrained(t *testing.T) {
	output := &boundedOutput{}
	data := bytes.Repeat([]byte("a"), 1<<20)
	n, err := output.Write(data)
	require.NoError(t, err)
	assert.Equal(t, len(data), n)
	assert.Len(t, output.data, 64<<10)
	assert.True(t, output.truncated)
}
