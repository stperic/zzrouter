package clientcli

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
)

type fakeResolvedAPI struct {
	provider, node, model string
	resolved              *pkgClient.ProviderResolved
}

func (f *fakeResolvedAPI) GetProviderResolved(provider, node, model string) (*pkgClient.ProviderResolved, error) {
	f.provider, f.node, f.model = provider, node, model
	return f.resolved, nil
}

func TestProvidersResolved_ShowsWhereEachValueComesFrom(t *testing.T) {
	sub, _, err := NewProvidersCmd().Find([]string{"resolved"})
	require.NoError(t, err)
	assert.Equal(t, "resolved <provider>", sub.Use)
	assert.NotNil(t, sub.Flags().Lookup("model"))
	assert.NotNil(t, sub.Flags().Lookup("node"))

	api := &fakeResolvedAPI{resolved: &pkgClient.ProviderResolved{
		Parameters: map[string]pkgClient.ResolvedValue{
			"chat-template-file": {Value: "agent.jinja", Tier: "model", Pattern: "qwen3.8-*", SHA256: "bfb7cc68aaaaaaaaaaaa"},
			"ctx-size":           {Value: "131072", Tier: "model", Model: "Qwen3.8-27B-Q8_0"},
		},
	}}
	cmd, out := newTestCmd("")
	require.NoError(t, runProviderResolved(cmd, api, "llamacpp", "worker-1", "Qwen3.8-27B-Q8_0"))
	assert.Equal(t, []string{"llamacpp", "worker-1", "Qwen3.8-27B-Q8_0"}, []string{api.provider, api.node, api.model})

	got := out.String()
	assert.Regexp(t, `chat-template-file\s+agent.jinja\s+model\s+qwen3.8-\*\s+bfb7cc68aaaa\n`, got)
	assert.Regexp(t, `ctx-size\s+131072\s+model\s+-\s+-\n`, got)
}
