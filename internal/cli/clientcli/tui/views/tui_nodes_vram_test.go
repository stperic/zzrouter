package views

import "testing"

func TestFormatVRAM(t *testing.T) {
	sixtyFive := 65.0
	full := 0.0

	tests := map[string]struct {
		free  *float64
		total float64
		want  string
	}{
		"measured card names both figures":     {free: &sixtyFive, total: 96, want: "65 GB free / 96 GB total"},
		"a full card is not an unmeasured one": {free: &full, total: 96, want: "0 GB free / 96 GB total"},
		"unmeasured card says so":              {free: nil, total: 96, want: "96 GB total, free N/A"},
		"card with no reported size":           {free: nil, total: 0, want: "size N/A"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := formatVRAM(tc.free, tc.total); got != tc.want {
				t.Errorf("formatVRAM(%v, %v) = %q, want %q", tc.free, tc.total, got, tc.want)
			}
		})
	}
}
