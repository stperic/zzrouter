package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOAuthIssuerValidation(t *testing.T) {
	for _, issuer := range []string{"", "https://router.example.com", "http://127.0.0.1:9090", "http://localhost:9090", "http://[::1]:9090"} {
		require.NoError(t, (OAuthConfig{IssuerURL: issuer}).Validate(), issuer)
	}
	for _, issuer := range []string{"http://router.example.com", "https://router.example.com/", "https://router.example.com/path", "https://user:password@router.example.com", "https://router.example.com?x=1", "https://router.example.com#fragment", "//router.example.com", "http://0.0.0.0:9090", "https://Router.Example.com", "https://router.example.com:443", "http://localhost:80", "https://router.example.com:0443", "HTTPS://router.example.com", "http://[0:0:0:0:0:0:0:1]:9090", "https://router.example.com:", "https://routér.example.com", "https://127.1", "https://router.example.com?", "https://router.example.com#", "https://0x7f000001", "https://127.0.0.01", "https://127.0.0.1."} {
		require.Error(t, (OAuthConfig{IssuerURL: issuer}).Validate(), issuer)
	}
}

func TestOAuthDefaultIssuer(t *testing.T) {
	for _, tc := range []struct {
		name    string
		oauth   OAuthConfig
		node    ServeConfig
		want    string
		invalid bool
	}{
		{name: "unset config", want: "http://localhost:9090"},
		{name: "configured port", node: ServeConfig{Port: 9199}, want: "http://localhost:9199"},
		{name: "network bind still local", node: ServeConfig{Bind: "0.0.0.0", Port: 9199}, want: "http://localhost:9199"},
		{name: "TLS", node: ServeConfig{Port: 9443, TLSCert: "cert", TLSKey: "key"}, want: "https://localhost:9443"},
		{name: "standard HTTP port", node: ServeConfig{Port: 80}, want: "http://localhost"},
		{name: "standard HTTPS port", node: ServeConfig{Port: 443, TLSCert: "cert", TLSKey: "key"}, want: "https://localhost"},
		{name: "explicit enabled", oauth: OAuthConfig{Enabled: new(true)}, want: "http://localhost:9090"},
		{name: "explicit issuer", oauth: OAuthConfig{IssuerURL: "https://router.example.com"}, node: ServeConfig{Port: 9199}, want: "https://router.example.com"},
		{name: "opt out", oauth: OAuthConfig{Enabled: new(false)}},
		{name: "opt out with staged issuer", oauth: OAuthConfig{Enabled: new(false), IssuerURL: "http://network.example.com"}},
		{name: "invalid explicit issuer", oauth: OAuthConfig{IssuerURL: "http://network.example.com"}, invalid: true},
		{name: "invalid configured port", node: ServeConfig{Port: 65536}, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issuer, err := tc.oauth.EffectiveIssuer(tc.node)
			if tc.invalid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, issuer)
		})
	}
}
