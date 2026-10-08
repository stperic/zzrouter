package prov_apps

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/config"
)

// Upgrade must not resolve to the config pin.
//
// The pin seeds fresh installs. Resolving upgrade to it makes "upgrade" mean
// "reinstall what new nodes start on": a no-op when the node already matches,
// and a silent downgrade after an operator has moved a node past the pin.
func TestResolveUpgradeTargetPrefersExplicitVersion(t *testing.T) {
	c := &InstallCoordinator{}
	assert.Equal(t, "b10600", c.resolveUpgradeTarget(context.Background(), "llamacpp", "b10600"),
		"an explicit version always wins")
}

// With no upstream reachable and no config loaded, the fallback is the pin
// rather than an error: a worse answer beats refusing to upgrade offline.
func TestResolveUpgradeTargetFallsBackWhenUpstreamUnavailable(t *testing.T) {
	c := &InstallCoordinator{}
	got := c.resolveUpgradeTarget(context.Background(), "definitely-not-a-provider", "")
	assert.Empty(t, got, "no upstream and no pin resolves to empty, not a panic")
}

func TestResolvePinnedExplicitVersionOverridesReleaseSeed(t *testing.T) {
	cfg := &config.AppsConfig{}
	require.NoError(t, cfg.AddApp("vllm", pinnedProvider("on-demand", "0.29.0")))
	coordinator := &InstallCoordinator{appsConfig: func() *config.AppsConfig { return cfg }}
	assert.Equal(t, "0.29.0", coordinator.resolvePinned("vllm", ""))
	assert.Equal(t, "0.19.0", coordinator.resolvePinned("vllm", "0.19.0"))
}
