package gpu

import (
	"context"
	"errors"
	"math"
	"testing"
)

func TestNormalizeVRAMUnits(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int64
	}{
		{"ROCm 5.x MB form 16384", "16384", 16384},
		{"ROCm 6.x byte form 17163091968", "17163091968", 17163091968 / (1 << 20)},
		{"zero", "0", 0},
		{"negative", "-1", 0},
		{"garbage", "abc", 0},
		{"empty", "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeVRAMUnits(tc.in)
			if got != tc.want {
				t.Errorf("normalizeVRAMUnits(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

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
			if total != tc.wantTotal || used != tc.wantUsed {
				t.Errorf("total=%d used=%d, want total=%d used=%d", total, used, tc.wantTotal, tc.wantUsed)
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
			if math.Abs(got-tc.want) > 0.001 {
				t.Errorf("parseRocmSMIUtilization = %.3f, want %.3f", got, tc.want)
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

func TestLiveNVIDIAMetrics(t *testing.T) {
	t.Run("two GPUs, aggregate sums, utilization averaged", func(t *testing.T) {
		rows := []nvidiaRow{
			{
				nvidiaFieldMemoryTotalMiB: "40960",
				nvidiaFieldMemoryUsedMiB:  "8192",
				nvidiaFieldMemoryFreeMiB:  "32768",
				nvidiaFieldUtilizationGPU: "50",
			},
			{
				nvidiaFieldMemoryTotalMiB: "24564",
				nvidiaFieldMemoryUsedMiB:  "2048",
				nvidiaFieldMemoryFreeMiB:  "22516",
				nvidiaFieldUtilizationGPU: "30",
			},
		}
		m := liveNVIDIAMetrics(context.Background(), stubNVIDIASMI(rows, nil))
		if m.Count != 2 {
			t.Errorf("Count = %d, want 2", m.Count)
		}
		if m.TotalMemoryMiB != 40960+24564 {
			t.Errorf("TotalMemoryMiB = %d, want 65524", m.TotalMemoryMiB)
		}
		if m.UsedMemoryMiB != 8192+2048 {
			t.Errorf("UsedMemoryMiB = %d, want 10240", m.UsedMemoryMiB)
		}
		if m.FreeMemoryMiB != 32768+22516 {
			t.Errorf("FreeMemoryMiB = %d, want 55284", m.FreeMemoryMiB)
		}
		if math.Abs(m.UtilizationPct-40.0) > 0.001 {
			t.Errorf("UtilizationPct = %.3f, want 40.0 (mean of 50 and 30)", m.UtilizationPct)
		}
	})

	t.Run("[N/A] utilization does not pull the average down", func(t *testing.T) {
		// Canary for the bug in the pre-refactor resource_tracker:
		// it divided totalUtilization by len(rows) unconditionally,
		// so one [N/A] card in a two-card set would report 25%
		// when the live card was at 50%. The correct answer is 50%
		// because only one card supplied a meaningful sample.
		rows := []nvidiaRow{
			{
				nvidiaFieldMemoryTotalMiB: "40960",
				nvidiaFieldMemoryUsedMiB:  "8192",
				nvidiaFieldMemoryFreeMiB:  "32768",
				nvidiaFieldUtilizationGPU: "50",
			},
			{
				nvidiaFieldMemoryTotalMiB: "40960",
				nvidiaFieldMemoryUsedMiB:  "8192",
				nvidiaFieldMemoryFreeMiB:  "32768",
				nvidiaFieldUtilizationGPU: "[N/A]",
			},
		}
		m := liveNVIDIAMetrics(context.Background(), stubNVIDIASMI(rows, nil))
		if m.Count != 2 {
			t.Errorf("Count = %d, want 2 (both rows counted for memory)", m.Count)
		}
		if math.Abs(m.UtilizationPct-50.0) > 0.001 {
			t.Errorf("UtilizationPct = %.3f, want 50.0 ([N/A] sample must not drag the average)", m.UtilizationPct)
		}
	})

	t.Run("all [N/A] utilization → zero without dividing by zero", func(t *testing.T) {
		rows := []nvidiaRow{
			{
				nvidiaFieldMemoryTotalMiB: "40960",
				nvidiaFieldUtilizationGPU: "[N/A]",
			},
		}
		m := liveNVIDIAMetrics(context.Background(), stubNVIDIASMI(rows, nil))
		if m.UtilizationPct != 0 {
			t.Errorf("UtilizationPct = %.3f, want 0", m.UtilizationPct)
		}
	})

	t.Run("nvidia-smi error → zero LiveMetrics with Vendor set", func(t *testing.T) {
		m := liveNVIDIAMetrics(context.Background(), stubNVIDIASMI(nil, errors.New("boom")))
		if m.Count != 0 {
			t.Errorf("Count = %d, want 0", m.Count)
		}
		if m.Vendor != VendorNVIDIA {
			t.Errorf("Vendor = %q, want nvidia (should always be set so callers can match)", m.Vendor)
		}
	})

	t.Run("nvidia-smi absent (nil, nil) → Count 0", func(t *testing.T) {
		m := liveNVIDIAMetrics(context.Background(), stubNVIDIASMI(nil, nil))
		if m.Count != 0 {
			t.Errorf("Count = %d, want 0", m.Count)
		}
	})
}

func TestLiveAMDMetrics(t *testing.T) {
	t.Run("two GPUs, ROCm 5.x MB format", func(t *testing.T) {
		query := stubRocmSMI(map[string][]byte{
			"--showid":           []byte("GPU[0]\nGPU[1]\n"),
			"--showmeminfo vram": []byte("GPU[0]\t\t: vram Total Memory (MB): 16384\nGPU[0]\t\t: vram Used Memory (MB): 4096\n"),
			"--showuse":          []byte("GPU[0]\t\t: GPU use (%): 25\n"),
		})
		m := liveAMDMetrics(context.Background(), query)
		if m.Count != 2 {
			t.Errorf("Count = %d, want 2", m.Count)
		}
		if m.TotalMemoryMiB != 16384 {
			t.Errorf("TotalMemoryMiB = %d, want 16384", m.TotalMemoryMiB)
		}
		if m.UsedMemoryMiB != 4096 {
			t.Errorf("UsedMemoryMiB = %d, want 4096", m.UsedMemoryMiB)
		}
		if m.FreeMemoryMiB != 16384-4096 {
			t.Errorf("FreeMemoryMiB = %d, want 12288", m.FreeMemoryMiB)
		}
		if math.Abs(m.UtilizationPct-25.0) > 0.001 {
			t.Errorf("UtilizationPct = %.3f, want 25.0", m.UtilizationPct)
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
		m := liveAMDMetrics(context.Background(), query)
		wantTotal := int64(17163091968 / (1 << 20)) // 16376 MiB
		wantUsed := int64(1073741824 / (1 << 20))   // 1024 MiB
		if m.TotalMemoryMiB != wantTotal {
			t.Errorf("TotalMemoryMiB = %d, want %d", m.TotalMemoryMiB, wantTotal)
		}
		if m.UsedMemoryMiB != wantUsed {
			t.Errorf("UsedMemoryMiB = %d, want %d", m.UsedMemoryMiB, wantUsed)
		}
		if m.FreeMemoryMiB != wantTotal-wantUsed {
			t.Errorf("FreeMemoryMiB = %d, want %d", m.FreeMemoryMiB, wantTotal-wantUsed)
		}
		if math.Abs(m.UtilizationPct-42.0) > 0.001 {
			t.Errorf("UtilizationPct = %.3f, want 42.0", m.UtilizationPct)
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
		m := liveAMDMetrics(context.Background(), query)
		if m.Count != 2 {
			t.Errorf("Count = %d, want 2", m.Count)
		}
		if m.TotalMemoryMiB != 16384+24576 {
			t.Errorf("TotalMemoryMiB = %d, want 40960 (single-card aggregate would be 16384)", m.TotalMemoryMiB)
		}
		if m.UsedMemoryMiB != 4096+8192 {
			t.Errorf("UsedMemoryMiB = %d, want 12288 (single-card aggregate would be 4096)", m.UsedMemoryMiB)
		}
		wantFree := (16384 + 24576) - (4096 + 8192)
		if m.FreeMemoryMiB != int64(wantFree) {
			t.Errorf("FreeMemoryMiB = %d, want %d", m.FreeMemoryMiB, wantFree)
		}
		if math.Abs(m.UtilizationPct-50.0) > 0.001 {
			t.Errorf("UtilizationPct = %.3f, want 50.0 (single-card aggregate would be 25.0)", m.UtilizationPct)
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
		m := liveAMDMetrics(context.Background(), query)
		wantTotal := int64(2 * (17163091968 / (1 << 20)))
		wantUsed := int64((1073741824 / (1 << 20)) + (536870912 / (1 << 20)))
		if m.TotalMemoryMiB != wantTotal {
			t.Errorf("TotalMemoryMiB = %d, want %d", m.TotalMemoryMiB, wantTotal)
		}
		if m.UsedMemoryMiB != wantUsed {
			t.Errorf("UsedMemoryMiB = %d, want %d", m.UsedMemoryMiB, wantUsed)
		}
		if math.Abs(m.UtilizationPct-50.0) > 0.001 {
			t.Errorf("UtilizationPct = %.3f, want 50.0 (mean of 0 and 100)", m.UtilizationPct)
		}
	})

	t.Run("--showid missing → Count 0, no further calls", func(t *testing.T) {
		m := liveAMDMetrics(context.Background(), stubRocmSMI(nil))
		if m.Count != 0 {
			t.Errorf("Count = %d, want 0", m.Count)
		}
	})

	t.Run("--showid present but parses to zero → Count 0", func(t *testing.T) {
		m := liveAMDMetrics(context.Background(), stubRocmSMI(map[string][]byte{
			"--showid": []byte("\n\n"),
		}))
		if m.Count != 0 {
			t.Errorf("Count = %d, want 0", m.Count)
		}
	})
}

func TestLiveMetricsContext(t *testing.T) {
	t.Run("mixed NVIDIA + AMD node", func(t *testing.T) {
		opts := liveMetricsOptions{
			queryNVIDIASMI: stubNVIDIASMI([]nvidiaRow{{
				nvidiaFieldMemoryTotalMiB: "24564",
				nvidiaFieldMemoryUsedMiB:  "4096",
				nvidiaFieldMemoryFreeMiB:  "20468",
				nvidiaFieldUtilizationGPU: "75",
			}}, nil),
			queryRocmSMI: stubRocmSMI(map[string][]byte{
				"--showid":           []byte("GPU[0]\n"),
				"--showmeminfo vram": []byte("Total Memory (B): 17163091968\nUsed Memory (B): 0\n"),
				"--showuse":          []byte("GPU[0]\t\t: GPU use (%): 0\n"),
			}),
		}
		set := liveMetricsContext(context.Background(), opts.withDefaults())
		if set.NVIDIA.Count != 1 || set.NVIDIA.TotalMemoryMiB != 24564 {
			t.Errorf("NVIDIA = %+v", set.NVIDIA)
		}
		if set.AMD.Count != 1 || set.AMD.TotalMemoryMiB != 17163091968/(1<<20) {
			t.Errorf("AMD = %+v", set.AMD)
		}
		if set.NVIDIA.Vendor != VendorNVIDIA || set.AMD.Vendor != VendorAMD {
			t.Errorf("Vendor tags wrong: NVIDIA=%q AMD=%q", set.NVIDIA.Vendor, set.AMD.Vendor)
		}
	})

	t.Run("no GPU on this node", func(t *testing.T) {
		opts := liveMetricsOptions{
			queryNVIDIASMI: stubNVIDIASMI(nil, nil),
			queryRocmSMI:   stubRocmSMI(nil),
		}
		set := liveMetricsContext(context.Background(), opts.withDefaults())
		if set.NVIDIA.Count != 0 || set.AMD.Count != 0 {
			t.Errorf("expected zero set, got %+v", set)
		}
	})

	t.Run("NVIDIA-only node: AMD is a zero LiveMetrics", func(t *testing.T) {
		opts := liveMetricsOptions{
			queryNVIDIASMI: stubNVIDIASMI([]nvidiaRow{{
				nvidiaFieldMemoryTotalMiB: "24564",
				nvidiaFieldUtilizationGPU: "10",
			}}, nil),
			queryRocmSMI: stubRocmSMI(nil),
		}
		set := liveMetricsContext(context.Background(), opts.withDefaults())
		if set.NVIDIA.Count != 1 {
			t.Errorf("NVIDIA.Count = %d, want 1", set.NVIDIA.Count)
		}
		if set.AMD.Count != 0 {
			t.Errorf("AMD.Count = %d, want 0", set.AMD.Count)
		}
		if set.AMD.Vendor != VendorAMD {
			t.Errorf("AMD.Vendor should still be set even when absent, got %q", set.AMD.Vendor)
		}
	})
}
