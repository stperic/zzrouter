//go:build darwin

package gpu

import "testing"

// TestAppleChipPattern exercises the regex that interprets
// sysctl machdep.cpu.brand_string into an Apple Silicon chip
// designation. The pattern must accept every shipped M-series
// variant and reject anything else (Intel Mac brand strings,
// random non-Apple input, etc.).
func TestAppleChipPattern(t *testing.T) {
	cases := []struct {
		name  string
		brand string
		want  string // expected match; "" means no match
	}{
		// Happy path — every generation Apple has shipped.
		{"M1 base", "Apple M1", "Apple M1"},
		{"M1 Pro", "Apple M1 Pro", "Apple M1 Pro"},
		{"M1 Max", "Apple M1 Max", "Apple M1 Max"},
		{"M1 Ultra", "Apple M1 Ultra", "Apple M1 Ultra"},
		{"M2 base", "Apple M2", "Apple M2"},
		{"M2 Pro", "Apple M2 Pro", "Apple M2 Pro"},
		{"M2 Max", "Apple M2 Max", "Apple M2 Max"},
		{"M3 base", "Apple M3", "Apple M3"},
		{"M3 Pro", "Apple M3 Pro", "Apple M3 Pro"},
		{"M4 base", "Apple M4", "Apple M4"},
		{"M4 Max", "Apple M4 Max", "Apple M4 Max"},

		// Case handling — sysctl has historically returned
		// brand strings in mixed case.
		{"lowercase apple m1", "apple m1", "apple m1"},
		{"uppercase APPLE M2 MAX", "APPLE M2 MAX", "APPLE M2 MAX"},

		// Embedded in a longer brand string — sysctl output on
		// some builds prefixes or suffixes the model name with
		// extra whitespace or copyright info.
		{"leading whitespace", "   Apple M3 Pro", "Apple M3 Pro"},
		{"trailing suffix", "Apple M2 @ 3.49 GHz", "Apple M2"},

		// Negative cases.
		{"empty string", "", ""},
		{"Intel Mac brand", "Intel(R) Core(TM) i9-9880H CPU @ 2.30GHz", ""},
		{"AMD brand (impossible on macOS but sanity check)", "AMD Ryzen 9 5950X", ""},
		{"garbage", "xyzzy", ""},
		{"Apple without the M-series", "Apple A14 Bionic", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := appleChipPattern.FindString(tc.brand)
			if got != tc.want {
				t.Errorf("appleChipPattern.FindString(%q) = %q, want %q",
					tc.brand, got, tc.want)
			}
		})
	}
}
