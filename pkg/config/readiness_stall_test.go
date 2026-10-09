package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestReadinessStallConfigStrictLoad(t *testing.T) {
	raw, err := templates.AppsFS.ReadFile("files/providers/on-demand/vllm/config.yaml")
	require.NoError(t, err)
	for _, tc := range []struct {
		value string
		valid bool
	}{{"240s", true}, {"4m", true}, {"bad-duration", false}, {"-1s", false}, {"0s", false}} {
		t.Run(tc.value, func(t *testing.T) {
			body := []byte(strings.Replace(string(raw), "stall_timeout: 240s", "stall_timeout: "+tc.value, 1))
			dir := t.TempDir()
			providerDir := filepath.Join(dir, "on-demand", "vllm")
			require.NoError(t, os.MkdirAll(providerDir, 0700))
			require.NoError(t, os.WriteFile(filepath.Join(providerDir, "config.yaml"), body, 0600))
			cfg, err := loadAppsConfigFromDir(dir)
			if !tc.valid {
				require.ErrorContains(t, err, "stall_timeout")
				return
			}
			require.NoError(t, err)
			service, ok := cfg.LookupApp("vllm")
			require.True(t, ok)
			probe := service.Runtime.HealthCheck.ReadinessProbe
			duration, err := probe.StallDuration()
			require.NoError(t, err)
			require.Equal(t, 240*time.Second, duration)
			require.Equal(t, "900s", probe.Timeout)
			encoded, err := yaml.Marshal(cfg.providers["vllm"])
			require.NoError(t, err)
			decoded, err := DecodeProviderBytes(KindOnDemand, "vllm", encoded)
			require.NoError(t, err)
			require.Equal(t, tc.value, decoded.(*OnDemandProvider).Runtime.HealthCheck.ReadinessProbe.StallTimeout)
		})
	}
}

func TestReadinessStallAbsent(t *testing.T) {
	raw, err := templates.AppsFS.ReadFile("files/providers/on-demand/vllm/config.yaml")
	require.NoError(t, err)
	body := []byte(strings.Replace(string(raw), "      stall_timeout: 240s  # CUDA graph capture can be silent for over 100s.\n", "", 1))
	provider, err := DecodeProviderBytes(KindOnDemand, "vllm", body)
	require.NoError(t, err)
	require.NoError(t, provider.Validate())
	duration, err := provider.(*OnDemandProvider).Runtime.HealthCheck.ReadinessProbe.StallDuration()
	require.NoError(t, err)
	require.Zero(t, duration)
}
