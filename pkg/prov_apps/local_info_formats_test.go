package prov_apps

import (
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
)

// formatsFor is what tells a node which model formats it can actually
// serve, so it decides both routing and GET /nodes/compatible. Cloud
// providers deliberately declare none: their "format" is the remote API.
func TestFormatsFor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		provider config.Provider
		want     []string
	}{
		{
			name:     "on-demand provider declares formats",
			provider: &config.OnDemandProvider{Capabilities: &config.AppCapabilities{Formats: []string{"safetensors", "pytorch"}}},
			want:     []string{"safetensors", "pytorch"},
		},
		{
			name:     "external provider declares formats",
			provider: &config.ExternalProvider{Capabilities: &config.AppCapabilities{Formats: []string{"gguf"}}},
			want:     []string{"gguf"},
		},
		{
			name:     "capabilities present but empty",
			provider: &config.OnDemandProvider{Capabilities: &config.AppCapabilities{}},
			want:     nil,
		},
		{
			name:     "no capabilities block",
			provider: &config.OnDemandProvider{},
			want:     nil,
		},
		{
			name:     "cloud provider declares no formats",
			provider: &config.CloudProvider{},
			want:     nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := formatsFor(tt.provider)
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("index %d: got %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// A cloud provider declaring no formats is what keeps it from making a
// node a candidate for a local model file: FormatSupported treats an
// empty format list as "serves nothing local". Pinning it because the
// previous helper read Capabilities off any provider kind, so this is a
// deliberate narrowing rather than an accident of the type switch.
func TestFormatsForCloudProviderCannotClaimLocalFormats(t *testing.T) {
	t.Parallel()

	// Even if a cloud provider somehow carried a capabilities block, its
	// formats must not surface: its "format" is the remote API.
	if got := formatsFor(&config.CloudProvider{}); got != nil {
		t.Fatalf("cloud provider declared formats: %v", got)
	}
}

// A returned slice must not alias the config: a caller sorting or
// appending to it would otherwise mutate the provider's declared formats.
func TestFormatsForDoesNotAliasConfig(t *testing.T) {
	t.Parallel()
	caps := &config.AppCapabilities{Formats: []string{"gguf"}}
	got := formatsFor(&config.OnDemandProvider{Capabilities: caps})
	got[0] = "mutated"
	if caps.Formats[0] != "gguf" {
		t.Fatalf("config was mutated through the returned slice: %v", caps.Formats)
	}
}
