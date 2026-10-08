package harness

import (
	"os"
	"path/filepath"
	"testing"
)

// Checked-in examples must pass strict decoding and structural validation.
func TestSiteOverlays_LoadAndValidate(t *testing.T) {
	// Load reads both keys; values are irrelevant to structural checks.
	t.Setenv("ZZROUTER_ADMIN_API_KEY", "test-admin-key-at-least-32-chars-long")
	t.Setenv("ZZROUTER_CLUSTER_NETWORK_KEY", "test-cluster-key-at-least-32-chars-long")

	sites, err := filepath.Glob(filepath.Join("..", "configs", "sites", "example.yaml"))
	if err != nil {
		t.Fatalf("glob sites: %v", err)
	}
	if len(sites) == 0 {
		t.Fatal("no site overlays found  :  did configs/sites/ move?")
	}

	for _, site := range sites {
		t.Run(filepath.Base(site), func(t *testing.T) {
			abs, err := filepath.Abs(site)
			if err != nil {
				t.Fatalf("abs: %v", err)
			}
			if _, err := os.Stat(abs); err != nil {
				t.Fatalf("stat: %v", err)
			}
			// Site files overlay the nightly profile (see the header
			// comment in example.yaml).
			if _, err := Load("nightly", abs); err != nil {
				t.Fatalf("Load(%s): %v", filepath.Base(site), err)
			}
		})
	}
}
