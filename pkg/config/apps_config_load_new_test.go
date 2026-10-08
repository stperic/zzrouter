package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stperic/zzrouter/pkg/constants"
)

// TestNewShape_RoundTripViaInstall is the golden round-trip: install
// the embedded providers/ tree into a temp dir, load it back, assert
// every shipped provider is present under its declared kind.
func TestNewShape_RoundTripViaInstall(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, templates.InstallDefaults(dir))

	cfg, err := LoadAppsConfig(dir)
	require.NoError(t, err)
	require.NotNil(t, cfg)
	require.NotEmpty(t, cfg.providers, "typed providers must be populated")

	// Every known provider file is present under its declared kind.
	expect := map[string]Kind{
		"vllm":         KindOnDemand,
		"llamacpp":     KindOnDemand,
		"mlx":          KindOnDemand,
		"ollama":       KindExternal,
		"openai":       KindCloud,
		"anthropic":    KindCloud,
		"groq":         KindCloud,
		"gemini":       KindCloud,
		"openrouter":   KindCloud,
		"cloudflare":   KindCloud,
		"bedrock":      KindCloud,
		"azure":        KindCloud,
		"ollama-cloud": KindCloud,
		"huggingface":  KindRegistry,
	}
	for name, wantKind := range expect {
		p := cfg.Find(name)
		require.NotNilf(t, p, "provider %q missing from typed view", name)
		assert.Equalf(t, wantKind, p.Kind(), "provider %q under wrong kind", name)

		sc, ok := cfg.LookupApp(name)
		require.Truef(t, ok, "provider %q missing from LookupApp view", name)
		switch wantKind {
		case KindOnDemand:
			assert.Equal(t, constants.AppModeOnDemand, sc.Mode, name)
		case KindExternal:
			assert.Equal(t, constants.AppModeExternal, sc.Mode, name)
		case KindCloud:
			assert.Equal(t, constants.AppModeCloud, sc.Mode, name)
		case KindRegistry:
			assert.Equal(t, constants.AppModeRegistry, sc.Mode, name)
		}
		assert.Equal(t, name, sc.Name, "synthesized ServiceConfig name mismatch")
	}

	// Spot check a cloud provider for runtime field fidelity.
	openai := cfg.GetCloud("openai")
	require.NotNil(t, openai)
	assert.Equal(t, "https://api.openai.com", openai.Runtime.Endpoint)
	assert.Equal(t, "bearer", openai.Runtime.API.AuthType)

	// Spot check on-demand execution survives the typed decode.
	vllm := cfg.GetOnDemand("vllm")
	require.NotNil(t, vllm)
	assert.Equal(t, "cli", vllm.Runtime.Execution.Type)
	assert.Equal(t, "vllm", vllm.Runtime.Execution.Command)

	// Capabilities.DefaultVariant survives the typed decode (load path
	// for the auto-deploy /runs feature). llamacpp ships with Q4_K_M.
	llamacpp := cfg.GetOnDemand("llamacpp")
	require.NotNil(t, llamacpp)
	require.NotNil(t, llamacpp.Capabilities)
	assert.Equal(t, "Q4_K_M", llamacpp.Capabilities.DefaultVariant)
}

// TestNewShape_BidirectionalRoundTrip closes the loop: marshal each
// typed provider back to YAML, re-decode into a fresh struct of the
// same type, and assert deep-equality. Catches silent field-drop where
// a struct field has no yaml tag (or the wrong tag).
func TestNewShape_BidirectionalRoundTrip(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, templates.InstallDefaults(dir))
	cfg, err := LoadAppsConfig(dir)
	require.NoError(t, err)

	for name, p := range cfg.providers {
		body, err := yaml.Marshal(p)
		require.NoErrorf(t, err, "marshal %s", name)

		var got Provider
		switch p.(type) {
		case *OnDemandProvider:
			got = new(OnDemandProvider)
		case *ExternalProvider:
			got = new(ExternalProvider)
		case *CloudProvider:
			got = new(CloudProvider)
		case *SearchRegistry:
			got = new(SearchRegistry)
		default:
			t.Fatalf("unknown concrete type for %s: %T", name, p)
		}
		require.NoErrorf(t, strictDecode(body, got), "re-decode %s", name)

		setProviderName(got, name)
		if diff := cmp.Diff(p, got, cmpopts.EquateEmpty()); diff != "" {
			t.Errorf("round-trip mismatch for %s (-orig +got):\n%s", name, diff)
		}
	}
}

func setProviderName(p Provider, name string) {
	switch tp := p.(type) {
	case *OnDemandProvider:
		tp.Name = name
	case *ExternalProvider:
		tp.Name = name
	case *CloudProvider:
		tp.Name = name
	case *SearchRegistry:
		tp.Name = name
	}
}

// TestNewShape_DuplicateNameAcrossKinds — same stem in two kind subdirs
// must hard-error (the dir layout's main invariant).
func TestNewShape_DuplicateNameAcrossKinds(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "on-demand", "dupe"), 0755))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "cloud", "dupe"), 0755))

	require.NoError(t, os.WriteFile(filepath.Join(dir, "on-demand", "dupe", "config.yaml"), []byte(`
protocol: openai
runtime:
  port_range: [8000, 8009]
  base_port: 8000
  execution: {type: cli, command: x}
capabilities:
  wire_endpoints: [chat_completions]
`), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cloud", "dupe", "config.yaml"), []byte(`
runtime:
  endpoint: https://example.com
  api: {auth_type: bearer}
capabilities:
  wire_endpoints: [chat_completions]
`), 0644))

	_, err := LoadAppsConfig(dir)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidConfig)
	assert.Contains(t, err.Error(), "declared in multiple kinds")
}

// TestNewShape_StrayLegacyYamlIsInert pins that pre-dir-form
// <kind>/<name>.yaml siblings (and editor junk) don't break the loader,
// don't surface as providers, and don't shadow the canonical dir form.
// Pairs with the warnLegacyProviderFile helper that emits a one-shot
// WARN; this test asserts behavior, not the log line.
func TestNewShape_StrayLegacyYamlIsInert(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "cloud", "anthropic"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cloud", "anthropic", "config.yaml"), []byte(`
runtime:
  endpoint: https://api.anthropic.com
  api: {auth_type: bearer}
capabilities:
  wire_endpoints: [chat_completions]
`), 0644))
	// Stray siblings: legacy .yaml + .yml + dotfile + editor swap.
	for _, name := range []string{"anthropic.yaml", "anthropic.yml", ".anthropic.yaml.swp", "#anthropic.yaml#"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "cloud", name), []byte("garbage: not real"), 0644))
	}

	cfg, err := LoadAppsConfig(dir)
	require.NoError(t, err)
	require.NotNil(t, cfg.GetCloud("anthropic"))
	// Stems from the stray files must not surface as providers.
	assert.Nil(t, cfg.GetCloud(".anthropic.yaml"))
	assert.Nil(t, cfg.GetCloud("#anthropic.yaml#"))
}

// TestNewShape_RejectsInvalidProviders pins the Validate() error paths
// on the directory loader. Each case is a minimal valid file with
// exactly one invariant violated — ensures the loader surfaces typed
// Validate() errors (not just schema errors).
func TestNewShape_RejectsInvalidProviders(t *testing.T) {
	cases := []struct {
		name string
		kind string
		body string
		want string
	}{
		{
			name: "on-demand missing port pool",
			kind: "on-demand",
			body: `
protocol: openai
runtime:
  execution: {type: cli, command: x}
`,
			want: "port_range",
		},
		{
			name: "external missing endpoint",
			kind: "external",
			body: `
protocol: openai
runtime: {}
`,
			want: "endpoint",
		},
		{
			name: "cloud missing api auth_type",
			kind: "cloud",
			body: `
runtime:
  endpoint: https://example.com
  api: {}
`,
			want: "auth_type",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			providerDir := filepath.Join(dir, tc.kind, "x")
			require.NoError(t, os.MkdirAll(providerDir, 0755))
			require.NoError(t, os.WriteFile(
				filepath.Join(providerDir, "config.yaml"),
				[]byte(tc.body), 0644))
			_, err := LoadAppsConfig(dir)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestAddApp(t *testing.T) {
	cfg := &AppsConfig{
		Version:   "1.0",
		providers: map[string]Provider{},
	}

	enabled := true
	sc := ServiceConfig{
		Name:    "openai",
		Mode:    constants.AppModeCloud,
		Enabled: &enabled,
		Runtime: &AppRuntimeConfig{
			Endpoint:  "https://api.openai.com",
			Execution: ExecutionConfig{Type: "api"},
			API:       &APIConfig{AuthType: "bearer"},
		},
		Capabilities: &AppCapabilities{WireEndpoints: []string{"chat_completions"}},
	}
	require.NoError(t, cfg.AddApp("openai", sc))

	assert.NotNil(t, cfg.Find("openai"))
	_, ok := cfg.LookupApp("openai")
	assert.True(t, ok)

	// Duplicate insert fails — AddApp is strictly insert-only. AddApp
	// returns ErrProviderExists wrapped with the provider name.
	err := cfg.AddApp("openai", sc)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrProviderExists)

	// Invalid Mode is rejected up-front, before writing.
	badSC := ServiceConfig{Name: "broken", Mode: "nonsense"}
	require.Error(t, cfg.AddApp("broken", badSC))
	assert.Nil(t, cfg.Find("broken"), "typed view must not contain rejected entry")
	_, bad := cfg.LookupApp("broken")
	assert.False(t, bad, "lookup must not return rejected entry")
}

// TestAllProviders_NilReceiver locks in the nil-check ordering in
// AllProviders.
func TestAllProviders_NilReceiver(t *testing.T) {
	var cfg *AppsConfig
	assert.Empty(t, cfg.AllProviders())
}
