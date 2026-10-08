package servercli

import (
	"strings"
	"testing"
)

func TestShortFingerprint(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "sha256 prefixed hex gets compact form",
			in:   "sha256:51aae8bdd0a31839e5389d59bdd0711f70feba0a504c86879c71d42c4d0018f7",
			want: "sha256:51aae8bd…18f7",
		},
		{
			name: "bare hex gets compact form",
			in:   "51aae8bdd0a31839e5389d59bdd0711f70feba0a504c86879c71d42c4d0018f7",
			want: "51aae8bd…18f7",
		},
		{
			name: "short input returned unchanged",
			in:   "tooshort",
			want: "tooshort",
		},
		{
			name: "empty input",
			in:   "",
			want: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := shortFingerprint(tc.in); got != tc.want {
				t.Errorf("shortFingerprint(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestFormatPairingCode(t *testing.T) {
	if got := formatPairingCode("ABCDEFGHIJKLMNOP"); got != "ABCD-EFGH-IJKL-MNOP" {
		t.Errorf("formatPairingCode didn't dash-group: %q", got)
	}
	if got := formatPairingCode("short"); got != "short" {
		t.Errorf("non-16-char input should pass through: %q", got)
	}
}

// TestDiscoverCoordinator_SkipsWhenSelfIsCoordinator pins the
// defensive gate: a coordinator running `cluster pair` (operator
// mistake — pair is worker-only) must not browse its own mDNS record
// and hand itself back as a pairing target.
func TestDiscoverCoordinator_SkipsWhenSelfIsCoordinator(t *testing.T) {
	entry, err := discoverCoordinator(true)
	if err != nil {
		t.Errorf("unexpected error on self-skip path: %v", err)
	}
	if entry != nil {
		t.Errorf("discoverCoordinator must return nil entry when self is coordinator; got %+v", entry)
	}
}

// TestDiscoverCoordinator_EmptyLANReturnsNil pins the no-coord-found
// semantics: an empty LAN returns (nil, nil) so callers fall through
// to interactive-prompt / flag-required paths. On CI where multicast
// is blocked, a transport-layer error is the acceptable alternative —
// but ONLY the known substrings from grandcat/zeroconf
// (`failed to browse services`, `failed to create resolver`), not any
// arbitrary error. A regression that returns a generic error would
// otherwise silently pass.
func TestDiscoverCoordinator_EmptyLANReturnsNil(t *testing.T) {
	t.Parallel()
	entry, err := discoverCoordinator(false)
	if err != nil {
		msg := err.Error()
		isKnownTransport := strings.Contains(msg, "failed to browse services") ||
			strings.Contains(msg, "failed to create resolver")
		if !isKnownTransport {
			t.Fatalf("unexpected non-transport error: %v", err)
		}
	}
	if entry != nil {
		t.Errorf("unexpected coordinator entry on empty LAN: %+v", entry)
	}
}
