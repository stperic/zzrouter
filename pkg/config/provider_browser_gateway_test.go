package config

import (
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestProviderBrowserGatewayIsolation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		gateway ProviderBrowserConfig
		auth    OAuthConfig
		invalid bool
	}{
		{name: "disabled"},
		{name: "HTTPS", gateway: ProviderBrowserConfig{Origin: "https://comfy.example.com"}, auth: OAuthConfig{IssuerURL: "https://admin.example.com"}},
		{name: "explicit loopback HTTP", gateway: ProviderBrowserConfig{Origin: "http://127.0.0.1:9092"}},
		{name: "IPv6 bind", gateway: ProviderBrowserConfig{Origin: "https://comfy.example.com", Listen: "[::1]:9092"}},
		{name: "same hostname different port", gateway: ProviderBrowserConfig{Origin: "http://localhost:9092"}, invalid: true},
		{name: "same trailing dot hostname", gateway: ProviderBrowserConfig{Origin: "https://admin.example.com."}, auth: OAuthConfig{IssuerURL: "https://admin.example.com"}, invalid: true},
		{name: "noncanonical origin", gateway: ProviderBrowserConfig{Origin: "https://Comfy.example.com"}, invalid: true},
		{name: "network HTTP", gateway: ProviderBrowserConfig{Origin: "http://comfy.example.com"}, invalid: true},
		{name: "origin path", gateway: ProviderBrowserConfig{Origin: "https://comfy.example.com/path"}, invalid: true},
		{name: "OAuth disabled", gateway: ProviderBrowserConfig{Origin: "https://comfy.example.com"}, auth: OAuthConfig{Enabled: new(false)}, invalid: true},
		{name: "network bind", gateway: ProviderBrowserConfig{Origin: "https://comfy.example.com", Listen: "0.0.0.0:9092"}, invalid: true},
		{name: "DNS bind", gateway: ProviderBrowserConfig{Origin: "https://comfy.example.com", Listen: "localhost:9092"}, invalid: true},
		{name: "ephemeral bind", gateway: ProviderBrowserConfig{Origin: "https://comfy.example.com", Listen: "127.0.0.1:0"}, invalid: true},
		{name: "listen without origin", gateway: ProviderBrowserConfig{Listen: "127.0.0.1:9092"}, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.gateway.Validate(tc.auth, ServeConfig{})
			if tc.invalid {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestProviderBrowserGatewayConfigRoundTrip(t *testing.T) {
	want := NodeConfig{Node: ServeConfig{Port: 9090}, Coordinator: CoordinatorConfig{ProviderBrowser: ProviderBrowserConfig{Origin: "https://comfy.example.com", Listen: "127.0.0.1:9192"}}}
	data, err := yaml.Marshal(want)
	require.NoError(t, err)
	var got NodeConfig
	require.NoError(t, yaml.Unmarshal(data, &got))
	require.Equal(t, want.Coordinator.ProviderBrowser, got.Coordinator.ProviderBrowser)
	require.NoError(t, got.Validate())
	got.Coordinator.ProviderBrowser.Listen = "0.0.0.0:9192"
	require.Error(t, got.Validate())
	got.Cluster.Mode = ClusterModeWorker
	require.NoError(t, got.Validate(), "workers ignore coordinator settings")
}
