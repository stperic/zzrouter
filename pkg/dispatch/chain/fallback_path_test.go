package chain

import (
	"testing"

	"github.com/stperic/zzrouter/pkg/config/backend"
	pfallback "github.com/stperic/zzrouter/pkg/fallback"
	"github.com/stretchr/testify/require"
)

type mappedCloudDeps struct {
	*fakeDeps
	target *backend.Resolved
}

func (d *mappedCloudDeps) Backend(string) (*backend.Resolved, bool) { return d.target, true }

func TestFallbackUsesProviderInferencePath(t *testing.T) {
	for _, cloud := range []bool{false, true} {
		deps := &mappedCloudDeps{fakeDeps: newFakeDeps(), target: &backend.Resolved{Endpoint: "https://fixture.test", OpenAIPath: "/compatible", Cloud: cloud}}
		proxy := &Proxy{deps: deps}
		target, _, _, err := proxy.resolveDeploymentTarget(t.Context(), pfallback.Candidate{App: "custom", Model: "m"}, "/v1/chat/completions", "trace=fixture", "chat")
		require.NoError(t, err)
		require.Equal(t, "https://fixture.test/compatible/chat/completions?trace=fixture", target)
	}
}
