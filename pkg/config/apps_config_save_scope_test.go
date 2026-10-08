package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Every store mutation persists through SaveToDir, which re-marshals
// every provider. Writing all of them for a change to one is not just
// wasted I/O: an mtime is what anything watching this tree reads as
// "this changed", so one edit used to announce itself as a change to
// every provider on the node.
//
// Backdating the files after the first save makes this deterministic —
// a skipped write leaves the old timestamp, a real one cannot.
func TestSaveToDir_LeavesUnchangedProvidersAlone(t *testing.T) {
	onDemand := func(name string, enabled bool) *OnDemandProvider {
		p := vllmTestProvider()
		p.Name = name
		p.Enabled = new(enabled)
		return p
	}
	cfg := providersFixture(map[string]Provider{
		"ollama": &ExternalProvider{
			Name: "ollama", Enabled: new(true), Protocol: ProtocolOllama,
			Runtime:      ExternalRuntime{Endpoint: "http://localhost:11434"},
			Capabilities: &AppCapabilities{WireEndpoints: []string{"chat_completions"}},
		},
		"vllm": onDemand("vllm", false),
		"mlx":  onDemand("mlx", false),
	})
	dir := t.TempDir()
	if err := cfg.SaveToDir(dir); err != nil {
		t.Fatalf("first SaveToDir: %v", err)
	}

	paths := map[string]string{
		"ollama": filepath.Join(dir, "external", "ollama", "config.yaml"),
		"vllm":   filepath.Join(dir, "on-demand", "vllm", "config.yaml"),
		"mlx":    filepath.Join(dir, "on-demand", "mlx", "config.yaml"),
	}
	backdated := time.Now().Add(-time.Hour)
	for name, p := range paths {
		if err := os.Chtimes(p, backdated, backdated); err != nil {
			t.Fatalf("backdate %s: %v", name, err)
		}
	}

	// Change exactly one provider and persist again.
	cfg.providers["vllm"] = onDemand("vllm", true)
	if err := cfg.SaveToDir(dir); err != nil {
		t.Fatalf("second SaveToDir: %v", err)
	}

	for _, name := range []string{"ollama", "mlx"} {
		st, err := os.Stat(paths[name])
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if st.ModTime().After(backdated) {
			t.Errorf("%s was rewritten by a change to a different provider", name)
		}
	}

	st, err := os.Stat(paths["vllm"])
	if err != nil {
		t.Fatalf("stat vllm: %v", err)
	}
	if !st.ModTime().After(backdated) {
		t.Error("the provider that actually changed was not written")
	}

	// The skip must not cost correctness: what is on disk still reloads
	// into the config that was saved.
	loaded, err := LoadAppsConfig(dir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	p := loaded.Find("vllm")
	if p == nil || !loaded.IsAppEnabled("vllm") {
		t.Error("the changed provider did not survive the round trip")
	}
	if loaded.Find("ollama") == nil || loaded.Find("mlx") == nil {
		t.Error("a skipped provider went missing from the reload")
	}
}
