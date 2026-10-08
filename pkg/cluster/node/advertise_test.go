package clusternode

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestValidateAdvertiseURL pins the shared parse-validation helper.
// Single source of truth for the format check; Server Start
// (fail-fast) and the pairing accept handler (defense-in-depth)
// both call it.
func TestValidateAdvertiseURL(t *testing.T) {
	cases := []struct {
		in      string
		wantErr bool
	}{
		{"https://coord-01.internal:9091", false},
		{"http://127.0.0.1:9091", false},
		{"https://coord.example.com", false}, // port optional
		{"coord.example.com:9091", true},     // missing scheme
		{"https://", true},                   // missing host
		{"::::not-a-url", true},              // unparseable
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			err := ValidateAdvertiseURL(tc.in)
			if tc.wantErr {
				assert.Error(t, err, "input %q", tc.in)
			} else {
				assert.NoError(t, err, "input %q", tc.in)
			}
		})
	}
}
