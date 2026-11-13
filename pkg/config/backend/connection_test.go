package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stperic/common/secrets"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stretchr/testify/assert"
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

func TestExternalConnectionSurvivesReconciledMetadata(t *testing.T) {
	for _, tc := range []struct {
		name, provider, endpoint, credentialEnv, header, authorization string
		endpointOverride                                               string
		customCredential                                               bool
	}{
		{name: "azure literal endpoint", provider: "azure", endpoint: "https://literal.openai.azure.com/openai", credentialEnv: "AZURE_OPENAI_API_KEY", header: "Api-Key", authorization: "upgrade-fixture-token"},                                 //nolint:gosec // Shipped env-variable name and synthetic test token.
		{name: "azure inline default endpoint", provider: "azure", endpoint: "${AZURE_OPENAI_ENDPOINT:-https://inline.openai.azure.com/openai}", credentialEnv: "AZURE_OPENAI_API_KEY", header: "Api-Key", authorization: "upgrade-fixture-token"}, //nolint:gosec // Shipped env-variable name and synthetic test token.
		{name: "azure custom credential variable", provider: "azure", endpoint: "https://custom.openai.azure.com/openai", credentialEnv: "TEST_UPGRADE_AZURE_TOKEN", header: "Api-Key", authorization: "upgrade-fixture-token", customCredential: true},
		{name: "cloudflare literal account", provider: "cloudflare", endpoint: "https://api.cloudflare.com/client/v4/accounts/literal_account/ai", credentialEnv: "CLOUDFLARE_API_KEY", header: "Authorization", authorization: "Bearer upgrade-fixture-token"},                                                                                   //nolint:gosec // Shipped env-variable name and synthetic test token.
		{name: "bedrock custom region default", provider: "bedrock", endpoint: "${AWS_BEDROCK_ENDPOINT:-https://bedrock-runtime.eu-west-1.amazonaws.com}", credentialEnv: "AWS_BEDROCK_TOKEN", header: "Authorization", authorization: "Bearer upgrade-fixture-token"},                                                                            //nolint:gosec // Shipped env-variable name and synthetic test token.
		{name: "bedrock explicit endpoint wins", provider: "bedrock", endpoint: "${AWS_BEDROCK_ENDPOINT:-https://bedrock-runtime.eu-west-1.amazonaws.com}", credentialEnv: "AWS_BEDROCK_TOKEN", header: "Authorization", authorization: "Bearer upgrade-fixture-token", endpointOverride: "https://bedrock-runtime.ap-southeast-2.amazonaws.com"}, //nolint:gosec // Shipped env-variable name and synthetic test token.
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, env := range []string{"AZURE_OPENAI_ENDPOINT", "AZURE_OPENAI_API_KEY", "CLOUDFLARE_ACCOUNT_ID", "CLOUDFLARE_API_KEY", "TEST_UPGRADE_AZURE_TOKEN", "AWS_BEDROCK_ENDPOINT", "AWS_BEDROCK_TOKEN"} {
				t.Setenv(env, "")
			}
			t.Setenv("AWS_BEDROCK_ENDPOINT", tc.endpointOverride)
			t.Setenv(tc.credentialEnv, "upgrade-fixture-token")
			data, err := templates.AppsFS.ReadFile("files/providers/cloud/" + tc.provider + "/config.yaml")
			require.NoError(t, err)
			var legacy map[string]any
			require.NoError(t, yaml.Unmarshal(data, &legacy))
			delete(legacy, "connection")
			legacy["enabled"] = true
			runtime := legacy["runtime"].(map[string]any)
			runtime["endpoint"] = tc.endpoint
			if tc.customCredential {
				runtime["api"] = map[string]any{"auth_type": "custom", "custom_headers": map[string]string{"api-key": "${" + tc.credentialEnv + "}"}}
			}
			data, err = yaml.Marshal(legacy)
			require.NoError(t, err)
			dir := t.TempDir()
			path := filepath.Join(dir, "cloud", tc.provider, "config.yaml")
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
			require.NoError(t, os.WriteFile(path, data, 0o600))
			cfg, err := config.LoadAppsConfig(dir)
			require.NoError(t, err)
			vault, err := secrets.Open(filepath.Join(t.TempDir(), "connections.enc"), bytes.Repeat([]byte{3}, 32))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, vault.Close()) })
			resolver := NewResolver(func() *config.AppsConfig { return cfg }, vault)
			before, ok := resolver.Resolve(tc.provider)
			require.True(t, ok, "the customized provider must work before the upgrade")
			require.Equal(t, tc.authorization, before.Upstream.Header().Get(tc.header))
			changed, err := templates.ReconcileManagedSpine(dir)
			require.NoError(t, err)
			require.Contains(t, changed, "cloud/"+tc.provider+"/config.yaml")
			cfg, err = config.LoadAppsConfig(dir)
			require.NoError(t, err)
			svc, ok := cfg.LookupApp(tc.provider)
			require.True(t, ok)
			require.NotNil(t, svc.Connection, "reconcile must install the shipped required fields")
			require.Len(t, svc.Connection.Fields, 2)
			require.True(t, svc.Connection.Fields[1].Required)
			require.Equal(t, tc.endpoint, svc.Runtime.Endpoint, "operator runtime declarations survive reconciliation")
			target, ok := resolver.Resolve(tc.provider)
			require.True(t, ok, "new form metadata must not disable an external runtime declaration")
			assert.Equal(t, before.Endpoint, target.Endpoint)
			require.Equal(t, before.Upstream.Header(), target.Upstream.Header())
			direct, ok := Target(&svc)
			require.True(t, ok)
			assert.Equal(t, before.Endpoint, direct.Endpoint, "package-level consumers retain the same external authority")
			require.Equal(t, before.Upstream.Header(), direct.Upstream.Header())
			status, err := resolver.Connection(tc.provider)
			require.NoError(t, err)
			require.True(t, status.Configured)
			if tc.provider == "bedrock" {
				require.Equal(t, "https://bedrock-runtime.us-east-1.amazonaws.com", status.Fields[0].Default)
				require.Contains(t, status.Values, "endpoint", "an explicit empty value prevents clients prefilling a managed default")
				assert.Equal(t, tc.endpointOverride, status.Values["endpoint"])
				if tc.endpointOverride == "" {
					require.NotContains(t, status.Present, "endpoint")
				}
			}
			require.True(t, resolver.Available(tc.provider))
			t.Setenv(tc.credentialEnv, "")
			_, ok = resolver.Resolve(tc.provider)
			require.False(t, ok, "the runtime's actual missing credential must still refuse resolution")
			t.Setenv(tc.credentialEnv, "upgrade-fixture-token")
			_, err = vault.Remove(t.Context(), tc.provider, 0)
			require.NoError(t, err)
			target, ok = resolver.Target(tc.provider)
			require.True(t, ok)
			require.False(t, target.Configured)
			require.Empty(t, target.Upstream.Header(), "disabled mode must not restore external credentials")
			_, ok = resolver.Resolve(tc.provider)
			require.False(t, ok)
			require.False(t, resolver.Available(tc.provider))
		})
	}
}

func TestManagedConnectionStillRequiresDeclaredFields(t *testing.T) {
	t.Setenv("TEST_CONNECTION_ENDPOINT", "https://ambient.example")
	t.Setenv("TEST_CONNECTION_TOKEN", "ambient-token")
	cfg, vault, _, _ := connectionFixture(t)
	require.NoError(t, cfg.UpdateApp("cloud", func(svc *config.ServiceConfig) error {
		svc.Runtime.Endpoint = "https://literal.example"
		return nil
	}))
	resolver := NewResolver(func() *config.AppsConfig { return cfg }, vault)
	_, err := vault.Replace(t.Context(), "cloud", 0, map[string]string{ConnectionEndpointField: "https://literal.example", "token": "stored-token"})
	require.NoError(t, err)
	target, ok := resolver.Target("cloud")
	require.True(t, ok)
	require.False(t, target.Configured, "a saved managed record must remain complete even with a literal runtime endpoint")
	_, ok = resolver.Resolve("cloud")
	require.False(t, ok)
	_, err = resolver.Candidate("cloud", map[string]string{"token": "candidate-token"})
	require.ErrorContains(t, err, `field "endpoint" is required`)
	_, err = resolver.Candidate("cloud", map[string]string{"endpoint": "https://literal.example"})
	require.ErrorContains(t, err, `field "token" is required`)
}

func TestConnectionCandidateDeclaredDefaults(t *testing.T) {
	for _, tc := range []struct {
		name              string
		values            map[string]string
		endpoint, account string
	}{
		{name: "omitted", values: map[string]string{"token": "candidate-default-token"}, endpoint: "https://declared-default.example", account: "default_account"},
		{name: "blank", values: map[string]string{"endpoint": "", "account": "", "token": "candidate-default-token"}, endpoint: "https://declared-default.example", account: "default_account"},
		{name: "explicit", values: map[string]string{"endpoint": "https://explicit.example", "account": "explicit_account", "token": "candidate-default-token"}, endpoint: "https://explicit.example", account: "explicit_account"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TEST_CONNECTION_ENDPOINT", "https://ambient.example")
			t.Setenv("TEST_CONNECTION_ACCOUNT", "ambient_account")
			t.Setenv("TEST_CONNECTION_TOKEN", "ambient-default-token")
			cfg, vault, _, _ := connectionFixture(t)
			require.NoError(t, cfg.UpdateApp("cloud", func(svc *config.ServiceConfig) error {
				svc.Connection.Fields[0].Default = "https://declared-default.example"
				svc.Connection.Fields = append(svc.Connection.Fields, config.ConnectionField{ID: "account", Label: "Account", Type: "identifier", Env: "TEST_CONNECTION_ACCOUNT", Required: true, Default: "default_account"})
				svc.Runtime.API.CustomHeaders = map[string]string{"X-Account": "${TEST_CONNECTION_ACCOUNT}"}
				return nil
			}))
			resolver := NewResolver(func() *config.AppsConfig { return cfg }, vault)
			values := map[string]string{}
			for id, value := range tc.values {
				values[id] = value
			}
			before := cfg.Fingerprint()
			candidate, err := resolver.Candidate("cloud", values)
			require.NoError(t, err)
			require.Equal(t, tc.values, values, "candidate resolution must not mutate caller fields")
			require.Equal(t, tc.endpoint, candidate.Endpoint)
			require.Equal(t, tc.account, candidate.Upstream.Header().Get("X-Account"))
			require.Equal(t, "Bearer candidate-default-token", candidate.Upstream.Header().Get("Authorization"))
			require.True(t, candidate.Configured)
			stored := map[string]string{ConnectionEndpointField: candidate.Endpoint}
			for id, value := range values {
				stored[id] = value
			}
			_, err = vault.Replace(t.Context(), "cloud", 0, stored)
			require.NoError(t, err)
			persisted, ok := resolver.Target("cloud")
			require.True(t, ok)
			require.Equal(t, candidate.Endpoint, persisted.Endpoint)
			require.Equal(t, candidate.Upstream.Header(), persisted.Upstream.Header())
			status, err := resolver.Connection("cloud")
			require.NoError(t, err)
			require.Equal(t, tc.endpoint, status.Values["endpoint"])
			require.Equal(t, tc.account, status.Values["account"])
			require.Equal(t, before, cfg.Fingerprint())
		})
	}
}

func TestConnectionCandidateDefaultsPreserveRefusals(t *testing.T) {
	t.Setenv("TEST_CONNECTION_TOKEN", "ambient-default-token")
	cfg, vault, _, _ := connectionFixture(t)
	require.NoError(t, cfg.UpdateApp("cloud", func(svc *config.ServiceConfig) error {
		svc.Connection.Fields[0].Default = "https://declared-default.example"
		return nil
	}))
	resolver := NewResolver(func() *config.AppsConfig { return cfg }, vault)
	_, err := resolver.Candidate("cloud", nil)
	require.ErrorContains(t, err, `field "token" is required`, "defaults must not borrow an ambient credential")
	_, err = vault.Replace(t.Context(), "cloud", 0, map[string]string{ConnectionEndpointField: "https://declared-default.example", "token": "stored-default-token"})
	require.NoError(t, err)
	_, err = resolver.Candidate("cloud", map[string]string{"endpoint": ""})
	require.ErrorContains(t, err, `field "token" is required`, "defaults must not borrow the saved credential")
	_, err = resolver.Candidate("cloud", map[string]string{"token": "candidate-default-token", "undeclared": "value"})
	require.ErrorContains(t, err, "undeclared fields")
	_, err = resolver.Candidate("cloud", map[string]string{"token": "candidate-default-token", "endpoint": "http://remote.example"})
	require.ErrorContains(t, err, `field "endpoint" must be an HTTPS endpoint`)
}

func TestManagedOptionalCredentialsAreAuthoritativeAfterUpgrade(t *testing.T) {
	for _, tc := range []struct {
		name, authType, token, header string
	}{
		{name: "bearer", authType: config.AuthTypeBearer, token: "${HF_TOKEN:-retired-known-token}", header: "Authorization"},          //nolint:gosec // Synthetic inline fallback for the credential regression.
		{name: "alias", authType: config.AuthTypeBearer, token: "${HUGGING_FACE_TOKEN:-retired-known-token}", header: "Authorization"}, //nolint:gosec // Synthetic inline fallback for the credential regression.
		{name: "custom header", authType: config.AuthTypeCustom, header: "X-Token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			for _, name := range []string{"HF_TOKEN", "HUGGING_FACE_TOKEN", "TEST_MANAGED_UNKNOWN"} {
				t.Setenv(name, "")
			}
			data, err := templates.AppsFS.ReadFile("files/providers/registries/huggingface/config.yaml")
			require.NoError(t, err)
			var legacy map[string]any
			require.NoError(t, yaml.Unmarshal(data, &legacy))
			delete(legacy, "connection")
			runtime := legacy["runtime"].(map[string]any)
			headers := map[string]string{"Anthropic-Version": "2023-06-01", "X-Unknown": "${TEST_MANAGED_UNKNOWN:-retired-unknown-token}"}
			if tc.authType == config.AuthTypeCustom {
				headers["X-Token"] = "${HF_TOKEN:-retired-known-token}"
			}
			runtime["api"] = map[string]any{"auth_type": tc.authType, "token": tc.token, "custom_headers": headers}
			data, err = yaml.Marshal(legacy)
			require.NoError(t, err)
			dir := t.TempDir()
			path := filepath.Join(dir, "registries", "huggingface", "config.yaml")
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
			require.NoError(t, os.WriteFile(path, data, 0o600))
			changed, err := templates.ReconcileManagedSpine(dir)
			require.NoError(t, err)
			require.Contains(t, changed, "registries/huggingface/config.yaml")
			cfg, err := config.LoadAppsConfig(dir)
			require.NoError(t, err)
			vault, err := secrets.Open(filepath.Join(t.TempDir(), "connections.enc"), bytes.Repeat([]byte{6}, 32))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, vault.Close()) })
			resolver := NewResolver(func() *config.AppsConfig { return cfg }, vault)
			svc, ok := cfg.LookupApp("huggingface")
			require.True(t, ok)
			require.Len(t, svc.Connection.Fields, 1)
			require.False(t, svc.Connection.Fields[0].Required)
			external, ok := resolver.Target("huggingface")
			require.True(t, ok)
			require.Equal(t, tc.token, svc.Runtime.API.Token, "upgrade keeps the operator runtime declaration")
			require.True(t, external.Configured)
			expected := "retired-known-token"
			if tc.authType == config.AuthTypeBearer {
				expected = "Bearer " + expected
			}
			require.Equal(t, expected, external.Upstream.Header().Get(tc.header))
			require.Equal(t, "retired-unknown-token", external.Upstream.Header().Get("X-Unknown"))
			direct, ok := Target(&svc)
			require.True(t, ok)
			require.Equal(t, external.Upstream.Header(), direct.Upstream.Header())
			require.Equal(t, external.Upstream.Header(), Credential(svc.Runtime.API).Header())
			standalone := http.Header{}
			svc.Runtime.API.ApplyAuthHeaders(standalone)
			require.Equal(t, external.Upstream.Header(), standalone)
			require.True(t, svc.Runtime.API.HasCredentials())
			values := map[string]string{"token": ""}
			candidate, err := resolver.Candidate("huggingface", values)
			require.NoError(t, err)
			require.Equal(t, map[string]string{"token": ""}, values)
			_, err = vault.Replace(t.Context(), "huggingface", 0, map[string]string{ConnectionEndpointField: candidate.Endpoint, "token": ""})
			require.NoError(t, err)
			persisted, ok := resolver.Target("huggingface")
			require.True(t, ok)
			for name, target := range map[string]*Resolved{"candidate": candidate, "persisted": persisted} {
				assert.False(t, target.Configured, name)
				request, err := target.NewRequest(t.Context(), http.MethodGet, "/api/whoami-v2", nil)
				require.NoError(t, err)
				assert.Empty(t, request.Header.Get(tc.header), name+" must not restore the blank credential")
				assert.Empty(t, request.Header.Get("X-Unknown"), name+" has no undeclared fallback authority")
				assert.Equal(t, "2023-06-01", request.Header.Get("Anthropic-Version"), name+" preserves literal metadata")
			}
			status, err := resolver.Connection("huggingface")
			require.NoError(t, err)
			assert.False(t, status.Configured)
			require.NotContains(t, status.Present, "token")
			fresh, err := resolver.Candidate("huggingface", map[string]string{"token": "replacement-token"})
			require.NoError(t, err)
			expected = "replacement-token"
			if tc.authType == config.AuthTypeBearer {
				expected = "Bearer " + expected
			}
			assert.Equal(t, expected, fresh.Upstream.Header().Get(tc.header))
			assert.Empty(t, fresh.Upstream.Header().Get("X-Unknown"))
			assert.Equal(t, "2023-06-01", fresh.Upstream.Header().Get("Anthropic-Version"))
			_, err = vault.Remove(t.Context(), "huggingface", 1)
			require.NoError(t, err)
			disabled, ok := resolver.Target("huggingface")
			require.True(t, ok)
			require.False(t, disabled.Configured)
			require.Empty(t, disabled.Upstream.Header())
		})
	}
}
