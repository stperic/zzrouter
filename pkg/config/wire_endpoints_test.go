package config

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stperic/zzrouter/pkg/config/templates"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestValidateWireEndpoints_AcceptsResponsesCompat(t *testing.T) {
	c := &AppCapabilities{WireEndpoints: []string{"chat_completions", "responses_compat"}}
	require.NoError(t, c.ValidateWireEndpoints())
}

func TestValidateWireEndpoints_RejectsNativePlusCompat(t *testing.T) {
	c := &AppCapabilities{WireEndpoints: []string{"chat_completions", "responses", "responses_compat"}}
	err := c.ValidateWireEndpoints()
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "mutually exclusive"))
}

func TestValidateWireEndpoints_RejectsCompatWithoutChat(t *testing.T) {
	c := &AppCapabilities{WireEndpoints: []string{"embeddings", "responses_compat"}}
	err := c.ValidateWireEndpoints()
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "requires 'chat_completions'"))
}

func TestProviderValidate_SkipsWireEndpointsWhenExplicitlyDisabled(t *testing.T) {
	disabled := false
	cloud := &CloudProvider{
		Name:    "openai",
		Enabled: &disabled,
		Runtime: CloudRuntime{Endpoint: "https://api.openai.com", API: APIConfig{AuthType: "bearer"}},
	}
	require.NoError(t, cloud.Validate(), "disabled cloud provider must not require capabilities.wire_endpoints")

	enabled := true
	cloudOn := &CloudProvider{
		Name:    "openai",
		Enabled: &enabled,
		Runtime: CloudRuntime{Endpoint: "https://api.openai.com", API: APIConfig{AuthType: "bearer"}},
	}
	require.Error(t, cloudOn.Validate(), "enabled cloud provider must still require capabilities.wire_endpoints")
}

func TestSupportsWireEndpoint_CompatNotMisreadAsNative(t *testing.T) {
	c := &AppCapabilities{WireEndpoints: []string{"chat_completions", "responses_compat"}}
	assert.False(t, c.SupportsWireEndpoint("responses"))
	assert.True(t, c.SupportsWireEndpoint("responses_compat"))
	assert.True(t, c.SupportsWireEndpoint("chat_completions"))
}

func TestValidateWireEndpoints_AcceptsMessages(t *testing.T) {
	c := &AppCapabilities{WireEndpoints: []string{"messages"}}
	require.NoError(t, c.ValidateWireEndpoints())
}

func TestValidateWireEndpoints_UnknownListsEveryValidValue(t *testing.T) {
	c := &AppCapabilities{WireEndpoints: []string{"mesages"}}
	err := c.ValidateWireEndpoints()
	require.Error(t, err)
	for e := range validWireEndpoints {
		assert.Contains(t, err.Error(), e)
	}
}

// Every translation shim follows the same two rules as responses_compat.
func TestValidateWireEndpoints_EveryShimFollowsTheRules(t *testing.T) {
	for shim, native := range compatShims {
		t.Run(shim, func(t *testing.T) {
			ok := &AppCapabilities{WireEndpoints: []string{"chat_completions", shim}}
			require.NoError(t, ok.ValidateWireEndpoints())

			both := &AppCapabilities{WireEndpoints: []string{"chat_completions", native, shim}}
			assert.ErrorContains(t, both.ValidateWireEndpoints(), "mutually exclusive")

			noChat := &AppCapabilities{WireEndpoints: []string{"embeddings", shim}}
			assert.ErrorContains(t, noChat.ValidateWireEndpoints(), "requires 'chat_completions'")
		})
	}
	for shim, native := range compatShims {
		assert.Contains(t, validWireEndpoints, shim)
		assert.Contains(t, validWireEndpoints, native)
	}
}

func TestSupportsWireEndpoint_DerivesShims(t *testing.T) {
	c := &AppCapabilities{WireEndpoints: []string{"chat_completions"}}
	for shim, native := range compatShims {
		assert.True(t, c.SupportsWireEndpoint(shim), shim)
		assert.False(t, c.SupportsWireEndpoint(native), native)
	}
	assert.Equal(t, []string{"chat_completions"}, c.WireEndpoints)
}

func TestEveryShippedProviderDerivesWireEndpoints(t *testing.T) {
	err := fs.WalkDir(templates.AppsFS, "files/providers", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Base(path) != "config.yaml" {
			return err
		}
		body, err := fs.ReadFile(templates.AppsFS, path)
		require.NoError(t, err)
		var doc struct {
			Capabilities *AppCapabilities `yaml:"capabilities"`
		}
		require.NoError(t, yaml.Unmarshal(body, &doc), path)
		if doc.Capabilities == nil {
			return nil
		}
		t.Run(path, func(t *testing.T) {
			c := doc.Capabilities
			require.NoError(t, c.ValidateWireEndpoints())
			for shim, native := range compatShims {
				assert.NotContains(t, c.WireEndpoints, shim, "shipped providers declare native endpoints only")
				if slices.Contains(c.WireEndpoints, "chat_completions") {
					assert.True(t, c.SupportsWireEndpoint(native) || c.SupportsWireEndpoint(shim))
					assert.Equal(t, !slices.Contains(c.WireEndpoints, native), c.SupportsWireEndpoint(shim))
				}
			}
		})
		return nil
	})
	require.NoError(t, err)
}

func TestLegacyCompatDeclarationsLoadAndReconcile(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, templates.InstallDefaults(dir))
	custom := filepath.Join(dir, "external", "custom", "config.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(custom), 0700))
	body := []byte("enabled: true\nprotocol: openai\nruntime:\n  endpoint: http://example.test\ncapabilities:\n  wire_endpoints: [chat_completions, messages_compat, responses_compat]\n")
	require.NoError(t, os.WriteFile(custom, body, 0600))
	ollama := filepath.Join(dir, "external", "ollama", "config.yaml")
	require.NoError(t, os.WriteFile(ollama, body, 0600))
	cfg, err := LoadAppsConfig(dir)
	require.NoError(t, err)
	before, ok := cfg.LookupApp("custom")
	require.True(t, ok)
	changed, err := templates.ReconcileManagedSpine(dir)
	require.NoError(t, err)
	assert.Contains(t, changed, "external/ollama/config.yaml")
	assert.NotContains(t, changed, "external/custom/config.yaml")
	got, err := os.ReadFile(custom)
	require.NoError(t, err)
	assert.Equal(t, body, got)
	cfg, err = LoadAppsConfig(dir)
	require.NoError(t, err)
	for _, name := range []string{"custom", "ollama"} {
		svc, ok := cfg.LookupApp(name)
		require.True(t, ok)
		for shim := range compatShims {
			assert.Equal(t, before.Capabilities.SupportsWireEndpoint(shim), svc.Capabilities.SupportsWireEndpoint(shim), name)
		}
		if name == "ollama" {
			assert.NotContains(t, svc.Capabilities.WireEndpoints, "messages_compat")
			assert.NotContains(t, svc.Capabilities.WireEndpoints, "responses_compat")
		}
	}
	changed, err = templates.ReconcileManagedSpine(dir)
	require.NoError(t, err)
	assert.Empty(t, changed)
}

func TestEffectiveWireEndpoints(t *testing.T) {
	for _, tc := range []struct {
		name           string
		declared, want []string
	}{
		{"chat only", []string{"chat_completions"}, []string{"chat_completions", "messages_compat", "responses_compat"}},
		{"native messages", []string{"chat_completions", "messages"}, []string{"chat_completions", "messages", "responses_compat"}},
		{"native responses", []string{"responses", "chat_completions"}, []string{"chat_completions", "messages_compat", "responses"}},
		{"explicit shims", []string{"responses_compat", "chat_completions", "messages_compat"}, []string{"chat_completions", "messages_compat", "responses_compat"}},
		{"non chat", []string{"embeddings"}, []string{"embeddings"}},
		{"empty", []string{}, []string{}},
		{"missing", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := slices.Clone(tc.declared)
			c := &AppCapabilities{WireEndpoints: tc.declared}
			got := c.EffectiveWireEndpoints()
			assert.Equal(t, tc.want, got)
			assert.Equal(t, original, c.WireEndpoints)
			assert.Equal(t, got, (&AppCapabilities{WireEndpoints: got}).EffectiveWireEndpoints())
			if len(got) > 0 {
				got[0] = "mutated"
				assert.Equal(t, original, c.WireEndpoints)
			}
		})
	}
	var c *AppCapabilities
	assert.Nil(t, c.EffectiveWireEndpoints())
	assert.False(t, c.SupportsWireEndpoint("messages_compat"))
}
