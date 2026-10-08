package upstream

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The regression this comparator exists for: with a semver parser, every one
// of these is silently unorderable, so Newest returns the highest *strict*
// version and an out-of-date node reads as up to date. All twelve are real
// vllm releases.
func TestPEP440OrdersRealVllmVersions(t *testing.T) {
	real := []string{
		"0.2.1.post1", "0.4.0.post1", "0.5.0.post1", "0.5.3.post1",
		"0.6.1.post1", "0.6.1.post2", "0.6.3.post1", "0.6.4.post1",
		"0.6.6.post1", "0.8.5.post1", "0.9.0.1", "0.10.1.1",
	}
	for _, v := range real {
		t.Run(v, func(t *testing.T) {
			parsed, ok := parsePEP440(v)
			require.True(t, ok, "%s must be orderable", v)
			assert.False(t, isPEP440PreRelease(parsed), "%s is a final release", v)
		})
	}

	src := pypiSource()
	// The exact shape that would have reported "up to date" wrongly.
	assert.Equal(t, StatusNewer, Compare(src, "0.6.1", "0.6.1.post2"))
	assert.Equal(t, "0.6.1.post2", Newest(src, []string{"0.6.1", "0.6.1.post1", "0.6.1.post2"}))
	assert.Equal(t, "0.10.1.1", Newest(src, []string{"0.10.1", "0.10.1.1", "0.9.0.1"}))
}

func TestPEP440Ordering(t *testing.T) {
	tests := []struct {
		name   string
		lower  string
		higher string
	}{
		{"patch", "1.2.3", "1.2.4"},
		{"minor over patch", "1.2.9", "1.3.0"},
		{"double digit minor", "0.9.2", "0.27.1"},
		{"extra segment outranks", "0.10.1", "0.10.1.1"},
		{"post outranks final", "0.6.1", "0.6.1.post1"},
		{"post ordering", "0.6.1.post1", "0.6.1.post2"},
		{"final outranks rc", "1.0.0rc1", "1.0.0"},
		{"rc ordering", "1.0.0rc1", "1.0.0rc2"},
		{"alpha before beta", "1.0.0a1", "1.0.0b1"},
		{"beta before rc", "1.0.0b2", "1.0.0rc1"},
		{"dev before release", "1.0.0.dev1", "1.0.0"},
		{"dev before alpha", "1.0.0.dev1", "1.0.0a1"},
		{"epoch dominates", "9.9.9", "1!1.0.0"},
		{"short form", "1.2", "1.3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lo, ok := parsePEP440(tt.lower)
			require.True(t, ok, tt.lower)
			hi, ok := parsePEP440(tt.higher)
			require.True(t, ok, tt.higher)
			assert.Equal(t, -1, comparePEP440(lo, hi), "%s < %s", tt.lower, tt.higher)
			assert.Equal(t, 1, comparePEP440(hi, lo), "%s > %s", tt.higher, tt.lower)
		})
	}
}

func TestPEP440Equality(t *testing.T) {
	// Trailing zeros and spelling variants are the same version.
	pairs := [][2]string{
		{"1.2", "1.2.0"},
		{"1.2.0", "1.2.0.0"},
		{"1.0.0rc1", "1.0.0-rc1"},
		{"1.0.0rc1", "1.0.0.rc.1"},
		{"1.0.0alpha1", "1.0.0a1"},
		{"1.0.0post1", "1.0.0-1"},
		{"1.0.0", "v1.0.0"},
		// Local identifiers do not participate in ordering.
		{"1.0.0", "1.0.0+cu118"},
	}
	for _, p := range pairs {
		t.Run(p[0]+"=="+p[1], func(t *testing.T) {
			a, ok := parsePEP440(p[0])
			require.True(t, ok, p[0])
			b, ok := parsePEP440(p[1])
			require.True(t, ok, p[1])
			assert.Equal(t, 0, comparePEP440(a, b))
		})
	}
}

func TestPEP440PreReleaseDetection(t *testing.T) {
	for _, v := range []string{"1.0.0a1", "1.0.0b1", "1.0.0rc1", "1.0.0.dev1", "0.28.0rc1"} {
		parsed, ok := parsePEP440(v)
		require.True(t, ok, v)
		assert.True(t, isPEP440PreRelease(parsed), "%s is a prerelease", v)
	}
	for _, v := range []string{"1.0.0", "0.6.1.post1", "0.10.1.1", "1!2.0"} {
		parsed, ok := parsePEP440(v)
		require.True(t, ok, v)
		assert.False(t, isPEP440PreRelease(parsed), "%s is final", v)
	}
}

func TestPEP440Rejects(t *testing.T) {
	for _, v := range []string{"", "nightly", "latest", "b10502", "1.2.x", "not.a.version", "1..2"} {
		t.Run(v, func(t *testing.T) {
			_, ok := parsePEP440(v)
			assert.False(t, ok, "%q must not parse", v)
		})
	}
}

// A prerelease must never win, even when it is numerically highest.
func TestNewestSkipsPEP440PreReleases(t *testing.T) {
	src := pypiSource()
	assert.Equal(t, "0.27.1", Newest(src, []string{"0.27.1", "0.28.0rc1", "0.28.0a1", "0.28.0.dev3"}))
}
