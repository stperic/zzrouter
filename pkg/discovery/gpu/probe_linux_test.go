//go:build linux

package gpu

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// writeFakeSysfsDevice creates a /sys/bus/pci/devices/<id>/vendor
// file under root and returns the containing directory. Matches
// the layout sysfsHasPCIVendor walks.
func writeFakeSysfsDevice(t *testing.T, root, instanceID, vendorHex string) {
	t.Helper()
	dir := filepath.Join(root, instanceID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	// sysfs files include a trailing newline just like the real
	// kernel exposure, and start with the 0x prefix.
	if err := os.WriteFile(filepath.Join(dir, "vendor"), []byte("0x"+vendorHex+"\n"), 0o644); err != nil {
		t.Fatalf("write vendor: %v", err)
	}
}

// stubQueryNVIDIASMI returns a function-typed value that produces
// a fixed result, suitable for plugging into linuxProbeOptions.
// Lets WSL2 / driver-error / driver-OK paths run through
// probeNVIDIALinux without a real nvidia-smi on PATH.
func stubQueryNVIDIASMI(rows []nvidiaRow, err error) func(ctx context.Context, fields ...string) ([]nvidiaRow, error) {
	return func(ctx context.Context, fields ...string) ([]nvidiaRow, error) {
		// Pad each row to the requested field set so probe code
		// indexing by field name (which uses zero-value lookup on
		// missing keys) doesn't see surprises.
		out := make([]nvidiaRow, 0, len(rows))
		for _, r := range rows {
			row := make(nvidiaRow, len(fields))
			for _, f := range fields {
				row[f] = r[f]
			}
			out = append(out, row)
		}
		return out, err
	}
}

func TestSysfsHasPCIVendor(t *testing.T) {
	t.Run("empty tree returns false", func(t *testing.T) {
		root := t.TempDir()
		if sysfsHasPCIVendor(root, PCIVendorNVIDIA) {
			t.Fatal("expected false on empty /sys tree")
		}
	})

	t.Run("NVIDIA present", func(t *testing.T) {
		root := t.TempDir()
		writeFakeSysfsDevice(t, root, "0000:01:00.0", PCIVendorNVIDIA)
		if !sysfsHasPCIVendor(root, PCIVendorNVIDIA) {
			t.Fatal("expected NVIDIA match via sysfs fallback")
		}
	})

	t.Run("AMD present, NVIDIA absent", func(t *testing.T) {
		root := t.TempDir()
		writeFakeSysfsDevice(t, root, "0000:03:00.0", PCIVendorAMD)
		if !sysfsHasPCIVendor(root, PCIVendorAMD) {
			t.Fatal("expected AMD match")
		}
		if sysfsHasPCIVendor(root, PCIVendorNVIDIA) {
			t.Fatal("NVIDIA should not match when only AMD is present")
		}
	})

	t.Run("case-insensitive match on the hex id", func(t *testing.T) {
		root := t.TempDir()
		// The fixture writes lowercase "10de"; call with uppercase.
		writeFakeSysfsDevice(t, root, "0000:01:00.0", "10de")
		if !sysfsHasPCIVendor(root, "10DE") {
			t.Fatal("sysfsHasPCIVendor should be case-insensitive on the vendor id")
		}
	})

	t.Run("multi-function device with mix of vendors", func(t *testing.T) {
		root := t.TempDir()
		// An NVIDIA card typically presents its audio function
		// on the same bus with a different device id but the same
		// vendor id, so any non-display vendor function in the
		// list should still short-circuit to true.
		writeFakeSysfsDevice(t, root, "0000:01:00.0", PCIVendorNVIDIA) // VGA
		writeFakeSysfsDevice(t, root, "0000:01:00.1", PCIVendorNVIDIA) // HDMI audio
		writeFakeSysfsDevice(t, root, "0000:02:00.0", "8086")          // Intel NIC, irrelevant
		if !sysfsHasPCIVendor(root, PCIVendorNVIDIA) {
			t.Fatal("expected NVIDIA match on multi-function bus")
		}
	})

	t.Run("malformed vendor file is skipped, not fatal", func(t *testing.T) {
		root := t.TempDir()
		dir := filepath.Join(root, "0000:01:00.0")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		// Random bytes where "0xNNNN" should be.
		if err := os.WriteFile(filepath.Join(dir, "vendor"), []byte("garbage"), 0o644); err != nil {
			t.Fatal(err)
		}
		// A real device on a sibling path — walker should skip
		// the broken entry and find the real one.
		writeFakeSysfsDevice(t, root, "0000:02:00.0", PCIVendorAMD)
		if !sysfsHasPCIVendor(root, PCIVendorAMD) {
			t.Fatal("walker should tolerate one malformed vendor file")
		}
	})

	t.Run("linuxHasPCIVendor falls through to sysfs walker", func(t *testing.T) {
		// Smoke test the shim between linuxHasPCIVendor and the
		// pure sysfs walker. Pass the tempdir as the third arg so
		// no package-level state is touched — the previous
		// implementation used a mutable global, which would have
		// raced under -race with parallel tests.
		root := t.TempDir()
		writeFakeSysfsDevice(t, root, "0000:01:00.0", PCIVendorNVIDIA)

		// lspci may not be present under `go test` in a CI sandbox;
		// linuxHasPCIVendor must fall through to the sysfs walker
		// and find our fixture regardless of whether lspci runs.
		if !linuxHasPCIVendor(context.Background(), PCIVendorNVIDIA, root) {
			t.Fatal("linuxHasPCIVendor should find the fixture via sysfs fallback")
		}
	})
}

func TestHasPCIVendorMatch(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"empty output", "", false},
		{"only whitespace", "   \n\t\n", false},
		{"real lspci row", "01:00.0 VGA compatible controller: NVIDIA Corporation AD102 [GeForce RTX 4090]\n", true},
		{"multiple rows", "01:00.0 VGA ...\n01:00.1 Audio ...\n", true},
		{"trailing blank lines don't confuse the scan", "01:00.0 VGA ...\n\n\n", true},
		{"4-digit domain prefix (lspci -D)", "0000:01:00.0 VGA compatible controller: NVIDIA Corporation AD102\n", true},
		// busybox-pciutils on Alpine routes permission-denied
		// warnings to stdout. The previous "any non-empty line"
		// check classified the warning as a match and flipped
		// a no-GPU Alpine container into StateHardwareNoDriver.
		{"busybox stdout warning, no device row", "Cannot open /proc/bus/pci\n", false},
		{"pcilib warning followed by nothing", "pcilib: Cannot open /sys/bus/pci/devices: Permission denied\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := hasPCIVendorMatch([]byte(tc.in), PCIVendorNVIDIA)
			if got != tc.want {
				t.Errorf("hasPCIVendorMatch(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestProbeNVIDIALinux exercises the probe paths that depend on
// the linuxProbeOptions injection seam. Each subtest constructs a
// minimal options struct that pins a tempdir sysfs and a fake
// queryNVIDIASMI, lets the probe run, and checks the resulting
// Detection's State / Count / Diagnostic / Name.
func TestProbeNVIDIALinux(t *testing.T) {
	t.Run("WSL2 path: empty PCI scan but nvidia-smi succeeds → DriverOK", func(t *testing.T) {
		opts := linuxProbeOptions{
			sysfsRoot: t.TempDir(), // empty — no PCI devices
			queryNVIDIASMI: stubQueryNVIDIASMI([]nvidiaRow{
				{
					nvidiaFieldName:           "NVIDIA GeForce RTX 4090",
					nvidiaFieldDriverVersion:  "550.54.14",
					nvidiaFieldMemoryTotalMiB: "24564",
					nvidiaFieldComputeCap:     "8.9",
				},
			}, nil),
		}.withDefaults()

		d, err := probeNVIDIALinux(context.Background(), opts)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if d.State != StateDriverOK {
			t.Errorf("State = %v, want DriverOK (WSL2 path should not require a PCI hit)", d.State)
		}
		if d.Count != 1 {
			t.Errorf("Count = %d, want 1", d.Count)
		}
		if d.Name != "NVIDIA GeForce RTX 4090" {
			t.Errorf("Name = %q", d.Name)
		}
		if d.Diagnostic != "" {
			t.Errorf("Diagnostic should be empty on the happy path, got %q", d.Diagnostic)
		}
	})

	t.Run("nvidia-smi error on real hardware → HardwareNoDriver with diagnostic", func(t *testing.T) {
		root := t.TempDir()
		writeFakeSysfsDevice(t, root, "0000:01:00.0", PCIVendorNVIDIA)
		opts := linuxProbeOptions{
			sysfsRoot:      root,
			queryNVIDIASMI: stubQueryNVIDIASMI(nil, errors.New("nvidia-smi: exit status 9: Failed to initialize NVML: Driver/library version mismatch")),
		}.withDefaults()

		d, err := probeNVIDIALinux(context.Background(), opts)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if d.State != StateHardwareNoDriver {
			t.Errorf("State = %v, want HardwareNoDriver", d.State)
		}
		if d.Diagnostic == "" {
			t.Error("Diagnostic should be populated from the wrapped nvidia-smi error")
		}
		if d.InstallHint == "" {
			t.Error("InstallHint should be populated for HardwareNoDriver")
		}
	})

	t.Run("hardware present but nvidia-smi missing → HardwareNoDriver, no diagnostic", func(t *testing.T) {
		root := t.TempDir()
		writeFakeSysfsDevice(t, root, "0000:01:00.0", PCIVendorNVIDIA)
		opts := linuxProbeOptions{
			sysfsRoot:      root,
			queryNVIDIASMI: stubQueryNVIDIASMI(nil, nil), // (nil, nil) = binary not found
		}.withDefaults()

		d, err := probeNVIDIALinux(context.Background(), opts)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if d.State != StateHardwareNoDriver {
			t.Errorf("State = %v, want HardwareNoDriver", d.State)
		}
		if d.Diagnostic != "" {
			t.Errorf("Diagnostic should be empty when nvidia-smi is absent (no error to wrap), got %q", d.Diagnostic)
		}
		if d.InstallHint == "" {
			t.Error("InstallHint should be populated for HardwareNoDriver")
		}
	})

	t.Run("no hardware, no driver → Absent", func(t *testing.T) {
		opts := linuxProbeOptions{
			sysfsRoot:      t.TempDir(),
			queryNVIDIASMI: stubQueryNVIDIASMI(nil, nil),
		}.withDefaults()

		d, err := probeNVIDIALinux(context.Background(), opts)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if d.State != StateAbsent {
			t.Errorf("State = %v, want Absent", d.State)
		}
		if d.Count != 0 {
			t.Errorf("Count = %d, want 0", d.Count)
		}
	})

	t.Run("ctx cancellation propagates instead of being reclassified", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // cancel before the probe runs

		opts := linuxProbeOptions{
			sysfsRoot:      t.TempDir(),
			queryNVIDIASMI: stubQueryNVIDIASMI(nil, nil),
		}.withDefaults()

		_, err := probeNVIDIALinux(ctx, opts)
		if err == nil {
			t.Fatal("expected ctx.Err() to bubble up, got nil")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("expected context.Canceled, got %v", err)
		}
	})
}

// TestProbeAMDLinux exercises the AMD-specific paths via the
// drmGlob and kfdPath injection seams.
func TestProbeAMDLinux(t *testing.T) {
	t.Run("hardware present, no kfd, no drm vendor match → HardwareNoDriver", func(t *testing.T) {
		root := t.TempDir()
		writeFakeSysfsDevice(t, root, "0000:03:00.0", PCIVendorAMD)
		opts := linuxProbeOptions{
			sysfsRoot: root,
			kfdPath:   filepath.Join(t.TempDir(), "no-kfd-here"),
			drmGlob:   filepath.Join(t.TempDir(), "no-drm-here", "*", "device", "vendor"),
		}.withDefaults()

		d, err := probeAMDLinux(context.Background(), opts)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if d.State != StateHardwareNoDriver {
			t.Errorf("State = %v, want HardwareNoDriver", d.State)
		}
		if d.InstallHint == "" {
			t.Error("InstallHint should be populated for HardwareNoDriver")
		}
	})

	t.Run("hardware present + kfd path exists → DriverOK", func(t *testing.T) {
		sysfs := t.TempDir()
		writeFakeSysfsDevice(t, sysfs, "0000:03:00.0", PCIVendorAMD)
		// Create a fake kfd file the probe can stat.
		kfdDir := t.TempDir()
		kfdPath := filepath.Join(kfdDir, "kfd")
		if err := os.WriteFile(kfdPath, []byte{}, 0o644); err != nil {
			t.Fatal(err)
		}
		// Set up two fake DRM cards so Count comes back as 2.
		drmRoot := t.TempDir()
		for _, card := range []string{"card0", "card1"} {
			cardDir := filepath.Join(drmRoot, card, "device")
			if err := os.MkdirAll(cardDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cardDir, "vendor"), []byte("0x"+PCIVendorAMD+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		opts := linuxProbeOptions{
			sysfsRoot: sysfs,
			kfdPath:   kfdPath,
			drmGlob:   filepath.Join(drmRoot, "card*", "device", "vendor"),
		}.withDefaults()

		d, err := probeAMDLinux(context.Background(), opts)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if d.State != StateDriverOK {
			t.Errorf("State = %v, want DriverOK", d.State)
		}
		if d.Count != 2 {
			t.Errorf("Count = %d, want 2 (two fake DRM cards)", d.Count)
		}
	})

	t.Run("no AMD hardware → Absent", func(t *testing.T) {
		opts := linuxProbeOptions{
			sysfsRoot: t.TempDir(),
		}.withDefaults()

		d, err := probeAMDLinux(context.Background(), opts)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if d.State != StateAbsent {
			t.Errorf("State = %v, want Absent", d.State)
		}
	})
}

// TestAMDRenderNodeCount pins the dedup'd helper that countAMDCards
// and hasAMDRenderNode used to share. Pure function — no globals,
// just a glob path.
func TestAMDRenderNodeCount(t *testing.T) {
	t.Run("two distinct AMD cards plus one NVIDIA", func(t *testing.T) {
		root := t.TempDir()
		for i, card := range []string{"card0", "card1", "card2"} {
			cardDir := filepath.Join(root, card, "device")
			if err := os.MkdirAll(cardDir, 0o755); err != nil {
				t.Fatal(err)
			}
			var vendor string
			if i < 2 {
				vendor = "0x" + PCIVendorAMD + "\n"
			} else {
				vendor = "0x10de\n" // NVIDIA — should not be counted
			}
			if err := os.WriteFile(filepath.Join(cardDir, "vendor"), []byte(vendor), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		got := amdRenderNodeCount(filepath.Join(root, "card*", "device", "vendor"))
		if got != 2 {
			t.Errorf("amdRenderNodeCount = %d, want 2 (two AMD cards, one NVIDIA ignored)", got)
		}
	})

	// Canary for the Ryzen APU + Radeon dGPU case, and for modern
	// amdgpu setups that expose the same physical card via two DRM
	// minors. Both card0/device and card1/device are symlinks
	// pointing at the same PCI node, so the count must collapse
	// to 1 instead of double-counting to 2.
	t.Run("two DRM minors behind one physical device dedupe to one", func(t *testing.T) {
		root := t.TempDir()
		// Real PCI node that both card symlinks will resolve to.
		realPCI := filepath.Join(root, "pci", "0000:03:00.0")
		if err := os.MkdirAll(realPCI, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(realPCI, "vendor"), []byte("0x"+PCIVendorAMD+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		// Two card* directories whose `device` is a symlink to
		// the same underlying PCI node.
		drmRoot := filepath.Join(root, "drm")
		for _, card := range []string{"card0", "card1"} {
			cardDir := filepath.Join(drmRoot, card)
			if err := os.MkdirAll(cardDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(realPCI, filepath.Join(cardDir, "device")); err != nil {
				t.Fatal(err)
			}
		}
		got := amdRenderNodeCount(filepath.Join(drmRoot, "card*", "device", "vendor"))
		if got != 1 {
			t.Errorf("amdRenderNodeCount = %d, want 1 (two DRM minors collapsed to one PCI device)", got)
		}
	})

	// Ryzen APU (card0) + Radeon dGPU (card1) — same vendor id on
	// both, but they resolve to different PCI parents, so both
	// should be counted.
	t.Run("iGPU + dGPU are not deduped", func(t *testing.T) {
		root := t.TempDir()
		for _, bdf := range []string{"0000:08:00.0", "0000:03:00.0"} {
			pci := filepath.Join(root, "pci", bdf)
			if err := os.MkdirAll(pci, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(pci, "vendor"), []byte("0x"+PCIVendorAMD+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		drmRoot := filepath.Join(root, "drm")
		cards := map[string]string{
			"card0": "0000:08:00.0",
			"card1": "0000:03:00.0",
		}
		for card, bdf := range cards {
			cardDir := filepath.Join(drmRoot, card)
			if err := os.MkdirAll(cardDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(root, "pci", bdf), filepath.Join(cardDir, "device")); err != nil {
				t.Fatal(err)
			}
		}
		got := amdRenderNodeCount(filepath.Join(drmRoot, "card*", "device", "vendor"))
		if got != 2 {
			t.Errorf("amdRenderNodeCount = %d, want 2 (iGPU + dGPU are distinct PCI devices)", got)
		}
	})
}
