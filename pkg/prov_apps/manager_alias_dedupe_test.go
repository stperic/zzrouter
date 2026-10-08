package prov_apps

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/config"
)

// aliasTestConfig is a provider whose args never reference
// ${MODEL_PATH}, so a launch does not have to find real weights on the
// machine running the test. What is under test here is which name the
// instance is keyed by, not whether the model exists.
func aliasTestConfig(t *testing.T) *config.AppsConfig {
	t.Helper()
	cfg := &config.AppsConfig{}
	require.NoError(t, cfg.AddApp("fake", config.ServiceConfig{
		Enabled:  new(true),
		Name:     "Fake",
		Protocol: config.ProtocolOpenAI,
		Mode:     "on-demand",
		Runtime: &config.AppRuntimeConfig{
			PortRange: []int{8200, 8205},
			BasePort:  8200,
			KeepAlive: "1m",
			Execution: config.ExecutionConfig{
				Type: "cli",
				// Exits immediately: the launch only has to register an
				// instance, and a child that lingers would make Stop
				// wait out its lifetime on every run.
				Command: "sleep",
				Args:    []string{"0"},
			},
			HealthCheck: config.HealthcheckConfig{Path: "/health"},
		},
		Capabilities: &config.AppCapabilities{WireEndpoints: []string{"chat_completions"}},
	}))
	return cfg
}

// One model is reachable by several names: the repo it came from
// ("nomic-ai/nomic-embed-text-v1.5-GGUF") and the file it landed in
// ("nomic-embed-text-v1.5.Q4_K_M"). The instance registry is keyed by
// whichever name the launch used, and until this fix only the inference
// path resolved aliases -- so launching by repo id and then serving a
// request for that same repo id started the model a second time, under
// its file stem, with both copies resident on the node at once.
//
// Canonicalizing inside LaunchInstance is what makes the two agree; the
// assertion that matters is the instance count, not the name.
func TestLaunchInstance_AliasFormsShareOneInstance(t *testing.T) {
	const (
		repoID    = "nomic-ai/nomic-embed-text-v1.5-GGUF"
		fileStem  = "nomic-embed-text-v1.5.Q4_K_M"
		otherName = "some-other-model"
	)

	m, err := NewProviderAppManager(aliasTestConfig(t),
		WithNodename(func() string { return "node1" }),
		WithModelCanonicalizer(func(name string) string {
			if name == repoID {
				return fileStem
			}
			return name
		}),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = m.Stop(context.Background()) })

	byRepo, err := m.LaunchInstance(context.Background(), LaunchRequest{
		Provider: "fake", Model: repoID,
	})
	require.NoError(t, err)
	require.NotNil(t, byRepo)

	byStem, err := m.LaunchInstance(context.Background(), LaunchRequest{
		Provider: "fake", Model: fileStem,
	})
	require.NoError(t, err)
	require.NotNil(t, byStem)

	assert.Equal(t, byRepo.ID, byStem.ID,
		"the same model under two names produced two instances (%s and %s)", byRepo.ID, byStem.ID)
	assert.Equal(t, fileStem, byRepo.Model,
		"the instance should be registered under the canonical name")

	var count int
	for _, info := range m.ListInstances("node1") {
		if info.Model == fileStem || info.Model == repoID {
			count++
		}
	}
	assert.Equal(t, 1, count, "one model, %d instances resident", count)
}

// A canonicalizer that finds nothing must not blank the name it was
// given. Returning "" for an unknown model is the normal cache miss,
// and launching an instance whose model is the empty string would be a
// worse failure than the duplicate this fix exists to stop.
func TestLaunchInstance_EmptyCanonicalLeavesTheNameAlone(t *testing.T) {
	m, err := NewProviderAppManager(aliasTestConfig(t),
		WithNodename(func() string { return "node1" }),
		WithModelCanonicalizer(func(string) string { return "" }),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = m.Stop(context.Background()) })

	inst, err := m.LaunchInstance(context.Background(), LaunchRequest{
		Provider: "fake", Model: "unknown-to-the-cache",
	})
	require.NoError(t, err)
	require.NotNil(t, inst)
	assert.Equal(t, "unknown-to-the-cache", inst.Model)
}

// Launching canonicalizes the name, so the registry holds the instance
// under the file stem. A caller who launched by repo id has only that
// name to ask with, and asking has to work: otherwise the run it just
// started is invisible to it.
func TestGetInstanceByModel_FindsAnInstanceByTheNameItWasLaunchedWith(t *testing.T) {
	const (
		repoID   = "nomic-ai/nomic-embed-text-v1.5-GGUF"
		fileStem = "nomic-embed-text-v1.5.Q4_K_M"
	)

	m, err := NewProviderAppManager(aliasTestConfig(t),
		WithNodename(func() string { return "node1" }),
		WithModelCanonicalizer(func(name string) string {
			if name == repoID {
				return fileStem
			}
			return name
		}),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = m.Stop(context.Background()) })

	launched, err := m.LaunchInstance(context.Background(), LaunchRequest{
		Provider: "fake", Model: repoID,
	})
	require.NoError(t, err)
	require.NotNil(t, launched)

	byRepo, ok := m.GetInstanceByModel(repoID)
	require.True(t, ok, "the model is not findable under the name it was launched with")
	assert.Equal(t, launched.ID, byRepo.ID)

	byStem, ok := m.GetInstanceByModel(fileStem)
	require.True(t, ok, "the model is not findable under its canonical name")
	assert.Equal(t, launched.ID, byStem.ID)

	if _, ok := m.GetInstanceByModel("never-launched"); ok {
		t.Error("a model that was never launched reported an instance")
	}
}
