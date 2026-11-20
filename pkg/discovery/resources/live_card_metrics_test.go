package resources

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func driverRows(gpus, apps string, appErr error) func(context.Context, ...string) ([]byte, error) {
	return func(_ context.Context, args ...string) ([]byte, error) {
		if strings.HasPrefix(args[0], "--query-gpu=") {
			return []byte(gpus), nil
		}
		return []byte(apps), appErr
	}
}

func TestLiveCardMetricsContext(t *testing.T) {
	pools, err := nvidiaPools(t.Context(), driverRows(
		"GPU-aaa, 0, 97887, 67875, 30012, 00000000:41:00.0, 42, 450.25\n"+
			"GPU-bbb, 1, 97887, 97887, 0, 0000:c1:00.0, 0, [N/A]", "", nil))
	require.NoError(t, err)
	require.Len(t, pools, 2)
	assert.Equal(t, "GPU-aaa", pools[0].ID)
	assert.Equal(t, "0000:41:00.0", pools[0].PCIAddress)
	assert.Equal(t, new(int64(67875)), pools[0].FreeMiB)
	assert.Equal(t, new(int64(30012)), pools[0].UsedMiB)
	assert.Equal(t, new(float64(42)), pools[0].UtilizationPct)
	assert.Equal(t, new(int64(97887)), pools[1].FreeMiB)
	assert.Equal(t, new(float64(450.25)), pools[0].PowerW)
	assert.Nil(t, pools[1].PowerW)
}

func TestCardMetricsIndex(t *testing.T) {
	snapshot := Snapshot{Pools: []Pool{
		{Kind: GPU, ID: "GPU-bbb", PCIAddress: "0000:c1:00.0", FreeMiB: new(int64(97887))},
		{Kind: GPU, ID: "GPU-aaa", PCIAddress: "0000:41:00.0", FreeMiB: new(int64(67875)), UsedMiB: new(int64(30012))},
	}}
	got, ok := snapshot.Device("GPU-aaa", "0000:c1:00.0")
	require.True(t, ok)
	assert.Equal(t, new(int64(67875)), got.FreeMiB)
	assert.Equal(t, new(int64(30012)), got.UsedMiB)
	got, ok = snapshot.Device("", "0000:c1:00.0")
	require.True(t, ok)
	assert.Equal(t, "GPU-bbb", got.ID)
	_, ok = snapshot.Device("", "00000000:41:00.0")
	assert.True(t, ok, "an 8-digit domain matches the canonical form")
	_, ok = (Snapshot{}).Device("GPU-aaa", "")
	assert.False(t, ok)
	_, ok = snapshot.Device("", "")
	assert.False(t, ok)
}

func TestLiveCardMemoryUnknownAndZeroAreIndependent(t *testing.T) {
	for _, value := range []string{"[N/A]", "", "bad", "-1", "0"} {
		t.Run(value, func(t *testing.T) {
			row := fmt.Sprintf("GPU-A, 0, 100, 40, %s, 0000:01:00.0, 0, [N/A]", value)
			pools, err := nvidiaPools(t.Context(), driverRows(row, "", nil))
			require.NoError(t, err)
			require.Len(t, pools, 1)
			assert.Equal(t, new(int64(40)), pools[0].FreeMiB)
			if value == "0" {
				assert.Equal(t, new(int64(0)), pools[0].UsedMiB)
			} else {
				assert.Nil(t, pools[0].UsedMiB)
			}
		})
	}
}

func TestNVIDIAFailedQueryAndUnmeasuredRowsHaveNoPools(t *testing.T) {
	for _, query := range []func(context.Context, ...string) ([]byte, error){
		driverRows("", "", nil),
		func(context.Context, ...string) ([]byte, error) { return nil, errors.New("driver mismatch") },
		driverRows("GPU-A, 0, [N/A], [N/A], [N/A], 0000:01:00.0, [N/A], [N/A]", "", nil),
	} {
		pools, err := nvidiaPools(t.Context(), query)
		require.Error(t, err)
		assert.Nil(t, pools)
	}
}

func TestNVIDIAPartialRowsAreDroppedAndAdmissionRefuses(t *testing.T) {
	pools, err := nvidiaPools(t.Context(), driverRows(
		"GPU-A, 0, 100, 40, 10, 0000:01:00.0, 0, 12\nGPU-B, 1, 100, 40, 10", "", nil))
	require.Len(t, pools, 1)
	assert.Equal(t, "GPU-A", pools[0].ID)
	devices, err := (Snapshot{Pools: pools, GPUError: err}).MemoryDevices()
	require.ErrorContains(t, err, "incomplete")
	assert.Nil(t, devices)
}

func TestNVIDIAFailedProcessListingPreservesPoolAndReason(t *testing.T) {
	pools, err := nvidiaPools(t.Context(), driverRows("GPU-A, 0, 100, 40, 10, 0000:01:00.0, 0, 12", "", errors.New("listing failed")))
	require.NoError(t, err)
	require.Nil(t, pools[0].Holders)
	devices, err := (Snapshot{Pools: pools}).MemoryDevices()
	require.NoError(t, err)
	require.Len(t, devices, 1)
	assert.Empty(t, devices[0].Processes)
	assert.Equal(t, "driver process attribution unavailable", devices[0].ProcessReason)
}

func TestNVIDIACanceledListingLeavesInventoryEvidenceButRefusesAdmission(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	pools, err := nvidiaPools(ctx, func(_ context.Context, args ...string) ([]byte, error) {
		if strings.HasPrefix(args[0], "--query-gpu=") {
			return []byte("GPU-A, 0, 100, 40, 10, 0000:01:00.0, 0, 12"), nil
		}
		cancel()
		return nil, ctx.Err()
	})
	require.ErrorIs(t, err, context.Canceled)
	require.Len(t, pools, 1)
	assert.Equal(t, "driver process attribution unavailable", pools[0].ProcessReason)
	assert.Nil(t, pools[0].Holders)
	devices, err := (Snapshot{Pools: pools, GPUError: err}).MemoryDevices()
	require.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, devices)
}

func TestNVIDIAWDDMProcessMemoryAndPowerStayUnknown(t *testing.T) {
	pools, err := nvidiaPools(t.Context(), driverRows(
		"GPU-A, 0, 100, 40, 10, 0000:01:00.0, 0, [N/A]", "GPU-A, 1234, [N/A]", nil))
	require.NoError(t, err)
	require.Len(t, pools[0].Holders, 1)
	assert.Nil(t, pools[0].PowerW)
	assert.Nil(t, pools[0].Holders[0].MemoryMiB)
	device, ok := pools[0].AsMemoryDevice()
	require.True(t, ok)
	assert.Equal(t, 1234, device.Processes[0].PID)
	assert.Nil(t, device.Processes[0].MemoryMiB)
}

func TestNVIDIADeviceCap(t *testing.T) {
	row := "GPU-A, 0, 100, 40, 10, 0000:01:00.0, 0, 12\n"
	pools, err := nvidiaPools(t.Context(), driverRows(strings.Repeat(row, 64), "", nil))
	require.NoError(t, err)
	assert.Len(t, pools, 64)
	pools, err = nvidiaPools(t.Context(), driverRows(strings.Repeat(row, 65), "", nil))
	require.Error(t, err)
	assert.Nil(t, pools)
}
