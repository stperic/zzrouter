package gpu

import (
	"bytes"
	"context"
	"encoding/json"
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

func TestMemorySnapshotUsedMemory(t *testing.T) {
	for _, used := range []string{"0", "200", "[N/A]", "", "-1", "97888", "NaN"} {
		t.Run(used, func(t *testing.T) {
			devices, err := memoryDevices([]nvidiaRow{{nvidiaFieldUUID: "GPU-A", nvidiaFieldIndex: "0", nvidiaFieldMemoryTotalMiB: "97887", nvidiaFieldMemoryFreeMiB: "97248", nvidiaFieldMemoryUsedMiB: used}})
			require.NoError(t, err)
			require.Len(t, devices, 1)
			wire, err := json.Marshal(devices[0])
			require.NoError(t, err)
			var fields map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(wire, &fields))
			switch used {
			case "0":
				require.NotNil(t, devices[0].UsedMiB)
				assert.Zero(t, *devices[0].UsedMiB)
				assert.JSONEq(t, "0", string(fields["used_mib"]))
			case "200":
				assert.Equal(t, new(int64(200)), devices[0].UsedMiB)
			default:
				assert.Nil(t, devices[0].UsedMiB)
				assert.NotContains(t, fields, "used_mib")
			}
		})
	}
}

func TestMemorySnapshotQueriesUsedMemory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix driver fixture")
	}
	root := t.TempDir()
	script := "#!/bin/sh\ncase \"$1\" in\n--query-gpu=uuid,index,memory.total,memory.free,memory.used) echo 'GPU-A, 0, 97887, 97248, 0' ;;\n--query-compute-apps=*) ;;\n*) exit 1 ;;\nesac\n"
	require.NoError(t, os.WriteFile(filepath.Join(root, "nvidia-smi"), []byte(script), 0o700))
	t.Setenv("PATH", root)
	devices, err := MemorySnapshotContext(t.Context())
	require.NoError(t, err)
	require.Len(t, devices, 1)
	assert.Equal(t, int64(97887), devices[0].TotalMiB)
	assert.Equal(t, int64(97248), devices[0].FreeMiB)
	assert.Equal(t, new(int64(0)), devices[0].UsedMiB)
	assert.Empty(t, devices[0].Processes)
	assert.Empty(t, devices[0].ProcessReason)
}

func TestMemorySnapshotPropagatesCancelledAttribution(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix driver fixture")
	}
	root := t.TempDir()
	marker := filepath.Join(root, "attribution")
	script := "#!/bin/sh\ncase \"$1\" in\n--query-gpu=*) echo 'GPU-A, 0, 10000, 6000, 4000' ;;\n*) echo reached > '" + marker + "'; exec /bin/sleep 60 ;;\nesac\n"
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
