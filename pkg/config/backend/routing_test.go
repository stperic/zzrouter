package backend

import (
	"net/http"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stretchr/testify/require"
)

func TestShippedCloudRoutingAndDeclaredCatalogs(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, templates.InstallDefaults(dir))
	cfg, err := config.LoadAppsConfig(dir)
	require.NoError(t, err)
	r := NewResolver(func() *config.AppsConfig { return cfg })
	for _, tc := range []struct {
		name, endpoint, chat, models, probe string
	}{
		{"gemini", "", "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions", "/models", "/models"},
		{"bedrock", "", "https://bedrock-runtime.us-east-1.amazonaws.com/openai/v1/chat/completions", "", ""},
		{"azure", "https://fixture.openai.azure.com/openai/v1/", "https://fixture.openai.azure.com/openai/v1/chat/completions", "/deployments?api-version=2023-03-15-preview", "/models"},
		{"openai", "", "https://api.openai.com/v1/chat/completions", "/v1/models", "/v1/models"},
		{"openrouter", "", "https://openrouter.ai/api/v1/chat/completions", "/v1/models", "/v1/models"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			values := map[string]string{"api_key": "fixture-token"}
			if tc.endpoint != "" {
				values["endpoint"] = tc.endpoint
			}
			target, err := r.Candidate(tc.name, values)
			require.NoError(t, err)
			require.Equal(t, tc.chat+"?trace=fixture", target.InferenceTarget("/v1/chat/completions", "trace=fixture"))
			require.Equal(t, tc.models, target.ModelListPath())
			require.Equal(t, tc.probe, target.TestPath)
			require.Equal(t, target.Endpoint+"/api/chat", target.InferenceTarget("/api/chat", ""))
			require.Equal(t, target.Endpoint+"/v10/models", target.InferenceTarget("/v10/models", ""))
			req, err := target.NewRequest(t.Context(), http.MethodGet, "/native/resource", nil)
			require.NoError(t, err)
			require.Equal(t, target.Endpoint+"/native/resource", req.URL.String(), "native calls do not reinterpret provider paths")
		})
	}
}

func TestCloudRoutingShapeSurvivesRoundTripAndChangesRevision(t *testing.T) {
	cfg := &config.AppsConfig{}
	require.NoError(t, cfg.AddApp("custom", config.ServiceConfig{
		Mode: "cloud", Enabled: new(true), Protocol: config.ProtocolOpenAI,
		Runtime:      &config.AppRuntimeConfig{Endpoint: "https://example.test", OpenAIPath: "/compatible", ModelsPath: new(""), API: &config.APIConfig{AuthType: config.AuthTypeBearer, Token: "fixture"}},
		Capabilities: &config.AppCapabilities{WireEndpoints: []string{"chat_completions"}},
	}))
	r := NewResolver(func() *config.AppsConfig { return cfg })
	before, ok := r.Resolve("custom")
	require.True(t, ok)
	require.Equal(t, "", before.ModelListPath())
	require.Equal(t, "", before.TestPath)
	require.Equal(t, "https://example.test/compatible/chat/completions", before.InferenceTarget("/v1/chat/completions", ""))
	require.NoError(t, cfg.UpdateApp("custom", func(s *config.ServiceConfig) error {
		s.Description = "round trip"
		return nil
	}))
	after, ok := r.Resolve("custom")
	require.True(t, ok)
	require.NotNil(t, after.ModelsPath)
	require.Empty(t, after.ModelListPath())
	require.Equal(t, before.Revision, after.Revision)
	require.NoError(t, cfg.UpdateApp("custom", func(s *config.ServiceConfig) error {
		s.Runtime.OpenAIPath = "/other"
		return nil
	}))
	after, ok = r.Resolve("custom")
	require.True(t, ok)
	require.NotEqual(t, before.Revision, after.Revision, "saved response owners cannot follow a changed API path")
}

func TestCloudRoutingPathValidation(t *testing.T) {
	for _, prefix := range []string{"relative", "//elsewhere.test", "/x/../y", "/x?query=1", "/x?", "/x#fragment", "/x#", "/%2Fv1", "/x//y"} {
		t.Run(prefix, func(t *testing.T) {
			p := config.CloudProvider{Name: "fixture", Enabled: new(false), Runtime: config.CloudRuntime{Endpoint: "https://example.test", OpenAIPath: prefix, API: config.APIConfig{AuthType: config.AuthTypeBearer}}}
			require.ErrorContains(t, p.Validate(), "openai_path")
		})
	}
}
