package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestArchitectureFiltering(t *testing.T) {
	candidates := map[string][]string{"llamacpp": nil, "strata": {"qwen4exp"}, "other": {"other"}}
	for _, tc := range []struct {
		name, architecture string
		allowUnknown       bool
		want               []string
	}{
		{"declaring provider shadows", "qwen4exp", false, []string{"strata"}},
		{"unclaimed architecture", "qwen3", false, []string{"llamacpp"}},
		{"unknown auto selection", "", false, []string{"llamacpp"}},
		{"unknown explicit or storage", "", true, []string{"llamacpp", "other", "strata"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, FilterArchitectures(candidates, tc.architecture, tc.allowUnknown))
		})
	}
}

func TestArchitectureCapabilitiesValidation(t *testing.T) {
	caps := &AppCapabilities{Architectures: []string{"qwen4exp"}}
	require.Error(t, caps.ValidateArchitectures())
	caps.Formats = []string{"gguf"}
	require.NoError(t, caps.ValidateArchitectures())
	provider := &OnDemandProvider{RunScope: RunScopeNode, Capabilities: caps}
	require.Error(t, provider.validateRunScope())
	caps.Formats = nil
	require.Error(t, provider.validateRunScope(), "architectures alone are model capabilities")
}
