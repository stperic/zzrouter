package clientcli

import (
	"testing"

	"github.com/stretchr/testify/assert"

	shared "github.com/stperic/zzrouter/internal/cli/clientcli/shared"
)

func TestProviderVersionCell(t *testing.T) {
	tests := []struct {
		name string
		in   shared.ProviderInfo
		want string
	}{
		{
			name: "newer shows the upgrade path",
			in:   shared.ProviderInfo{Version: "b10453", LatestVersion: "b10502", VersionStatus: "newer"},
			want: "b10453 -> b10502",
		},
		{
			name: "same is left alone",
			in:   shared.ProviderInfo{Version: "b10502", LatestVersion: "b10502", VersionStatus: "same"},
			want: "b10502",
		},
		{
			// An unchecked or failed lookup must not imply currency.
			name: "unknown is left alone",
			in:   shared.ProviderInfo{Version: "b10453", LatestVersion: "b10502", VersionStatus: "unknown"},
			want: "b10453",
		},
		{
			name: "no check run",
			in:   shared.ProviderInfo{Version: "0.19.0"},
			want: "0.19.0",
		},
		{
			name: "missing version",
			in:   shared.ProviderInfo{},
			want: "unknown",
		},
		{
			// Truncation applies only to the bare form; the annotated form
			// carries both versions and gets the wider column.
			name: "long bare version truncates",
			in:   shared.ProviderInfo{Version: "0.19.0-rc1+build.12345"},
			want: "0.19.0-rc1+b...",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, providerVersionCell(tt.in))
		})
	}
}
