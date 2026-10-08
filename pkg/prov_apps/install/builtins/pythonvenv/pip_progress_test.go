package pythonvenv

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPipProgress_DownloadingWheelAdvancesBytes(t *testing.T) {
	var s pipProgressState
	u, ok := s.onLine("  Downloading torch-2.5.1-cp310-cp310-manylinux1_x86_64.whl (1234.5 MB)")
	assert.True(t, ok)
	assert.Contains(t, u.Desc, "torch-2.5.1")
	// Done is cumulative across completed wheels (0 here), Total is
	// the cumulative + in-flight wheel size.
	assert.Equal(t, int64(0), u.Done, "first wheel announcement: nothing cumulative yet")
	assert.Equal(t, int64(1234500000), u.Total)

	u, ok = s.onLine("  Downloading vllm-0.7.0-cp310-cp310-manylinux1_x86_64.whl (512 MB)")
	assert.True(t, ok)
	assert.Equal(t, int64(1234500000), u.Done, "prior wheel flushed into cumulative")
	assert.Equal(t, int64(1234500000+512000000), u.Total, "new total = cumulative + in-flight")
}

func TestPipProgress_CollectingEmitsDesc(t *testing.T) {
	var s pipProgressState
	u, ok := s.onLine("Collecting torch==2.5.1")
	assert.True(t, ok)
	assert.Equal(t, "Collecting torch==2.5.1", u.Desc)
	assert.Equal(t, int64(0), u.Done, "Collecting doesn't advance bytes until Downloading lands")
}

func TestPipProgress_SuccessfullyInstalledFlipsPercent(t *testing.T) {
	var s pipProgressState
	u, ok := s.onLine("Successfully installed torch-2.5.1 vllm-0.7.0 numpy-1.26.0")
	assert.True(t, ok)
	assert.Equal(t, 99, u.Percent)
	assert.Equal(t, "Finalizing install", u.Desc)
}

func TestPipProgress_NoiseLinesIgnored(t *testing.T) {
	var s pipProgressState
	noise := []string{
		"",
		"WARNING: You are using pip version 21.3.1",
		"Installing collected packages: torch, vllm",
		"Requirement already satisfied: numpy in /venv/lib/python3.10",
	}
	for _, line := range noise {
		_, ok := s.onLine(line)
		assert.False(t, ok, "noise line triggered an update: %q", line)
	}
}

func TestPipProgress_Units(t *testing.T) {
	cases := map[string]int64{
		"1 B":    1,
		"1 kB":   1_000,
		"1 MB":   1_000_000,
		"1 GB":   1_000_000_000,
		"3.5 MB": 3_500_000,
	}
	for size, expected := range cases {
		var s pipProgressState
		u, ok := s.onLine("  Downloading foo.whl (" + size + ")")
		assert.True(t, ok, "failed to parse size: %q", size)
		assert.Equal(t, expected, u.Total, "size mismatch for %q — first wheel Total equals its size", size)
	}
}

func TestPipProgress_ProgressBarAdvancesWithinWheel(t *testing.T) {
	var s pipProgressState
	_, ok := s.onLine("  Downloading torch-2.5.1.whl (1000 MB)")
	require.True(t, ok)

	u, ok := s.onLine("   ━━━━━━━━━━━━━━━━━━━━━━━ 250 MB/1000 MB 32.0 MB/s eta 0:00:23")
	require.True(t, ok, "progress bar line should advance state")
	assert.Equal(t, int64(250_000_000), u.Done, "Done reflects cumulative + in-flight bytes")
	assert.Equal(t, int64(1000_000_000), u.Total)

	u, ok = s.onLine("   ━━━━━━━━━━━━━━━━━━━━━━━ 500 MB/1000 MB 32.0 MB/s eta 0:00:15")
	require.True(t, ok)
	assert.Equal(t, int64(500_000_000), u.Done)
}

func TestPipProgress_ProgressBarDedupesRepeats(t *testing.T) {
	var s pipProgressState
	_, ok := s.onLine("  Downloading torch-2.5.1.whl (1000 MB)")
	require.True(t, ok)
	_, ok = s.onLine("   ━━━ 100 MB/1000 MB 10 MB/s eta 0:01:30")
	require.True(t, ok)
	_, ok = s.onLine("   ━━━ 100 MB/1000 MB 10 MB/s eta 0:01:30")
	assert.False(t, ok, "identical consecutive bar readings must dedupe")
}

func TestPipProgress_ProgressBarGatedOnActiveDownload(t *testing.T) {
	var s pipProgressState
	_, ok := s.onLine("   ━━━ 250 MB/1000 MB 32.0 MB/s eta 0:00:23")
	assert.False(t, ok, "progress bar outside an active Downloading context must not trigger")
}

func TestPipProgress_SuccessfullyInstalledFlushesInFlight(t *testing.T) {
	var s pipProgressState
	_, ok := s.onLine("  Downloading torch-2.5.1.whl (1000 MB)")
	require.True(t, ok)
	u, ok := s.onLine("Successfully installed torch-2.5.1")
	require.True(t, ok)
	assert.Equal(t, int64(1000_000_000), u.Done,
		"terminal line must fold the in-flight wheel into cumulative bytes")
}

func TestPipProgress_MetadataLinesDoNotCount(t *testing.T) {
	// pip emits ".whl.metadata" lines during resolution; treat same as
	// normal .whl announcements (they have the filename, just with a
	// metadata suffix). Parser regex requires plain `.whl` suffix so
	// .metadata lines are ignored — that's intentional because the
	// metadata payload is tiny and doesn't represent real download work.
	var s pipProgressState
	_, ok := s.onLine("  Downloading torch-2.5.1-cp310.whl.metadata (28 kB)")
	assert.False(t, ok, "metadata-only lines should not count as byte progress")
}
