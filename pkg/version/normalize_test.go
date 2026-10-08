package version

import "testing"

func TestNormalizeHostURL(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"192.0.2.10:9090", "http://192.0.2.10:9090"},
		{"192.0.2.10", "http://192.0.2.10"},
		{"host.example:9090", "http://host.example:9090"},
		{"http://192.0.2.10:9090", "http://192.0.2.10:9090"},
		{"https://192.0.2.10:9090", "https://192.0.2.10:9090"},
		{"  192.0.2.10:9090  ", "http://192.0.2.10:9090"},
		{"[::1]:9090", "http://[::1]:9090"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := NormalizeHostURL(tc.in); got != tc.want {
			t.Errorf("NormalizeHostURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
