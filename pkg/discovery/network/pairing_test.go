package network

import (
	"testing"
	"time"
)

// TestNodeEntry_PairingURL pins the URL-formation contract for
// browse-only worker discovery. A missing ClusterPort or a
// non-coordinator advertiser returns empty — callers fall through to
// the manual-entry prompt. The CA fingerprint is never carried over
// mDNS, so PairingURL doesn't gate on it.
func TestNodeEntry_PairingURL(t *testing.T) {
	tests := []struct {
		name  string
		entry NodeEntry
		want  string
	}{
		{
			name: "complete coordinator entry",
			entry: NodeEntry{
				Node:          "10.0.0.5",
				Port:          9090,
				ClusterPort:   9091,
				IsCoordinator: true,
				LastSeen:      time.Now(),
			},
			want: "https://10.0.0.5:9091",
		},
		{
			name: "non-coordinator skipped",
			entry: NodeEntry{
				Node:          "10.0.0.5",
				ClusterPort:   9091,
				IsCoordinator: false,
			},
			want: "",
		},
		{
			name: "missing ClusterPort skipped",
			entry: NodeEntry{
				Node:          "10.0.0.5",
				ClusterPort:   0,
				IsCoordinator: true,
			},
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.entry.PairingURL(); got != tc.want {
				t.Errorf("PairingURL() = %q, want %q", got, tc.want)
			}
		})
	}
}
