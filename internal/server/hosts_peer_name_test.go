package server

import (
	"testing"

	"github.com/stperic/zzrouter/pkg/cluster/mesh"
)

// GET /nodes must never advertise a name the rest of the API rejects.
//
// The registry learns a peer's name only by probing it, and
// RegisterAdminEndpoint seeds Endpoint.Name with the URL, so before the
// first probe the old fallback chain reported "http://host:9090" as the
// node's name. Every `node` selector in the API takes a name, so that
// value 404s — the list endpoint was handing out an identifier its own
// siblings reject. The cached name from node.yaml covers the window.
func TestEndpointToNodeInfo_NameFallback(t *testing.T) {
	const url = "http://198.51.100.235:9090"

	cached := func(address string) string {
		if address == "198.51.100.235:9090" {
			return "WINDOWS-WORKER"
		}
		return ""
	}

	for _, tc := range []struct {
		name     string
		ep       *mesh.Endpoint
		peerName func(string) string
		want     string
	}{
		{
			name: "probed peer reports its own name",
			ep:   &mesh.Endpoint{URL: url, Name: url, NodeName: "WINDOWS-WORKER"},
			want: "WINDOWS-WORKER",
		},
		{
			// The case this exists for.
			name:     "unprobed peer falls back to the cached name",
			ep:       &mesh.Endpoint{URL: url, Name: url},
			peerName: cached,
			want:     "WINDOWS-WORKER",
		},
		{
			// A probe always wins: the peer owns its name, the cache
			// is only a stand-in until one succeeds.
			name:     "probe beats a stale cached name",
			ep:       &mesh.Endpoint{URL: url, Name: url, NodeName: "renamed-box"},
			peerName: cached,
			want:     "renamed-box",
		},
		{
			name:     "unknown address keeps the previous behaviour",
			ep:       &mesh.Endpoint{URL: url, Name: url},
			peerName: func(string) string { return "" },
			want:     url,
		},
		{
			name: "no resolver keeps the previous behaviour",
			ep:   &mesh.Endpoint{URL: url, Name: url},
			want: url,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := &NodesService{peerName: tc.peerName}
			got, _ := svc.endpointToNodeInfo(tc.ep)["name"].(string)
			if got != tc.want {
				t.Errorf("name = %q, want %q", got, tc.want)
			}
		})
	}
}
