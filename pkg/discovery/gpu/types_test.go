package gpu

import "testing"

func TestParseVendor(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    Vendor
		wantErr bool
	}{
		{"empty stays empty so callers can default", "", "", false},
		{"lowercase nvidia", "nvidia", VendorNVIDIA, false},
		{"uppercase NVIDIA", "NVIDIA", VendorNVIDIA, false},
		{"mixed-case Nvidia", "Nvidia", VendorNVIDIA, false},
		{"leading whitespace", "  nvidia", VendorNVIDIA, false},
		{"trailing whitespace", "amd\t", VendorAMD, false},
		{"apple", "apple", VendorApple, false},
		{"typo nvdia is rejected", "nvdia", "", true},
		{"random garbage is rejected", "foobar", "", true},
		{"amdgpu is rejected — vendor is just 'amd'", "amdgpu", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseVendor(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseVendor(%q): expected error, got nil", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseVendor(%q): unexpected error %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("ParseVendor(%q): got %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestParsePolicy(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    Policy
		wantErr bool
	}{
		{"empty returns PolicyNone", "", PolicyNone, false},
		{"lowercase advisory", "advisory", PolicyAdvisory, false},
		{"uppercase REQUIRED", "REQUIRED", PolicyRequired, false},
		{"mixed-case Advisory", "Advisory", PolicyAdvisory, false},
		{"whitespace-padded required", "  required  ", PolicyRequired, false},
		{"typo advisori is rejected", "advisori", "", true},
		{"typo requrd is rejected", "requrd", "", true},
		{"optional is rejected — only advisory/required exist", "optional", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParsePolicy(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParsePolicy(%q): expected error, got nil", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParsePolicy(%q): unexpected error %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("ParsePolicy(%q): got %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestStateString(t *testing.T) {
	cases := []struct {
		state State
		want  string
	}{
		{StateAbsent, "absent"},
		{StateHardwareNoDriver, "hardware-present/no-driver"},
		{StateDriverOK, "driver-ok"},
		{StateUnknown, "unknown"},
		// Any future value should land on the "unknown" bucket
		// rather than panic or print a cryptic integer.
		{State(999), "unknown"},
	}
	for _, tc := range cases {
		if got := tc.state.String(); got != tc.want {
			t.Errorf("State(%d).String() = %q, want %q", tc.state, got, tc.want)
		}
	}
}
