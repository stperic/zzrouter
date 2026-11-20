package resources

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMemorySnapshotRejectsUnknownFigures(t *testing.T) {
	for _, free := range []string{"[N/A]", "", "-1", "10001", "NaN", "0", "6000"} {
		devices, err := testMemoryDevices(t.Context(), fmt.Sprintf("GPU-A, 0, 10000, %s, [N/A], 00000000:01:00.0, 0, 14", free))
		if free == "0" || free == "6000" {
			require.NoError(t, err)
			require.Len(t, devices, 1)
		} else {
			require.Error(t, err)
		}
	}
	_, err := testMemoryDevices(t.Context(), "")
	require.Error(t, err)
}

func TestMemorySnapshotUsedMemory(t *testing.T) {
	for _, used := range []string{"0", "200", "[N/A]", "", "-1", "97888", "NaN"} {
		t.Run(used, func(t *testing.T) {
			devices, err := testMemoryDevices(t.Context(), fmt.Sprintf("GPU-A, 0, 97887, 97248, %s, 00000000:01:00.0, 0, 14", used))
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
	counter := filepath.Join(root, "queries")
	script := "#!/bin/sh\ncase \"$1\" in\n--query-gpu=uuid,index,memory.total,memory.free,memory.used,pci.bus_id,utilization.gpu,power.draw) echo gpu >> '" + counter + "'; echo 'GPU-A, 0, 97887, 97248, 0, 00000000:01:00.0, 0, 14' ;;\n--query-compute-apps=gpu_uuid,pid,used_gpu_memory) echo apps >> '" + counter + "' ;;\n*) exit 1 ;;\nesac\n"
	require.NoError(t, os.WriteFile(filepath.Join(root, "nvidia-smi"), []byte(script), 0o700))
	t.Setenv("PATH", root)
	devices, err := nvidiaMemoryDevices(t.Context())
	require.NoError(t, err)
	require.Len(t, devices, 1)
	assert.Equal(t, int64(97887), devices[0].TotalMiB)
	assert.Equal(t, int64(97248), devices[0].FreeMiB)
	assert.Equal(t, new(int64(0)), devices[0].UsedMiB)
	assert.Empty(t, devices[0].Processes)
	assert.Empty(t, devices[0].ProcessReason)
	calls, err := os.ReadFile(counter)
	require.NoError(t, err)
	assert.Equal(t, "gpu\napps\n", string(calls))
}

func TestMemorySnapshotPropagatesCancelledAttribution(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix driver fixture")
	}
	root := t.TempDir()
	marker := filepath.Join(root, "attribution")
	script := "#!/bin/sh\ncase \"$1\" in\n--query-gpu=*) echo 'GPU-A, 0, 10000, 6000, 4000, 00000000:01:00.0, 0, 14' ;;\n*) echo reached > '" + marker + "'; exec /bin/sleep 60 ;;\nesac\n"
	require.NoError(t, os.WriteFile(filepath.Join(root, "nvidia-smi"), []byte(script), 0o700))
	t.Setenv("PATH", root)
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	devices, err := nvidiaMemoryDevices(ctx)
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

func TestNVIDIARejectsOversizeDriverOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix driver fixture")
	}
	root := t.TempDir()
	script := "#!/bin/sh\nexec /usr/bin/head -c 65537 /dev/zero\n"
	require.NoError(t, os.WriteFile(filepath.Join(root, "nvidia-smi"), []byte(script), 0o700))
	t.Setenv("PATH", root)
	pools, err := NVIDIAPools(t.Context())
	require.ErrorContains(t, err, "exceeds observation limit")
	assert.Nil(t, pools)
}

func TestMemorySnapshotMissingDriverIsAnAdmissionError(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	devices, err := nvidiaMemoryDevices(t.Context())
	require.ErrorIs(t, err, exec.ErrNotFound)
	assert.Nil(t, devices)
}

func testMemoryDevices(ctx context.Context, row string) ([]MemoryDevice, error) {
	pools, err := nvidiaPools(ctx, func(_ context.Context, args ...string) ([]byte, error) {
		if strings.HasPrefix(args[0], "--query-gpu=") {
			return []byte(row), nil
		}
		return nil, nil
	})
	return (Snapshot{Pools: pools, GPUError: err}).MemoryDevices()
}

func nvidiaMemoryDevices(ctx context.Context) ([]MemoryDevice, error) {
	pools, err := NVIDIAPools(ctx)
	return (Snapshot{Pools: pools, GPUError: err}).MemoryDevices()
}

func TestMemorySnapshotHonorsCancellationBeforeObserving(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	snapshot := Sample(ctx, nil, nil)
	require.Empty(t, snapshot.Pools)
	devices, err := snapshot.MemoryDevices()
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, devices)
}

func TestAppleMemorySnapshotUsesUnifiedPool(t *testing.T) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("Apple Silicon memory observation")
	}
	t.Setenv("PATH", t.TempDir())
	devices, err := Sample(t.Context(), nil, nil).MemoryDevices()
	require.NoError(t, err)
	require.Len(t, devices, 1)
	require.True(t, devices[0].Unified)
	require.Equal(t, "unified", devices[0].UUID)
	require.Positive(t, devices[0].TotalMiB)
	require.GreaterOrEqual(t, devices[0].FreeMiB, int64(0))
	require.LessOrEqual(t, devices[0].FreeMiB, devices[0].TotalMiB)
	require.Nil(t, devices[0].UsedMiB)
	require.NotEmpty(t, devices[0].ProcessReason)
}
