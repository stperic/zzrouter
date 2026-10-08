package hardware

import (
	"testing"

	gpuProbe "github.com/stperic/zzrouter/pkg/discovery/gpu"
)

// TestInventoryToHardwareInfo pins the reshape from
// gpu.Inventory to HardwareInfo. This is the whole of commit 2's
// new logic — the fields that need to match the legacy contract,
// the unit conversion from MiB to GB, the Apple compute-cap
// fallback, and the priority-based GPUType selection.
func TestInventoryToHardwareInfo(t *testing.T) {
	t.Run("mixed NVIDIA + AMD + Intel node — NVIDIA wins GPUType", func(t *testing.T) {
		inv := gpuProbe.Inventory{
			Cards: []gpuProbe.Card{
				{
					Vendor:            gpuProbe.VendorNVIDIA,
					PCIAddress:        "0000:01:00.0",
					Name:              "NVIDIA A100-SXM4-40GB",
					MemoryMiB:         40960,
					DriverVersion:     "550.54.14",
					ComputeCapability: "80",
				},
				{
					Vendor:        gpuProbe.VendorAMD,
					PCIAddress:    "0000:03:00.0",
					Name:          "AMD Radeon Pro W7900",
					MemoryMiB:     49152,
					DriverVersion: "6.1.0",
				},
				{
					Vendor:     gpuProbe.VendorIntel,
					PCIAddress: "0000:00:02.0",
					Name:       "UHD Graphics 630",
				},
			},
		}
		hw := InventoryToHardwareInfo(inv)
		if hw.GPUCount != 3 {
			t.Errorf("GPUCount = %d, want 3", hw.GPUCount)
		}
		if hw.GPUType != "nvidia" {
			t.Errorf("GPUType = %q, want nvidia (priority winner over AMD + Intel)", hw.GPUType)
		}
		if len(hw.GPUs) != 3 {
			t.Fatalf("len(GPUs) = %d, want 3", len(hw.GPUs))
		}
		// MiB → GB conversion: 40960 MiB / 1024 = 40 GB.
		if hw.GPUs[0].MemoryGB != 40.0 {
			t.Errorf("NVIDIA MemoryGB = %.2f, want 40.0", hw.GPUs[0].MemoryGB)
		}
		if hw.GPUs[0].PCIAddress != "0000:01:00.0" {
			t.Errorf("PCIAddress not preserved: %q", hw.GPUs[0].PCIAddress)
		}
		if hw.GPUs[0].ComputeCapability != "80" {
			t.Errorf("ComputeCapability = %q", hw.GPUs[0].ComputeCapability)
		}
		if hw.GPUs[1].MemoryGB != 48.0 { // 49152 / 1024
			t.Errorf("AMD MemoryGB = %.2f, want 48.0", hw.GPUs[1].MemoryGB)
		}
		// Intel card has no memory — MemoryGB stays zero.
		if hw.GPUs[2].MemoryGB != 0 {
			t.Errorf("Intel MemoryGB should be 0 (ghw has no VRAM), got %.2f", hw.GPUs[2].MemoryGB)
		}
	})

	t.Run("no cards → GPUType none, no GPUs slice", func(t *testing.T) {
		hw := InventoryToHardwareInfo(gpuProbe.Inventory{})
		if hw.GPUType != "none" {
			t.Errorf("GPUType = %q, want none", hw.GPUType)
		}
		if hw.GPUCount != 0 {
			t.Errorf("GPUCount = %d, want 0", hw.GPUCount)
		}
		if len(hw.GPUs) != 0 {
			t.Errorf("GPUs should be empty, got %+v", hw.GPUs)
		}
	})

	t.Run("Apple Silicon card gets compute cap from chip name", func(t *testing.T) {
		inv := gpuProbe.Inventory{
			Cards: []gpuProbe.Card{
				{Vendor: gpuProbe.VendorApple, Name: "Apple M3 Pro"},
			},
		}
		hw := InventoryToHardwareInfo(inv)
		if hw.GPUType != "apple" {
			t.Errorf("GPUType = %q", hw.GPUType)
		}
		if hw.GPUs[0].ComputeCapability != "30" {
			t.Errorf("Apple M3 should map to compute cap 30, got %q", hw.GPUs[0].ComputeCapability)
		}
		if hw.GPUs[0].MemoryGB != 0 {
			t.Errorf("Apple Silicon uses unified memory, MemoryGB should stay 0")
		}
	})

	t.Run("AMD-only node → GPUType amd", func(t *testing.T) {
		inv := gpuProbe.Inventory{
			Cards: []gpuProbe.Card{{
				Vendor:    gpuProbe.VendorAMD,
				MemoryMiB: 16384,
			}},
		}
		hw := InventoryToHardwareInfo(inv)
		if hw.GPUType != "amd" {
			t.Errorf("GPUType = %q, want amd", hw.GPUType)
		}
		if hw.GPUs[0].MemoryGB != 16.0 {
			t.Errorf("MemoryGB = %.2f, want 16.0", hw.GPUs[0].MemoryGB)
		}
	})

	t.Run("Intel iGPU only → GPUType intel", func(t *testing.T) {
		inv := gpuProbe.Inventory{
			Cards: []gpuProbe.Card{{Vendor: gpuProbe.VendorIntel, Name: "UHD Graphics 630"}},
		}
		hw := InventoryToHardwareInfo(inv)
		if hw.GPUType != "intel" {
			t.Errorf("GPUType = %q, want intel", hw.GPUType)
		}
	})
}

func TestPickBestGPUType(t *testing.T) {
	cases := []struct {
		name  string
		cards []gpuProbe.Card
		want  string
	}{
		{
			name:  "NVIDIA + AMD → nvidia",
			cards: []gpuProbe.Card{{Vendor: gpuProbe.VendorAMD}, {Vendor: gpuProbe.VendorNVIDIA}},
			want:  "nvidia",
		},
		{
			name:  "AMD + Intel → amd",
			cards: []gpuProbe.Card{{Vendor: gpuProbe.VendorIntel}, {Vendor: gpuProbe.VendorAMD}},
			want:  "amd",
		},
		{
			name:  "Intel only → intel",
			cards: []gpuProbe.Card{{Vendor: gpuProbe.VendorIntel}},
			want:  "intel",
		},
		{
			name:  "Apple only → apple",
			cards: []gpuProbe.Card{{Vendor: gpuProbe.VendorApple}},
			want:  "apple",
		},
		{
			name:  "Other only → other",
			cards: []gpuProbe.Card{{Vendor: gpuProbe.VendorOther}},
			want:  "other",
		},
		{
			name:  "empty → other (defensive)",
			cards: nil,
			want:  "other",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := pickBestGPUType(tc.cards)
			if got != tc.want {
				t.Errorf("pickBestGPUType = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAppleComputeCapFromName(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"Apple M1", "10"},
		{"Apple M1 Pro", "10"},
		{"Apple M2 Max", "20"},
		{"Apple M3 Pro", "30"},
		{"Apple M4 Ultra", "40"},
		{"Apple M10", "100"}, // defensive: two-digit chip number
		{"not an apple chip", ""},
		{"", ""},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := appleComputeCapFromName(tc.in)
			if got != tc.want {
				t.Errorf("appleComputeCapFromName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
