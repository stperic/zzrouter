package resources

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseRocmSMIMemory(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		wantTotal int64
		wantUsed  int64
	}{
		{
			name:      "ROCm 5.x MB format, single GPU",
			in:        "GPU[0]\t\t: vram Total Memory (MB): 16384\nGPU[0]\t\t: vram Used Memory (MB): 4096\n",
			wantTotal: 16384,
			wantUsed:  4096,
		},
		{
			name:      "ROCm 6.x byte format, single GPU",
			in:        "GPU[0]\t\t: VRAM Total Memory (B): 17163091968\nGPU[0]\t\t: VRAM Used Memory (B): 536870912\n",
			wantTotal: 17163091968 / (1 << 20),
			wantUsed:  536870912 / (1 << 20),
		},
		{
			// Regression for the multi-GPU aggregation bug found
			// in senior review of the commit 7b52a24 arc. The old
			// code used FindSubmatch (first match only), so on a
			// two-card box the Total / Used aggregate was GPU[0]'s
			// figures alone — the caller saw Count=2 alongside
			// single-card memory and the UI reported wrong numbers.
			name: "two GPUs ROCm 5.x → memory sums across cards",
			in: "GPU[0]\t\t: vram Total Memory (MB): 16384\n" +
				"GPU[0]\t\t: vram Used Memory (MB): 4096\n" +
				"GPU[1]\t\t: vram Total Memory (MB): 24576\n" +
				"GPU[1]\t\t: vram Used Memory (MB): 8192\n",
			wantTotal: 16384 + 24576,
			wantUsed:  4096 + 8192,
		},
		{
			name: "two GPUs ROCm 6.x byte format → memory sums across cards",
			in: "GPU[0]\t\t: VRAM Total Memory (B): 17163091968\n" +
				"GPU[0]\t\t: VRAM Used Memory (B): 1073741824\n" +
				"GPU[1]\t\t: VRAM Total Memory (B): 17163091968\n" +
				"GPU[1]\t\t: VRAM Used Memory (B): 536870912\n",
			wantTotal: 2 * (17163091968 / (1 << 20)),
			wantUsed:  (1073741824 / (1 << 20)) + (536870912 / (1 << 20)),
		},
		{
			name:      "total present but used missing",
			in:        "Total Memory (B): 17163091968\n",
			wantTotal: 17163091968 / (1 << 20),
			wantUsed:  0,
		},
		{
			name:      "used zero is valid",
			in:        "Total Memory (B): 17163091968\nUsed Memory (B): 0\n",
			wantTotal: 17163091968 / (1 << 20),
			wantUsed:  0,
		},
		{
			name:      "empty",
			in:        "",
			wantTotal: 0,
			wantUsed:  0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			total, used := parseRocmSMIMemory([]byte(tc.in))
			if tc.in == "" {
				assert.Nil(t, total)
				assert.Nil(t, used)
				return
			}
			assert.Equal(t, &tc.wantTotal, total)
			if tc.name == "total present but used missing" {
				assert.Nil(t, used)
			} else {
				assert.Equal(t, &tc.wantUsed, used)
			}
		})
	}
}

func TestParseRocmSMIUtilization(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want float64
	}{
		{
			name: "ROCm 5.x form: GPU use (%): 25",
			in:   "======== Show GPU Use ========\nGPU[0]\t\t: GPU use (%): 25\n",
			want: 25,
		},
		{
			name: "ROCm 6.x form with trailing percent",
			in:   "GPU[0]\t\t: GPU Use: 42%\n",
			want: 42,
		},
		{
			name: "fractional value",
			in:   "GPU[0]\t\t: GPU use (%): 12.5\n",
			want: 12.5,
		},
		{
			// Companion regression for the FindSubmatch →
			// FindAllSubmatch fix. Two GPUs at 25% and 75% must
			// average to 50%; the pre-fix code returned 25% (the
			// first match only).
			name: "two GPUs average across cards",
			in: "GPU[0]\t\t: GPU use (%): 25\n" +
				"GPU[1]\t\t: GPU use (%): 75\n",
			want: 50,
		},
		{
			name: "three GPUs with one malformed row dropped from average",
			in: "GPU[0]\t\t: GPU use (%): 10\n" +
				"GPU[1]\t\t: GPU use (%): 90\n" +
				"GPU[2]\t\t: GPU use (%): not-a-number\n",
			// Only GPUs 0 and 1 are parseable. The GPU[2] line
			// matches the regex (it anchors on "use" and captures
			// the first digit run — there is no digit run, so the
			// regex doesn't match, and the row is not counted).
			want: 50,
		},
		{
			name: "missing",
			in:   "no gpu use line here\n",
			want: 0,
		},
		{
			name: "empty",
			in:   "",
			want: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseRocmSMIUtilization([]byte(tc.in))
			if tc.name == "missing" || tc.name == "empty" {
				assert.Nil(t, got)
			} else {
				require.NotNil(t, got)
				assert.InDelta(t, tc.want, *got, 0.001)
			}
		})
	}
}

func TestCountRocmSMIIDs(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"two GPUs", "GPU[0]\nGPU[1]\n", 2},
		{"blank lines ignored", "\nGPU[0]\n\nGPU[1]\n\n", 2},
		{"empty", "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := countRocmSMIIDs([]byte(tc.in))
			if got != tc.want {
				t.Errorf("countRocmSMIIDs = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestLiveAMDMetrics(t *testing.T) {
	t.Run("two GPUs, ROCm 5.x MB format", func(t *testing.T) {
		query := stubRocmSMI(map[string][]byte{
			"--showid":           []byte("GPU[0]\nGPU[1]\n"),
			"--showmeminfo vram": []byte("GPU[0]\t\t: vram Total Memory (MB): 16384\nGPU[0]\t\t: vram Used Memory (MB): 4096\n"),
			"--showuse":          []byte("GPU[0]\t\t: GPU use (%): 25\n"),
		})
		m := liveAMD(context.Background(), query)
		if m.Count != 2 {
			t.Errorf("Count = %d, want 2", m.Count)
		}
		if *m.TotalMiB != 16384 {
			t.Errorf("TotalMemoryMiB = %d, want 16384", *m.TotalMiB)
		}
		if *m.UsedMiB != 4096 {
			t.Errorf("UsedMemoryMiB = %d, want 4096", *m.UsedMiB)
		}
		if *m.FreeMiB != 16384-4096 {
			t.Errorf("FreeMemoryMiB = %d, want 12288", *m.FreeMiB)
		}
		if math.Abs(*m.UtilizationPct-25.0) > 0.001 {
			t.Errorf("UtilizationPct = %.3f, want 25.0", *m.UtilizationPct)
		}
	})

	t.Run("one GPU, ROCm 6.x byte format (canary for unit inference)", func(t *testing.T) {
		// The pre-refactor implementation hardcoded bytes → MiB via
		// `bytes / (1024 * 1024)`. On ROCm 5.x this silently
		// mis-reported memory because 16384 MB was interpreted as
		// 16384 bytes → 0 MiB. This test pins the fix: 6.x byte
		// counts still convert correctly, AND the ROCm 5.x test
		// above shows the 5.x format works at the same time.
		query := stubRocmSMI(map[string][]byte{
			"--showid":           []byte("GPU[0]\n"),
			"--showmeminfo vram": []byte("GPU[0]\t\t: VRAM Total Memory (B): 17163091968\nGPU[0]\t\t: VRAM Used Memory (B): 1073741824\n"),
			"--showuse":          []byte("GPU[0]\t\t: GPU use (%): 42\n"),
		})
		m := liveAMD(context.Background(), query)
		wantTotal := int64(17163091968 / (1 << 20)) // 16376 MiB
		wantUsed := int64(1073741824 / (1 << 20))   // 1024 MiB
		if *m.TotalMiB != wantTotal {
			t.Errorf("TotalMemoryMiB = %d, want %d", *m.TotalMiB, wantTotal)
		}
		if *m.UsedMiB != wantUsed {
			t.Errorf("UsedMemoryMiB = %d, want %d", *m.UsedMiB, wantUsed)
		}
		if *m.FreeMiB != wantTotal-wantUsed {
			t.Errorf("FreeMemoryMiB = %d, want %d", *m.FreeMiB, wantTotal-wantUsed)
		}
		if math.Abs(*m.UtilizationPct-42.0) > 0.001 {
			t.Errorf("UtilizationPct = %.3f, want 42.0", *m.UtilizationPct)
		}
	})

	t.Run("two GPUs ROCm 5.x, full aggregation (senior-review regression)", func(t *testing.T) {
		// The must-not-regress canary. Before the FindAllSubmatch
		// fix, this test would have reported card 0's figures
		// alongside Count=2: Total=16384 instead of 40960, Used
		// =4096 instead of 12288, Utilization=25 instead of 50.
		query := stubRocmSMI(map[string][]byte{
			"--showid": []byte("GPU[0]\nGPU[1]\n"),
			"--showmeminfo vram": []byte(
				"GPU[0]\t\t: vram Total Memory (MB): 16384\n" +
					"GPU[0]\t\t: vram Used Memory (MB): 4096\n" +
					"GPU[1]\t\t: vram Total Memory (MB): 24576\n" +
					"GPU[1]\t\t: vram Used Memory (MB): 8192\n",
			),
			"--showuse": []byte(
				"GPU[0]\t\t: GPU use (%): 25\n" +
					"GPU[1]\t\t: GPU use (%): 75\n",
			),
		})
		m := liveAMD(context.Background(), query)
		if m.Count != 2 {
			t.Errorf("Count = %d, want 2", m.Count)
		}
		if *m.TotalMiB != 16384+24576 {
			t.Errorf("TotalMemoryMiB = %d, want 40960 (single-card aggregate would be 16384)", *m.TotalMiB)
		}
		if *m.UsedMiB != 4096+8192 {
			t.Errorf("UsedMemoryMiB = %d, want 12288 (single-card aggregate would be 4096)", *m.UsedMiB)
		}
		wantFree := (16384 + 24576) - (4096 + 8192)
		if *m.FreeMiB != int64(wantFree) {
			t.Errorf("FreeMemoryMiB = %d, want %d", *m.FreeMiB, wantFree)
		}
		if math.Abs(*m.UtilizationPct-50.0) > 0.001 {
			t.Errorf("UtilizationPct = %.3f, want 50.0 (single-card aggregate would be 25.0)", *m.UtilizationPct)
		}
	})

	t.Run("two GPUs ROCm 6.x bytes, full aggregation", func(t *testing.T) {
		query := stubRocmSMI(map[string][]byte{
			"--showid": []byte("GPU[0]\nGPU[1]\n"),
			"--showmeminfo vram": []byte(
				"GPU[0]\t\t: VRAM Total Memory (B): 17163091968\n" +
					"GPU[0]\t\t: VRAM Used Memory (B): 1073741824\n" +
					"GPU[1]\t\t: VRAM Total Memory (B): 17163091968\n" +
					"GPU[1]\t\t: VRAM Used Memory (B): 536870912\n",
			),
			"--showuse": []byte(
				"GPU[0]\t\t: GPU use (%): 0\n" +
					"GPU[1]\t\t: GPU use (%): 100\n",
			),
		})
		m := liveAMD(context.Background(), query)
		wantTotal := int64(2 * (17163091968 / (1 << 20)))
		wantUsed := int64((1073741824 / (1 << 20)) + (536870912 / (1 << 20)))
		if *m.TotalMiB != wantTotal {
			t.Errorf("TotalMemoryMiB = %d, want %d", *m.TotalMiB, wantTotal)
		}
		if *m.UsedMiB != wantUsed {
			t.Errorf("UsedMemoryMiB = %d, want %d", *m.UsedMiB, wantUsed)
		}
		if math.Abs(*m.UtilizationPct-50.0) > 0.001 {
			t.Errorf("UtilizationPct = %.3f, want 50.0 (mean of 0 and 100)", *m.UtilizationPct)
		}
	})

	t.Run("--showid missing → Count 0, no further calls", func(t *testing.T) {
		m := liveAMD(context.Background(), stubRocmSMI(nil))
		if m.Count != 0 {
			t.Errorf("Count = %d, want 0", m.Count)
		}
	})

	t.Run("--showid present but parses to zero → Count 0", func(t *testing.T) {
		m := liveAMD(context.Background(), stubRocmSMI(map[string][]byte{
			"--showid": []byte("\n\n"),
		}))
		if m.Count != 0 {
			t.Errorf("Count = %d, want 0", m.Count)
		}
	})
}

func stubRocmSMI(responses map[string][]byte) func(context.Context, ...string) ([]byte, error) {
	return func(_ context.Context, args ...string) ([]byte, error) {
		return responses[strings.Join(args, " ")], nil
	}
}

func TestLiveNVIDIAMetrics(t *testing.T) {
	const a = "GPU-A, 0, 40960, 32768, 8192, 0000:01:00.0, 50, 12"
	for _, tc := range []struct {
		name, rows        string
		count             int
		total, used, free *int64
		utilization       *float64
	}{
		{"sum", a + "\nGPU-B, 1, 24564, 22516, 2048, 0000:02:00.0, 30, 12", 2, new(int64(65524)), new(int64(10240)), new(int64(55284)), new(40.0)},
		{"unknown utilization", a + "\nGPU-B, 1, 40960, 32768, 8192, 0000:02:00.0, [N/A], 12", 2, new(int64(81920)), new(int64(16384)), new(int64(65536)), new(50.0)},
		{"all utilization unknown", "GPU-A, 0, 40960, [N/A], [N/A], 0000:01:00.0, [N/A], [N/A]", 1, new(int64(40960)), nil, nil, nil},
		{"no GPU", "", 0, nil, nil, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pools, _ := nvidiaPools(t.Context(), driverRows(tc.rows, "", nil))
			want := GPUStats{tc.count, tc.total, tc.used, tc.free, tc.utilization}
			assert.Equal(t, want, (Snapshot{Pools: pools}).GPUStats("nvidia"))
		})
	}
}

func TestLiveMetricsContext(t *testing.T) {
	for _, vendor := range []string{"mixed", "nvidia", "none"} {
		t.Run(vendor, func(t *testing.T) {
			var snapshot Snapshot
			if vendor != "none" {
				pools, err := nvidiaPools(t.Context(), driverRows("GPU-A, 0, 24564, 20468, 4096, 0000:01:00.0, 75, 12", "", nil))
				require.NoError(t, err)
				snapshot.Pools = pools
			}
			if vendor == "mixed" {
				snapshot.amd = liveAMD(t.Context(), stubRocmSMI(map[string][]byte{
					"--showid":           []byte("GPU[0]\n"),
					"--showmeminfo vram": []byte("Total Memory (B): 17163091968\nUsed Memory (B): 0\n"),
					"--showuse":          []byte("GPU[0]\t\t: GPU use (%): 0\n"),
				}))
				assert.Equal(t, new(int64(17163091968/(1<<20))), snapshot.GPUStats("amd").TotalMiB)
			} else {
				assert.Zero(t, snapshot.GPUStats("amd").Count)
			}
			if vendor == "none" {
				assert.Zero(t, snapshot.GPUStats("nvidia").Count)
			} else {
				assert.Equal(t, new(int64(24564)), snapshot.GPUStats("nvidia").TotalMiB)
			}
		})
	}
}

func TestLiveAMDDoesNotInventMissingMemory(t *testing.T) {
	stats := liveAMD(t.Context(), stubRocmSMI(map[string][]byte{
		"--showid":           []byte("GPU[0]"),
		"--showmeminfo vram": []byte("Total Memory (B): 17163091968"),
	}))
	assert.Equal(t, 1, stats.Count)
	assert.Equal(t, new(int64(17163091968/(1<<20))), stats.TotalMiB)
	assert.Nil(t, stats.UsedMiB)
	assert.Nil(t, stats.FreeMiB)
	assert.Nil(t, stats.UtilizationPct)
	zero := liveAMD(t.Context(), stubRocmSMI(map[string][]byte{
		"--showid":  []byte("GPU[0]\n"),
		"--showuse": []byte("GPU[0]: GPU use (%): 0\n"),
	}))
	assert.Equal(t, new(0.0), zero.UtilizationPct)
}
