package server

import (
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps"
)

// The rule decides which providers can make a node a candidate for a
// model, and it has failed in both directions before: counting a merely
// configured vllm let a Mac claim it could serve safetensors, and
// requiring routability made an Ollama whose version probe had not
// landed yet look absent, which 400s every pull to that node.
func TestCountsForCompatibility(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		info prov_apps.LocalProviderInfo
		want bool
	}{
		{
			name: "external provider counts even when its version is unknown",
			info: prov_apps.LocalProviderInfo{Kind: string(config.KindExternal), State: prov_apps.StateNotInstalled},
			want: true,
		},
		{
			name: "external provider counts when live",
			info: prov_apps.LocalProviderInfo{Kind: string(config.KindExternal), State: prov_apps.StateLive},
			want: true,
		},
		{
			name: "on-demand provider counts when installed and running",
			info: prov_apps.LocalProviderInfo{Kind: string(config.KindOnDemand), State: prov_apps.StateLive},
			want: true,
		},
		{
			name: "on-demand provider counts when installed but stopped",
			info: prov_apps.LocalProviderInfo{Kind: string(config.KindOnDemand), State: prov_apps.StateDormant},
			want: true,
		},
		{
			name: "on-demand provider configured but never installed does not count",
			info: prov_apps.LocalProviderInfo{Kind: string(config.KindOnDemand), State: prov_apps.StateNotInstalled},
			want: false,
		},
		{
			name: "on-demand provider mid-install does not count yet",
			info: prov_apps.LocalProviderInfo{Kind: string(config.KindOnDemand), State: prov_apps.StateInstalling},
			want: false,
		},
		{
			// A cloud provider cannot hold a model file, and this rule
			// also gates downloads. Excluded explicitly so the outcome
			// does not depend on cloud configs happening to declare no
			// formats.
			name: "cloud provider never counts for a local model",
			info: prov_apps.LocalProviderInfo{Kind: string(config.KindCloud), State: prov_apps.StateCloudAvailable},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := countsForCompatibility(tt.info); got != tt.want {
				t.Errorf("countsForCompatibility(%s/%s) = %v, want %v", tt.info.Kind, tt.info.State, got, tt.want)
			}
		})
	}
}
