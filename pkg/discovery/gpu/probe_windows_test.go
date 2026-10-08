//go:build windows

package gpu

import "testing"

func TestStripRegIndirectPrefix(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty string", "", ""},
		{
			name: "real NVIDIA DeviceDesc indirect string",
			in:   "@oem42.inf,%nvidia_dev.2684.01%;NVIDIA GeForce RTX 4090",
			want: "NVIDIA GeForce RTX 4090",
		},
		{
			name: "real AMD DeviceDesc indirect string",
			in:   "@oem17.inf,%amdkmdag.devicedesc%;AMD Radeon RX 7900 XTX",
			want: "AMD Radeon RX 7900 XTX",
		},
		{
			name: "no semicolon — Windows fell back to the raw description",
			in:   "NVIDIA GeForce RTX 3080",
			want: "NVIDIA GeForce RTX 3080",
		},
		{
			name: "semicolon with empty tail — return the input rather than drop to empty",
			in:   "@oem42.inf,%k%;",
			want: "@oem42.inf,%k%;",
		},
		{
			name: "multiple semicolons — take everything after the last one",
			in:   "@oem42.inf;%k%;Actual Name",
			want: "Actual Name",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripRegIndirectPrefix(tc.in); got != tc.want {
				t.Errorf("stripRegIndirectPrefix(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestIsVendorDriverService(t *testing.T) {
	cases := map[string]bool{
		// Real vendor display drivers — all accepted.
		"nvlddmkm": true,
		"NVLDDMKM": true, // case-insensitive
		"amdkmdag": true,
		"amdgpu":   true,
		"atikmdag": true,
		"radeon":   true,

		// Windows fallback display drivers — all rejected.
		"":             false,
		"BasicDisplay": false,
		"basicdisplay": false, // case-insensitive
		"vgasave":      false,
		"VGASave":      false,
		"basicrender":  false,

		// Unknown service names are now rejected (allow-list).
		// If a new vendor driver service appears in the wild we
		// add it to knownVendorDriverServices and update this
		// test in the same change.
		"SomeOEMService":  false,
		"WeirdGenericGPU": false,
	}
	for service, want := range cases {
		t.Run(service, func(t *testing.T) {
			if got := isVendorDriverService(service); got != want {
				t.Errorf("isVendorDriverService(%q) = %v, want %v", service, got, want)
			}
		})
	}
}
