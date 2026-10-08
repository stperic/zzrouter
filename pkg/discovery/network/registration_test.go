package network

import (
	"strings"
	"testing"
)

// TestRegister_WorkerSkipsBroadcast pins the coordinator-only mDNS
// invariant: a worker-mode NodeDiscovery must not touch zeroconf or
// avahi when Register is called. The gate lets the coordinator be the
// sole broadcaster on the LAN so browse-only workers have exactly one
// record to pick up.
func TestRegister_WorkerSkipsBroadcast(t *testing.T) {
	hd := NewNodeDiscovery("worker-1", false, 0)
	if err := hd.Register(9090); err != nil {
		t.Fatalf("Register on worker should be a no-op, got: %v", err)
	}
	if hd.server != nil {
		t.Error("worker should not hold a zeroconf server handle")
	}
	if hd.avahiProcess != nil {
		t.Error("worker should not hold an avahi process handle")
	}
}

// TestBuildTXTRecords pins the TXT-record shape. mDNS is an
// unauthenticated LAN broadcast, so the CA fingerprint is
// intentionally NOT advertised — operators supply the pin via an
// OOB channel (--ca-fingerprint flag or the --secure wizard prompt).
func TestBuildTXTRecords(t *testing.T) {
	const (
		name        = "coord-1"
		port        = 9090
		clusterPort = 9091
		ver         = "0.1.1"
		ts          = int64(1729555200)
	)

	t.Run("coordinator emits cluster_port but no ca_fingerprint", func(t *testing.T) {
		records := buildTXTRecords(name, port, clusterPort, true, ver, ts)
		want := map[string]string{
			"nodename":     name,
			"port":         "9090",
			"cluster_port": "9091",
			"coordinator":  "true",
			"version":      ver,
			"service":      "zzrouter",
			"timestamp":    "1729555200",
		}
		assertTXTKV(t, records, want)
		for _, r := range records {
			if strings.HasPrefix(r, "ca_fingerprint") {
				t.Errorf("TXT must not advertise ca_fingerprint, got %q", r)
			}
		}
	})

	t.Run("worker (hypothetical) omits cluster_port", func(t *testing.T) {
		records := buildTXTRecords(name, port, 0, false, ver, ts)
		for _, r := range records {
			if strings.HasPrefix(r, "cluster_port=") {
				t.Errorf("worker TXT must not advertise cluster_port, got %q", r)
			}
		}
		if v := findTXTValue(records, "coordinator"); v != "false" {
			t.Errorf("coordinator= should be false for worker, got %q", v)
		}
	})

	t.Run("zero cluster_port omitted, not empty-advertised", func(t *testing.T) {
		records := buildTXTRecords(name, port, 0, true, ver, ts)
		for _, r := range records {
			if r == "cluster_port=0" || r == "cluster_port=" {
				t.Errorf("must not advertise zero cluster_port, got %q", r)
			}
		}
	})
}

func assertTXTKV(t *testing.T, records []string, want map[string]string) {
	t.Helper()
	got := map[string]string{}
	for _, r := range records {
		k, v, ok := strings.Cut(r, "=")
		if !ok {
			continue
		}
		got[k] = v
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("TXT[%s] = %q, want %q", k, got[k], v)
		}
	}
}

func findTXTValue(records []string, key string) string {
	prefix := key + "="
	for _, r := range records {
		if strings.HasPrefix(r, prefix) {
			return strings.TrimPrefix(r, prefix)
		}
	}
	return ""
}
