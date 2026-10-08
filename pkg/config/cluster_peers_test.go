package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// A membership list written before names existed must still load.
//
// Every running coordinator has `endpoints:` as a bare string list. If
// the structured form were the only one accepted, upgrading would fail
// to load node.yaml on every node at once — the loudest possible
// failure, and entirely avoidable.
func TestClusterPeers_DecodesBareAddressesAndStructuredEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "node.yaml")
	body := `
node:
  name: coord
  port: 9090
cluster:
  mode: coordinator
  endpoints:
    - 192.0.2.10:9090
    - address: 198.51.100.235:9090
      name: WINDOWS-WORKER
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := readNodeConfigFile(path)
	if err != nil {
		t.Fatalf("readNodeConfigFile: %v", err)
	}

	peers := cfg.Cluster.Endpoints
	if len(peers) != 2 {
		t.Fatalf("len(peers) = %d, want 2: %+v", len(peers), peers)
	}
	if peers[0].Address != "192.0.2.10:9090" || peers[0].Name != "" {
		t.Errorf("bare entry = %+v, want address only", peers[0])
	}
	if peers[1].Address != "198.51.100.235:9090" || peers[1].Name != "WINDOWS-WORKER" {
		t.Errorf("structured entry = %+v", peers[1])
	}

	if got := peers.NameFor("198.51.100.235:9090"); got != "WINDOWS-WORKER" {
		t.Errorf("NameFor = %q, want WINDOWS-WORKER", got)
	}
	// An unprobed peer has no name — the caller must fall back, not
	// invent one.
	if got := peers.NameFor("192.0.2.10:9090"); got != "" {
		t.Errorf("NameFor unprobed = %q, want empty", got)
	}
	if got := peers.NameFor("10.9.9.9:9090"); got != "" {
		t.Errorf("NameFor unknown = %q, want empty", got)
	}
}

// Composing our hook must not drop viper's own: passing
// viper.DecodeHook REPLACES the default rather than adding to it, so a
// regression here silently breaks comma-separated list decoding
// everywhere in node.yaml.
func TestNodeConfigDecodeHook_PreservesCommaSliceDecoding(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "node.yaml")
	body := `
node:
  name: coord
  port: 9090
cluster:
  advertise_ips: 198.51.100.10,198.51.100.11
  endpoints:
    - 192.0.2.10:9090
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := readNodeConfigFile(path)
	if err != nil {
		t.Fatalf("readNodeConfigFile: %v", err)
	}
	want := []string{"198.51.100.10", "198.51.100.11"}
	got := cfg.Cluster.AdvertiseIPs
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("AdvertiseIPs = %v, want %v", got, want)
	}
}

// A nameless peer marshals back to a bare string, so adding names does
// not rewrite an untouched membership list into a noisier shape.
func TestClusterPeer_MarshalsBareWhenUnnamed(t *testing.T) {
	out, err := yaml.Marshal(PeersFromAddresses("192.0.2.10:9090"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(out)); got != "- 192.0.2.10:9090" {
		t.Errorf("unnamed marshal = %q, want a bare string", got)
	}

	named, err := yaml.Marshal(ClusterPeers{{Address: "198.51.100.235:9090", Name: "WINDOWS-WORKER"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(named), "name: WINDOWS-WORKER") {
		t.Errorf("named marshal lost the name: %s", named)
	}
}
