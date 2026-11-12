package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/stperic/common/secrets"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestConnectionsEveryShippedCloudAndHuggingFace(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	require.NoError(t, templates.InstallDefaults(dir))
	cfg, err := config.LoadAppsConfig(dir)
	require.NoError(t, err)
	vault, err := secrets.Open(filepath.Join(t.TempDir(), "connections.enc"), bytes.Repeat([]byte{7}, 32))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	resolver := NewResolver(func() *config.AppsConfig { return cfg }, vault)
	providers := []string{"openai", "anthropic", "gemini", "groq", "openrouter", "ollama-cloud", "azure", "bedrock", "cloudflare", "huggingface"}
	require.Len(t, cfg.CloudProviders(), 9)
	before := cfg.Fingerprint()
	for _, name := range providers {
		t.Run(name, func(t *testing.T) {
			svc, ok := cfg.LookupApp(name)
			require.True(t, ok)
			require.NotNil(t, svc.Connection)
			values := map[string]string{}
			for _, field := range svc.Connection.Fields {
				switch field.Type {
				case "endpoint":
					values[field.ID] = "https://fixture.example"
				case "identifier":
					values[field.ID] = "account123"
				default:
					values[field.ID] = "candidate-fixture-secret"
				}
			}
			target, err := resolver.Candidate(name, values)
			require.NoError(t, err)
			require.True(t, target.Configured)
			values[ConnectionEndpointField] = target.Endpoint
			_, err = vault.Replace(t.Context(), name, 0, values)
			require.NoError(t, err)
			target, ok = resolver.Target(name)
			require.True(t, ok)
			req, err := target.NewRequest(t.Context(), http.MethodGet, "/models?limit=1", nil)
			require.NoError(t, err)
			header, prefix := "Authorization", "Bearer "
			if name == "anthropic" {
				header, prefix = "X-Api-Key", ""
			}
			if name == "azure" {
				header, prefix = "Api-Key", ""
			}
			require.Equal(t, prefix+"candidate-fixture-secret", req.Header.Get(header))
			state, err := resolver.Connection(name)
			require.NoError(t, err)
			body, err := json.Marshal(state)
			require.NoError(t, err)
			require.NotContains(t, string(body), "candidate-fixture-secret")
			require.NotContains(t, state.Values, "api_key")
			require.NotContains(t, state.Values, "token")
			serialized, err := yaml.Marshal(svc)
			require.NoError(t, err)
			require.NotContains(t, string(serialized), "candidate-fixture-secret")
		})
	}
	require.Equal(t, before, cfg.Fingerprint(), "resolving or changing credentials must never mutate declared configuration")
	// YAML clone/conversion retains the same declarative schema.
	require.NoError(t, cfg.UpdateApp("huggingface", func(sc *config.ServiceConfig) error { sc.Description = "new description"; return nil }))
	hf, _ := cfg.LookupApp("huggingface")
	require.Equal(t, "token", hf.Connection.Fields[0].ID)
	require.Equal(t, "${HF_TOKEN}", hf.Runtime.API.Token)
}

func connectionFixture(t *testing.T) (*config.AppsConfig, *secrets.Store, string, []byte) {
	t.Helper()
	cfg := &config.AppsConfig{}
	require.NoError(t, cfg.AddApp("cloud", config.ServiceConfig{Mode: "cloud", Enabled: new(true), Protocol: config.ProtocolOpenAI,
		Capabilities: &config.AppCapabilities{WireEndpoints: []string{"chat_completions"}},
		Runtime:      &config.AppRuntimeConfig{Endpoint: "${TEST_CONNECTION_ENDPOINT}", API: &config.APIConfig{AuthType: config.AuthTypeBearer, Token: "${TEST_CONNECTION_TOKEN}"}},
		Connection: &config.ConnectionConfig{Fields: []config.ConnectionField{
			{ID: "endpoint", Label: "Endpoint", Type: "endpoint", Env: "TEST_CONNECTION_ENDPOINT", Required: true},
			{ID: "token", Label: "Token", Type: "text", Env: "TEST_CONNECTION_TOKEN", Secret: true, Required: true},
		}},
	}))
	path, key := filepath.Join(t.TempDir(), "connections.enc"), bytes.Repeat([]byte{5}, 32)
	vault, err := secrets.Open(path, key)
	require.NoError(t, err)
	t.Cleanup(func() { _ = vault.Close() })
	return cfg, vault, path, key
}

func TestConnectionSnapshotRotationRemovalAndRestart(t *testing.T) {
	t.Setenv("TEST_CONNECTION_ENDPOINT", "https://external.example")
	t.Setenv("TEST_CONNECTION_TOKEN", "external-fixture-token")
	cfg, vault, path, key := connectionFixture(t)
	resolver := NewResolver(func() *config.AppsConfig { return cfg }, vault)
	target, ok := resolver.Resolve("cloud")
	require.True(t, ok)
	require.Equal(t, "Bearer external-fixture-token", target.Upstream.Header().Get("Authorization"))
	_, err := vault.Replace(t.Context(), "cloud", 0, map[string]string{ConnectionEndpointField: "https://managed.example", "endpoint": "https://managed.example", "token": "managed-fixture-token"})
	require.NoError(t, err)
	selected, ok := resolver.Resolve("cloud")
	require.True(t, ok)
	require.NotEqual(t, target.Revision, selected.Revision)
	// A captured request keeps its endpoint and credential together after replacement.
	_, err = vault.Replace(t.Context(), "cloud", 1, map[string]string{ConnectionEndpointField: "https://replacement.example", "endpoint": "https://replacement.example", "token": "replacement-fixture-token"})
	require.NoError(t, err)
	req, err := selected.NewRequest(t.Context(), "GET", "/v1/models", nil)
	require.NoError(t, err)
	require.Equal(t, "managed.example", req.URL.Host)
	require.Equal(t, "Bearer managed-fixture-token", req.Header.Get("Authorization"))
	_, err = resolver.Candidate("cloud", map[string]string{"endpoint": "https://untrusted.example"})
	require.Error(t, err, "changing destination never borrows the stored secret")
	_, err = vault.Remove(t.Context(), "cloud", 2)
	require.NoError(t, err)
	_, ok = resolver.Resolve("cloud")
	require.False(t, ok)
	require.False(t, resolver.Available("cloud"))
	status, err := resolver.Connection("cloud")
	require.NoError(t, err)
	require.Equal(t, secrets.Disabled, status.Mode)
	require.Empty(t, status.Present)
	require.NoError(t, vault.Close())
	_, ok = resolver.Target("cloud")
	require.False(t, ok, "closed storage cannot restore external fallback")
	reopened, err := secrets.Open(path, key)
	require.NoError(t, err)
	defer func() { _ = reopened.Close() }()
	resolver = NewResolver(func() *config.AppsConfig { return cfg }, reopened)
	_, ok = resolver.Resolve("cloud")
	require.False(t, ok)
	_, err = reopened.UseExternal(context.Background(), "cloud", 3)
	require.NoError(t, err)
	target, ok = resolver.Resolve("cloud")
	require.True(t, ok)
	require.Equal(t, "Bearer external-fixture-token", target.Upstream.Header().Get("Authorization"))
	status, err = resolver.Connection("cloud")
	require.NoError(t, err)
	status.Fields[1].Secret = false
	status, err = resolver.Connection("cloud")
	require.NoError(t, err)
	require.True(t, status.Fields[1].Secret, "client metadata cannot mutate the declared secret classification")
	revision := target.Revision
	require.NoError(t, cfg.UpdateApp("cloud", func(s *config.ServiceConfig) error { s.Runtime.ModelsPath = "/different"; return nil }))
	target, ok = resolver.Resolve("cloud")
	require.True(t, ok)
	require.NotEqual(t, revision, target.Revision, "catalog configuration participates in cached credential snapshots")
}

func TestManagedConnectionRemainsBoundToDeclaredDestinationAndCredential(t *testing.T) {
	cfg, vault, _, _ := connectionFixture(t)
	resolver := NewResolver(func() *config.AppsConfig { return cfg }, vault)
	_, err := vault.Replace(t.Context(), "cloud", 0, map[string]string{ConnectionEndpointField: "https://managed.example", "endpoint": "https://managed.example", "token": "managed-fixture-token"})
	require.NoError(t, err)
	_, ok := resolver.Resolve("cloud")
	require.True(t, ok)
	require.NoError(t, cfg.UpdateApp("cloud", func(s *config.ServiceConfig) error { s.Runtime.Endpoint = "https://different.example"; return nil }))
	_, ok = resolver.Target("cloud")
	require.False(t, ok, "changing a declared destination cannot move a saved secret")
	_, err = resolver.Candidate("cloud", map[string]string{"endpoint": "https://different.example", "token": "explicit-replacement"})
	require.NoError(t, err)
	require.NoError(t, cfg.UpdateApp("cloud", func(s *config.ServiceConfig) error { s.Runtime.API.Token = "${SOME_OTHER_OPERATOR_TOKEN}"; return nil }))
	_, err = resolver.Candidate("cloud", map[string]string{"endpoint": "https://different.example", "token": "ignored-candidate"})
	require.Error(t, err, "a managed form cannot ignore the submitted token")
}
