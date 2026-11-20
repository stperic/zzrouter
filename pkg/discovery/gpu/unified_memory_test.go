package gpu

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUnifiedMemoryObservationKeepsUnknownUsage(t *testing.T) {
	const gib = 1024 * 1024 * 1024
	device, err := UnifiedMemoryDevice(16*gib, 6*gib)
	require.NoError(t, err)
	require.Equal(t, "unified", device.UUID)
	require.True(t, device.Unified)
	require.Equal(t, int64(16384), device.TotalMiB)
	require.Equal(t, int64(6144), device.FreeMiB)
	require.Nil(t, device.UsedMiB)
	require.Empty(t, device.Processes)
	require.NotEmpty(t, device.ProcessReason)
	for _, sample := range [][2]int64{{0, 0}, {-1, 0}, {1, 0}, {gib, -1}, {gib, gib + 1}} {
		_, err := UnifiedMemoryDevice(sample[0], sample[1])
		require.Error(t, err, sample)
	}
	device, err = UnifiedMemoryDevice(gib, 0)
	require.NoError(t, err)
	require.Zero(t, device.FreeMiB, "known zero availability is valid evidence")
}
