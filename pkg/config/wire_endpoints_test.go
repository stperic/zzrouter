package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
