package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNodeGPUEvictionPolicyRoundTrip(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "default_off", true: "opt_in"}[enabled], func(t *testing.T) {
			cfg := &NodeConfig{}
			assert.False(t, cfg.GPU.EvictIdleOnMemoryPressure)
			cfg.GPU.EvictIdleOnMemoryPressure = enabled
			store := NewNodeConfigStoreFromConfig(cfg)
			store.dir = t.TempDir()
			require.NoError(t, store.cm.SaveNodeConfigToDir(cfg, store.dir))
			loaded, err := store.cm.LoadNodeConfigFromDir(store.dir)
			require.NoError(t, err)
			assert.Equal(t, enabled, loaded.GPU.EvictIdleOnMemoryPressure)
		})
	}
}
