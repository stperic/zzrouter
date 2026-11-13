package control

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestKeyFingerprintRedactsShortKeys(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  string
		want string
	}{
		{name: "empty"},
		{name: "one byte", key: "x", want: "[redacted]"},
		{name: "four bytes", key: "tiny", want: "[redacted]"},
		{name: "eight bytes", key: "test-key", want: "[redacted]"},
		{name: "twelve bytes", key: "short-secret", want: "[redacted]"},
		{name: "thirteen bytes", key: "abcdefghijklm", want: "abcd...jklm"},
		{name: "long key", key: "test-fingerprint-long-enough-secret", want: "test...cret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, KeyFingerprint(tc.key))
		})
	}
}
