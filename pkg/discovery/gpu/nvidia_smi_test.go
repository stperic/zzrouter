package gpu

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
)

func TestParseNVIDIASMIRows(t *testing.T) {
	// Fields in the order every caller that cares about bus-id
	// matching currently requests. Tests deliberately use a
	// superset so the parser's padding behavior on short rows
	// is exercised.
	fields := []string{
		nvidiaFieldPCIBusID,
		nvidiaFieldName,
		nvidiaFieldDriverVersion,
		nvidiaFieldMemoryTotalMiB,
		nvidiaFieldComputeCap,
	}

	cases := []struct {
		name     string
		body     string
		wantRows []nvidiaRow
	}{
		{
			name:     "empty output returns no rows",
			body:     "",
			wantRows: []nvidiaRow{},
		},
		{
			name: "single 4090 — canonical driver output",
			body: "00000000:01:00.0, NVIDIA GeForce RTX 4090, 550.54.14, 24564, 8.9\n",
			wantRows: []nvidiaRow{
				{
					nvidiaFieldPCIBusID:       "00000000:01:00.0",
					nvidiaFieldName:           "NVIDIA GeForce RTX 4090",
					nvidiaFieldDriverVersion:  "550.54.14",
					nvidiaFieldMemoryTotalMiB: "24564",
					nvidiaFieldComputeCap:     "8.9",
				},
			},
		},
		{
			name: "two A100s — NVML order (by compute cap) not by bus",
			body: `00000000:81:00.0, NVIDIA A100-SXM4-80GB, 535.104.05, 81920, 8.0
00000000:01:00.0, NVIDIA A100-SXM4-80GB, 535.104.05, 81920, 8.0
`,
			wantRows: []nvidiaRow{
				{
					nvidiaFieldPCIBusID:       "00000000:81:00.0",
					nvidiaFieldName:           "NVIDIA A100-SXM4-80GB",
					nvidiaFieldDriverVersion:  "535.104.05",
					nvidiaFieldMemoryTotalMiB: "81920",
					nvidiaFieldComputeCap:     "8.0",
				},
				{
					nvidiaFieldPCIBusID:       "00000000:01:00.0",
					nvidiaFieldName:           "NVIDIA A100-SXM4-80GB",
					nvidiaFieldDriverVersion:  "535.104.05",
					nvidiaFieldMemoryTotalMiB: "81920",
					nvidiaFieldComputeCap:     "8.0",
				},
			},
		},
		{
			name: "H100 with 4-digit domain (newer driver form)",
			body: "0000:17:00.0, NVIDIA H100 80GB HBM3, 550.90.07, 81559, 9.0\n",
			wantRows: []nvidiaRow{
				{
					nvidiaFieldPCIBusID:       "0000:17:00.0",
					nvidiaFieldName:           "NVIDIA H100 80GB HBM3",
					nvidiaFieldDriverVersion:  "550.90.07",
					nvidiaFieldMemoryTotalMiB: "81559",
					nvidiaFieldComputeCap:     "9.0",
				},
			},
		},
		{
			name: "row with [N/A] compute cap from unsupported driver",
			body: "00000000:01:00.0, Tesla K80, 470.256.02, 11441, [N/A]\n",
			wantRows: []nvidiaRow{
				{
					nvidiaFieldPCIBusID:       "00000000:01:00.0",
					nvidiaFieldName:           "Tesla K80",
					nvidiaFieldDriverVersion:  "470.256.02",
					nvidiaFieldMemoryTotalMiB: "11441",
					nvidiaFieldComputeCap:     "[N/A]",
				},
			},
		},
		{
			name: "short row — missing trailing fields padded with empty strings",
			body: "00000000:01:00.0, NVIDIA L4\n",
			wantRows: []nvidiaRow{
				{
					nvidiaFieldPCIBusID:       "00000000:01:00.0",
					nvidiaFieldName:           "NVIDIA L4",
					nvidiaFieldDriverVersion:  "",
					nvidiaFieldMemoryTotalMiB: "",
					nvidiaFieldComputeCap:     "",
				},
			},
		},
		{
			name: "blank lines between rows are ignored",
			body: "\n00000000:01:00.0, RTX 4090, 550.54.14, 24564, 8.9\n\n\n",
			wantRows: []nvidiaRow{
				{
					nvidiaFieldPCIBusID:       "00000000:01:00.0",
					nvidiaFieldName:           "RTX 4090",
					nvidiaFieldDriverVersion:  "550.54.14",
					nvidiaFieldMemoryTotalMiB: "24564",
					nvidiaFieldComputeCap:     "8.9",
				},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseNVIDIASMIRows([]byte(tc.body), fields)
			if len(got) != len(tc.wantRows) {
				t.Fatalf("row count: got %d, want %d\ngot: %+v",
					len(got), len(tc.wantRows), got)
			}
			for i, gotRow := range got {
				wantRow := tc.wantRows[i]
				for _, f := range fields {
					if gotRow[f] != wantRow[f] {
						t.Errorf("row %d field %q: got %q, want %q",
							i, f, gotRow[f], wantRow[f])
					}
				}
			}
		})
	}
}

func TestPopulateNVIDIADetection(t *testing.T) {
	t.Run("full row", func(t *testing.T) {
		var d Detection
		row := nvidiaRow{
			nvidiaFieldName:           "NVIDIA H100 80GB HBM3",
			nvidiaFieldDriverVersion:  "550.90.07",
			nvidiaFieldMemoryTotalMiB: "81559",
			nvidiaFieldComputeCap:     "9.0",
		}
		populateNVIDIADetection(&d, row)
		if d.Name != "NVIDIA H100 80GB HBM3" {
			t.Errorf("Name: got %q", d.Name)
		}
		if d.DriverVersion != "550.90.07" {
			t.Errorf("DriverVersion: got %q", d.DriverVersion)
		}
		if d.MemoryMB != 81559 {
			t.Errorf("MemoryMB: got %d, want 81559", d.MemoryMB)
		}
		if d.ComputeCapability != "90" {
			// Dot is stripped so the form matches
			// FormatComputeCapability in pkg/discovery/hardware.
			t.Errorf("ComputeCapability: got %q, want %q", d.ComputeCapability, "90")
		}
	})

	t.Run("N/A placeholder leaves field unset", func(t *testing.T) {
		d := Detection{ComputeCapability: "already-set"}
		row := nvidiaRow{nvidiaFieldComputeCap: "[N/A]"}
		populateNVIDIADetection(&d, row)
		if d.ComputeCapability != "already-set" {
			t.Errorf("[N/A] should not overwrite existing value, got %q", d.ComputeCapability)
		}
	})

	t.Run("empty row leaves Detection zero", func(t *testing.T) {
		var d Detection
		populateNVIDIADetection(&d, nvidiaRow{})
		if d.Name != "" || d.DriverVersion != "" || d.MemoryMB != 0 || d.ComputeCapability != "" {
			t.Errorf("empty row should leave Detection zero, got %+v", d)
		}
	})

	t.Run("non-numeric memory is silently skipped", func(t *testing.T) {
		var d Detection
		row := nvidiaRow{nvidiaFieldMemoryTotalMiB: "not-a-number"}
		populateNVIDIADetection(&d, row)
		if d.MemoryMB != 0 {
			t.Errorf("MemoryMB: got %d, want 0", d.MemoryMB)
		}
	})
}

func TestIsMeaningful(t *testing.T) {
	cases := map[string]bool{
		"":                false,
		"value":           true,
		"[N/A]":           false,
		"[n/a]":           false,
		"N/A":             false,
		"n/a":             false,
		"0":               true, // a real zero value, not a missing field
		"  spaces only  ": true, // trim is the caller's job; we don't second-guess
	}
	for in, want := range cases {
		if got := isMeaningful(in); got != want {
			t.Errorf("isMeaningful(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestWrapWithStderr(t *testing.T) {
	t.Run("plain error passes through unchanged", func(t *testing.T) {
		base := errors.New("boom")
		got := wrapWithStderr(base, "nvidia-smi")
		// Plain (non-ExitError) errors should round-trip with no
		// added context — there is no Stderr to fold in.
		if got != base {
			t.Errorf("expected pass-through for plain error, got %v", got)
		}
	})

	t.Run("ExitError with Stderr is folded into the message", func(t *testing.T) {
		base := &exec.ExitError{
			Stderr: []byte("Failed to initialize NVML: Driver/library version mismatch\n"),
		}
		got := wrapWithStderr(base, "nvidia-smi")
		msg := got.Error()
		if !strings.Contains(msg, "nvidia-smi") {
			t.Errorf("expected wrapped message to name the binary, got %q", msg)
		}
		if !strings.Contains(msg, "Driver/library version mismatch") {
			t.Errorf("expected stderr text in wrapped message, got %q", msg)
		}
	})

	t.Run("errors.Is unwraps through the wrap", func(t *testing.T) {
		// fmt.Errorf("%s: %w: %s", ...) is what wrapWithStderr
		// builds, so errors.Is should still walk from the wrapped
		// error back to the original ExitError.
		base := &exec.ExitError{Stderr: []byte("anything")}
		got := wrapWithStderr(base, "nvidia-smi")
		if !errors.Is(got, base) {
			t.Errorf("errors.Is should walk through wrapWithStderr to the wrapped *exec.ExitError")
		}
	})

	t.Run("ExitError with empty Stderr is left alone", func(t *testing.T) {
		base := &exec.ExitError{Stderr: nil}
		got := wrapWithStderr(base, "nvidia-smi")
		// No stderr → nothing meaningful to add → return the
		// original error unwrapped so callers' error matching
		// stays simple.
		if got != base {
			t.Errorf("expected pass-through when Stderr is empty, got %v", got)
		}
	})
}

func TestParseNVIDIACUDAVersion(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "driver 550 banner",
			in: `Sun Apr 13 12:00:00 2026
+-----------------------------------------------------------------------------------------+
| NVIDIA-SMI 550.54.14              Driver Version: 550.54.14      CUDA Version: 12.4     |
|-----------------------------------------+------------------------+----------------------+
`,
			want: "12.4",
		},
		{
			name: "driver 470 legacy banner with CUDA 11.4",
			in:   "| NVIDIA-SMI 470.256.02   Driver Version: 470.256.02   CUDA Version: 11.4     |\n",
			want: "11.4",
		},
		{
			name: "extra whitespace between token and value",
			in:   "CUDA Version:     13.0",
			want: "13.0",
		},
		{
			name: "no CUDA marker returns empty",
			in:   "| NVIDIA-SMI 550.54.14              Driver Version: 550.54.14 |\n",
			want: "",
		},
		{
			name: "empty input",
			in:   "",
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseNVIDIACUDAVersion([]byte(tc.in)); got != tc.want {
				t.Errorf("parseNVIDIACUDAVersion = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNormalizePCIAddress(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"ghw canonical form", "0000:01:00.0", "0000:01:00.0"},
		{"NVML 8-digit domain collapses to 4", "00000000:01:00.0", "0000:01:00.0"},
		{"uppercase hex lowered", "0000:0A:00.0", "0000:0a:00.0"},
		{"leading whitespace trimmed", "  0000:01:00.0", "0000:01:00.0"},
		{"5-digit domain collapses to canonical 4-digit form", "00001:01:00.0", "0001:01:00.0"},
		{"single-digit domain padded", "0:01:00.0", "0000:01:00.0"},
		{"no colon returned as-is (caller gives us garbage)", "garbage", "garbage"},
		{"all-zero domain stays zero-padded", "0000:00:00.0", "0000:00:00.0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizePCIAddress(tc.in); got != tc.want {
				t.Errorf("normalizePCIAddress(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
