package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// auth_type is a closed set. A typo used to load and then behave as
// bearer, sending a credential the operator never meant to configure.
func TestAPIConfigValidate(t *testing.T) {
	cases := []struct {
		name string
		api  *APIConfig
		ok   bool
	}{
		{"absent", nil, true},
		{"empty is bearer", &APIConfig{Token: "${K}"}, true},
		{"bearer", &APIConfig{AuthType: AuthTypeBearer, Token: "${K}"}, true},
		{"api-key", &APIConfig{AuthType: AuthTypeAPIKey, AuthHeader: "x-api-key", Token: "${K}"}, true},
		{"custom", &APIConfig{AuthType: AuthTypeCustom, CustomHeaders: map[string]string{"X-K": "${K}"}}, true},
		{"none", &APIConfig{AuthType: AuthTypeNone}, true},
		{"caller", &APIConfig{AuthType: AuthTypeCaller}, true},
		{"unknown", &APIConfig{AuthType: "beare"}, false},
		{"none with a token", &APIConfig{AuthType: AuthTypeNone, Token: "${K}"}, false},
		{"caller with a header", &APIConfig{AuthType: AuthTypeCaller, CustomHeaders: map[string]string{"X-K": "v"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.api.Validate()
			if tc.ok {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
			}
		})
	}
}

// Both kinds of provider that carry an api block reject what it cannot
// mean for them; a cloud provider has to send a credential of its own.
func TestProvidersValidateAuthType(t *testing.T) {
	ext := &ExternalProvider{Name: "e", Protocol: ProtocolOpenAI,
		Capabilities: &AppCapabilities{WireEndpoints: []string{"chat_completions"}},
		Runtime:      ExternalRuntime{Endpoint: "http://h:1", API: &APIConfig{AuthType: "beare"}}}
	assert.ErrorContains(t, ext.Validate(), "auth_type")

	for _, at := range []string{AuthTypeNone, AuthTypeCaller} {
		cloud := &CloudProvider{Name: "c",
			Runtime: CloudRuntime{Endpoint: "https://h", API: APIConfig{AuthType: at}}}
		assert.ErrorContains(t, cloud.Validate(), "own credential", at)
	}
}
