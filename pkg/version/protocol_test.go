package version

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCheckClusterProtocol(t *testing.T) {
	tests := []struct {
		name        string
		localProto  int
		localMin    int
		remoteProto int
		wantErr     bool
		errContains string
	}{
		{
			name:        "exact match accepted",
			localProto:  1,
			localMin:    1,
			remoteProto: 1,
			wantErr:     false,
		},
		{
			name:        "peer within window accepted",
			localProto:  3,
			localMin:    1,
			remoteProto: 2,
			wantErr:     false,
		},
		{
			name:        "peer at floor accepted",
			localProto:  3,
			localMin:    2,
			remoteProto: 2,
			wantErr:     false,
		},
		{
			name:        "peer at ceiling accepted",
			localProto:  3,
			localMin:    1,
			remoteProto: 3,
			wantErr:     false,
		},
		{
			name:        "peer below floor rejected",
			localProto:  2,
			localMin:    2,
			remoteProto: 1,
			wantErr:     true,
			errContains: "upgrade the peer",
		},
		{
			name:        "peer above ceiling rejected",
			localProto:  1,
			localMin:    1,
			remoteProto: 2,
			wantErr:     true,
			errContains: "upgrade the coordinator",
		},
		{
			name:        "zero remote rejected",
			localProto:  1,
			localMin:    1,
			remoteProto: 0,
			wantErr:     true,
			errContains: "pre-protocol build",
		},
		{
			name:        "negative remote rejected as below floor",
			localProto:  2,
			localMin:    1,
			remoteProto: -1,
			wantErr:     true,
			errContains: "upgrade the peer",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckClusterProtocol(tc.localProto, tc.localMin, tc.remoteProto)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.errContains != "" && !strings.Contains(err.Error(), tc.errContains) {
				t.Fatalf("error %q missing substring %q", err.Error(), tc.errContains)
			}
		})
	}
}

func TestClusterProtocolConstants(t *testing.T) {
	if ClusterProtocolVersion < 1 {
		t.Errorf("ClusterProtocolVersion must be >= 1, got %d", ClusterProtocolVersion)
	}
	if MinClusterProtocolVersion < 1 {
		t.Errorf("MinClusterProtocolVersion must be >= 1, got %d", MinClusterProtocolVersion)
	}
	if MinClusterProtocolVersion > ClusterProtocolVersion {
		t.Errorf("MinClusterProtocolVersion (%d) cannot exceed ClusterProtocolVersion (%d)",
			MinClusterProtocolVersion, ClusterProtocolVersion)
	}
}

func TestVersionInfoCarriesProtocol(t *testing.T) {
	info := GetCurrentVersionInfo(ServiceTypeHost, []string{"cluster"})
	if info.ClusterProtocol != ClusterProtocolVersion {
		t.Errorf("ClusterProtocol = %d, want %d", info.ClusterProtocol, ClusterProtocolVersion)
	}
	if info.MinClusterProtocol != MinClusterProtocolVersion {
		t.Errorf("MinClusterProtocol = %d, want %d", info.MinClusterProtocol, MinClusterProtocolVersion)
	}
}

// Round-trip the JSON tags to catch silent typos like `cluster-protocol` or
// a missing field — operators and peers parse these exact names.
func TestVersionInfoProtocolJSONTags(t *testing.T) {
	info := GetCurrentVersionInfo(ServiceTypeHost, []string{"cluster"})
	info.ClusterProtocol = 7
	info.MinClusterProtocol = 3

	raw, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(raw)
	if !strings.Contains(s, `"cluster_protocol":7`) {
		t.Errorf("missing cluster_protocol=7 in JSON: %s", s)
	}
	if !strings.Contains(s, `"min_cluster_protocol":3`) {
		t.Errorf("missing min_cluster_protocol=3 in JSON: %s", s)
	}

	var decoded VersionInfo
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.ClusterProtocol != 7 || decoded.MinClusterProtocol != 3 {
		t.Errorf("round-trip lost protocol fields: got %+v", decoded)
	}
}

func TestLocalProtocolWindow(t *testing.T) {
	w := LocalProtocolWindow()
	if w.Min != MinClusterProtocolVersion || w.Max != ClusterProtocolVersion {
		t.Errorf("LocalProtocolWindow = %+v, want {Min:%d Max:%d}",
			w, MinClusterProtocolVersion, ClusterProtocolVersion)
	}
	if err := w.Check(ClusterProtocolVersion); err != nil {
		t.Errorf("local window should accept local version: %v", err)
	}
	if err := w.Check(0); err == nil {
		t.Errorf("local window must reject zero")
	}
}

func TestLocalProtocolWindowRejectsOutsidePeers(t *testing.T) {
	w := LocalProtocolWindow()
	for _, remote := range []int{w.Min - 1, w.Max + 1} {
		if err := w.Check(remote); err == nil {
			t.Errorf("protocol %d admitted by window %+v", remote, w)
		}
	}
	if err := (ProtocolWindow{Min: w.Max - 1, Max: w.Max - 1}).Check(w.Max); err == nil {
		t.Fatal("an older coordinator admitted a newer worker")
	}
}
